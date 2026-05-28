package controller

import (
	"testing"

	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/metrics"
)

// TestPhase8_ReconcileResultClassification_RequeuePaths
// Both finalizer-add early return and debounced paths should be classified as "requeue".
func TestPhase8_ReconcileResultClassification_RequeuePaths(t *testing.T) {
	tests := []struct {
		name  string
		stage ReconcileMetricStage
	}{
		{"finalizer-add-early-return", ReconcileStageFinalizerAddEarlyReturn},
		{"debounced", ReconcileStageDebounced},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := ClassifyReconcileMetricResult(tc.stage)
			if result != metrics.ReconcileResultRequeue {
				t.Errorf("ClassifyReconcileMetricResult(%q) = %q, want %q",
					tc.stage, result, metrics.ReconcileResultRequeue)
			}
		})
	}
}

// TestPhase8_ReconcileResultClassification_StatusWriteFailureIsError
// Status write failures should be classified as "error".
func TestPhase8_ReconcileResultClassification_StatusWriteFailureIsError(t *testing.T) {
	result := ClassifyReconcileMetricResult(ReconcileStageStatusWriteError)
	if result != metrics.ReconcileResultError {
		t.Errorf("ClassifyReconcileMetricResult(%q) = %q, want %q",
			ReconcileStageStatusWriteError, result, metrics.ReconcileResultError)
	}
}

// TestPhase8_ReconcileResultClassification_ValidationAndSteadyStateAreSuccess
// Both validation-terminal and steady-state-success should classify as "success".
func TestPhase8_ReconcileResultClassification_ValidationAndSteadyStateAreSuccess(t *testing.T) {
	tests := []struct {
		name  string
		stage ReconcileMetricStage
	}{
		{"validation-terminal", ReconcileStageValidationTerminal},
		{"steady-state-success", ReconcileStageSteadyStateSuccess},
		{"delete-complete", ReconcileStageDeleteComplete},
		{"mapping-not-found", ReconcileStageMappingNotFound},
		{"status-sentinel-terminal", ReconcileStageStatusSentinelTerminal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := ClassifyReconcileMetricResult(tc.stage)
			if result != metrics.ReconcileResultSuccess {
				t.Errorf("ClassifyReconcileMetricResult(%q) = %q, want %q",
					tc.stage, result, metrics.ReconcileResultSuccess)
			}
		})
	}
}

// TestPhase8_ReconcileResultClassification_ActionFailures
// Mixed action failures should be partial_failure; all-failure should be error.
func TestPhase8_ReconcileResultClassification_ActionFailures(t *testing.T) {
	tests := []struct {
		name   string
		stage  ReconcileMetricStage
		expect metrics.ReconcileResult
	}{
		{"mixed-failure", ReconcileStageActionMixedFailure, metrics.ReconcileResultPartialFailure},
		{"all-failure", ReconcileStageActionAllFailure, metrics.ReconcileResultError},
		{"system-error", ReconcileStageSystemError, metrics.ReconcileResultError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := ClassifyReconcileMetricResult(tc.stage)
			if result != tc.expect {
				t.Errorf("ClassifyReconcileMetricResult(%q) = %q, want %q",
					tc.stage, result, tc.expect)
			}
		})
	}
}

// --- CRD Resolution Operation Classification Tests (Section 5.4) ---

// TestPhase8_CRDResolutionClassification_CreateOnlyAdd
// If only CreatePrefixSet actions exist → "add".
func TestPhase8_CRDResolutionClassification_CreateOnlyAdd(t *testing.T) {
	actions := []engine.Action{
		{Kind: engine.CreatePrefixSet},
		{Kind: engine.CreatePrefixSet},
	}
	op := ClassifyCRDResolutionOperation(actions)
	if op != "add" {
		t.Errorf("ClassifyCRDResolutionOperation(creates-only) = %q, want %q", op, "add")
	}
}

// TestPhase8_CRDResolutionClassification_DeleteOnlyDelete
// If only DeletePrefixSet actions exist → "delete".
func TestPhase8_CRDResolutionClassification_DeleteOnlyDelete(t *testing.T) {
	actions := []engine.Action{
		{Kind: engine.DeletePrefixSet},
	}
	op := ClassifyCRDResolutionOperation(actions)
	if op != "delete" {
		t.Errorf("ClassifyCRDResolutionOperation(deletes-only) = %q, want %q", op, "delete")
	}
}

// TestPhase8_CRDResolutionClassification_MixedCreateDeleteUpdate
// If both Create and Delete exist (but no Update) → "update".
func TestPhase8_CRDResolutionClassification_MixedCreateDeleteUpdate(t *testing.T) {
	actions := []engine.Action{
		{Kind: engine.CreatePrefixSet},
		{Kind: engine.DeletePrefixSet},
	}
	op := ClassifyCRDResolutionOperation(actions)
	if op != "update" {
		t.Errorf("ClassifyCRDResolutionOperation(create+delete) = %q, want %q", op, "update")
	}
}

// TestPhase8_CRDResolutionClassification_UpdatePresentIsUpdate
// If any UpdatePrefixSet exists → "update" (highest priority).
func TestPhase8_CRDResolutionClassification_UpdatePresentIsUpdate(t *testing.T) {
	actions := []engine.Action{
		{Kind: engine.CreatePrefixSet},
		{Kind: engine.UpdatePrefixSet},
		{Kind: engine.DeletePrefixSet},
	}
	op := ClassifyCRDResolutionOperation(actions)
	if op != "update" {
		t.Errorf("ClassifyCRDResolutionOperation(with-update) = %q, want %q", op, "update")
	}
}

// TestPhase8_CRDResolutionClassification_NoActionsDefaultsUpdate
// Empty action set → "update" (deterministic no-op classification).
func TestPhase8_CRDResolutionClassification_NoActionsDefaultsUpdate(t *testing.T) {
	actions := []engine.Action{}
	op := ClassifyCRDResolutionOperation(actions)
	if op != "update" {
		t.Errorf("ClassifyCRDResolutionOperation(empty) = %q, want %q", op, "update")
	}
}

// --- Phase 4: PatchPrefixSet Classification Tests ---

// TestPhase4_CRDResolutionClassification_PatchIsUpdate verifies that
// PatchPrefixSet is classified as "update" in CRD resolution metrics.
func TestPhase4_CRDResolutionClassification_PatchIsUpdate(t *testing.T) {
	actions := []engine.Action{
		{Kind: engine.PatchPrefixSet},
	}
	op := ClassifyCRDResolutionOperation(actions)
	if op != "update" {
		t.Errorf("ClassifyCRDResolutionOperation(PatchPrefixSet) = %q, want %q", op, "update")
	}
}

// TestPhase4_CRDResolutionClassification_PatchWithCreateIsUpdate verifies that
// PatchPrefixSet combined with CreatePrefixSet is classified as "update".
func TestPhase4_CRDResolutionClassification_PatchWithCreateIsUpdate(t *testing.T) {
	actions := []engine.Action{
		{Kind: engine.CreatePrefixSet},
		{Kind: engine.PatchPrefixSet},
	}
	op := ClassifyCRDResolutionOperation(actions)
	if op != "update" {
		t.Errorf("ClassifyCRDResolutionOperation(create+patch) = %q, want %q", op, "update")
	}
}

// TestPhase4_CRDResolutionClassification_PatchOnlyNeverProducesUnknown verifies
// that PatchPrefixSet never produces an "unknown" label (regression guard).
func TestPhase4_CRDResolutionClassification_PatchOnlyNeverProducesUnknown(t *testing.T) {
	actions := []engine.Action{
		{Kind: engine.PatchPrefixSet},
		{Kind: engine.PatchPrefixSet},
	}
	op := ClassifyCRDResolutionOperation(actions)
	validLabels := map[string]bool{"add": true, "update": true, "delete": true}
	if !validLabels[op] {
		t.Errorf("ClassifyCRDResolutionOperation(patches-only) = %q, want one of add|update|delete", op)
	}
}