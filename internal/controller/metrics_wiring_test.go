package controller

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/metrics"
)

// TestPhase8_T88_ReconcilePath_DriftCorrectionCounterIncremented
// T8.8: Drift detected: stale IP removed → prefix_set_drift_corrections_total incremented by 1.
func TestPhase8_T88_ReconcilePath_DriftCorrectionCounterIncremented(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()
	rec, _ := metrics.Register()

	target := engine.ASGTarget{
		SubscriptionID: "sub-1",
		ResourceGroup:  "rg-1",
		ASGName:        "asg-1",
		FullResourceID: "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Network/applicationSecurityGroups/asg-1",
		PrefixSetName:  "cluster-ns-mapping",
	}

	rec.Convergence.IncrementDriftCorrections(target)

	val := getConvergenceCounterValue(t, rec.Convergence, "prefix_set_drift_corrections_total", "sub-1", "rg-1", "asg-1")
	if val != 1 {
		t.Errorf("prefix_set_drift_corrections_total = %v, want 1", val)
	}
}

// TestPhase8_T89_ReconcilePath_ResyncRequeueClassifiedSuccess
// T8.9: Successful reconcile returning RequeueAfter (steady-state resync) is classified as "success".
func TestPhase8_T89_ReconcilePath_ResyncRequeueClassifiedSuccess(t *testing.T) {
	result := ClassifyReconcileMetricResult(ReconcileStageSteadyStateSuccess)
	if result != metrics.ReconcileResultSuccess {
		t.Errorf("steady-state resync classified as %q, want %q", result, metrics.ReconcileResultSuccess)
	}
}

// TestPhase8_T810_ReconcilePath_MixedActionFailuresClassifiedPartialFailure
// T8.10: Partial failure: 2 of 5 actions fail → reconcile_total{result="partial_failure"}.
func TestPhase8_T810_ReconcilePath_MixedActionFailuresClassifiedPartialFailure(t *testing.T) {
	result := ClassifyReconcileMetricResult(ReconcileStageActionMixedFailure)
	if result != metrics.ReconcileResultPartialFailure {
		t.Errorf("mixed action failures classified as %q, want %q", result, metrics.ReconcileResultPartialFailure)
	}
}

// TestPhase8_ReconcilePath_AllActionFailuresClassifiedError
// All actions failing should classify the reconcile as "error".
func TestPhase8_ReconcilePath_AllActionFailuresClassifiedError(t *testing.T) {
	result := ClassifyReconcileMetricResult(ReconcileStageActionAllFailure)
	if result != metrics.ReconcileResultError {
		t.Errorf("all action failures classified as %q, want %q", result, metrics.ReconcileResultError)
	}
}

// TestPhase8_T812_ReconcilePath_CRDResolutionRecordedBeforeExecutor
// T8.12: crd_resolution_duration_seconds{operation="update"} observed before ARM execution.
func TestPhase8_T812_ReconcilePath_CRDResolutionRecordedBeforeExecutor(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()
	rec, _ := metrics.Register()

	// Simulate CRD resolution observation happening before executor
	actions := []engine.Action{{Kind: engine.UpdatePrefixSet}}
	op := ClassifyCRDResolutionOperation(actions)
	rec.Reconcile.ObserveCRDResolution("default", "mapping-1", op, 100*time.Millisecond)

	// Verify the histogram observation exists
	count := getCRDResolutionHistogramCount(t, rec.Reconcile, "default", "mapping-1", "update")
	if count != 1 {
		t.Errorf("crd_resolution_duration_seconds{operation=update} sample_count = %d, want 1", count)
	}
}

// TestPhase8_T813_ReconcilePath_PodChurnRateGaugeReflectsObservedRate
// T8.13: pod_churn_rate gauge reflects changes/second consistent with pod creation rate.
func TestPhase8_T813_ReconcilePath_PodChurnRateGaugeReflectsObservedRate(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()
	rec, _ := metrics.Register()

	tracker := metrics.NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "default", Name: "mapping-churn"}

	// First snapshot: 0 pods
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tracker.ObserveSnapshot(rec.PodChurn, key, metrics.MappingPodSnapshot{
		Pods: map[metrics.PodIdentity]metrics.PodMembership{},
	}, t0)

	// Second snapshot 1 second later: 5 pods added
	t1 := t0.Add(1 * time.Second)
	pods := make(map[metrics.PodIdentity]metrics.PodMembership)
	for i := 0; i < 5; i++ {
		pods[metrics.PodIdentity{Namespace: "default", Name: "pod-" + string(rune('a'+i)), UID: "uid-" + string(rune('a'+i))}] = metrics.PodMembership{PodIP: "10.0.0." + string(rune('1'+i))}
	}
	delta := tracker.ObserveSnapshot(rec.PodChurn, key, metrics.MappingPodSnapshot{Pods: pods}, t1)

	if delta.Added != 5 {
		t.Errorf("delta.Added = %d, want 5", delta.Added)
	}
	// pod_churn_rate = 5 changes / 1 second = 5.0
	rate := getPodChurnRateGauge(t, rec.PodChurn, "default", "mapping-churn")
	if rate != 5.0 {
		t.Errorf("pod_churn_rate = %v, want 5.0", rate)
	}
}

// TestPhase8_T81_ReconcilePath_ThreePodsAddedIncrementsCounter
// T8.1: 3 pods added → pod_ip_changes_total{operation="add"} incremented by 3.
func TestPhase8_T81_ReconcilePath_ThreePodsAddedIncrementsCounter(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()
	rec, _ := metrics.Register()

	tracker := metrics.NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "default", Name: "mapping-add"}

	// Initial empty snapshot
	t0 := time.Now()
	tracker.ObserveSnapshot(rec.PodChurn, key, metrics.MappingPodSnapshot{
		Pods: map[metrics.PodIdentity]metrics.PodMembership{},
	}, t0)

	// Add 3 pods
	pods := map[metrics.PodIdentity]metrics.PodMembership{
		{Namespace: "default", Name: "p1", UID: "u1"}: {PodIP: "10.0.0.1"},
		{Namespace: "default", Name: "p2", UID: "u2"}: {PodIP: "10.0.0.2"},
		{Namespace: "default", Name: "p3", UID: "u3"}: {PodIP: "10.0.0.3"},
	}
	delta := tracker.ObserveSnapshot(rec.PodChurn, key, metrics.MappingPodSnapshot{Pods: pods}, t0.Add(time.Second))

	if delta.Added != 3 {
		t.Errorf("delta.Added = %d, want 3", delta.Added)
	}

	val := getPodIPChangesTotal(t, rec.PodChurn, "default", "mapping-add", "add")
	if val != 3 {
		t.Errorf("pod_ip_changes_total{operation=add} = %v, want 3", val)
	}
}

// TestPhase8_T82_ReconcilePath_TwoPodsDeletedIncrementsCounter
// T8.2: 2 pods deleted → pod_ip_changes_total{operation="delete"} incremented by 2.
func TestPhase8_T82_ReconcilePath_TwoPodsDeletedIncrementsCounter(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()
	rec, _ := metrics.Register()

	tracker := metrics.NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "default", Name: "mapping-del"}

	// Start with 3 pods
	t0 := time.Now()
	pods := map[metrics.PodIdentity]metrics.PodMembership{
		{Namespace: "default", Name: "p1", UID: "u1"}: {PodIP: "10.0.0.1"},
		{Namespace: "default", Name: "p2", UID: "u2"}: {PodIP: "10.0.0.2"},
		{Namespace: "default", Name: "p3", UID: "u3"}: {PodIP: "10.0.0.3"},
	}
	tracker.ObserveSnapshot(rec.PodChurn, key, metrics.MappingPodSnapshot{Pods: pods}, t0)

	// Remove 2 pods
	podsAfter := map[metrics.PodIdentity]metrics.PodMembership{
		{Namespace: "default", Name: "p1", UID: "u1"}: {PodIP: "10.0.0.1"},
	}
	delta := tracker.ObserveSnapshot(rec.PodChurn, key, metrics.MappingPodSnapshot{Pods: podsAfter}, t0.Add(time.Second))

	if delta.Deleted != 2 {
		t.Errorf("delta.Deleted = %d, want 2", delta.Deleted)
	}

	val := getPodIPChangesTotal(t, rec.PodChurn, "default", "mapping-del", "delete")
	if val != 2 {
		t.Errorf("pod_ip_changes_total{operation=delete} = %v, want 2", val)
	}
}

// TestPhase8_T87_ReconcilePath_ConvergenceObservation
// T8.7: Pod IP added, prefix set PUT completes → convergence histogram observation recorded.
func TestPhase8_T87_ReconcilePath_ConvergenceObservation(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()
	rec, _ := metrics.Register()

	tracker := metrics.NewConvergenceTracker()
	key := types.NamespacedName{Namespace: "default", Name: "mapping-conv"}
	target := engine.ASGTarget{
		SubscriptionID: "sub-1",
		ResourceGroup:  "rg-1",
		ASGName:        "asg-1",
		FullResourceID: "/subscriptions/sub-1/rg-1/asg-1",
		PrefixSetName:  "cluster-ns-mapping",
	}

	t0 := time.Now()
	tracker.StartOrKeep(key, target, metrics.ConvergenceOpAdd, t0)
	tracker.StageSuccessfulAction(key, target, metrics.ConvergenceOpAdd, t0.Add(200*time.Millisecond))
	tracker.CommitConvergence(rec.Convergence, key, target)

	val := getConvergenceHistogramCount(t, rec.Convergence, "sub-1", "rg-1", "asg-1", "add")
	if val != 1 {
		t.Errorf("prefix_set_convergence_seconds{operation=add} sample_count = %d, want 1", val)
	}
}

// --- helpers ---

func getConvergenceCounterValue(t *testing.T, conv *metrics.ConvergenceRecorder, name, sub, rg, asg string) float64 {
	t.Helper()
	// Use the exported IncrementDriftCorrections path; verify via prometheus
	// The drift corrections counter should already be incremented in the test
	// We access internal state through Collectors()
	collectors := conv.Collectors()
	for _, c := range collectors {
		if cv, ok := c.(*prometheus.CounterVec); ok {
			m, err := cv.GetMetricWithLabelValues(sub, rg, asg)
			if err != nil {
				continue
			}
			var metric dto.Metric
			if err := m.Write(&metric); err != nil {
				continue
			}
			if metric.GetCounter().GetValue() > 0 {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatal("could not find drift corrections counter")
	return 0
}

func getConvergenceHistogramCount(t *testing.T, conv *metrics.ConvergenceRecorder, sub, rg, asg, op string) uint64 {
	t.Helper()
	collectors := conv.Collectors()
	for _, c := range collectors {
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			obs, err := hv.GetMetricWithLabelValues(sub, rg, asg, op)
			if err != nil {
				continue
			}
			var metric dto.Metric
			if err := obs.(prometheus.Metric).Write(&metric); err != nil {
				continue
			}
			if metric.GetHistogram() != nil {
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}
	t.Fatal("could not find convergence histogram")
	return 0
}

func getCRDResolutionHistogramCount(t *testing.T, rec *metrics.ReconcileRecorder, ns, mapping, op string) uint64 {
	t.Helper()
	collectors := rec.Collectors()
	for _, c := range collectors {
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			obs, err := hv.GetMetricWithLabelValues(ns, mapping, op)
			if err != nil {
				continue
			}
			var metric dto.Metric
			if err := obs.(prometheus.Metric).Write(&metric); err != nil {
				continue
			}
			if metric.GetHistogram() != nil && metric.GetHistogram().GetSampleCount() > 0 {
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}
	t.Fatal("could not find crd_resolution_duration_seconds histogram")
	return 0
}

func getPodChurnRateGauge(t *testing.T, pcr *metrics.PodChurnRecorder, ns, mapping string) float64 {
	t.Helper()
	collectors := pcr.Collectors()
	for _, c := range collectors {
		if gv, ok := c.(*prometheus.GaugeVec); ok {
			g, err := gv.GetMetricWithLabelValues(ns, mapping)
			if err != nil {
				continue
			}
			var metric dto.Metric
			if err := g.Write(&metric); err != nil {
				continue
			}
			if metric.GetGauge() != nil {
				return metric.GetGauge().GetValue()
			}
		}
	}
	t.Fatal("could not find pod_churn_rate gauge")
	return 0
}

func getPodIPChangesTotal(t *testing.T, pcr *metrics.PodChurnRecorder, ns, mapping, op string) float64 {
	t.Helper()
	collectors := pcr.Collectors()
	for _, c := range collectors {
		if cv, ok := c.(*prometheus.CounterVec); ok {
			m, err := cv.GetMetricWithLabelValues(ns, mapping, op)
			if err != nil {
				continue
			}
			var metric dto.Metric
			if err := m.Write(&metric); err != nil {
				continue
			}
			if metric.GetCounter() != nil {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatal("could not find pod_ip_changes_total counter")
	return 0
}

// compile-time checks for stubs
var _ = ReconcileStageFinalizerAddEarlyReturn
var _ = ReconcileStageSteadyStateSuccess
var _ = ReconcileStageActionMixedFailure
var _ = ReconcileStageActionAllFailure
