package metrics

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// pollGaugeUntil polls the queue depth gauge until it equals the expected value
// or the deadline expires. Returns the last observed value.
func pollGaugeUntil(t *testing.T, rec *ReconcileRecorder, expected float64, timeout time.Duration) float64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var val float64
	for time.Now().Before(deadline) {
		val = getQueueDepthGauge(t, rec)
		if val == expected {
			return val
		}
		time.Sleep(10 * time.Millisecond)
	}
	return val
}

// TestPhase8_QueueDepth_AddAfterDelayed_DoesNotInflateImmediateDepth verifies that
// AddAfter with a positive delay does NOT inflate the immediate queue depth gauge.
// Items added with delay are not ready yet and should not count toward depth.
func TestPhase8_QueueDepth_AddAfterDelayed_DoesNotInflateImmediateDepth(t *testing.T) {
	ResetForTesting()
	defer ResetForTesting()
	rec, _ := Register()

	factory := NewInstrumentedQueueFactory(rec.Reconcile)
	rateLimiter := workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]()
	queue := factory("test-delayed", rateLimiter)
	defer queue.ShutDown()

	// Add one item immediately
	queue.Add(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "immediate"}})

	depth := getQueueDepthGauge(t, rec.Reconcile)
	if depth != 1 {
		t.Fatalf("after Add: queue_depth = %v, want 1", depth)
	}

	// Add item with delay - should NOT inflate depth
	queue.AddAfter(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "delayed-1"}}, 5*time.Second)
	queue.AddAfter(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "delayed-2"}}, 10*time.Second)

	depthAfterDelayed := getQueueDepthGauge(t, rec.Reconcile)
	if depthAfterDelayed != 1 {
		t.Errorf("after AddAfter(5s): queue_depth = %v, want 1 (delayed items should not inflate depth)", depthAfterDelayed)
	}
}

// TestPhase8_QueueDepth_AddAfterDelayed_IncrementsWhenItemBecomesReady verifies that
// when a delayed item becomes ready (after its delay expires), the queue depth
// gauge is updated to reflect the newly ready item.
func TestPhase8_QueueDepth_AddAfterDelayed_IncrementsWhenItemBecomesReady(t *testing.T) {
	ResetForTesting()
	defer ResetForTesting()
	rec, _ := Register()

	factory := NewInstrumentedQueueFactory(rec.Reconcile)
	rateLimiter := workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]()
	queue := factory("test-ready", rateLimiter)
	defer queue.ShutDown()

	// Add item with very short delay so it becomes ready quickly
	queue.AddAfter(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "short-delay"}}, 50*time.Millisecond)

	// Initially depth should be 0 (item is delayed)
	initialDepth := getQueueDepthGauge(t, rec.Reconcile)
	if initialDepth != 0 {
		t.Errorf("immediately after AddAfter(50ms): queue_depth = %v, want 0", initialDepth)
	}

	// Poll until the sampler detects the item has become ready
	readyDepth := pollGaugeUntil(t, rec.Reconcile, 1, 2*time.Second)
	if readyDepth != 1 {
		t.Errorf("after delay expires: queue_depth = %v, want 1 (item should be ready)", readyDepth)
	}

	// Drain the item
	item, shutdown := queue.Get()
	if shutdown {
		t.Fatal("unexpected shutdown")
	}
	queue.Done(item)

	finalDepth := getQueueDepthGauge(t, rec.Reconcile)
	if finalDepth != 0 {
		t.Errorf("after draining: queue_depth = %v, want 0", finalDepth)
	}
}

// TestPhase8_QueueDepth_AddRateLimited_DelayedBehaviorMatchesReadyDepthSemantics verifies
// that AddRateLimited sets depth from Len() (ready-only count), matching the
// semantics where delayed items do not inflate immediate depth.
func TestPhase8_QueueDepth_AddRateLimited_DelayedBehaviorMatchesReadyDepthSemantics(t *testing.T) {
	ResetForTesting()
	defer ResetForTesting()
	rec, _ := Register()

	factory := NewInstrumentedQueueFactory(rec.Reconcile)
	rateLimiter := workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]()
	queue := factory("test-rate-limited", rateLimiter)
	defer queue.ShutDown()

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "rl-item"}}

	// First AddRateLimited should add immediately (baseDelay from default limiter)
	queue.AddRateLimited(req)

	// The depth should reflect Len() which counts only ready items.
	// Poll until the rate-limited item becomes ready.
	pollGaugeUntil(t, rec.Reconcile, float64(1), 2*time.Second)

	depth := getQueueDepthGauge(t, rec.Reconcile)
	actualLen := queue.Len()

	// The depth gauge must match Len() (ready-only depth)
	if int(depth) != actualLen {
		t.Errorf("queue_depth gauge (%v) != Len() (%d); depth must reflect ready-only semantics",
			depth, actualLen)
	}
}

// TestPhase8_QueueDepth_AddAfterZeroDelay_ImmediatelyReflected verifies that
// AddAfter with duration <= 0 is treated as immediate and reflected in depth.
func TestPhase8_QueueDepth_AddAfterZeroDelay_ImmediatelyReflected(t *testing.T) {
	ResetForTesting()
	defer ResetForTesting()
	rec, _ := Register()

	factory := NewInstrumentedQueueFactory(rec.Reconcile)
	rateLimiter := workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]()
	queue := factory("test-zero-delay", rateLimiter)
	defer queue.ShutDown()

	// AddAfter with 0 duration = immediate
	queue.AddAfter(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "zero-delay"}}, 0)

	// Poll until the queue processes the zero-delay item
	depth := pollGaugeUntil(t, rec.Reconcile, 1, 2*time.Second)
	if depth != 1 {
		t.Errorf("after AddAfter(0): queue_depth = %v, want 1 (zero delay = immediate)", depth)
	}
}

// ---------------------------------------------------------------------------
// Phase 3: TestPhase3_QueueDepth_BurstAddAfterSameKey_CoalescesToSingleItem
// Burst AddAfter of identical request coalesces to one ready item.
// ---------------------------------------------------------------------------
func TestPhase3_QueueDepth_BurstAddAfterSameKey_CoalescesToSingleItem(t *testing.T) {
	ResetForTesting()
	defer ResetForTesting()
	rec, _ := Register()

	factory := NewInstrumentedQueueFactory(rec.Reconcile)
	rateLimiter := workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]()
	queue := factory("test-burst-coalesce", rateLimiter)
	defer queue.ShutDown()

	// Burst 10 AddAfter calls with the same key and short delay
	sameReq := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "burst-key"}}
	for i := 0; i < 10; i++ {
		queue.AddAfter(sameReq, 50*time.Millisecond)
	}

	// Immediately after burst, depth should be 0 (all delayed)
	immediateDepth := getQueueDepthGauge(t, rec.Reconcile)
	if immediateDepth != 0 {
		t.Errorf("Phase 3: immediately after burst AddAfter, queue_depth = %v, want 0 (items are delayed)", immediateDepth)
	}

	// Wait for delay to expire
	readyDepth := pollGaugeUntil(t, rec.Reconcile, 1, 2*time.Second)
	if readyDepth != 1 {
		t.Errorf("Phase 3: after delay expires, burst of same key should coalesce to 1 ready item, got queue_depth = %v", readyDepth)
	}

	// Drain and verify only 1 item is available
	item, shutdown := queue.Get()
	if shutdown {
		t.Fatal("unexpected shutdown")
	}
	queue.Done(item)

	if item.NamespacedName != sameReq.NamespacedName {
		t.Errorf("Phase 3: drained item = %v, want %v", item.NamespacedName, sameReq.NamespacedName)
	}

	// Queue should now be empty
	finalDepth := getQueueDepthGauge(t, rec.Reconcile)
	if finalDepth != 0 {
		t.Errorf("Phase 3: after draining single coalesced item, queue_depth = %v, want 0", finalDepth)
	}
}
