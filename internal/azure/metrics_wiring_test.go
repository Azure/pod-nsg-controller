package azure

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/metrics"
)

// TestPhase8_T83T85_DoRequest_RecordsEveryAttemptIncludingRetries
// T8.3 + T8.5: ARM PUT succeeds after retries → arm_requests_total incremented per attempt.
func TestPhase8_T83T85_DoRequest_RecordsEveryAttemptIncludingRetries(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n <= 3 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"error":{"code":"InternalServerError","message":"transient"}}`)
			return
		}
		w.Header().Set("ETag", `"test-etag-1"`)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"name":"test","properties":{"addressPrefixes":["10.0.0.1/32"]}}`)
	}))
	defer server.Close()

	log := zaptest.NewLogger(t)
	obs := &fakeARMObserver{}
	retryObs := &fakeARMRetryObserver{}

	client := NewAddressPrefixSetClient(
		log,
		nil, // no credential (test mode)
		server.Client(),
		WithARMBaseURL(server.URL),
		WithARMMetrics(obs, retryObs),
		WithRetryPolicy(RetryPolicy{MaxRetries: 5, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}),
	)

	_, err := client.Get(context.Background(), "sub-1", "rg-1", "asg-1", "prefix-set-1")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	// 4 total attempts (3 failures + 1 success)
	if obs.requestCount != 4 {
		t.Errorf("arm_requests_total observation count = %d, want 4", obs.requestCount)
	}

	// 3 retries with reason "server-error"
	if retryObs.retryCount != 3 {
		t.Errorf("arm_retries_total count = %d, want 3", retryObs.retryCount)
	}
	for i, reason := range retryObs.reasons {
		if reason != "server-error" {
			t.Errorf("retry[%d] reason = %q, want %q", i, reason, "server-error")
		}
	}
}

// TestPhase8_DoRequest_TransportErrorRecordedAsStatusZero
// Transport failures (no HTTP response) should emit arm_requests_total with status_code="0".
func TestPhase8_DoRequest_TransportErrorRecordedAsStatusZero(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	log := zaptest.NewLogger(t)
	obs := &fakeARMObserver{}
	retryObs := &fakeARMRetryObserver{}

	// Use a URL that will cause a transport failure
	client := NewAddressPrefixSetClient(
		log,
		nil,
		&http.Client{Timeout: 10 * time.Millisecond},
		WithARMBaseURL("http://192.0.2.1:1"), // RFC 5737 TEST-NET, should fail
		WithARMMetrics(obs, retryObs),
		WithRetryPolicy(RetryPolicy{MaxRetries: 0, NetworkMaxRetries: 0}),
	)

	_, err := client.Get(context.Background(), "sub-1", "rg-1", "asg-1", "prefix-set-1")
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}

	// Should have recorded the attempt with status_code="0"
	if obs.requestCount != 1 {
		t.Errorf("request observation count = %d, want 1", obs.requestCount)
	}
	if len(obs.statusCodes) > 0 && obs.statusCodes[0] != "0" {
		t.Errorf("status_code = %q, want %q", obs.statusCodes[0], "0")
	}
}

// TestPhase8_DoRequest_PreRequestFailuresDoNotEmitRequestMetric
// Pre-request failures (rate limiter denied, context cancelled) should NOT emit request metrics.
func TestPhase8_DoRequest_PreRequestFailuresDoNotEmitRequestMetric(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	log := zaptest.NewLogger(t)
	obs := &fakeARMObserver{}
	retryObs := &fakeARMRetryObserver{}

	// Use a cancelled context to trigger pre-request failure
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled immediately

	client := NewAddressPrefixSetClient(
		log,
		nil,
		http.DefaultClient,
		WithARMBaseURL("http://localhost:1"),
		WithARMMetrics(obs, retryObs),
	)

	_, err := client.Get(ctx, "sub-1", "rg-1", "asg-1", "prefix-set-1")
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}

	// No request metric should have been emitted
	if obs.requestCount != 0 {
		t.Errorf("pre-request failure emitted %d request metrics, want 0", obs.requestCount)
	}
}

// TestPhase8_T84T85_DoRequest_EmitsRetryReasons
// T8.4: ARM returns 429 → arm_retries_total{retry_reason="429-retry-after"} incremented.
func TestPhase8_T84T85_DoRequest_EmitsRetryReasons(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"error":{"code":"TooManyRequests","message":"throttled"}}`)
			return
		}
		w.Header().Set("ETag", `"test-etag-2"`)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"name":"test","properties":{"addressPrefixes":["10.0.0.1/32"]}}`)
	}))
	defer server.Close()

	log := zaptest.NewLogger(t)
	obs := &fakeARMObserver{}
	retryObs := &fakeARMRetryObserver{}

	client := NewAddressPrefixSetClient(
		log,
		nil,
		server.Client(),
		WithARMBaseURL(server.URL),
		WithARMMetrics(obs, retryObs),
		WithRetryPolicy(RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}),
	)

	_, err := client.Get(context.Background(), "sub-1", "rg-1", "asg-1", "prefix-set-1")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	if retryObs.retryCount != 1 {
		t.Errorf("retry count = %d, want 1", retryObs.retryCount)
	}
	if len(retryObs.reasons) > 0 && retryObs.reasons[0] != "429-retry-after" {
		t.Errorf("retry reason = %q, want %q", retryObs.reasons[0], "429-retry-after")
	}
}

// TestPhase8_Executor_ETagConflict_EmitsRetryReasonMetric
// ETag conflict during executor PUT should emit arm_retries_total{retry_reason="etag-conflict"}.
func TestPhase8_Executor_ETagConflict_EmitsRetryReasonMetric(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.WriteHeader(http.StatusPreconditionFailed)
			fmt.Fprintf(w, `{"error":{"code":"PreconditionFailed","message":"etag mismatch"}}`)
			return
		}
		w.Header().Set("ETag", `"test-etag-3"`)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"name":"test","properties":{"addressPrefixes":["10.0.0.1/32"]}}`)
	}))
	defer server.Close()

	log := zaptest.NewLogger(t)
	retryObs := &fakeARMRetryObserver{}

	// Executor with retry observer
	factory := NewClientFactoryWithCredential(log, nil, server.Client(),
		WithFactoryARMBaseURL(server.URL),
		WithFactoryRetryPolicy(RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}),
	)

	executor := NewExecutor(log, factory, 1, WithExecutorRetryMetrics(retryObs))

	actions := []engine.Action{
		{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub-1",
				ResourceGroup:  "rg-1",
				ASGName:        "asg-1",
				PrefixSetName:  "ps-1",
			},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Check for etag-conflict retry reason
	found := false
	for _, reason := range retryObs.reasons {
		if reason == "etag-conflict" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected retry reason 'etag-conflict', got reasons: %v", retryObs.reasons)
	}
}

// TestPhase8_T86_RateLimiter_EmitsOnlyPositiveDelayMetrics
// T8.6: Rate limiter delays a burst → arm_rate_limit_delays_total incremented;
// zero delays do NOT increment.
func TestPhase8_T86_RateLimiter_EmitsOnlyPositiveDelayMetrics(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	log := zaptest.NewLogger(t)
	obs := &fakeRateLimitObserver{}

	// Create a rate limiter with very low RPS to force delays
	limiter := NewARMRateLimiter(log, 1.0, // 1 RPS
		WithRateLimitMetrics(obs),
	)

	ctx := context.Background()

	// First call should not be delayed (burst=1)
	if err := limiter.Wait(ctx, "sub-1"); err != nil {
		t.Fatalf("first Wait() failed: %v", err)
	}

	// Second call should be delayed (rate limited)
	if err := limiter.Wait(ctx, "sub-1"); err != nil {
		t.Fatalf("second Wait() failed: %v", err)
	}

	// First call had zero delay → no metric emitted
	// Second call had positive delay → metric emitted
	if obs.delayCount == 0 {
		t.Error("expected at least 1 positive delay metric, got 0")
	}

	// Verify no negative/zero delays were recorded
	for i, d := range obs.delays {
		if d <= 0 {
			t.Errorf("delay[%d] = %v, expected positive", i, d)
		}
	}
}

// --- fake observers ---

type fakeARMObserver struct {
	requestCount int
	statusCodes  []string
	durations    []time.Duration
}

func (f *fakeARMObserver) ObserveRequest(subscriptionID, operation, statusCode string, duration time.Duration) {
	f.requestCount++
	f.statusCodes = append(f.statusCodes, statusCode)
	f.durations = append(f.durations, duration)
}

type fakeARMRetryObserver struct {
	retryCount int
	reasons    []string
}

func (f *fakeARMRetryObserver) ObserveRetry(subscriptionID, operation, retryReason string) {
	f.retryCount++
	f.reasons = append(f.reasons, retryReason)
}

type fakeRateLimitObserver struct {
	delayCount int
	delays     []time.Duration
}

func (f *fakeRateLimitObserver) ObserveRateLimitDelay(subscriptionID string, delay time.Duration) {
	f.delayCount++
	f.delays = append(f.delays, delay)
}

// ---------------------------------------------------------------------------
// Phase 6: WithExecutorMetrics wiring assertion
// ---------------------------------------------------------------------------

func TestPhase6_WithExecutorMetrics_WiresObserver(t *testing.T) {
	log := zaptest.NewLogger(t)
	factory := newStubFactory()
	client := newStubClient()
	factory.Register("sub1", client)

	obs := &fakeExecutorMetricsObserver{}
	executor := NewExecutor(log, factory, 1, WithExecutorMetrics(obs))

	actions := []engine.Action{
		{
			Kind:       engine.CreatePrefixSet,
			Target:     engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "ps1"},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("expected success, got: %+v", results)
	}

	// WithExecutorMetrics should have wired the observer so it receives calls.
	if obs.durationCount == 0 {
		t.Error("WithExecutorMetrics: observer did not receive duration observation")
	}
	if obs.incCount == 0 {
		t.Error("WithExecutorMetrics: observer did not receive concurrency increment")
	}
	if obs.incCount != obs.decCount {
		t.Errorf("WithExecutorMetrics: unbalanced inc/dec: inc=%d, dec=%d", obs.incCount, obs.decCount)
	}
}
