package azure

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap/zaptest"

	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/metrics"
)

// TestPhase8_Integration_ARMRecorder_PrometheusRegistry_RetriesAndRequests
// Integration: Azure HTTP client → retry logic → real ARMRecorder → prometheus.Registry
//
// Verifies that when the ARM client encounters retries (500 → 500 → 200), the
// real ARMRecorder (implementing ARMObserver + ARMRetryObserver) correctly
// populates a prometheus registry with:
//   - arm_requests_total with per-attempt status codes
//   - arm_request_duration_seconds histogram observations
//   - arm_retries_total with retry reasons
func TestPhase8_Integration_ARMRecorder_PrometheusRegistry_RetriesAndRequests(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	// Set up prometheus registry with real ARMRecorder
	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	// HTTP server: 2 failures then success
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"error":{"code":"InternalServerError","message":"transient %d"}}`, n)
			return
		}
		w.Header().Set("ETag", `"etag-success"`)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"name":"test-ps","properties":{"addressPrefixes":["10.0.0.1/32"]}}`)
	}))
	defer server.Close()

	log := zaptest.NewLogger(t)

	// Wire real ARMRecorder as the observer (matching production wiring in cmd/main.go)
	factory := NewClientFactoryWithCredential(log, nil, server.Client(),
		WithFactoryARMBaseURL(server.URL),
		WithFactoryRetryPolicy(RetryPolicy{MaxRetries: 5, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}),
		WithFactoryARMRecorder(rec.ARM),
	)

	executor := NewExecutor(log, factory, 1, WithExecutorRetryMetrics(rec.ARM))

	actions := []engine.Action{
		{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub-integ",
				ResourceGroup:  "rg-integ",
				ASGName:        "asg-integ",
				PrefixSetName:  "ps-integ",
			},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("expected success, got error: %v", results[0].Err)
	}

	// Gather from prometheus registry and verify metrics
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}

	// Verify arm_requests_total: should have 3 observations (2x500 + 1x200)
	requestsTotal := findMetricFamily(mfs, "pod_nsg_controller_arm_requests_total")
	if requestsTotal == nil {
		t.Fatal("arm_requests_total not found in registry")
	}
	totalRequests := sumAllCounters(requestsTotal)
	if totalRequests < 3 {
		t.Errorf("arm_requests_total sum = %v, want >= 3 (2 failures + 1 success)", totalRequests)
	}

	// Verify 500 status code counter
	status500 := findCounterWithLabels(requestsTotal, map[string]string{
		"subscription_id": "sub-integ",
		"status_code":     "500",
	})
	if status500 < 2 {
		t.Errorf("arm_requests_total{status_code=500} = %v, want >= 2", status500)
	}

	// Verify 200 status code counter
	status200 := findCounterWithLabels(requestsTotal, map[string]string{
		"subscription_id": "sub-integ",
		"status_code":     "200",
	})
	if status200 < 1 {
		t.Errorf("arm_requests_total{status_code=200} = %v, want >= 1", status200)
	}

	// Verify arm_request_duration_seconds histogram has observations
	durationHist := findMetricFamily(mfs, "pod_nsg_controller_arm_request_duration_seconds")
	if durationHist == nil {
		t.Fatal("arm_request_duration_seconds not found in registry")
	}
	durationCount := sumAllHistogramCounts(durationHist)
	if durationCount < 3 {
		t.Errorf("arm_request_duration_seconds sample_count = %d, want >= 3", durationCount)
	}

	// Verify arm_retries_total: 2 server-error retries
	retriesTotal := findMetricFamily(mfs, "pod_nsg_controller_arm_retries_total")
	if retriesTotal == nil {
		t.Fatal("arm_retries_total not found in registry")
	}
	serverErrorRetries := findCounterWithLabels(retriesTotal, map[string]string{
		"subscription_id": "sub-integ",
		"retry_reason":    "server-error",
	})
	if serverErrorRetries < 2 {
		t.Errorf("arm_retries_total{retry_reason=server-error} = %v, want >= 2", serverErrorRetries)
	}
}

// TestPhase8_Integration_ARMRecorder_PrometheusRegistry_429RetryAfter
// Integration: Azure HTTP client → 429 with Retry-After → ARMRecorder → prometheus.Registry
//
// Verifies the retry_reason=429-retry-after label propagates through the real
// prometheus metrics pipeline.
func TestPhase8_Integration_ARMRecorder_PrometheusRegistry_429RetryAfter(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"error":{"code":"TooManyRequests","message":"throttled"}}`)
			return
		}
		w.Header().Set("ETag", `"etag-ok"`)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"name":"ps","properties":{"addressPrefixes":["10.0.1.1/32"]}}`)
	}))
	defer server.Close()

	log := zaptest.NewLogger(t)

	factory := NewClientFactoryWithCredential(log, nil, server.Client(),
		WithFactoryARMBaseURL(server.URL),
		WithFactoryRetryPolicy(RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}),
		WithFactoryARMRecorder(rec.ARM),
	)

	executor := NewExecutor(log, factory, 1, WithExecutorRetryMetrics(rec.ARM))

	actions := []engine.Action{
		{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub-429",
				ResourceGroup:  "rg-429",
				ASGName:        "asg-429",
				PrefixSetName:  "ps-429",
			},
			DesiredIPs: []string{"10.0.1.1/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("expected success, got: %+v", results)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}

	// Verify arm_retries_total{retry_reason=429-retry-after}
	retriesTotal := findMetricFamily(mfs, "pod_nsg_controller_arm_retries_total")
	if retriesTotal == nil {
		t.Fatal("arm_retries_total not found in registry")
	}
	throttleRetries := findCounterWithLabels(retriesTotal, map[string]string{
		"subscription_id": "sub-429",
		"retry_reason":    "429-retry-after",
	})
	if throttleRetries < 1 {
		t.Errorf("arm_retries_total{retry_reason=429-retry-after} = %v, want >= 1", throttleRetries)
	}

	// Verify arm_requests_total has 429 and 200
	requestsTotal := findMetricFamily(mfs, "pod_nsg_controller_arm_requests_total")
	if requestsTotal == nil {
		t.Fatal("arm_requests_total not found")
	}
	if findCounterWithLabels(requestsTotal, map[string]string{"status_code": "429"}) < 1 {
		t.Error("arm_requests_total{status_code=429} not recorded")
	}
	if findCounterWithLabels(requestsTotal, map[string]string{"status_code": "200"}) < 1 {
		t.Error("arm_requests_total{status_code=200} not recorded")
	}
}

// TestPhase8_Integration_ARMRecorder_RateLimiter_PrometheusRegistry
// Integration: ARMRateLimiter → real ARMRecorder → prometheus.Registry
//
// Verifies that rate limiter delays are recorded in the prometheus registry
// when the real ARMRecorder is used as the observer.
func TestPhase8_Integration_ARMRecorder_RateLimiter_PrometheusRegistry(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	log := zaptest.NewLogger(t)

	// Very low RPS to force delays on the second call
	limiter := NewARMRateLimiter(log, 1.0,
		WithRateLimitMetrics(rec.ARM),
	)

	ctx := context.Background()

	// First call: no delay (within burst)
	if err := limiter.Wait(ctx, "sub-rl"); err != nil {
		t.Fatalf("first Wait() failed: %v", err)
	}

	// Second call: should be rate-limited
	if err := limiter.Wait(ctx, "sub-rl"); err != nil {
		t.Fatalf("second Wait() failed: %v", err)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}

	// arm_rate_limit_delays_total should have at least 1 observation
	delaysTotal := findMetricFamily(mfs, "pod_nsg_controller_arm_rate_limit_delays_total")
	if delaysTotal == nil {
		t.Fatal("arm_rate_limit_delays_total not found in registry")
	}
	delayCount := findCounterWithLabels(delaysTotal, map[string]string{"subscription_id": "sub-rl"})
	if delayCount < 1 {
		t.Errorf("arm_rate_limit_delays_total{subscription_id=sub-rl} = %v, want >= 1", delayCount)
	}

	// arm_rate_limit_delay_seconds should have positive observations
	delayHist := findMetricFamily(mfs, "pod_nsg_controller_arm_rate_limit_delay_seconds")
	if delayHist == nil {
		t.Fatal("arm_rate_limit_delay_seconds not found in registry")
	}
	histCount := sumAllHistogramCounts(delayHist)
	if histCount < 1 {
		t.Errorf("arm_rate_limit_delay_seconds sample_count = %d, want >= 1", histCount)
	}
}

// --- helpers ---

func findMetricFamily(mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

func findCounterWithLabels(mf *dto.MetricFamily, filter map[string]string) float64 {
	for _, m := range mf.GetMetric() {
		if matchMetricLabels(m, filter) && m.GetCounter() != nil {
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

func matchMetricLabels(m *dto.Metric, filter map[string]string) bool {
	labels := make(map[string]string)
	for _, l := range m.GetLabel() {
		labels[l.GetName()] = l.GetValue()
	}
	for k, v := range filter {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func sumAllCounters(mf *dto.MetricFamily) float64 {
	var total float64
	for _, m := range mf.GetMetric() {
		if m.GetCounter() != nil {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

func sumAllHistogramCounts(mf *dto.MetricFamily) uint64 {
	var total uint64
	for _, m := range mf.GetMetric() {
		if m.GetHistogram() != nil {
			total += m.GetHistogram().GetSampleCount()
		}
	}
	return total
}
