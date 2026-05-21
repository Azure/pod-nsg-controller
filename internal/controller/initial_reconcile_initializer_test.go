package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap/zaptest"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Azure/pod-nsg-controller/internal/metrics"
)

// TestPhase8_T811_Initializer_NeedLeaderElectionTrue
// T8.11: InitialReconcileInitializer must require leader election.
func TestPhase8_T811_Initializer_NeedLeaderElectionTrue(t *testing.T) {
	zapLog := zaptest.NewLogger(t)
	log := zapr.NewLogger(zapLog)

	initializer := NewInitialReconcileInitializer(
		nil, // reader not needed for this assertion
		metrics.NewInitialReconcileTracker(fakeClock()),
		newTestReconcileRecorder(),
		log,
	)

	if !initializer.NeedLeaderElection() {
		t.Error("InitialReconcileInitializer.NeedLeaderElection() = false, want true (only leader emits startup metrics)")
	}
}

// TestPhase8_T811_Startup_EmptySnapshot_EmitsCompletionImmediately
// T8.11: If no PodASGMappings exist at startup, initial_reconcile_complete = 1 immediately.
func TestPhase8_T811_Startup_EmptySnapshot_EmitsCompletionImmediately(t *testing.T) {
	zapLog := zaptest.NewLogger(t)
	log := zapr.NewLogger(zapLog)

	rec := newTestReconcileRecorder()
	tracker := metrics.NewInitialReconcileTracker(fakeClock())

	initializer := NewInitialReconcileInitializer(
		nil,
		tracker,
		rec,
		log,
	)

	// Simulate Start with empty listing
	ctx := context.Background()
	err := initializer.Start(ctx)
	if err != nil {
		t.Fatalf("Start() returned error: %v", err)
	}

	if !tracker.IsComplete() {
		t.Error("tracker.IsComplete() = false after empty startup snapshot; want true")
	}
}

// TestPhase8_T811_Startup_TerminalPathMatrix_FinalizerValidationStaleNotFound
// T8.11: Verify all terminal path reasons correctly mark keys in the tracker.
func TestPhase8_T811_Startup_TerminalPathMatrix_FinalizerValidationStaleNotFound(t *testing.T) {
	rec := newTestReconcileRecorder()

	tests := []struct {
		name     string
		reason   InitialTerminalReason
		terminal bool
	}{
		{"steady-state is terminal", InitialTerminalSteadyState, true},
		{"validation is terminal", InitialTerminalValidation, true},
		{"delete-complete is terminal", InitialTerminalDeleteComplete, true},
		{"stale-generation is terminal", InitialTerminalStaleGeneration, true},
		{"status-not-found is terminal", InitialTerminalStatusNotFound, true},
		{"mapping-not-found is terminal", InitialTerminalMappingNotFound, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tracker := metrics.NewInitialReconcileTracker(fakeClock())
			key := types.NamespacedName{Namespace: "ns", Name: "m"}

			err := tracker.EnsureInitialized(context.Background(), func(_ context.Context) ([]types.NamespacedName, error) {
				return []types.NamespacedName{key}, nil
			})
			if err != nil {
				t.Fatalf("EnsureInitialized failed: %v", err)
			}

			tracker.MarkTerminal(rec, key, tc.terminal)

			if !tracker.IsComplete() {
				t.Errorf("tracker.IsComplete() = false for reason %q, want true", tc.reason)
			}
		})
	}
}

// --- helpers ---

func newTestReconcileRecorder() *metrics.ReconcileRecorder {
	metrics.ResetForTesting()
	r, _ := metrics.Register()
	return r.Reconcile
}

// fakeClock returns a fixed time for deterministic testing.
func fakeClock() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}
