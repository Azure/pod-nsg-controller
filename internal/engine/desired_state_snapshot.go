package engine

import (
	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// PodIdentity uniquely identifies a pod in a snapshot.
type PodIdentity struct {
	Namespace string
	Name      string
	UID       string
}

// PodMembership holds the IP membership for a pod.
type PodMembership struct {
	PodIP string
}

// PodSnapshot is a point-in-time snapshot of all matched pods for a mapping.
type PodSnapshot struct {
	Pods map[PodIdentity]PodMembership
}

// matchPodToRuleIndices returns the indices of mapping rules whose selectors
// match the given pod labels. Rules with invalid selectors are skipped.
// Used by incremental cache paths (OnPodAdd/Update/Delete) where per-event
// overhead is negligible. Batch paths should use precompiledMapping instead.
func matchPodToRuleIndices(mapping *v1alpha1.PodASGMapping, podLabels labels.Set) []int {
	var indices []int
	for i, rule := range mapping.Spec.Mappings {
		selector, err := model.CompileSelector(rule.PodSelector)
		if err != nil {
			continue
		}
		if selector.Matches(podLabels) {
			indices = append(indices, i)
		}
	}
	return indices
}

// resolveRuleTargets resolves the ASG targets for a mapping rule, skipping
// entries with invalid resource IDs.
// Used by incremental cache paths (OnPodAdd/Update/Delete) where per-event
// overhead is negligible. Batch paths should use precompiledMapping instead.
func resolveRuleTargets(rule v1alpha1.Mapping, prefixSetName string) []ASGTarget {
	var targets []ASGTarget
	for _, asgRef := range rule.ApplicationSecurityGroups {
		parsed, err := model.ParseASGResourceID(asgRef.ResourceID)
		if err != nil {
			continue
		}
		targets = append(targets, ASGTarget{
			SubscriptionID: parsed.SubscriptionID,
			ResourceGroup:  parsed.ResourceGroup,
			ASGName:        parsed.ASGName,
			FullResourceID: parsed.FullResourceID,
			PrefixSetName:  prefixSetName,
		})
	}
	return targets
}

// precompiledMapping holds precompiled selectors and pre-resolved ASG targets
// for a PodASGMapping, avoiding repeated compilation/parsing in batch paths.
// Indexed by original rule index; a nil selector means compilation failed
// (that rule is skipped during matching, preserving parity with
// matchPodToRuleIndices).
type precompiledMapping struct {
	selectors []labels.Selector // one per rule; nil = compile error, skip
	targets   [][]ASGTarget     // pre-resolved targets per rule (read-only)
}

// precompileMapping compiles selectors and resolves ASG targets once per rule.
func precompileMapping(mapping *v1alpha1.PodASGMapping, prefixSetName string) precompiledMapping {
	n := len(mapping.Spec.Mappings)
	pc := precompiledMapping{
		selectors: make([]labels.Selector, n),
		targets:   make([][]ASGTarget, n),
	}
	for i, rule := range mapping.Spec.Mappings {
		sel, err := model.CompileSelector(rule.PodSelector)
		if err == nil {
			pc.selectors[i] = sel
		}
		pc.targets[i] = resolveRuleTargets(rule, prefixSetName)
	}
	return pc
}

// matchPod returns the rule indices whose precompiled selectors match podLabels.
func (pc *precompiledMapping) matchPod(podLabels labels.Set) []int {
	var indices []int
	for i, sel := range pc.selectors {
		if sel != nil && sel.Matches(podLabels) {
			indices = append(indices, i)
		}
	}
	return indices
}

// ComputeDesiredStateWithSnapshot computes desired state and returns a PodSnapshot
// of all matched pods. This is the snapshot-aware variant of ComputeDesiredState.
// It precompiles selectors and ASG targets once per mapping to avoid redundant
// compilation during the per-pod loop, while preserving parity with the
// incremental cache paths (which use the same CompileSelector / ParseASGResourceID).
func ComputeDesiredStateWithSnapshot(
	clusterName string,
	mappings []v1alpha1.PodASGMapping,
	pods []corev1.Pod,
) (map[ASGTarget]DesiredPrefixSet, PodSnapshot) {
	internal := make(map[string]*internalEntry)
	podsByNamespace := make(map[string][]*corev1.Pod)
	snapshot := PodSnapshot{Pods: make(map[PodIdentity]PodMembership)}

	for i := range pods {
		pod := &pods[i]
		podsByNamespace[pod.Namespace] = append(podsByNamespace[pod.Namespace], pod)
	}

	for i := range mappings {
		m := &mappings[i]
		prefixSetName := model.OwnershipKey(clusterName, m.Namespace, m.Name)
		pc := precompileMapping(m, prefixSetName)
		namespacePods := podsByNamespace[m.Namespace]

		for _, pod := range namespacePods {
			if pod.Status.PodIP == "" {
				continue
			}

			matchedIndices := pc.matchPod(labels.Set(pod.Labels))
			if len(matchedIndices) == 0 {
				continue
			}

			cidr := toCIDR(pod.Status.PodIP)
			id := PodIdentity{
				Namespace: pod.Namespace,
				Name:      pod.Name,
				UID:       string(pod.UID),
			}
			snapshot.Pods[id] = PodMembership{PodIP: pod.Status.PodIP}

			for _, ruleIdx := range matchedIndices {
				for _, target := range pc.targets[ruleIdx] {
					normKey := targetIdentityKey(target.FullResourceID, prefixSetName)

					entry, exists := internal[normKey]
					if !exists {
						entry = &internalEntry{
							target: target,
							ips:    make(map[string]struct{}),
						}
						internal[normKey] = entry
					}
					entry.ips[cidr] = struct{}{}
				}
			}
		}
	}

	result := make(map[ASGTarget]DesiredPrefixSet, len(internal))
	for _, entry := range internal {
		result[entry.target] = DesiredPrefixSet{IPs: entry.ips}
	}
	return result, snapshot
}

// DesiredStateRecomputeArtifacts holds the complete output of a single-pass
// recompute analysis, eliminating the need for multiple passes over pods/selectors.
type DesiredStateRecomputeArtifacts struct {
	Desired            map[ASGTarget]DesiredPrefixSet
	Snapshot           PodSnapshot
	MatchedPodsByIndex []int
	HasPendingIPPods   bool
	PodRules           map[PodIdentity]map[int]struct{}
	PendingIPCount     int
}

// ComputeDesiredStateRecomputeArtifacts computes all recompute-derived data in
// a single pass over namespace pods. This replaces the multi-pass approach of
// calling ComputeMatchedPodsByMapping + ComputeDesiredStateWithSnapshot
// separately.
//
// PARITY CONTRACT: The artifacts produced here must remain semantically
// equivalent to the incremental cache mutation paths (OnPodAdd, OnPodUpdate,
// OnPodDelete) in DesiredStateCache. Any change to matching/IP-resolution
// logic here must be mirrored in those methods and vice-versa.
func ComputeDesiredStateRecomputeArtifacts(
	clusterName string,
	mapping v1alpha1.PodASGMapping,
	pods []corev1.Pod,
) DesiredStateRecomputeArtifacts {
	internal := make(map[string]*internalEntry)
	prefixSetName := model.OwnershipKey(clusterName, mapping.Namespace, mapping.Name)
	pc := precompileMapping(&mapping, prefixSetName)

	arts := DesiredStateRecomputeArtifacts{
		Desired:            make(map[ASGTarget]DesiredPrefixSet),
		Snapshot:           PodSnapshot{Pods: make(map[PodIdentity]PodMembership)},
		MatchedPodsByIndex: make([]int, len(mapping.Spec.Mappings)),
		HasPendingIPPods:   false,
		PodRules:           make(map[PodIdentity]map[int]struct{}),
		PendingIPCount:     0,
	}

	for pi := range pods {
		pod := &pods[pi]
		if pod.Namespace != mapping.Namespace {
			continue
		}

		matchedIndices := pc.matchPod(labels.Set(pod.Labels))
		if len(matchedIndices) == 0 {
			continue
		}

		podID := PodIdentity{Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID)}

		// Track per-rule counts (includes pods without IPs)
		for _, i := range matchedIndices {
			arts.MatchedPodsByIndex[i]++
			if arts.PodRules[podID] == nil {
				arts.PodRules[podID] = make(map[int]struct{})
			}
			arts.PodRules[podID][i] = struct{}{}
		}

		if pod.Status.PodIP == "" {
			arts.HasPendingIPPods = true
			arts.PendingIPCount++
			continue
		}

		cidr := toCIDR(pod.Status.PodIP)
		arts.Snapshot.Pods[podID] = PodMembership{PodIP: pod.Status.PodIP}

		for _, ruleIdx := range matchedIndices {
			for _, target := range pc.targets[ruleIdx] {
				normKey := targetIdentityKey(target.FullResourceID, prefixSetName)
				entry, exists := internal[normKey]
				if !exists {
					entry = &internalEntry{
						target: target,
						ips:    make(map[string]struct{}),
					}
					internal[normKey] = entry
				}
				entry.ips[cidr] = struct{}{}
			}
		}
	}

	for _, entry := range internal {
		arts.Desired[entry.target] = DesiredPrefixSet{IPs: entry.ips}
	}

	return arts
}
