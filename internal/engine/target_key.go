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
