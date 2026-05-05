package controller

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/pkg/errors"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
)

// OwnedASGRef tracks a single ASG that has been written to by this mapping.
type OwnedASGRef struct {
	SubscriptionID string `json:"subscriptionId"`
	ResourceGroup  string `json:"resourceGroup"`
	ASGName        string `json:"asgName"`
	FullResourceID string `json:"fullResourceId,omitempty"`
}

// LoadOwnedASGs parses the ownership annotation.
func LoadOwnedASGs(mapping *v1alpha1.PodASGMapping) ([]OwnedASGRef, error) {
	if mapping.Annotations == nil {
		return nil, nil
	}
	raw, ok := mapping.Annotations[OwnedASGsAnnotationKey]
	if !ok || raw == "" {
		return nil, nil
	}
	var refs []OwnedASGRef
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return nil, errors.Wrap(err, "parsing owned-asgs annotation")
	}
	return refs, nil
}

// StoreOwnedASGs serializes refs into the ownership annotation.
func StoreOwnedASGs(mapping *v1alpha1.PodASGMapping, refs []OwnedASGRef) error {
	data, err := json.Marshal(refs)
	if err != nil {
		return errors.Wrap(err, "marshalling owned-asgs annotation")
	}
	if mapping.Annotations == nil {
		mapping.Annotations = make(map[string]string)
	}
	mapping.Annotations[OwnedASGsAnnotationKey] = string(data)
	return nil
}

// TargetsFromOwnedASGs converts owned refs to ASGTarget set.
func TargetsFromOwnedASGs(refs []OwnedASGRef, ownershipKey string) map[engine.ASGTarget]struct{} {
	targets := make(map[engine.ASGTarget]struct{}, len(refs))
	for _, ref := range refs {
		targets[engine.ASGTarget{
			SubscriptionID: ref.SubscriptionID,
			ResourceGroup:  ref.ResourceGroup,
			ASGName:        ref.ASGName,
			FullResourceID: ref.FullResourceID,
			PrefixSetName:  ownershipKey,
		}] = struct{}{}
	}
	return targets
}

// BuildOwnedASGsFromDesired creates OwnedASGRef list from desired state.
func BuildOwnedASGsFromDesired(desired map[engine.ASGTarget]engine.DesiredPrefixSet) []OwnedASGRef {
	refs := make([]OwnedASGRef, 0, len(desired))
	for t := range desired {
		refs = append(refs, OwnedASGRef{
			SubscriptionID: t.SubscriptionID,
			ResourceGroup:  t.ResourceGroup,
			ASGName:        t.ASGName,
			FullResourceID: t.FullResourceID,
		})
	}
	sort.Slice(refs, func(i, j int) bool {
		return ownedASGRefSortKey(refs[i]) < ownedASGRefSortKey(refs[j])
	})
	return refs
}

// UpdateOwnedASGsAfterResults updates ownership after execution.
// Rules per design §5.4:
// 1. Start from existing refs.
// 2. Successful delete removes target ref.
// 3. Successful create/update ensures target ref exists.
// 4. Desired targets with no action are preserved.
// 5. Failed deletes retain the ref for retry.
// 6. Returns deterministic sorted refs.
func UpdateOwnedASGsAfterResults(existing []OwnedASGRef, desired map[engine.ASGTarget]engine.DesiredPrefixSet, results []azure.ActionResult) []OwnedASGRef {
	// Build set of successfully deleted targets.
	successfulDeletes := make(map[string]struct{})
	for _, r := range results {
		if r.Success && r.Action.Kind == engine.DeletePrefixSet {
			key := ownedASGTargetKey(r.Action.Target)
			successfulDeletes[key] = struct{}{}
		}
	}

	seen := make(map[string]struct{})
	var merged []OwnedASGRef

	// Add all desired targets.
	for t := range desired {
		key := ownedASGTargetKey(t)
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			merged = append(merged, OwnedASGRef{
				SubscriptionID: t.SubscriptionID,
				ResourceGroup:  t.ResourceGroup,
				ASGName:        t.ASGName,
				FullResourceID: t.FullResourceID,
			})
		}
	}

	// Retain existing refs not in desired, unless successfully deleted.
	for _, ref := range existing {
		key := ownedASGRefKey(ref)
		if _, ok := seen[key]; ok {
			continue
		}
		if _, deleted := successfulDeletes[key]; deleted {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, ref)
	}

	sort.Slice(merged, func(i, j int) bool {
		return ownedASGRefSortKey(merged[i]) < ownedASGRefSortKey(merged[j])
	})
	return merged
}

// ownershipAnnotationChanged returns true if the updated refs differ from existing.
// Both slices are assumed sorted by full target key (as produced by UpdateOwnedASGsAfterResults).
func ownershipAnnotationChanged(existing, updated []OwnedASGRef) bool {
	if len(existing) != len(updated) {
		return true
	}
	for i := range existing {
		if existing[i] != updated[i] {
			return true
		}
	}
	return false
}

func ownedASGRefSortKey(ref OwnedASGRef) string {
	return ownedASGRefKey(ref)
}

func ownedASGRefKey(ref OwnedASGRef) string {
	if ref.FullResourceID != "" {
		return strings.ToLower(ref.FullResourceID)
	}
	return strings.ToLower(ref.SubscriptionID + "/" + ref.ResourceGroup + "/" + ref.ASGName)
}

func ownedASGTargetKey(target engine.ASGTarget) string {
	if target.FullResourceID != "" {
		return strings.ToLower(target.FullResourceID)
	}
	return strings.ToLower(target.SubscriptionID + "/" + target.ResourceGroup + "/" + target.ASGName)
}

// PrefixSetNameEqualsOwnership checks if a prefix set matches an ownership key.
func PrefixSetNameEqualsOwnership(ps azure.AddressPrefixSet, ownershipKey string) bool {
	if ps.Name == nil {
		return false
	}
	return strings.EqualFold(*ps.Name, ownershipKey)
}
