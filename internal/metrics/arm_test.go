package metrics

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
)

// helper to get counter value from ARM recorder
func getARMCounterValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	counter, err := cv.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("failed to get counter metric: %v", err)
	}
	var m dto.Metric
	if err := counter.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// helper to get histogram sample count
func getHistogramSampleCount(t *testing.T, hv *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	obs, err := hv.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("failed to get histogram metric: %v", err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestPhase8_T83_ARMSuccessRecordsRequestAndLatencyBuckets
// T8.3: ARM PUT succeeds on first call → arm_requests_total{status_code="200"} incremented;
// arm_request_duration_seconds observation recorded.
func TestPhase8_T83_ARMSuccessRecordsRequestAndLatencyBuckets(t *testing.T) {
	rec := newARMRecorder()

	rec.ObserveRequest("sub-123", "CreateOrUpdate", "200", 250*time.Millisecond)

	val := getARMCounterValue(t, rec.requestsTotal, "sub-123", "CreateOrUpdate", "200")
	if val != 1 {
		t.Errorf("arm_requests_total{status_code=200} = %v, want 1", val)
	}

	count := getHistogramSampleCount(t, rec.requestDuration, "sub-123", "CreateOrUpdate")
	if count != 1 {
		t.Errorf("arm_request_duration_seconds sample count = %d, want 1", count)
	}
}

// TestPhase8_T84_ARM429RetryReasonRecorded
// T8.4: ARM returns 429, retried once → arm_retries_total{retry_reason="429-retry-after"} incremented by 1
func TestPhase8_T84_ARM429RetryReasonRecorded(t *testing.T) {
	rec := newARMRecorder()

	// Simulate: first call gets 429, records request + retry
	rec.ObserveRequest("sub-123", "CreateOrUpdate", "429", 100*time.Millisecond)
	rec.ObserveRetry("sub-123", "CreateOrUpdate", "429-retry-after")

	// Then succeeds
	rec.ObserveRequest("sub-123", "CreateOrUpdate", "200", 200*time.Millisecond)

	retryVal := getARMCounterValue(t, rec.retriesTotal, "sub-123", "CreateOrUpdate", "429-retry-after")
	if retryVal != 1 {
		t.Errorf("arm_retries_total{retry_reason=429-retry-after} = %v, want 1", retryVal)
	}
}

// TestPhase8_T85_ARM500RetriesRecorded
// T8.5: ARM returns 500 three times then 200 → arm_retries_total{retry_reason="server-error"} incremented by 3
func TestPhase8_T85_ARM500RetriesRecorded(t *testing.T) {
	rec := newARMRecorder()

	// 3 failed attempts (500)
	for i := 0; i < 3; i++ {
		rec.ObserveRequest("sub-456", "Get", "500", 100*time.Millisecond)
		rec.ObserveRetry("sub-456", "Get", "server-error")
	}
	// Then success
	rec.ObserveRequest("sub-456", "Get", "200", 100*time.Millisecond)

	retryVal := getARMCounterValue(t, rec.retriesTotal, "sub-456", "Get", "server-error")
	if retryVal != 3 {
		t.Errorf("arm_retries_total{retry_reason=server-error} = %v, want 3", retryVal)
	}

	// Verify total requests: 3 failures + 1 success = 4 total
	failCount := getARMCounterValue(t, rec.requestsTotal, "sub-456", "Get", "500")
	if failCount != 3 {
		t.Errorf("arm_requests_total{status_code=500} = %v, want 3", failCount)
	}
	successCount := getARMCounterValue(t, rec.requestsTotal, "sub-456", "Get", "200")
	if successCount != 1 {
		t.Errorf("arm_requests_total{status_code=200} = %v, want 1", successCount)
	}
}

// TestPhase8_T86_RateLimiterDelayMetricsRecorded
// T8.6: Rate limiter delays a burst of calls → arm_rate_limit_delays_total incremented;
// arm_rate_limit_delay_seconds histogram records positive values.
func TestPhase8_T86_RateLimiterDelayMetricsRecorded(t *testing.T) {
	rec := newARMRecorder()

	// Simulate 3 rate-limited calls
	rec.ObserveRateLimitDelay("sub-789", 500*time.Millisecond)
	rec.ObserveRateLimitDelay("sub-789", 1*time.Second)
	rec.ObserveRateLimitDelay("sub-789", 250*time.Millisecond)

	delayCount := getARMCounterValue(t, rec.rateLimitDelays, "sub-789")
	if delayCount != 3 {
		t.Errorf("arm_rate_limit_delays_total = %v, want 3", delayCount)
	}

	histCount := getHistogramSampleCount(t, rec.rateLimitDuration, "sub-789")
	if histCount != 3 {
		t.Errorf("arm_rate_limit_delay_seconds sample count = %d, want 3", histCount)
	}
}

// TestPhase8_ARMHistogram_UsesSpecifiedBuckets verifies the ARM request duration histogram
// uses the spec-mandated buckets: 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30.
func TestPhase8_ARMHistogram_UsesSpecifiedBuckets(t *testing.T) {
	rec := newARMRecorder()

	// Record one observation to materialize the histogram
	rec.ObserveRequest("sub-check", "Get", "200", 1*time.Second)

	obs, err := rec.requestDuration.GetMetricWithLabelValues("sub-check", "Get")
	if err != nil {
		t.Fatalf("failed to get histogram: %v", err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}

	expectedBuckets := []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	buckets := m.GetHistogram().GetBucket()
	if len(buckets) != len(expectedBuckets) {
		t.Fatalf("expected %d buckets, got %d", len(expectedBuckets), len(buckets))
	}
	for i, b := range buckets {
		if b.GetUpperBound() != expectedBuckets[i] {
			t.Errorf("bucket[%d] upper bound = %v, want %v", i, b.GetUpperBound(), expectedBuckets[i])
		}
	}
}
