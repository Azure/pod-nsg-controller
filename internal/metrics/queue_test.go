package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TestPhase8_QueueFactory_TracksQueueDepthAcrossAddGetDoneForget
// Verifies the instrumented queue factory correctly tracks queue depth gauge
// across Add, Get, Done, and Forget operations.
func TestPhase8_QueueFactory_TracksQueueDepthAcrossAddGetDoneForget(t *testing.T) {
	ResetForTesting()
	defer ResetForTesting()
	rec, _ := Register()

	factory := NewInstrumentedQueueFactory(rec.Reconcile)
	if factory == nil {
		t.Fatal("NewInstrumentedQueueFactory returned nil")
	}

	rateLimiter := workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]()
	queue := factory("test-controller", rateLimiter)
	if queue == nil {
		t.Fatal("queue factory returned nil queue")
	}

	// Add 3 items
	queue.Add(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "a"}})
	queue.Add(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "b"}})
	queue.Add(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "c"}})

	// Verify depth = 3
	depth := getQueueDepthGauge(t, rec.Reconcile)
	if depth != 3 {
		t.Errorf("after 3 Add: queue_depth = %v, want 3", depth)
	}

	// Get 1 item (processing)
	item, shutdown := queue.Get()
	if shutdown {
		t.Fatal("unexpected queue shutdown")
	}

	// Get reduces pending, but item is still tracked until Done
	depthAfterGet := getQueueDepthGauge(t, rec.Reconcile)
	if depthAfterGet != 2 {
		t.Errorf("after Get: queue_depth = %v, want 2", depthAfterGet)
	}

	// Done marks item as complete
	queue.Done(item)
	depthAfterDone := getQueueDepthGauge(t, rec.Reconcile)
	if depthAfterDone != 2 {
		t.Errorf("after Done: queue_depth = %v, want 2", depthAfterDone)
	}

	// Get and Done remaining
	item2, _ := queue.Get()
	queue.Done(item2)
	item3, _ := queue.Get()
	queue.Done(item3)

	finalDepth := getQueueDepthGauge(t, rec.Reconcile)
	if finalDepth != 0 {
		t.Errorf("after draining: queue_depth = %v, want 0", finalDepth)
	}

	queue.ShutDown()
}

func getQueueDepthGauge(t *testing.T, rec *ReconcileRecorder) float64 {
	t.Helper()
	collectors := rec.Collectors()
	for _, c := range collectors {
		if g, ok := c.(prometheus.Gauge); ok {
			var m dto.Metric
			if err := g.Write(&m); err != nil {
				continue
			}
			if m.GetGauge() != nil {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatal("could not read reconcile_queue_depth gauge")
	return 0
}
