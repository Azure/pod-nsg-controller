package controller

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/model"
)

// OwnedTarget represents an owned addressPrefixSet target.
type OwnedTarget struct {
	ResourceID    string `json:"resourceId"`
	PrefixSetName string `json:"prefixSetName"`
}

// OwnedTargetsFromDesired extracts owned targets from the desired state map.
func OwnedTargetsFromDesired(desired map[engine.ASGTarget]engine.DesiredPrefixSet) []OwnedTarget {
	targets := make([]OwnedTarget, 0, len(desired))
	for t := range desired {
		targets = append(targets, OwnedTarget{
			ResourceID:    t.FullResourceID,
			PrefixSetName: t.PrefixSetName,
		})
	}
	return CanonicalizeOwnedTargets(targets)
}

// OwnedTargetsFromSpec derives owned targets from the mapping spec and cluster name.
func OwnedTargetsFromSpec(clusterName string, mapping *v1alpha1.PodASGMapping) ([]OwnedTarget, error) {
	prefixSetName := model.OwnershipKey(clusterName, mapping.Namespace, mapping.Name)
	var targets []OwnedTarget
	seen := make(map[string]struct{})

	for _, rule := range mapping.Spec.Mappings {
		for _, asgRef := range rule.ApplicationSecurityGroups {
			parsed, err := model.ParseASGResourceID(asgRef.ResourceID)
			if err != nil {
				return nil, fmt.Errorf("parsing ASG resource ID %q: %w", asgRef.ResourceID, err)
			}
			key := strings.ToLower(parsed.FullResourceID + "/" + prefixSetName)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			targets = append(targets, OwnedTarget{
				ResourceID:    parsed.FullResourceID,
				PrefixSetName: prefixSetName,
			})
		}
	}
	return CanonicalizeOwnedTargets(targets), nil
}

// ParseOwnedTargets parses the JSON annotation value into OwnedTarget slice.
func ParseOwnedTargets(raw string) ([]OwnedTarget, error) {
	if raw == "" {
		return nil, nil
	}
	var targets []OwnedTarget
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		return nil, fmt.Errorf("parsing owned targets annotation: %w", err)
	}
	return targets, nil
}

// UnionOwnedTargets returns the union of two OwnedTarget slices (deduped by ResourceID+PrefixSetName).
func UnionOwnedTargets(a, b []OwnedTarget) []OwnedTarget {
	seen := make(map[string]struct{})
	var result []OwnedTarget
	for _, t := range a {
		key := strings.ToLower(t.ResourceID + "/" + t.PrefixSetName)
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			result = append(result, t)
		}
	}
	for _, t := range b {
		key := strings.ToLower(t.ResourceID + "/" + t.PrefixSetName)
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			result = append(result, t)
		}
	}
	return CanonicalizeOwnedTargets(result)
}

// OwnedTargetsToASGTargets converts OwnedTarget slice to engine.ASGTarget slice.
func OwnedTargetsToASGTargets(in []OwnedTarget) ([]engine.ASGTarget, error) {
	targets := make([]engine.ASGTarget, 0, len(in))
	for _, ot := range in {
		parsed, err := model.ParseASGResourceID(ot.ResourceID)
		if err != nil {
			return nil, fmt.Errorf("parsing resource ID %q: %w", ot.ResourceID, err)
		}
		targets = append(targets, engine.ASGTarget{
			SubscriptionID: parsed.SubscriptionID,
			ResourceGroup:  parsed.ResourceGroup,
			ASGName:        parsed.ASGName,
			FullResourceID: parsed.FullResourceID,
			PrefixSetName:  ot.PrefixSetName,
		})
	}
	return targets, nil
}

// CanonicalizeOwnedTargets returns a sorted, deduped copy of the input.
func CanonicalizeOwnedTargets(in []OwnedTarget) []OwnedTarget {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var deduped []OwnedTarget
	for _, t := range in {
		key := strings.ToLower(t.ResourceID + "/" + t.PrefixSetName)
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			deduped = append(deduped, t)
		}
	}
	sort.Slice(deduped, func(i, j int) bool {
		if deduped[i].ResourceID != deduped[j].ResourceID {
			return deduped[i].ResourceID < deduped[j].ResourceID
		}
		return deduped[i].PrefixSetName < deduped[j].PrefixSetName
	})
	return deduped
}
