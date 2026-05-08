package controller

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/model"
)

// StatusPhase indicates whether the status computation is pre-Azure (Pending) or post-Azure (Final).
type StatusPhase string

const (
	StatusPhasePending StatusPhase = "Pending"
	StatusPhaseFinal   StatusPhase = "Final"
)

// ValidationIssue records a validation failure for a specific mapping/ASG pair.
type ValidationIssue struct {
	MappingIndex int
	ASGIndex     int
	ResourceID   string
	Err          error
}

// ComputeStatusInput contains all inputs needed to compute a PodASGMappingStatus.
type ComputeStatusInput struct {
	Spec               v1alpha1.PodASGMappingSpec
	PreviousStatus     v1alpha1.PodASGMappingStatus
	PrefixSetName      string
	Results            []azure.ActionResult
	MatchedPodsByIndex []int
	ValidationIssues   []ValidationIssue
	ReconcileErr       error
	ObservedGeneration int64
	Phase              StatusPhase
	Now                metav1.Time
}

// ComputeStatus builds a new PodASGMappingStatus from the given inputs.
func ComputeStatus(in ComputeStatusInput) v1alpha1.PodASGMappingStatus {
	status := v1alpha1.PodASGMappingStatus{
		MappingCount: len(in.Spec.Mappings),
	}

	hasValidationIssues := len(in.ValidationIssues) > 0

	// Set conditions based on state.
	if hasValidationIssues {
		SetCondition(&status, in.ObservedGeneration, ConditionAccepted, metav1.ConditionFalse, ReasonSpecInvalid, aggregateValidationMessage(in.ValidationIssues))
		SetCondition(&status, in.ObservedGeneration, ConditionReconciled, metav1.ConditionFalse, ReasonValidationFailed, "validation failed")
	} else if in.Phase == StatusPhasePending {
		SetCondition(&status, in.ObservedGeneration, ConditionAccepted, metav1.ConditionTrue, ReasonSpecValid, "")
		SetCondition(&status, in.ObservedGeneration, ConditionReconciled, metav1.ConditionUnknown, ReasonReconciling, "")
	} else {
		SetCondition(&status, in.ObservedGeneration, ConditionAccepted, metav1.ConditionTrue, ReasonSpecValid, "")
		hasActionFailures := hasFailedResults(in.Results)
		if in.ReconcileErr != nil {
			SetCondition(&status, in.ObservedGeneration, ConditionReconciled, metav1.ConditionFalse, ReasonReconcileFailed, in.ReconcileErr.Error())
		} else if hasActionFailures {
			SetCondition(&status, in.ObservedGeneration, ConditionReconciled, metav1.ConditionFalse, ReasonReconcileFailed, "one or more actions failed")
		} else {
			SetCondition(&status, in.ObservedGeneration, ConditionReconciled, metav1.ConditionTrue, ReasonReconcileSucceeded, "")
		}
	}

	// Build target failure map from results.
	targetFailures := make(map[string][]string)
	for _, r := range in.Results {
		if !r.Success && r.Err != nil {
			key := engine.TargetIdentityKey(r.Action.Target.FullResourceID, r.Action.Target.PrefixSetName)
			msg := fmt.Sprintf("%s %s: %s", r.Action.Kind, r.Action.Target.ASGName, r.Err.Error())
			targetFailures[key] = append(targetFailures[key], msg)
		}
	}

	// Systemic pre-execution failure: reconcile error with no results and no validation issues.
	isSystemicFailure := in.ReconcileErr != nil && len(in.Results) == 0 && !hasValidationIssues && in.Phase == StatusPhaseFinal

	// Determine preservation strategy: use selector-hash-based when previous
	// status has SelectorHash populated, otherwise fall back to index-based
	// for backward compatibility with pre-Phase-6 status data.
	useSelectorBasedPreservation := false
	for _, ps := range in.PreviousStatus.MappingStatuses {
		if ps.SelectorHash != "" {
			useSelectorBasedPreservation = true
			break
		}
	}

	var prevBySelector map[string][]metav1.Time
	if useSelectorBasedPreservation {
		prevBySelector = buildPreviousLastSyncBySelector(in.PreviousStatus.MappingStatuses)
	}

	// Build mapping statuses.
	status.MappingStatuses = make([]v1alpha1.MappingStatus, len(in.Spec.Mappings))

	for i, m := range in.Spec.Mappings {
		ms := &status.MappingStatuses[i]
		ms.SelectorHash = ComputeSelectorHash(m.PodSelector.MatchLabels)

		if i < len(in.MatchedPodsByIndex) {
			ms.MatchedPods = in.MatchedPodsByIndex[i]
		}

		switch {
		case hasValidationIssues:
			ms.ASGSyncState = SyncStateError
			if msgs := validationMessagesForMapping(in.ValidationIssues, i); len(msgs) > 0 {
				ms.Error = strings.Join(msgs, "; ")
			} else {
				ms.Error = "spec validation failed"
			}
			if useSelectorBasedPreservation {
				preserveLastSyncTimeBySelector(ms, ms.SelectorHash, prevBySelector)
			} else {
				preserveLastSyncTime(ms, in.PreviousStatus, i)
			}

		case in.Phase == StatusPhasePending:
			ms.ASGSyncState = SyncStatePending
			if useSelectorBasedPreservation {
				preserveLastSyncTimeBySelector(ms, ms.SelectorHash, prevBySelector)
			} else {
				preserveLastSyncTime(ms, in.PreviousStatus, i)
			}

		case isSystemicFailure:
			ms.ASGSyncState = SyncStateError
			ms.Error = in.ReconcileErr.Error()
			if useSelectorBasedPreservation {
				preserveLastSyncTimeBySelector(ms, ms.SelectorHash, prevBySelector)
			} else {
				preserveLastSyncTime(ms, in.PreviousStatus, i)
			}

		default:
			// Final phase: determine row state from target failures.
			var rowErrors []string
			for _, asgRef := range m.ApplicationSecurityGroups {
				parsed, err := model.ParseASGResourceID(asgRef.ResourceID)
				if err != nil {
					continue
				}
				key := engine.TargetIdentityKey(parsed.FullResourceID, in.PrefixSetName)
				if msgs, ok := targetFailures[key]; ok {
					rowErrors = append(rowErrors, msgs...)
				}
			}

			if len(rowErrors) > 0 {
				ms.ASGSyncState = SyncStateError
				sort.Strings(rowErrors)
				rowErrors = dedupStrings(rowErrors)
				ms.Error = strings.Join(rowErrors, "; ")
				if useSelectorBasedPreservation {
					preserveLastSyncTimeBySelector(ms, ms.SelectorHash, prevBySelector)
				} else {
					preserveLastSyncTime(ms, in.PreviousStatus, i)
				}
			} else {
				ms.ASGSyncState = SyncStateSynced
				ms.LastSyncTime = in.Now
			}
		}
	}

	return status
}

func preserveLastSyncTime(ms *v1alpha1.MappingStatus, prev v1alpha1.PodASGMappingStatus, idx int) {
	if idx < len(prev.MappingStatuses) {
		ms.LastSyncTime = prev.MappingStatuses[idx].LastSyncTime
	}
}

// buildPreviousLastSyncBySelector builds a map from selectorHash to a queue of
// LastSyncTime values from previous status rows. This replaces index-based
// preservation and handles reorder/insert/delete deterministically.
func buildPreviousLastSyncBySelector(prev []v1alpha1.MappingStatus) map[string][]metav1.Time {
	result := make(map[string][]metav1.Time)
	for _, ms := range prev {
		result[ms.SelectorHash] = append(result[ms.SelectorHash], ms.LastSyncTime)
	}
	return result
}

// popPreviousLastSync pops the earliest LastSyncTime from the queue for the
// given selectorHash. Returns the time and true if found, zero time and false
// if the queue is empty or the key doesn't exist.
func popPreviousLastSync(bySelector map[string][]metav1.Time, selectorHash string) (metav1.Time, bool) {
	queue, ok := bySelector[selectorHash]
	if !ok || len(queue) == 0 {
		return metav1.Time{}, false
	}
	t := queue[0]
	bySelector[selectorHash] = queue[1:]
	return t, true
}

// preserveLastSyncTimeBySelector preserves the LastSyncTime for a mapping
// status row using selector-hash-based lookup instead of index-based.
func preserveLastSyncTimeBySelector(
	ms *v1alpha1.MappingStatus,
	selectorHash string,
	prevBySelector map[string][]metav1.Time,
) {
	if t, ok := popPreviousLastSync(prevBySelector, selectorHash); ok {
		ms.LastSyncTime = t
	}
}

func validationMessagesForMapping(issues []ValidationIssue, mappingIdx int) []string {
	var msgs []string
	for _, vi := range issues {
		if vi.MappingIndex == mappingIdx {
			msgs = append(msgs, fmt.Sprintf("invalid resource ID %q: %v", vi.ResourceID, vi.Err))
		}
	}
	return msgs
}

func aggregateValidationMessage(issues []ValidationIssue) string {
	var msgs []string
	for _, vi := range issues {
		msgs = append(msgs, fmt.Sprintf("mapping[%d].asg[%d]: %v", vi.MappingIndex, vi.ASGIndex, vi.Err))
	}
	return strings.Join(msgs, "; ")
}

func dedupStrings(ss []string) []string {
	if len(ss) == 0 {
		return ss
	}
	result := []string{ss[0]}
	for i := 1; i < len(ss); i++ {
		if ss[i] != ss[i-1] {
			result = append(result, ss[i])
		}
	}
	return result
}

// hasFailedResults returns true if any ActionResult indicates failure.
func hasFailedResults(results []azure.ActionResult) bool {
	for _, r := range results {
		if !r.Success {
			return true
		}
	}
	return false
}

// ComputeMatchedPodsByMapping counts selector-matched pods per mapping index.
func ComputeMatchedPodsByMapping(spec v1alpha1.PodASGMappingSpec, pods []corev1.Pod) []int {
	result := make([]int, len(spec.Mappings))
	for i, m := range spec.Mappings {
		selector, err := model.CompileSelector(m.PodSelector)
		if err != nil {
			continue
		}
		for j := range pods {
			if selector.Matches(labels.Set(pods[j].Labels)) {
				result[i]++
			}
		}
	}
	return result
}

// ComputeSelectorHash returns a deterministic hash string for the given matchLabels.
func ComputeSelectorHash(matchLabels map[string]string) string {
	if len(matchLabels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(matchLabels))
	for k := range matchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(matchLabels[k])
	}

	h := sha256.Sum256([]byte(sb.String()))
	return fmt.Sprintf("%x", h[:8])
}
