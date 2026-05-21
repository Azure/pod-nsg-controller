package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestPhase8_Metrics_RegisterOnce verifies that Register() is idempotent and
// returns the same Recorder on repeated calls.
func TestPhase8_Metrics_RegisterOnce(t *testing.T) {
	ResetForTesting()
	defer ResetForTesting()

	r1, err := Register()
	if err != nil {
		t.Fatalf("Register() returned error: %v", err)
	}
	if r1 == nil {
		t.Fatal("Register() returned nil Recorder")
	}

	r2, err := Register()
	if err != nil {
		t.Fatalf("second Register() returned error: %v", err)
	}
	if r1 != r2 {
		t.Error("Register() returned different Recorder instances on second call; expected singleton")
	}
}

// TestPhase8_Metrics_AllCollectorsPresent verifies that every expected metric
// collector is present in the Recorder after registration.
func TestPhase8_Metrics_AllCollectorsPresent(t *testing.T) {
	ResetForTesting()
	defer ResetForTesting()

	r, err := Register()
	if err != nil {
		t.Fatalf("Register() returned error: %v", err)
	}

	// Verify sub-recorders exist
	if r.PodChurn == nil {
		t.Error("Recorder.PodChurn is nil")
	}
	if r.ARM == nil {
		t.Error("Recorder.ARM is nil")
	}
	if r.Convergence == nil {
		t.Error("Recorder.Convergence is nil")
	}
	if r.Reconcile == nil {
		t.Error("Recorder.Reconcile is nil")
	}

	// Verify all sub-recorders produce non-empty collector lists
	tests := []struct {
		name       string
		collectors []prometheus.Collector
		minCount   int
	}{
		{"PodChurn", r.PodChurn.Collectors(), 2},
		{"ARM", r.ARM.Collectors(), 5},
		{"Convergence", r.Convergence.Collectors(), 3},
		{"Reconcile", r.Reconcile.Collectors(), 7},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.collectors) < tc.minCount {
				t.Errorf("expected at least %d collectors, got %d", tc.minCount, len(tc.collectors))
			}
			for i, c := range tc.collectors {
				if c == nil {
					t.Errorf("collector[%d] is nil", i)
				}
			}
		})
	}

	// Verify AllCollectors aggregation
	allCollectors := r.AllCollectors()
	if allCollectors == nil {
		t.Error("AllCollectors() returned nil; expected aggregated list of all collectors")
	}
	// Total should be >= 2+5+3+7 = 17
	if len(allCollectors) < 17 {
		t.Errorf("AllCollectors() returned %d collectors; expected at least 17", len(allCollectors))
	}
}
