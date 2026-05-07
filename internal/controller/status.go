package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StatusPhase identifies which phase of the reconcile loop is reporting status.
type StatusPhase string

const (
	StatusPhaseValidationFailed StatusPhase = "ValidationFailed"
	StatusPhaseBootstrapPending StatusPhase = "BootstrapPending"
	StatusPhasePostExecution    StatusPhase = "PostExecution"
)

// ReconcileStatusInput carries information needed to build status after reconcile.
type ReconcileStatusInput struct {
	Phase                      StatusPhase
	ProcessedGen               int64
	ProcessedSpec              v1alpha1.PodASGMappingSpec
	PreviousMappingStatuses    []v1alpha1.MappingStatus
	PreviousObservedGeneration int64
	Results                    []azure.ActionResult
	PodCounts                  map[string]int
	ValidationErrors           map[int][]string
	ReconcileErr               error
}

// StatusGenerationDriftError is returned when the mapping generation has changed
// between reconcile start and status write, indicating a stale status write.
type StatusGenerationDriftError struct {
	ProcessedGeneration int64
	LiveGeneration      int64
}

func (e *StatusGenerationDriftError) Error() string {
	return fmt.Sprintf("generation drift: processed=%d, live=%d", e.ProcessedGeneration, e.LiveGeneration)
}

// MappingStatusUpdater implements StatusUpdater using the status subresource.
type MappingStatusUpdater struct {
	client client.Client
	nowFn  func() time.Time
}

// NewMappingStatusUpdater creates a new MappingStatusUpdater.
func NewMappingStatusUpdater(c client.Client, nowFn func() time.Time) *MappingStatusUpdater {
	return &MappingStatusUpdater{
		client: c,
		nowFn:  nowFn,
	}
}

// SelectorHash computes a deterministic hash for a matchLabels map.
// It sorts keys, builds a "key=value" CSV, SHA-256 hashes it, and returns
// the first 16 hex characters.
func SelectorHash(matchLabels map[string]string) string {
	keys := make([]string, 0, len(matchLabels))
	for k := range matchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+matchLabels[k])
	}
	csv := strings.Join(parts, ",")
	hash := sha256.Sum256([]byte(csv))
	return fmt.Sprintf("%x", hash[:])[:16]
}

// canonicalASGKey returns the lower-cased canonical ARM ASG resource ID.
func canonicalASGKey(resourceID string) string {
	return strings.ToLower(resourceID)
}

// ComputeStatus computes the PodASGMappingStatus from reconcile results.
// This is the post-execution reducer per spec Phase 6.
func ComputeStatus(
	spec v1alpha1.PodASGMappingSpec,
	reconcileResults []azure.ActionResult,
	podCounts map[string]int,
) v1alpha1.PodASGMappingStatus {
	// Group results by canonical ASG key.
	resultsByASGKey := make(map[string][]azure.ActionResult)
	for _, r := range reconcileResults {
		key := canonicalASGKey(r.Action.Target.FullResourceID)
		resultsByASGKey[key] = append(resultsByASGKey[key], r)
	}

	statuses := make([]v1alpha1.MappingStatus, 0, len(spec.Mappings))

	for _, mapping := range spec.Mappings {
		hash := SelectorHash(mapping.PodSelector.MatchLabels)
		matchedPods := podCounts[hash]

		// Collect canonical ASG keys for this mapping (deduplicated).
		asgKeys := make(map[string]struct{})
		for _, asgRef := range mapping.ApplicationSecurityGroups {
			parsed, err := model.ParseASGResourceID(asgRef.ResourceID)
			if err != nil {
				continue
			}
			asgKeys[canonicalASGKey(parsed.FullResourceID)] = struct{}{}
		}

		// Determine state: Error if any referenced ASG has failed results.
		syncState := "Synced"
		var errorMsgs []string
		for key := range asgKeys {
			results, ok := resultsByASGKey[key]
			if !ok {
				continue
			}
			for _, r := range results {
				if !r.Success && r.Err != nil {
					syncState = "Error"
					errorMsgs = append(errorMsgs, fmt.Sprintf("asg=%s: %v", key, r.Err))
				}
			}
		}

		// Deduplicate and sort error messages.
		errorMsg := ""
		if len(errorMsgs) > 0 {
			unique := deduplicateStrings(errorMsgs)
			sort.Strings(unique)
			errorMsg = strings.Join(unique, "; ")
		}

		statuses = append(statuses, v1alpha1.MappingStatus{
			SelectorHash: hash,
			MatchedPods:  matchedPods,
			ASGSyncState: syncState,
			Error:        errorMsg,
		})
	}

	return v1alpha1.PodASGMappingStatus{
		MappingCount:    len(spec.Mappings),
		MappingStatuses: statuses,
	}
}

// deduplicateStrings removes duplicates from a string slice.
func deduplicateStrings(input []string) []string {
	seen := make(map[string]struct{}, len(input))
	result := make([]string, 0, len(input))
	for _, s := range input {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			result = append(result, s)
		}
	}
	return result
}

// UpdateAfterReconcile implements the StatusUpdater interface on MappingStatusUpdater.
func (u *MappingStatusUpdater) UpdateAfterReconcile(
	ctx context.Context,
	mapping *v1alpha1.PodASGMapping,
	input ReconcileStatusInput,
) error {
	// Generation drift gate: only check when ProcessedGen is explicitly set.
	if input.ProcessedGen != 0 && mapping.Generation != input.ProcessedGen {
		return &StatusGenerationDriftError{
			ProcessedGeneration: input.ProcessedGen,
			LiveGeneration:      mapping.Generation,
		}
	}

	now := u.nowFn()

	// Resolve spec: use ProcessedSpec if provided, otherwise fall back to live spec.
	spec := input.ProcessedSpec
	if len(spec.Mappings) == 0 {
		spec = mapping.Spec
	}

	var newStatus v1alpha1.PodASGMappingStatus

	switch input.Phase {
	case StatusPhasePostExecution:
		newStatus = ComputeStatus(spec, input.Results, input.PodCounts)

		// Set conditions.
		if input.ReconcileErr == nil {
			SetConditionForGeneration(&newStatus, ConditionAccepted, metav1.ConditionTrue, "SpecValid", "spec is valid", input.ProcessedGen)
			SetConditionForGeneration(&newStatus, ConditionReconciled, metav1.ConditionTrue, "AllSynced", "all mappings synced", input.ProcessedGen)
		} else {
			SetConditionForGeneration(&newStatus, ConditionAccepted, metav1.ConditionTrue, "SpecValid", "spec is valid", input.ProcessedGen)
			SetConditionForGeneration(&newStatus, ConditionReconciled, metav1.ConditionFalse, "ReconcileError", input.ReconcileErr.Error(), input.ProcessedGen)
		}

		// Resolve previous status data: prefer explicit input, fall back to live mapping.
		previousStatuses := input.PreviousMappingStatuses
		previousObservedGen := input.PreviousObservedGeneration
		if previousStatuses == nil {
			// Legacy path: use live mapping status with index-based carry-forward.
			applyLastSyncTimeRules(&newStatus, mapping.Status.MappingStatuses, now)
		} else {
			// Identity-based carry-forward with explicit previous data.
			applyLastSyncTimeByIdentity(
				&newStatus,
				spec,
				previousStatuses,
				previousObservedGen,
				input.ProcessedGen,
				now,
			)
		}

	case StatusPhaseValidationFailed:
		newStatus = buildValidationFailedStatus(spec, input.ValidationErrors, input.PodCounts)
		SetConditionForGeneration(&newStatus, ConditionAccepted, metav1.ConditionFalse, "InvalidSpec", "spec validation failed", input.ProcessedGen)
		SetConditionForGeneration(&newStatus, ConditionReconciled, metav1.ConditionFalse, "ValidationFailed", "spec validation failed", input.ProcessedGen)
		// Preserve prior lastSyncTime values.
		preserveLastSyncTimes(&newStatus, mapping.Status.MappingStatuses)

	case StatusPhaseBootstrapPending:
		newStatus = buildBootstrapPendingStatus(spec, input.PodCounts)
		SetConditionForGeneration(&newStatus, ConditionAccepted, metav1.ConditionTrue, "SpecValid", "spec is valid", input.ProcessedGen)
		SetConditionForGeneration(&newStatus, ConditionReconciled, metav1.ConditionFalse, "BootstrapPending", "awaiting first reconcile cycle", input.ProcessedGen)
		// Preserve prior lastSyncTime values.
		preserveLastSyncTimes(&newStatus, mapping.Status.MappingStatuses)
	}

	mapping.Status = newStatus
	return u.client.Status().Update(ctx, mapping)
}

// applyLastSyncTimeRules sets lastSyncTime to now for Synced mappings and
// preserves the previous lastSyncTime for Error mappings (index-based fallback).
func applyLastSyncTimeRules(newStatus *v1alpha1.PodASGMappingStatus, previous []v1alpha1.MappingStatus, now time.Time) {
	nowMeta := metav1.NewTime(now)
	for i := range newStatus.MappingStatuses {
		if newStatus.MappingStatuses[i].ASGSyncState == "Synced" {
			newStatus.MappingStatuses[i].LastSyncTime = nowMeta
		} else {
			// Preserve previous lastSyncTime for Error mappings.
			if i < len(previous) {
				newStatus.MappingStatuses[i].LastSyncTime = previous[i].LastSyncTime
			}
		}
	}
}

// preserveLastSyncTimes carries over lastSyncTime from previous status.
func preserveLastSyncTimes(newStatus *v1alpha1.PodASGMappingStatus, previous []v1alpha1.MappingStatus) {
	for i := range newStatus.MappingStatuses {
		if i < len(previous) {
			newStatus.MappingStatuses[i].LastSyncTime = previous[i].LastSyncTime
		}
	}
}

// buildValidationFailedStatus constructs status for validation failure path.
func buildValidationFailedStatus(spec v1alpha1.PodASGMappingSpec, validationErrors map[int][]string, podCounts map[string]int) v1alpha1.PodASGMappingStatus {
	statuses := make([]v1alpha1.MappingStatus, 0, len(spec.Mappings))
	for i, mapping := range spec.Mappings {
		hash := SelectorHash(mapping.PodSelector.MatchLabels)
		matchedPods := podCounts[hash]

		errorMsg := ""
		if errs, ok := validationErrors[i]; ok && len(errs) > 0 {
			sort.Strings(errs)
			errorMsg = strings.Join(errs, "; ")
		}

		statuses = append(statuses, v1alpha1.MappingStatus{
			SelectorHash: hash,
			MatchedPods:  matchedPods,
			ASGSyncState: "Error",
			Error:        errorMsg,
		})
	}
	return v1alpha1.PodASGMappingStatus{
		MappingCount:    len(spec.Mappings),
		MappingStatuses: statuses,
	}
}

// buildBootstrapPendingStatus constructs status for the bootstrap pending path.
func buildBootstrapPendingStatus(spec v1alpha1.PodASGMappingSpec, podCounts map[string]int) v1alpha1.PodASGMappingStatus {
	statuses := make([]v1alpha1.MappingStatus, 0, len(spec.Mappings))
	for _, mapping := range spec.Mappings {
		hash := SelectorHash(mapping.PodSelector.MatchLabels)
		matchedPods := podCounts[hash]

		statuses = append(statuses, v1alpha1.MappingStatus{
			SelectorHash: hash,
			MatchedPods:  matchedPods,
			ASGSyncState: "Pending",
		})
	}
	return v1alpha1.PodASGMappingStatus{
		MappingCount:    len(spec.Mappings),
		MappingStatuses: statuses,
	}
}

// observedGenerationForCondition returns the ObservedGeneration from a condition of the given type.
func observedGenerationForCondition(conditions []metav1.Condition, condType string) int64 {
	for _, c := range conditions {
		if c.Type == condType {
			return c.ObservedGeneration
		}
	}
	return 0
}

// MappingIdentity computes the unique identity for a mapping rule used in
// lastSyncTime carry-forward. Identity = selectorHash + "|" + normalizedASGSetHash.
func MappingIdentity(mapping v1alpha1.Mapping) string {
	selectorHash := SelectorHash(mapping.PodSelector.MatchLabels)

	// Collect, deduplicate, normalize, and sort ASG resource IDs.
	asgSet := make(map[string]struct{})
	for _, asgRef := range mapping.ApplicationSecurityGroups {
		asgSet[strings.ToLower(asgRef.ResourceID)] = struct{}{}
	}
	asgKeys := make([]string, 0, len(asgSet))
	for k := range asgSet {
		asgKeys = append(asgKeys, k)
	}
	sort.Strings(asgKeys)

	asgCSV := strings.Join(asgKeys, ",")
	asgHash := sha256.Sum256([]byte(asgCSV))
	normalizedASGSetHash := fmt.Sprintf("%x", asgHash[:])[:16]

	return selectorHash + "|" + normalizedASGSetHash
}

// applyLastSyncTimeByIdentity applies lastSyncTime carry-forward using identity matching.
// Rules:
// 1. Synced mappings get LastSyncTime = now
// 2. Non-synced mappings carry forward from previous IF:
//   - PreviousObservedGeneration == ProcessedGen
//   - Previous and current mapping count match
//   - Identity is unique in current processed spec
func applyLastSyncTimeByIdentity(
	newStatus *v1alpha1.PodASGMappingStatus,
	processedSpec v1alpha1.PodASGMappingSpec,
	previousStatuses []v1alpha1.MappingStatus,
	previousObservedGen int64,
	processedGen int64,
	now time.Time,
) {
	nowMeta := metav1.NewTime(now)

	// Check carry-forward preconditions.
	carryForwardAllowed := previousObservedGen == processedGen &&
		len(previousStatuses) == len(processedSpec.Mappings)

	// Build identity map for current spec and check uniqueness.
	currentIdentities := make([]string, len(processedSpec.Mappings))
	identityCount := make(map[string]int)
	for i, m := range processedSpec.Mappings {
		id := MappingIdentity(m)
		currentIdentities[i] = id
		identityCount[id]++
	}

	// Build previous identity → index map (only if carry-forward allowed).
	previousByIdentity := make(map[string]int) // identity → index in previous
	if carryForwardAllowed {
		prevIdentityCount := make(map[string]int)
		for i := range previousStatuses {
			id := MappingIdentity(processedSpec.Mappings[i])
			prevIdentityCount[id]++
			previousByIdentity[id] = i
		}
		// Invalidate ambiguous identities in previous.
		for id, count := range prevIdentityCount {
			if count > 1 {
				delete(previousByIdentity, id)
			}
		}
	}

	for i := range newStatus.MappingStatuses {
		if newStatus.MappingStatuses[i].ASGSyncState == "Synced" {
			newStatus.MappingStatuses[i].LastSyncTime = nowMeta
		} else if carryForwardAllowed {
			identity := currentIdentities[i]
			// Only carry forward if identity is unique in current spec.
			if identityCount[identity] == 1 {
				if prevIdx, ok := previousByIdentity[identity]; ok {
					newStatus.MappingStatuses[i].LastSyncTime = previousStatuses[prevIdx].LastSyncTime
				}
			}
			// If ambiguous, leave zero.
		}
		// If carry-forward not allowed and not Synced, leave zero.
	}
}
