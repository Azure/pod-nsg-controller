// Package controllertest provides test-only helpers for Phase 7 acceptance
// validation. These helpers are separated from the production controller
// package to keep test scaffolding out of the shipped binary.
package controllertest

import (
	"fmt"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/controller"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

// PartialFailureReport aggregates Phase 7 partial failure analysis for acceptance testing.
type PartialFailureReport struct {
	Summary          controller.ActionFailureSummary
	SucceededTargets []string
	FailedTargets    []string
	RequeueResult    ctrl.Result
	StatusSnapshot   v1alpha1.PodASGMappingStatus
}

// BuildPartialFailureReport analyzes action results against the spec and policy,
// producing a combined report that includes classification, status mapping, and requeue decision.
func BuildPartialFailureReport(
	results []azure.ActionResult,
	spec v1alpha1.PodASGMappingSpec,
	prefixSetName string,
	p controller.RequeuePolicy,
) PartialFailureReport {
	report := PartialFailureReport{}

	normalized := make([]azure.ActionResult, len(results))
	copy(normalized, results)
	for i := range normalized {
		if normalized[i].Err == nil {
			normalized[i].Success = true
		}
	}

	report.Summary = controller.ClassifyActionResults(normalized)

	for _, r := range normalized {
		if r.Err == nil {
			report.SucceededTargets = append(report.SucceededTargets, r.Action.Target.ASGName)
		} else {
			report.FailedTargets = append(report.FailedTargets, r.Action.Target.ASGName)
		}
	}

	report.RequeueResult = controller.DecideRequeueFromActionSummary(report.Summary, p)

	var reconcileErr error
	if len(report.FailedTargets) > 0 {
		reconcileErr = fmt.Errorf("partial failure: %d of %d actions failed", len(report.FailedTargets), len(results))
	}

	report.StatusSnapshot = controller.ComputeStatus(controller.ComputeStatusInput{
		Spec:               spec,
		PrefixSetName:      prefixSetName,
		Results:            normalized,
		ReconcileErr:       reconcileErr,
		ObservedGeneration: 1,
		Phase:              controller.StatusPhaseFinal,
		Now:                metav1.Now(),
	})

	return report
}

// StatusContractViolation describes a violation of the Phase 7 status contract.
type StatusContractViolation struct {
	Field   string
	Got     string
	Want    string
	Message string
}

// ValidatePhase7StatusContract checks if a PodASGMappingStatus satisfies the Phase 7
// partial failure status contract requirements (conditions, mappingStatuses, sync states).
func ValidatePhase7StatusContract(
	status v1alpha1.PodASGMappingStatus,
	results []azure.ActionResult,
	spec v1alpha1.PodASGMappingSpec,
	prefixSetName string,
) []StatusContractViolation {
	var violations []StatusContractViolation

	hasFailures := false
	for _, r := range results {
		if r.Err != nil {
			hasFailures = true
			break
		}
	}

	reconciledCond := findCondition(status.Conditions, controller.ConditionReconciled)
	if hasFailures {
		if reconciledCond == nil {
			violations = append(violations, StatusContractViolation{
				Field:   "conditions[Reconciled]",
				Got:     "<missing>",
				Want:    "False",
				Message: "Reconciled condition must exist when there are failures",
			})
		} else if reconciledCond.Status != metav1.ConditionFalse {
			violations = append(violations, StatusContractViolation{
				Field:   "conditions[Reconciled].Status",
				Got:     string(reconciledCond.Status),
				Want:    string(metav1.ConditionFalse),
				Message: "Reconciled must be False when actions fail",
			})
		} else if reconciledCond.Reason != controller.ReasonReconcileFailed {
			violations = append(violations, StatusContractViolation{
				Field:   "conditions[Reconciled].Reason",
				Got:     reconciledCond.Reason,
				Want:    controller.ReasonReconcileFailed,
				Message: "Reconciled reason must be ReconcileFailed",
			})
		}
	}

	if len(status.MappingStatuses) != len(spec.Mappings) {
		violations = append(violations, StatusContractViolation{
			Field:   "mappingStatuses",
			Got:     fmt.Sprintf("%d rows", len(status.MappingStatuses)),
			Want:    fmt.Sprintf("%d rows", len(spec.Mappings)),
			Message: "mappingStatuses count must match spec mappings count",
		})
	}

	failedTargets := make(map[string]bool)
	for _, r := range results {
		if r.Err != nil {
			key := engine.TargetIdentityKey(r.Action.Target.FullResourceID, r.Action.Target.PrefixSetName)
			failedTargets[key] = true
		}
	}

	for i, m := range spec.Mappings {
		if i >= len(status.MappingStatuses) {
			break
		}
		ms := status.MappingStatuses[i]

		hasRowFailure := false
		for _, asgRef := range m.ApplicationSecurityGroups {
			parsed, err := model.ParseASGResourceID(asgRef.ResourceID)
			if err != nil {
				continue
			}
			key := engine.TargetIdentityKey(parsed.FullResourceID, prefixSetName)
			if failedTargets[key] {
				hasRowFailure = true
				break
			}
		}

		if hasRowFailure {
			if ms.ASGSyncState != controller.SyncStateError {
				violations = append(violations, StatusContractViolation{
					Field:   fmt.Sprintf("mappingStatuses[%d].asgSyncState", i),
					Got:     ms.ASGSyncState,
					Want:    controller.SyncStateError,
					Message: "row with failed target must be Error",
				})
			}
		} else {
			if ms.ASGSyncState != controller.SyncStateSynced {
				violations = append(violations, StatusContractViolation{
					Field:   fmt.Sprintf("mappingStatuses[%d].asgSyncState", i),
					Got:     ms.ASGSyncState,
					Want:    controller.SyncStateSynced,
					Message: "row with all successful targets must be Synced",
				})
			}
		}
	}

	if hasFailures && len(status.MappingStatuses) > 0 {
		hasErrorRow := false
		for _, ms := range status.MappingStatuses {
			if ms.ASGSyncState == controller.SyncStateError {
				hasErrorRow = true
				break
			}
		}
		if !hasErrorRow {
			violations = append(violations, StatusContractViolation{
				Field:   "mappingStatuses",
				Got:     "no Error rows",
				Want:    "at least one Error row",
				Message: "action failures exist but no MappingStatus row is Error",
			})
		}
	}

	return violations
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
