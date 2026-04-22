package model

import (
	"strings"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

type compiledEntry struct {
	namespace string
	selector  labels.Selector
	asgs      []v1alpha1.ASGReference
}

// MappingIndex holds compiled mapping entries for efficient pod-to-ASG lookup.
type MappingIndex struct {
	entries []compiledEntry
}

// BuildIndex compiles a slice of PodASGMapping CRs into a MappingIndex.
// Order is preserved: input slice order → each CR's Spec.Mappings order → each rule's ASGs order.
func BuildIndex(mappings []v1alpha1.PodASGMapping) *MappingIndex {
	var entries []compiledEntry
	for i := range mappings {
		m := &mappings[i]
		ns := m.Namespace
		for j := range m.Spec.Mappings {
			rule := &m.Spec.Mappings[j]
			sel, _ := CompileSelector(rule.PodSelector)
			asgs := append([]v1alpha1.ASGReference(nil), rule.ApplicationSecurityGroups...)
			entries = append(entries, compiledEntry{
				namespace: ns,
				selector:  sel,
				asgs:      asgs,
			})
		}
	}
	return &MappingIndex{entries: entries}
}

// MatchingASGs returns the deduplicated, ordered set of ASGReferences matching a pod.
// Returns an empty (non-nil) slice for a nil receiver, nil pod, or no matches.
// Deduplication uses canonical resource ID; first-seen metadata wins.
func (idx *MappingIndex) MatchingASGs(pod *corev1.Pod) []v1alpha1.ASGReference {
	if idx == nil || pod == nil {
		return []v1alpha1.ASGReference{}
	}

	podLabels := labels.Set(pod.Labels)
	seen := make(map[string]struct{})
	result := make([]v1alpha1.ASGReference, 0)

	for i := range idx.entries {
		e := &idx.entries[i]
		if e.namespace != pod.Namespace {
			continue
		}
		if !e.selector.Matches(podLabels) {
			continue
		}
		for _, asg := range e.asgs {
			key := canonicalASGKey(asg.ResourceID)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, asg)
		}
	}
	return result
}

// canonicalASGKey returns a lowercase canonical key for ASG deduplication.
func canonicalASGKey(resourceID string) string {
	parsed, err := ParseASGResourceID(resourceID)
	if err != nil {
		return strings.ToLower(resourceID)
	}
	return strings.ToLower(parsed.FullResourceID)
}
