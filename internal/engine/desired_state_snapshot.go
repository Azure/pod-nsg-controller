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

// ComputeDesiredStateWithSnapshot computes desired state and returns a PodSnapshot
// of all matched pods. This is the snapshot-aware variant of ComputeDesiredState.
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

		for _, rule := range m.Spec.Mappings {
			selector, err := model.CompileSelector(rule.PodSelector)
			if err != nil {
				continue
			}

			var matchedIPs []string
			for _, pod := range namespacePods {
				if !selector.Matches(labels.Set(pod.Labels)) {
					continue
				}
				if pod.Status.PodIP != "" {
					matchedIPs = append(matchedIPs, pod.Status.PodIP)
					// Add matched pod to snapshot (deduplicates via map key)
					id := PodIdentity{
						Namespace: pod.Namespace,
						Name:      pod.Name,
						UID:       string(pod.UID),
					}
					snapshot.Pods[id] = PodMembership{PodIP: pod.Status.PodIP}
				}
			}

			for _, asgRef := range rule.ApplicationSecurityGroups {
				parsed, err := model.ParseASGResourceID(asgRef.ResourceID)
				if err != nil {
					continue
				}

				normKey := targetIdentityKey(parsed.FullResourceID, prefixSetName)

				entry, exists := internal[normKey]
				if !exists {
					entry = &internalEntry{
						target: ASGTarget{
							SubscriptionID: parsed.SubscriptionID,
							ResourceGroup:  parsed.ResourceGroup,
							ASGName:        parsed.ASGName,
							FullResourceID: parsed.FullResourceID,
							PrefixSetName:  prefixSetName,
						},
						ips: make(map[string]struct{}),
					}
					internal[normKey] = entry
				}

				for _, ip := range matchedIPs {
					entry.ips[ip] = struct{}{}
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
