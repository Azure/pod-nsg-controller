package metrics

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/types"
)

// helper to get counter value from a CounterVec
func getCounterValue(t *testing.T, rec *PodChurnRecorder, namespace, mapping, operation string) float64 {
	t.Helper()
	counter, err := rec.podIPChangesTotal.GetMetricWithLabelValues(namespace, mapping, operation)
	if err != nil {
		t.Fatalf("failed to get counter metric: %v", err)
	}
	var m dto.Metric
	if err := counter.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// helper to get gauge value
func getGaugeValue(t *testing.T, rec *PodChurnRecorder, namespace, mapping string) float64 {
	t.Helper()
	gauge, err := rec.podChurnRate.GetMetricWithLabelValues(namespace, mapping)
	if err != nil {
		t.Fatalf("failed to get gauge metric: %v", err)
	}
	var m dto.Metric
	if err := gauge.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return m.GetGauge().GetValue()
}

// TestPhase8_T81_PodAddsIncrementPodIPChangesTotal
// T8.1: 3 pods added, reconcile completes → pod_ip_changes_total{operation="add"} incremented by 3
func TestPhase8_T81_PodAddsIncrementPodIPChangesTotal(t *testing.T) {
	rec := newPodChurnRecorder()
	tracker := NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "default", Name: "test-mapping"}
	now := time.Now()

	// First observation: empty -> 3 pods = 3 adds
	snapshot := MappingPodSnapshot{
		Pods: map[PodIdentity]PodMembership{
			{Namespace: "default", Name: "pod-1", UID: "uid-1"}: {PodIP: "10.0.0.1"},
			{Namespace: "default", Name: "pod-2", UID: "uid-2"}: {PodIP: "10.0.0.2"},
			{Namespace: "default", Name: "pod-3", UID: "uid-3"}: {PodIP: "10.0.0.3"},
		},
	}

	// First call with no previous snapshot sets all as adds
	delta := tracker.ObserveSnapshot(rec, key, snapshot, now)
	if delta.Added != 3 {
		t.Errorf("expected 3 adds, got %d", delta.Added)
	}

	val := getCounterValue(t, rec, "default", "test-mapping", "add")
	if val != 3 {
		t.Errorf("pod_ip_changes_total{operation=add} = %v, want 3", val)
	}
}

// TestPhase8_T82_PodDeletesIncrementPodIPChangesTotal
// T8.2: 2 pods deleted, reconcile completes → pod_ip_changes_total{operation="delete"} incremented by 2
func TestPhase8_T82_PodDeletesIncrementPodIPChangesTotal(t *testing.T) {
	rec := newPodChurnRecorder()
	tracker := NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "default", Name: "test-mapping"}
	now := time.Now()

	// Set up initial state with 3 pods
	initialSnapshot := MappingPodSnapshot{
		Pods: map[PodIdentity]PodMembership{
			{Namespace: "default", Name: "pod-1", UID: "uid-1"}: {PodIP: "10.0.0.1"},
			{Namespace: "default", Name: "pod-2", UID: "uid-2"}: {PodIP: "10.0.0.2"},
			{Namespace: "default", Name: "pod-3", UID: "uid-3"}: {PodIP: "10.0.0.3"},
		},
	}
	tracker.ObserveSnapshot(rec, key, initialSnapshot, now)

	// Second observation: remove 2 pods
	reducedSnapshot := MappingPodSnapshot{
		Pods: map[PodIdentity]PodMembership{
			{Namespace: "default", Name: "pod-1", UID: "uid-1"}: {PodIP: "10.0.0.1"},
		},
	}
	later := now.Add(10 * time.Second)
	delta := tracker.ObserveSnapshot(rec, key, reducedSnapshot, later)
	if delta.Deleted != 2 {
		t.Errorf("expected 2 deletes, got %d", delta.Deleted)
	}

	val := getCounterValue(t, rec, "default", "test-mapping", "delete")
	if val != 2 {
		t.Errorf("pod_ip_changes_total{operation=delete} = %v, want 2", val)
	}
}

// TestPhase8_PodChurn_NoDoubleCountAcrossTargetFanout ensures pod churn is counted
// by pod identity, not per-target (no overcounting when pod maps to multiple ASGs).
func TestPhase8_PodChurn_NoDoubleCountAcrossTargetFanout(t *testing.T) {
	rec := newPodChurnRecorder()
	tracker := NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "default", Name: "multi-asg-mapping"}
	now := time.Now()

	// A single pod that maps to multiple ASG targets should only be counted once.
	snapshot := MappingPodSnapshot{
		Pods: map[PodIdentity]PodMembership{
			{Namespace: "default", Name: "pod-1", UID: "uid-1"}: {PodIP: "10.0.0.1"},
		},
	}

	delta := tracker.ObserveSnapshot(rec, key, snapshot, now)
	if delta.Added != 1 {
		t.Errorf("expected 1 add (not multiplied by targets), got %d", delta.Added)
	}

	val := getCounterValue(t, rec, "default", "multi-asg-mapping", "add")
	if val != 1 {
		t.Errorf("pod_ip_changes_total{operation=add} = %v, want 1 (not per-target)", val)
	}
}

// TestPhase8_T813_PodChurnRateGaugeReflectsSlidingWindow
// T8.13: Pod churn during reconcile → pod_churn_rate gauge reflects changes/second
func TestPhase8_T813_PodChurnRateGaugeReflectsSlidingWindow(t *testing.T) {
	rec := newPodChurnRecorder()
	tracker := NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "default", Name: "rate-mapping"}
	t0 := time.Now()

	// First snapshot: 0 pods (baseline)
	emptySnapshot := MappingPodSnapshot{Pods: map[PodIdentity]PodMembership{}}
	tracker.ObserveSnapshot(rec, key, emptySnapshot, t0)

	// Second snapshot: 10 pods added after 5 seconds
	fivePodsSnapshot := MappingPodSnapshot{
		Pods: map[PodIdentity]PodMembership{
			{Namespace: "default", Name: "p1", UID: "u1"}: {PodIP: "10.0.0.1"},
			{Namespace: "default", Name: "p2", UID: "u2"}: {PodIP: "10.0.0.2"},
			{Namespace: "default", Name: "p3", UID: "u3"}: {PodIP: "10.0.0.3"},
			{Namespace: "default", Name: "p4", UID: "u4"}: {PodIP: "10.0.0.4"},
			{Namespace: "default", Name: "p5", UID: "u5"}: {PodIP: "10.0.0.5"},
			{Namespace: "default", Name: "p6", UID: "u6"}: {PodIP: "10.0.0.6"},
			{Namespace: "default", Name: "p7", UID: "u7"}: {PodIP: "10.0.0.7"},
			{Namespace: "default", Name: "p8", UID: "u8"}: {PodIP: "10.0.0.8"},
			{Namespace: "default", Name: "p9", UID: "u9"}: {PodIP: "10.0.0.9"},
			{Namespace: "default", Name: "p10", UID: "u10"}: {PodIP: "10.0.0.10"},
		},
	}
	t1 := t0.Add(5 * time.Second)
	tracker.ObserveSnapshot(rec, key, fivePodsSnapshot, t1)

	rate := getGaugeValue(t, rec, "default", "rate-mapping")
	// Windowed rate: 10 changes in interval [t0,t1]=5s, fully within 60s window.
	// Weighted contribution: 10 * (5/5) = 10. Rate = 10/60 ≈ 0.1667
	expectedRate := 10.0 / 60.0
	if rate < expectedRate*0.9 || rate > expectedRate*1.1 {
		t.Errorf("pod_churn_rate = %v, want ~%v (10 changes in 5s, 60s window)", rate, expectedRate)
	}
}
