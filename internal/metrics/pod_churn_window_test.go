package metrics

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// TestPhase8_T813_PodChurnRate_WeightedOverlapFixedWindow validates the weighted-overlap
// fixed-window churn rate algorithm (T8.13).
// The gauge must reflect changes/second consistent with the test's pod creation rate
// using a 60-second fixed window with weighted-overlap intervals.
func TestPhase8_T813_PodChurnRate_WeightedOverlapFixedWindow(t *testing.T) {
	rec := newPodChurnRecorder()
	tracker := NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "default", Name: "window-test"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Baseline: empty snapshot
	empty := MappingPodSnapshot{Pods: map[PodIdentity]PodMembership{}}
	tracker.ObserveSnapshot(rec, key, empty, t0)

	// +10s: add 5 pods (5 changes over 10s interval)
	t1 := t0.Add(10 * time.Second)
	snap1 := MappingPodSnapshot{
		Pods: map[PodIdentity]PodMembership{
			{Namespace: "default", Name: "p1", UID: "u1"}: {PodIP: "10.0.0.1"},
			{Namespace: "default", Name: "p2", UID: "u2"}: {PodIP: "10.0.0.2"},
			{Namespace: "default", Name: "p3", UID: "u3"}: {PodIP: "10.0.0.3"},
			{Namespace: "default", Name: "p4", UID: "u4"}: {PodIP: "10.0.0.4"},
			{Namespace: "default", Name: "p5", UID: "u5"}: {PodIP: "10.0.0.5"},
		},
	}
	tracker.ObserveSnapshot(rec, key, snap1, t1)

	// +30s: add 3 more pods (3 changes over 20s interval)
	t2 := t0.Add(30 * time.Second)
	snap2 := MappingPodSnapshot{
		Pods: map[PodIdentity]PodMembership{
			{Namespace: "default", Name: "p1", UID: "u1"}: {PodIP: "10.0.0.1"},
			{Namespace: "default", Name: "p2", UID: "u2"}: {PodIP: "10.0.0.2"},
			{Namespace: "default", Name: "p3", UID: "u3"}: {PodIP: "10.0.0.3"},
			{Namespace: "default", Name: "p4", UID: "u4"}: {PodIP: "10.0.0.4"},
			{Namespace: "default", Name: "p5", UID: "u5"}: {PodIP: "10.0.0.5"},
			{Namespace: "default", Name: "p6", UID: "u6"}: {PodIP: "10.0.0.6"},
			{Namespace: "default", Name: "p7", UID: "u7"}: {PodIP: "10.0.0.7"},
			{Namespace: "default", Name: "p8", UID: "u8"}: {PodIP: "10.0.0.8"},
		},
	}
	tracker.ObserveSnapshot(rec, key, snap2, t2)

	// At t2 (30s into window), both intervals are fully within the 60s window:
	// Interval 1: [t0, t1] = 10s, 5 changes → contribution = 5 * min(10,10)/10 = 5
	// Interval 2: [t1, t2] = 20s, 3 changes → contribution = 3 * min(20,20)/20 = 3
	// Total weighted changes = 8, window = 60s → rate = 8/60 ≈ 0.1333
	rate := getGaugeValue(t, rec, "default", "window-test")
	expectedRate := 8.0 / 60.0 // ≈ 0.1333
	if rate < expectedRate*0.9 || rate > expectedRate*1.1 {
		t.Errorf("pod_churn_rate at t2 = %v, want ~%v (8 changes / 60s window)", rate, expectedRate)
	}
}

// TestPhase8_PodChurnWindow_PruneExpiredIntervals verifies that intervals older than
// the 60s window are pruned and do not contribute to the rate.
func TestPhase8_PodChurnWindow_PruneExpiredIntervals(t *testing.T) {
	rec := newPodChurnRecorder()
	tracker := NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "default", Name: "prune-test"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Baseline
	empty := MappingPodSnapshot{Pods: map[PodIdentity]PodMembership{}}
	tracker.ObserveSnapshot(rec, key, empty, t0)

	// +5s: add 10 pods
	t1 := t0.Add(5 * time.Second)
	snap1 := MappingPodSnapshot{
		Pods: map[PodIdentity]PodMembership{
			{Namespace: "default", Name: "p1", UID: "u1"}:   {PodIP: "10.0.0.1"},
			{Namespace: "default", Name: "p2", UID: "u2"}:   {PodIP: "10.0.0.2"},
			{Namespace: "default", Name: "p3", UID: "u3"}:   {PodIP: "10.0.0.3"},
			{Namespace: "default", Name: "p4", UID: "u4"}:   {PodIP: "10.0.0.4"},
			{Namespace: "default", Name: "p5", UID: "u5"}:   {PodIP: "10.0.0.5"},
			{Namespace: "default", Name: "p6", UID: "u6"}:   {PodIP: "10.0.0.6"},
			{Namespace: "default", Name: "p7", UID: "u7"}:   {PodIP: "10.0.0.7"},
			{Namespace: "default", Name: "p8", UID: "u8"}:   {PodIP: "10.0.0.8"},
			{Namespace: "default", Name: "p9", UID: "u9"}:   {PodIP: "10.0.0.9"},
			{Namespace: "default", Name: "p10", UID: "u10"}: {PodIP: "10.0.0.10"},
		},
	}
	tracker.ObserveSnapshot(rec, key, snap1, t1)

	// +70s (beyond 60s window): no changes, same snapshot
	t2 := t0.Add(70 * time.Second)
	tracker.ObserveSnapshot(rec, key, snap1, t2)

	// The original interval [t0, t1] ends at t0+5s, which is 65s before t2.
	// That's outside the 60s window [t2-60s, t2] = [t0+10s, t0+70s].
	// So it should be fully pruned.
	// Rate should be 0 (no changes within window).
	rate := getGaugeValue(t, rec, "default", "prune-test")
	if rate != 0 {
		t.Errorf("pod_churn_rate after window expiry = %v, want 0 (interval fully pruned)", rate)
	}
}

// TestPhase8_PodChurnWindow_PartialOverlap verifies correct weighted contribution
// when an interval partially overlaps the 60s window boundary.
func TestPhase8_PodChurnWindow_PartialOverlap(t *testing.T) {
	rec := newPodChurnRecorder()
	tracker := NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "default", Name: "overlap-test"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Baseline at t0
	empty := MappingPodSnapshot{Pods: map[PodIdentity]PodMembership{}}
	tracker.ObserveSnapshot(rec, key, empty, t0)

	// +20s: add 6 pods (interval [t0, t0+20s], 20s duration, 6 changes)
	t1 := t0.Add(20 * time.Second)
	snap1 := MappingPodSnapshot{
		Pods: map[PodIdentity]PodMembership{
			{Namespace: "default", Name: "p1", UID: "u1"}: {PodIP: "10.0.0.1"},
			{Namespace: "default", Name: "p2", UID: "u2"}: {PodIP: "10.0.0.2"},
			{Namespace: "default", Name: "p3", UID: "u3"}: {PodIP: "10.0.0.3"},
			{Namespace: "default", Name: "p4", UID: "u4"}: {PodIP: "10.0.0.4"},
			{Namespace: "default", Name: "p5", UID: "u5"}: {PodIP: "10.0.0.5"},
			{Namespace: "default", Name: "p6", UID: "u6"}: {PodIP: "10.0.0.6"},
		},
	}
	tracker.ObserveSnapshot(rec, key, snap1, t1)

	// +70s: same snapshot (no new changes)
	// Window is [t0+10s, t0+70s]. Interval [t0, t0+20s] partially overlaps:
	// overlap = [t0+10s, t0+20s] = 10s out of 20s interval duration
	// Weighted contribution = 6 * (10/20) = 3
	// Rate = 3 / 60 = 0.05
	t2 := t0.Add(70 * time.Second)
	tracker.ObserveSnapshot(rec, key, snap1, t2)

	rate := getGaugeValue(t, rec, "default", "overlap-test")
	expectedRate := 3.0 / 60.0 // 0.05
	if rate < expectedRate*0.9 || rate > expectedRate*1.1 {
		t.Errorf("pod_churn_rate partial overlap = %v, want ~%v", rate, expectedRate)
	}
}

// TestPhase8_PodChurnWindow_NonMonotonicTimeSkipsInterval verifies that
// non-monotonic timestamps (now <= prev) skip interval insertion safely.
func TestPhase8_PodChurnWindow_NonMonotonicTimeSkipsInterval(t *testing.T) {
	rec := newPodChurnRecorder()
	tracker := NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "default", Name: "mono-test"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Baseline
	empty := MappingPodSnapshot{Pods: map[PodIdentity]PodMembership{}}
	tracker.ObserveSnapshot(rec, key, empty, t0)

	// Add pod at t0+10s
	t1 := t0.Add(10 * time.Second)
	snap := MappingPodSnapshot{
		Pods: map[PodIdentity]PodMembership{
			{Namespace: "default", Name: "p1", UID: "u1"}: {PodIP: "10.0.0.1"},
		},
	}
	tracker.ObserveSnapshot(rec, key, snap, t1)

	// Non-monotonic: observe again at t1 (same time or earlier)
	snap2 := MappingPodSnapshot{
		Pods: map[PodIdentity]PodMembership{
			{Namespace: "default", Name: "p1", UID: "u1"}: {PodIP: "10.0.0.1"},
			{Namespace: "default", Name: "p2", UID: "u2"}: {PodIP: "10.0.0.2"},
		},
	}
	// Same time - should not panic or produce NaN
	delta := tracker.ObserveSnapshot(rec, key, snap2, t1)

	// Counter should still record the add
	if delta.Added != 1 {
		t.Errorf("delta.Added = %d, want 1 (counter still records even with non-monotonic time)", delta.Added)
	}

	// Rate gauge should not be NaN or negative
	rate := getGaugeValue(t, rec, "default", "mono-test")
	if rate < 0 {
		t.Errorf("pod_churn_rate = %v, want >= 0 with non-monotonic time", rate)
	}
}

// TestPhase8_PodChurnWindow_QuietWindowResetsToZero verifies that the gauge
// is set to 0 when there are no changes within the window.
func TestPhase8_PodChurnWindow_QuietWindowResetsToZero(t *testing.T) {
	rec := newPodChurnRecorder()
	tracker := NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "default", Name: "quiet-test"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Baseline
	empty := MappingPodSnapshot{Pods: map[PodIdentity]PodMembership{}}
	tracker.ObserveSnapshot(rec, key, empty, t0)

	// Add a pod
	t1 := t0.Add(5 * time.Second)
	snap := MappingPodSnapshot{
		Pods: map[PodIdentity]PodMembership{
			{Namespace: "default", Name: "p1", UID: "u1"}: {PodIP: "10.0.0.1"},
		},
	}
	tracker.ObserveSnapshot(rec, key, snap, t1)

	// Verify non-zero rate initially
	rate1 := getGaugeValue(t, rec, "default", "quiet-test")
	if rate1 <= 0 {
		t.Fatalf("expected positive rate after initial add, got %v", rate1)
	}

	// 120s later, same snapshot (no changes, well beyond 60s window)
	t2 := t0.Add(120 * time.Second)
	tracker.ObserveSnapshot(rec, key, snap, t2)

	rate2 := getGaugeValue(t, rec, "default", "quiet-test")
	if rate2 != 0 {
		t.Errorf("pod_churn_rate after quiet period = %v, want 0", rate2)
	}
}
