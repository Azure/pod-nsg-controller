package controller

import (
	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
)

// processedStatusSnapshot captures an immutable snapshot of the mapping state
// at the point reconciliation begins processing. This snapshot is used to build
// the final status input, preventing stale status writes when the live mapping
// is modified between snapshot capture and status update.
type processedStatusSnapshot struct {
	Generation              int64
	Spec                    v1alpha1.PodASGMappingSpec
	PreviousMappingStatuses []v1alpha1.MappingStatus
	PreviousObservedGen     int64
	PodCounts               map[string]int
}

// captureProcessedStatusSnapshot creates an immutable deep-copy snapshot of the
// mapping's generation, spec, previous status context, and pod counts. This must
// be called before any metadata patches or Azure execution to ensure the status
// input is not derived from a refetched (potentially newer) mapping version.
func captureProcessedStatusSnapshot(
	mapping *v1alpha1.PodASGMapping,
	podCounts map[string]int,
) processedStatusSnapshot {
	// Deep-copy spec mappings.
	specCopy := v1alpha1.PodASGMappingSpec{
		Mappings: make([]v1alpha1.Mapping, len(mapping.Spec.Mappings)),
	}
	for i, m := range mapping.Spec.Mappings {
		asgsCopy := make([]v1alpha1.ASGReference, len(m.ApplicationSecurityGroups))
		copy(asgsCopy, m.ApplicationSecurityGroups)
		labelsCopy := make(map[string]string, len(m.PodSelector.MatchLabels))
		for k, v := range m.PodSelector.MatchLabels {
			labelsCopy[k] = v
		}
		specCopy.Mappings[i] = v1alpha1.Mapping{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: labelsCopy},
			ApplicationSecurityGroups: asgsCopy,
		}
	}

	// Deep-copy previous mapping statuses.
	prevStatuses := make([]v1alpha1.MappingStatus, len(mapping.Status.MappingStatuses))
	copy(prevStatuses, mapping.Status.MappingStatuses)

	// Deep-copy pod counts map.
	countsCopy := make(map[string]int, len(podCounts))
	for k, v := range podCounts {
		countsCopy[k] = v
	}

	return processedStatusSnapshot{
		Generation:              mapping.Generation,
		Spec:                    specCopy,
		PreviousMappingStatuses: prevStatuses,
		PreviousObservedGen:     observedGenerationForCondition(mapping.Status.Conditions, ConditionReconciled),
		PodCounts:               countsCopy,
	}
}

// buildPostExecutionStatusInput constructs a ReconcileStatusInput from the
// immutable processed snapshot and execution outcomes. This ensures the status
// input always reflects the spec generation that was actually reconciled, not
// the live (possibly updated) generation.
func buildPostExecutionStatusInput(
	snapshot processedStatusSnapshot,
	results []azure.ActionResult,
	reconcileErr error,
) ReconcileStatusInput {
	return ReconcileStatusInput{
		Phase:                      StatusPhasePostExecution,
		ProcessedGen:               snapshot.Generation,
		ProcessedSpec:              snapshot.Spec,
		PreviousMappingStatuses:    snapshot.PreviousMappingStatuses,
		PreviousObservedGeneration: snapshot.PreviousObservedGen,
		PodCounts:                  snapshot.PodCounts,
		Results:                    results,
		ReconcileErr:               reconcileErr,
	}
}
