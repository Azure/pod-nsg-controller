package metrics

import (
	"context"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"
)

// helper to get reconcile counter value
func getReconcileCounterValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
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

// helper to get reconcile histogram sample count
func getReconcileHistogramCount(t *testing.T, hv *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	obs, err := hv.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("failed to get histogram: %v", err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// helper to get gauge value
func getReconcileGaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return m.GetGauge().GetValue()
}

// TestPhase8_T89_ReconcileDurationAndSuccessCounterRecorded
// T8.9: Full reconcile cycle completes → reconcile_duration_seconds observation recorded;
// reconcile_total{result="success"} incremented.
func TestPhase8_T89_ReconcileDurationAndSuccessCounterRecorded(t *testing.T) {
	rec := newReconcileRecorder()

	rec.ObserveReconcile("default", "mapping-1", ReconcileResultSuccess, 500*time.Millisecond)

	count := getReconcileHistogramCount(t, rec.reconcileDuration, "default", "mapping-1", "success")
	if count != 1 {
		t.Errorf("reconcile_duration_seconds sample count = %d, want 1", count)
	}

	counterVal := getReconcileCounterValue(t, rec.reconcileTotal, "default", "mapping-1", "success")
	if counterVal != 1 {
		t.Errorf("reconcile_total{result=success} = %v, want 1", counterVal)
	}
}

// TestPhase8_T810_ReconcilePartialFailureRecorded
// T8.10: Partial failure: 2 of 5 actions fail → reconcile_total{result="partial_failure"} incremented
func TestPhase8_T810_ReconcilePartialFailureRecorded(t *testing.T) {
	rec := newReconcileRecorder()

	rec.ObserveReconcile("default", "mapping-pf", ReconcileResultPartialFailure, 2*time.Second)

	counterVal := getReconcileCounterValue(t, rec.reconcileTotal, "default", "mapping-pf", "partial_failure")
	if counterVal != 1 {
		t.Errorf("reconcile_total{result=partial_failure} = %v, want 1", counterVal)
	}
}

// TestPhase8_T811_InitialReconcileDurationAndCompletionSemantics
// T8.11: Controller starts, reconciles all CRs → initial_reconcile_duration_seconds set to positive;
// initial_reconcile_complete set to 1.
func TestPhase8_T811_InitialReconcileDurationAndCompletionSemantics(t *testing.T) {
	rec := newReconcileRecorder()
	startTime := time.Now()
	tracker := NewInitialReconcileTracker(startTime)

	// Initialize with 2 keys
	keys := []types.NamespacedName{
		{Namespace: "ns1", Name: "m1"},
		{Namespace: "ns2", Name: "m2"},
	}
	err := tracker.EnsureInitialized(context.Background(), func(_ context.Context) ([]types.NamespacedName, error) {
		return keys, nil
	})
	if err != nil {
		t.Fatalf("EnsureInitialized failed: %v", err)
	}

	// Verify not yet complete
	if tracker.IsComplete() {
		t.Error("tracker should not be complete before all keys are marked terminal")
	}

	// Simulate some processing time
	time.Sleep(10 * time.Millisecond)

	// Mark first key as terminal
	tracker.MarkTerminal(rec, keys[0], true)
	if tracker.IsComplete() {
		t.Error("tracker should not be complete with 1 of 2 keys done")
	}

	// Mark second key as terminal
	tracker.MarkTerminal(rec, keys[1], true)
	if !tracker.IsComplete() {
		t.Error("tracker should be complete after all keys marked terminal")
	}

	// Check metrics
	complete := getReconcileGaugeValue(t, rec.initialReconcileComplete)
	if complete != 1 {
		t.Errorf("initial_reconcile_complete = %v, want 1", complete)
	}

	duration := getReconcileGaugeValue(t, rec.initialReconcileDuration)
	if duration <= 0 {
		t.Errorf("initial_reconcile_duration_seconds = %v, want positive value", duration)
	}
}

// TestPhase8_InitialReconcile_FinalizerOnlyNotTerminal
// Finalizer-only reconcile (non-terminal) should NOT mark the key as complete.
func TestPhase8_InitialReconcile_FinalizerOnlyNotTerminal(t *testing.T) {
	rec := newReconcileRecorder()
	startTime := time.Now()
	tracker := NewInitialReconcileTracker(startTime)

	keys := []types.NamespacedName{{Namespace: "ns1", Name: "m1"}}
	err := tracker.EnsureInitialized(context.Background(), func(_ context.Context) ([]types.NamespacedName, error) {
		return keys, nil
	})
	if err != nil {
		t.Fatalf("EnsureInitialized failed: %v", err)
	}

	// Mark as non-terminal (finalizer-only reconcile)
	tracker.MarkTerminal(rec, keys[0], false)

	if tracker.IsComplete() {
		t.Error("finalizer-only (non-terminal) reconcile should NOT complete initial tracking")
	}
}

// TestPhase8_InitialReconcile_ValidationOnlyTerminal
// Validation-only terminal path should mark the key as complete.
func TestPhase8_InitialReconcile_ValidationOnlyTerminal(t *testing.T) {
	rec := newReconcileRecorder()
	startTime := time.Now()
	tracker := NewInitialReconcileTracker(startTime)

	keys := []types.NamespacedName{{Namespace: "ns1", Name: "m1"}}
	err := tracker.EnsureInitialized(context.Background(), func(_ context.Context) ([]types.NamespacedName, error) {
		return keys, nil
	})
	if err != nil {
		t.Fatalf("EnsureInitialized failed: %v", err)
	}

	// Validation-only is terminal
	tracker.MarkTerminal(rec, keys[0], true)

	if !tracker.IsComplete() {
		t.Error("validation-only (terminal) reconcile should complete initial tracking")
	}
}

// TestPhase8_InitialReconcile_StartupCreatedAfterStartExcluded
// Objects created after startup snapshot do not block initial completion.
func TestPhase8_InitialReconcile_StartupCreatedAfterStartExcluded(t *testing.T) {
	rec := newReconcileRecorder()
	startTime := time.Now()
	tracker := NewInitialReconcileTracker(startTime)

	// Initialize with 1 key (existing at startup)
	keys := []types.NamespacedName{{Namespace: "ns1", Name: "m1"}}
	err := tracker.EnsureInitialized(context.Background(), func(_ context.Context) ([]types.NamespacedName, error) {
		return keys, nil
	})
	if err != nil {
		t.Fatalf("EnsureInitialized failed: %v", err)
	}

	// Mark a key that was NOT in the initial set (created after startup)
	postStartupKey := types.NamespacedName{Namespace: "ns2", Name: "new-mapping"}
	tracker.MarkTerminal(rec, postStartupKey, true)

	// Should NOT be complete - the initial key is still pending
	if tracker.IsComplete() {
		t.Error("new key created after startup should not affect initial reconcile completion")
	}

	// Now mark the actual initial key
	tracker.MarkTerminal(rec, keys[0], true)
	if !tracker.IsComplete() {
		t.Error("should be complete after all initial keys are terminal")
	}
}

// TestPhase8_T812_CRDResolutionDurationObservedBeforeARM
// T8.12: CRD update triggers reconcile → crd_resolution_duration_seconds{operation="update"} observed.
func TestPhase8_T812_CRDResolutionDurationObservedBeforeARM(t *testing.T) {
	rec := newReconcileRecorder()

	rec.ObserveCRDResolution("default", "mapping-crd", "update", 50*time.Millisecond)

	count := getReconcileHistogramCount(t, rec.crdResolutionDuration, "default", "mapping-crd", "update")
	if count != 1 {
		t.Errorf("crd_resolution_duration_seconds{operation=update} sample count = %d, want 1", count)
	}
}

// TestPhase8_ReconcileHistogram_UsesSpecifiedBuckets
// Verifies the reconcile duration histogram uses spec-mandated buckets.
func TestPhase8_ReconcileHistogram_UsesSpecifiedBuckets(t *testing.T) {
	rec := newReconcileRecorder()

	rec.ObserveReconcile("default", "bucket-test", ReconcileResultSuccess, 100*time.Millisecond)

	obs, err := rec.reconcileDuration.GetMetricWithLabelValues("default", "bucket-test", "success")
	if err != nil {
		t.Fatalf("failed to get histogram: %v", err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}

	expectedBuckets := []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
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

// TestPhase8_ReconcileQueueDepthGaugeTracksQueue
// Verifies the queue depth gauge can be set and read correctly.
func TestPhase8_ReconcileQueueDepthGaugeTracksQueue(t *testing.T) {
	rec := newReconcileRecorder()

	rec.SetQueueDepth(5)
	val := getReconcileGaugeValue(t, rec.reconcileQueueDepth)
	if val != 5 {
		t.Errorf("reconcile_queue_depth = %v, want 5", val)
	}

	rec.SetQueueDepth(0)
	val = getReconcileGaugeValue(t, rec.reconcileQueueDepth)
	if val != 0 {
		t.Errorf("reconcile_queue_depth = %v, want 0", val)
	}
}

// TestPhase8_ReconcileRecorder_DeleteForMapping_ClearsAllSeries
// Verifies that DeleteForMapping removes all per-mapping series to prevent
// unbounded cardinality growth.
func TestPhase8_ReconcileRecorder_DeleteForMapping_ClearsAllSeries(t *testing.T) {
	rec := newReconcileRecorder()

	ns, mapping := "default", "delete-test"

	// Create series across all per-mapping metric vectors
	rec.ObserveReconcile(ns, mapping, ReconcileResultSuccess, 100*time.Millisecond)
	rec.ObserveReconcile(ns, mapping, ReconcileResultError, 200*time.Millisecond)
	rec.ObserveActionsPerCycle(ns, mapping, 3)
	rec.ObserveCRDResolution(ns, mapping, "update", 50*time.Millisecond)

	// Verify series exist
	if count := getReconcileCounterValue(t, rec.reconcileTotal, ns, mapping, "success"); count != 1 {
		t.Fatalf("pre-delete: reconcile_total{success} = %v, want 1", count)
	}

	// Delete all series for this mapping
	rec.DeleteForMapping(ns, mapping)

	// Verify series are gone: GetMetricWithLabelValues after delete should
	// return a fresh zero-valued metric (not the old accumulated value).
	val := getReconcileCounterValue(t, rec.reconcileTotal, ns, mapping, "success")
	if val != 0 {
		t.Errorf("post-delete: reconcile_total{success} = %v, want 0", val)
	}
	val = getReconcileCounterValue(t, rec.reconcileTotal, ns, mapping, "error")
	if val != 0 {
		t.Errorf("post-delete: reconcile_total{error} = %v, want 0", val)
	}

	count := getReconcileHistogramCount(t, rec.reconcileActionsPerCycle, ns, mapping)
	if count != 0 {
		t.Errorf("post-delete: reconcile_actions_per_cycle count = %d, want 0", count)
	}
}

// TestPhase8_PodChurnRecorder_DeleteForMapping_ClearsAllSeries
// Verifies that PodChurnRecorder.DeleteForMapping removes counter and gauge series.
func TestPhase8_PodChurnRecorder_DeleteForMapping_ClearsAllSeries(t *testing.T) {
	rec := newPodChurnRecorder()

	ns, mapping := "default", "churn-delete-test"

	// Create series
	rec.podIPChangesTotal.WithLabelValues(ns, mapping, "add").Add(5)
	rec.podIPChangesTotal.WithLabelValues(ns, mapping, "delete").Add(3)
	rec.podChurnRate.WithLabelValues(ns, mapping).Set(0.5)

	// Delete all series
	rec.DeleteForMapping(ns, mapping)

	// Verify counters reset (fresh metric returns 0)
	counter, _ := rec.podIPChangesTotal.GetMetricWithLabelValues(ns, mapping, "add")
	var m dto.Metric
	if err := counter.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetCounter().GetValue() != 0 {
		t.Errorf("post-delete: pod_ip_changes_total{add} = %v, want 0", m.GetCounter().GetValue())
	}

	// Verify gauge deleted
	gauge, _ := rec.podChurnRate.GetMetricWithLabelValues(ns, mapping)
	if err := gauge.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 0 {
		t.Errorf("post-delete: pod_churn_rate = %v, want 0", m.GetGauge().GetValue())
	}
}
