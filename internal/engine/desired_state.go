package engine

import (
	"net"
	"strings"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// internalEntry holds a representative ASGTarget and its accumulated IPs.
type internalEntry struct {
	target ASGTarget
	ips    map[string]struct{}
}

// ComputeDesiredState computes the desired addressPrefixSet contents per ASG target
// from PodASGMapping CRs and live Pods. Target identity is normalized
// case-insensitively end-to-end, including PrefixSetName/ownership, because Azure
// addressPrefixSet names are case-insensitive.
func ComputeDesiredState(
	clusterName string,
	mappings []v1alpha1.PodASGMapping,
	pods []corev1.Pod,
) map[ASGTarget]DesiredPrefixSet {
	internal := make(map[string]*internalEntry)
	podsByNamespace := make(map[string][]*corev1.Pod)

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
					matchedIPs = append(matchedIPs, toCIDR(pod.Status.PodIP))
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
	return result
}

// toCIDR converts a bare IP address to CIDR notation (/32 for IPv4, /128 for IPv6).
// If the IP already contains a slash (CIDR), it is returned as-is.
func toCIDR(ip string) string {
	if strings.Contains(ip, "/") {
		return ip
	}
	if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() == nil {
		return ip + "/128"
	}
	return ip + "/32"
}
