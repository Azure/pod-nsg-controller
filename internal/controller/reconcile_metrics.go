package controller

import (
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/metrics"
)

// ReconcileMetricStage identifies the exit path of a reconcile cycle for metric classification.
type ReconcileMetricStage string

const (
	ReconcileStageFinalizerAddEarlyReturn ReconcileMetricStage = "finalizer-add-early-return"
	ReconcileStageValidationTerminal      ReconcileMetricStage = "validation-terminal"
	ReconcileStageSteadyStateSuccess      ReconcileMetricStage = "steady-state-success"
	ReconcileStageActionMixedFailure      ReconcileMetricStage = "action-mixed-failure"
	ReconcileStageActionAllFailure        ReconcileMetricStage = "action-all-failure"
	ReconcileStageSystemError             ReconcileMetricStage = "system-error"
	ReconcileStageStatusWriteError        ReconcileMetricStage = "status-write-error"
	ReconcileStageDeleteComplete          ReconcileMetricStage = "delete-complete"
	ReconcileStageMappingNotFound         ReconcileMetricStage = "mapping-not-found"
	ReconcileStageStatusSentinelTerminal  ReconcileMetricStage = "status-sentinel-terminal"
)

// InitialTerminalReason describes why a reconcile outcome is terminal for initial-reconcile tracking.
type InitialTerminalReason string

const (
	InitialTerminalSteadyState    InitialTerminalReason = "steady-state"
	InitialTerminalValidation     InitialTerminalReason = "validation"
	InitialTerminalDeleteComplete InitialTerminalReason = "delete-complete"
	InitialTerminalStaleGeneration InitialTerminalReason = "stale-generation"
	InitialTerminalStatusNotFound InitialTerminalReason = "status-not-found"
	InitialTerminalMappingNotFound InitialTerminalReason = "mapping-not-found"
)

// ClassifyReconcileMetricResult maps a reconcile exit stage to its metric result label.
func ClassifyReconcileMetricResult(stage ReconcileMetricStage) metrics.ReconcileResult {
	switch stage {
	case ReconcileStageFinalizerAddEarlyReturn:
		return metrics.ReconcileResultRequeue
	case ReconcileStageValidationTerminal,
		ReconcileStageSteadyStateSuccess,
		ReconcileStageDeleteComplete,
		ReconcileStageMappingNotFound,
		ReconcileStageStatusSentinelTerminal:
		return metrics.ReconcileResultSuccess
	case ReconcileStageActionMixedFailure:
		return metrics.ReconcileResultPartialFailure
	case ReconcileStageActionAllFailure,
		ReconcileStageSystemError,
		ReconcileStageStatusWriteError:
		return metrics.ReconcileResultError
	default:
		return metrics.ReconcileResultError
	}
}

// ClassifyCRDResolutionOperation classifies an action set into a CRD resolution operation label.
// Rules (from spec 4.7):
// 1. If any UpdatePrefixSet → "update"
// 2. If both Create and Delete → "update"
// 3. If only Create → "add"
// 4. If only Delete → "delete"
// 5. Empty → "update" (deterministic no-op)
func ClassifyCRDResolutionOperation(actions []engine.Action) string {
	if len(actions) == 0 {
		return "update"
	}

	hasCreate := false
	hasUpdate := false
	hasDelete := false

	for _, a := range actions {
		switch a.Kind {
		case engine.CreatePrefixSet:
			hasCreate = true
		case engine.UpdatePrefixSet:
			hasUpdate = true
		case engine.DeletePrefixSet:
			hasDelete = true
		}
	}

	if hasUpdate {
		return "update"
	}
	if hasCreate && hasDelete {
		return "update"
	}
	if hasCreate {
		return "add"
	}
	if hasDelete {
		return "delete"
	}
	return "update"
}
