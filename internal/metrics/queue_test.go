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
// and in-flight gauge across Add, Get, Done, and Forget operations.
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

	// Verify depth = 3, inflight = 0
	depth := getQueueDepthGauge(t, rec.Reconcile)
	if depth != 3 {
		t.Errorf("after 3 Add: queue_depth = %v, want 3", depth)
	}
	inflight := getInflightGauge(t, rec.Reconcile)
	if inflight != 0 {
		t.Errorf("after 3 Add: inflight = %v, want 0", inflight)
	}

	// Get 1 item (processing)
	item, shutdown := queue.Get()
	if shutdown {
		t.Fatal("unexpected queue shutdown")
	}

	// Get reduces pending; inflight increases
	depthAfterGet := getQueueDepthGauge(t, rec.Reconcile)
	if depthAfterGet != 2 {
		t.Errorf("after Get: queue_depth = %v, want 2", depthAfterGet)
	}
	inflightAfterGet := getInflightGauge(t, rec.Reconcile)
	if inflightAfterGet != 1 {
		t.Errorf("after Get: inflight = %v, want 1", inflightAfterGet)
	}

	// Done marks item as complete; inflight decreases
	queue.Done(item)
	depthAfterDone := getQueueDepthGauge(t, rec.Reconcile)
	if depthAfterDone != 2 {
		t.Errorf("after Done: queue_depth = %v, want 2", depthAfterDone)
	}
	inflightAfterDone := getInflightGauge(t, rec.Reconcile)
	if inflightAfterDone != 0 {
		t.Errorf("after Done: inflight = %v, want 0", inflightAfterDone)
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
	finalInflight := getInflightGauge(t, rec.Reconcile)
	if finalInflight != 0 {
		t.Errorf("after draining: inflight = %v, want 0", finalInflight)
	}

	queue.ShutDown()
}

func readGaugeByName(t *testing.T, rec *ReconcileRecorder, name string) float64 {
	t.Helper()
	for _, c := range rec.Collectors() {
		g, ok := c.(prometheus.Gauge)
		if !ok {
			continue
		}
		desc := g.Desc().String()
		if !containsMetricName(desc, name) {
			continue
		}
		var m dto.Metric
		if err := g.Write(&m); err != nil {
			continue
		}
		if m.GetGauge() != nil {
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("could not read %s gauge", name)
	return 0
}

func containsMetricName(desc, name string) bool {
	// Desc().String() contains fqName="<name>"
	return len(desc) > 0 && contains(desc, "\""+name+"\"")
}

func contains(s, substr string) bool {
	return len(substr) <= len(s) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func getQueueDepthGauge(t *testing.T, rec *ReconcileRecorder) float64 {
	t.Helper()
	return readGaugeByName(t, rec, "pod_nsg_controller_reconcile_queue_depth")
}

func getInflightGauge(t *testing.T, rec *ReconcileRecorder) float64 {
	t.Helper()
	return readGaugeByName(t, rec, "pod_nsg_controller_reconcile_inflight")
}
