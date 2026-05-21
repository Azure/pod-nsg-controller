package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestPhase8_Register_CollectorsVisibleInControllerRuntimeRegistry
// Verifies that after Register(), all collectors can be registered in a
// prometheus.Registry (simulating controller-runtime's metrics.Registry).
func TestPhase8_Register_CollectorsVisibleInControllerRuntimeRegistry(t *testing.T) {
	ResetForTesting()
	defer ResetForTesting()

	rec, err := Register()
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	// Simulate controller-runtime registry by creating a fresh prometheus.Registry
	reg := prometheus.NewRegistry()
	for i, c := range rec.AllCollectors() {
		if err := reg.Register(c); err != nil {
			t.Errorf("collector[%d] registration failed: %v", i, err)
		}
	}

	// Verify we can gather metrics from the registry
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	if len(mfs) == 0 {
		t.Error("expected non-zero metric families after registration, got 0")
	}
}

// TestPhase8_RegisterWith_AlreadyRegisteredCollectorsRebound
// Verifies that RegisterWith handles AlreadyRegisteredError by rebinding to existing collectors.
func TestPhase8_RegisterWith_AlreadyRegisteredCollectorsRebound(t *testing.T) {
	ResetForTesting()
	defer ResetForTesting()

	reg := prometheus.NewRegistry()

	// First registration
	r1, err := RegisterWith(reg)
	if err != nil {
		t.Fatalf("first RegisterWith() error: %v", err)
	}
	if r1 == nil {
		t.Fatal("first RegisterWith() returned nil Recorder")
	}

	// Reset singleton but re-register with same prometheus registry (simulating restart)
	ResetForTesting()

	r2, err := RegisterWith(reg)
	if err != nil {
		t.Fatalf("second RegisterWith() error: %v", err)
	}
	if r2 == nil {
		t.Fatal("second RegisterWith() returned nil Recorder")
	}

	// Both should be usable (recorder should rebind to existing collectors)
	// Verify no panic on use
	r2.Reconcile.SetQueueDepth(5)
}

// TestPhase8_RegisterWith_TypeMismatchReturnsError
// Verifies that RegisterWith returns an error (not panic) when a collector
// with mismatched type is already registered.
func TestPhase8_RegisterWith_TypeMismatchReturnsError(t *testing.T) {
	ResetForTesting()
	defer ResetForTesting()

	reg := prometheus.NewRegistry()

	// Pre-register a counter with the same name as one of our histograms
	conflicting := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "pod_nsg_controller",
		Name:      "reconcile_duration_seconds",
		Help:      "conflicting type",
	})
	if err := reg.Register(conflicting); err != nil {
		t.Fatalf("pre-registration failed: %v", err)
	}

	// RegisterWith should return error, not panic
	_, err := RegisterWith(reg)
	if err == nil {
		t.Error("RegisterWith() with type-mismatched registry should return error, got nil")
	}
}
