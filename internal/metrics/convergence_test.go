package metrics

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Azure/pod-nsg-controller/internal/engine"
)

// helper to get convergence histogram sample count
func getConvergenceHistogramCount(t *testing.T, rec *ConvergenceRecorder, subID, rg, asg, op string) uint64 {
	t.Helper()
	obs, err := rec.convergenceSeconds.GetMetricWithLabelValues(subID, rg, asg, op)
	if err != nil {
		t.Fatalf("failed to get convergence histogram: %v", err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// helper to get convergence histogram sum (total observed seconds)
func getConvergenceHistogramSum(t *testing.T, rec *ConvergenceRecorder, subID, rg, asg, op string) float64 {
	t.Helper()
	obs, err := rec.convergenceSeconds.GetMetricWithLabelValues(subID, rg, asg, op)
	if err != nil {
		t.Fatalf("failed to get convergence histogram: %v", err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

// helper to get drift correction counter
func getDriftCorrectionCount(t *testing.T, rec *ConvergenceRecorder, subID, rg, asg string) float64 {
	t.Helper()
	counter, err := rec.driftCorrections.GetMetricWithLabelValues(subID, rg, asg)
	if err != nil {
		t.Fatalf("failed to get drift correction counter: %v", err)
	}
	var m dto.Metric
	if err := counter.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// helper to get prefix set action counter
func getPrefixSetActionCount(t *testing.T, rec *ConvergenceRecorder, operation, result string) float64 {
	t.Helper()
	counter, err := rec.prefixSetActions.GetMetricWithLabelValues(operation, result)
	if err != nil {
		t.Fatalf("failed to get prefix set actions counter: %v", err)
	}
	var m dto.Metric
	if err := counter.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// TestPhase8_T87_ConvergenceAddRecordedFromDetectionToPUTSuccess
// T8.7: Pod IP added, prefix set PUT completes → prefix_set_convergence_seconds{operation="add"}
// observation recorded between detection and PUT success.
func TestPhase8_T87_ConvergenceAddRecordedFromDetectionToPUTSuccess(t *testing.T) {
	rec := newConvergenceRecorder()
	tracker := NewConvergenceTracker()

	key := types.NamespacedName{Namespace: "default", Name: "mapping-1"}
	target := engine.ASGTarget{
		SubscriptionID: "sub-123",
		ResourceGroup:  "rg-prod",
		ASGName:        "asg-web",
		FullResourceID: "/subscriptions/sub-123/resourceGroups/rg-prod/providers/Microsoft.Network/applicationSecurityGroups/asg-web",
	}

	detectedAt := time.Now()
	completedAt := detectedAt.Add(2 * time.Second)

	// Start convergence tracking
	tracker.StartOrKeep(key, target, ConvergenceOpAdd, detectedAt)

	// Stage successful ARM action
	tracker.StageSuccessfulAction(key, target, ConvergenceOpAdd, completedAt)

	// Commit after status write
	tracker.CommitConvergence(rec, key, target)

	// Verify histogram observation
	count := getConvergenceHistogramCount(t, rec, "sub-123", "rg-prod", "asg-web", "add")
	if count != 1 {
		t.Errorf("prefix_set_convergence_seconds{operation=add} sample count = %d, want 1", count)
	}

	sum := getConvergenceHistogramSum(t, rec, "sub-123", "rg-prod", "asg-web", "add")
	if sum < 1.9 || sum > 2.1 {
		t.Errorf("prefix_set_convergence_seconds sum = %v, want ~2.0 seconds", sum)
	}
}

// TestPhase8_ConvergenceOperationMapping_CreateToAdd_UpdateClassifiedByDelta
// Verifies that ClassifyConvergenceOperation correctly maps deltas to operations.
func TestPhase8_ConvergenceOperationMapping_CreateToAdd_UpdateClassifiedByDelta(t *testing.T) {
	tests := []struct {
		name     string
		delta    TargetDelta
		wantOp   ConvergenceOperation
		wantHas  bool
	}{
		{
			name:    "only additions → add",
			delta:   TargetDelta{AddedIPs: map[string]struct{}{"10.0.0.1": {}}, RemovedIPs: nil},
			wantOp:  ConvergenceOpAdd,
			wantHas: true,
		},
		{
			name:    "only removals → delete",
			delta:   TargetDelta{AddedIPs: nil, RemovedIPs: map[string]struct{}{"10.0.0.1": {}}},
			wantOp:  ConvergenceOpDelete,
			wantHas: true,
		},
		{
			name:    "both additions and removals → update",
			delta:   TargetDelta{AddedIPs: map[string]struct{}{"10.0.0.2": {}}, RemovedIPs: map[string]struct{}{"10.0.0.1": {}}},
			wantOp:  ConvergenceOpUpdate,
			wantHas: true,
		},
		{
			name:    "empty delta → no operation",
			delta:   TargetDelta{AddedIPs: nil, RemovedIPs: nil},
			wantOp:  "",
			wantHas: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op, has := ClassifyConvergenceOperation(tc.delta)
			if has != tc.wantHas {
				t.Errorf("ClassifyConvergenceOperation() has = %v, want %v", has, tc.wantHas)
			}
			if op != tc.wantOp {
				t.Errorf("ClassifyConvergenceOperation() op = %v, want %v", op, tc.wantOp)
			}
		})
	}
}

// TestPhase8_ConvergenceRetryPreservesOriginalStartTime
// Verifies that retries/requeues preserve the original detection start time.
func TestPhase8_ConvergenceRetryPreservesOriginalStartTime(t *testing.T) {
	rec := newConvergenceRecorder()
	tracker := NewConvergenceTracker()

	key := types.NamespacedName{Namespace: "default", Name: "mapping-retry"}
	target := engine.ASGTarget{
		SubscriptionID: "sub-retry",
		ResourceGroup:  "rg-retry",
		ASGName:        "asg-retry",
		FullResourceID: "/subscriptions/sub-retry/resourceGroups/rg-retry/providers/Microsoft.Network/applicationSecurityGroups/asg-retry",
	}

	originalStart := time.Now()
	// First attempt starts tracking
	tracker.StartOrKeep(key, target, ConvergenceOpAdd, originalStart)

	// Retry after 5s - StartOrKeep should NOT reset the start time
	retryTime := originalStart.Add(5 * time.Second)
	tracker.StartOrKeep(key, target, ConvergenceOpAdd, retryTime)

	// Final success at 10s from original start
	completedAt := originalStart.Add(10 * time.Second)
	tracker.StageSuccessfulAction(key, target, ConvergenceOpAdd, completedAt)
	tracker.CommitConvergence(rec, key, target)

	// Duration should be 10s (from original start), not 5s (from retry)
	sum := getConvergenceHistogramSum(t, rec, "sub-retry", "rg-retry", "asg-retry", "add")
	if sum < 9.9 || sum > 10.1 {
		t.Errorf("convergence duration = %v, want ~10.0 (from original start, not retry)", sum)
	}
}

// TestPhase8_T88_DriftCorrectionCounterIncremented
// T8.8: Drift detected: stale IP removed → prefix_set_drift_corrections_total incremented by 1
func TestPhase8_T88_DriftCorrectionCounterIncremented(t *testing.T) {
	rec := newConvergenceRecorder()

	target := engine.ASGTarget{
		SubscriptionID: "sub-drift",
		ResourceGroup:  "rg-drift",
		ASGName:        "asg-drift",
	}

	rec.IncrementDriftCorrections(target)

	val := getDriftCorrectionCount(t, rec, "sub-drift", "rg-drift", "asg-drift")
	if val != 1 {
		t.Errorf("prefix_set_drift_corrections_total = %v, want 1", val)
	}
}

// TestPhase8_T810_PrefixSetActionsTotalTracksSuccessAndFailure
// T8.10 (partial): prefix_set_actions_total{result="failure"} incremented by 2
func TestPhase8_T810_PrefixSetActionsTotalTracksSuccessAndFailure(t *testing.T) {
	rec := newConvergenceRecorder()

	// 3 successes
	rec.RecordPrefixSetAction("update", "success")
	rec.RecordPrefixSetAction("update", "success")
	rec.RecordPrefixSetAction("create", "success")

	// 2 failures
	rec.RecordPrefixSetAction("update", "failure")
	rec.RecordPrefixSetAction("create", "failure")

	successUpdate := getPrefixSetActionCount(t, rec, "update", "success")
	if successUpdate != 2 {
		t.Errorf("prefix_set_actions_total{operation=update,result=success} = %v, want 2", successUpdate)
	}
	successCreate := getPrefixSetActionCount(t, rec, "create", "success")
	if successCreate != 1 {
		t.Errorf("prefix_set_actions_total{operation=create,result=success} = %v, want 1", successCreate)
	}
	failureUpdate := getPrefixSetActionCount(t, rec, "update", "failure")
	if failureUpdate != 1 {
		t.Errorf("prefix_set_actions_total{operation=update,result=failure} = %v, want 1", failureUpdate)
	}
	failureCreate := getPrefixSetActionCount(t, rec, "create", "failure")
	if failureCreate != 1 {
		t.Errorf("prefix_set_actions_total{operation=create,result=failure} = %v, want 1", failureCreate)
	}
}
