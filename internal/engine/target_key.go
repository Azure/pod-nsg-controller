package engine

import "strings"

// targetIdentityKey returns the shared case-insensitive identity for an ASG target.
// Azure ASG resource IDs and addressPrefixSet names are treated case-insensitively,
// so PrefixSetName participates in normalization too. Because PrefixSetName is
// derived from OwnershipKey(clusterName, namespace, mappingName), controller
// instances whose cluster names differ only by case are invalid configuration:
// they converge on the same Azure addressPrefixSet.
func targetIdentityKey(fullResourceID, prefixSetName string) string {
	return strings.ToLower(fullResourceID + "/" + prefixSetName)
}

// TargetIdentityKey is the exported form of targetIdentityKey.
func TargetIdentityKey(fullResourceID, prefixSetName string) string {
	return targetIdentityKey(fullResourceID, prefixSetName)
}

func canonicalASGResourceID(subscriptionID, resourceGroup, asgName string) string {
	return "/subscriptions/" + subscriptionID +
		"/resourceGroups/" + resourceGroup +
		"/providers/Microsoft.Network/applicationSecurityGroups/" + asgName
}

func targetKey(target ASGTarget) string {
	resourceID := target.FullResourceID
	if resourceID == "" {
		resourceID = canonicalASGResourceID(
			target.SubscriptionID,
			target.ResourceGroup,
			target.ASGName,
		)
	}

	return targetIdentityKey(resourceID, target.PrefixSetName)
}

// TargetKey returns the normalized identity key for an ASGTarget, suitable for
// use as a convergence tracking key. It uses the full resource ID when available,
// falling back to a canonical form constructed from component fields.
func TargetKey(target ASGTarget) string {
	return targetKey(target)
}
