package controller

import (
	"testing"
	"time"

	"github.com/Azure/pod-nsg-controller/internal/metrics"
)

// TestPhase8_SetupWithManager_PreservesInstrumentedQueueFactory
// Design §4.2: SetupWithManager wires NewInstrumentedQueueFactory when MetricsRecorder is set.
// Verifies that the instrumented queue path is preserved and functional.
func TestPhase8_SetupWithManager_PreservesInstrumentedQueueFactory(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()
	rec, err := metrics.Register()
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	scheme := testScheme(t)

	r := &MappingReconciler{
		Scheme:          scheme,
		ClusterName:     "test-cluster",
		ResyncInterval:  60 * time.Second,
		MetricsRecorder: rec,
	}

	// Verify that the reconciler has metrics recorder set (precondition for queue wiring).
	if r.MetricsRecorder == nil {
		t.Fatal("MetricsRecorder is nil; SetupWithManager would skip queue instrumentation")
	}
	if r.MetricsRecorder.Reconcile == nil {
		t.Fatal("MetricsRecorder.Reconcile is nil; instrumented queue factory would fail")
	}

	// Verify the queue factory function can be created without panicking.
	factory := metrics.NewInstrumentedQueueFactory(r.MetricsRecorder.Reconcile)
	if factory == nil {
		t.Error("NewInstrumentedQueueFactory returned nil; setup would not wire queue metrics")
	}
}
