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

// ComputeDesiredStateWithSnapshot computes desired state and returns a PodSnapshot
// of all matched pods. This is the snapshot-aware variant of ComputeDesiredState.
// It uses the shared matchPodToRuleIndices and resolveRuleTargets helpers to keep
// selector/target evaluation aligned with the incremental cache paths.
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
		namespacePods := podsByNamespace[m.Namespace]

		for _, pod := range namespacePods {
			if pod.Status.PodIP == "" {
				continue
			}

			matchedIndices := matchPodToRuleIndices(m, labels.Set(pod.Labels))
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
				rule := m.Spec.Mappings[ruleIdx]
				targets := resolveRuleTargets(rule, prefixSetName)
				for _, target := range targets {
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
