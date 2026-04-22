package model

import (
	"fmt"
	"strings"
)

// ParsedASGReference holds the parsed components of an Azure ASG resource ID.
type ParsedASGReference struct {
	SubscriptionID string
	ResourceGroup  string
	ASGName        string
	FullResourceID string
}

// ParseASGResourceID parses an Azure ASG resource ID string into its components.
// Static segments are validated case-insensitively and rebuilt with canonical casing.
// Dynamic segments (subscription, resource group, ASG name) preserve their original casing.
func ParseASGResourceID(resourceID string) (ParsedASGReference, error) {
	if resourceID == "" {
		return ParsedASGReference{}, fmt.Errorf("resource ID must not be empty")
	}
	if !strings.HasPrefix(resourceID, "/") {
		return ParsedASGReference{}, fmt.Errorf("resource ID must start with '/': %q", resourceID)
	}
	if strings.HasSuffix(resourceID, "/") {
		return ParsedASGReference{}, fmt.Errorf("resource ID must not have trailing slash: %q", resourceID)
	}

	// Split on '/' — leading slash produces an empty first element.
	// Expected: ["", "subscriptions", sub, "resourceGroups", rg, "providers", provider, type, name]
	parts := strings.Split(resourceID, "/")
	if len(parts) != 9 {
		return ParsedASGReference{}, fmt.Errorf("resource ID has %d segments, want 9: %q", len(parts), resourceID)
	}

	if !strings.EqualFold(parts[1], "subscriptions") {
		return ParsedASGReference{}, fmt.Errorf("expected 'subscriptions' segment, got %q", parts[1])
	}
	sub := parts[2]
	if sub == "" {
		return ParsedASGReference{}, fmt.Errorf("subscription ID must not be empty: %q", resourceID)
	}

	if !strings.EqualFold(parts[3], "resourceGroups") {
		return ParsedASGReference{}, fmt.Errorf("expected 'resourceGroups' segment, got %q", parts[3])
	}
	rg := parts[4]
	if rg == "" {
		return ParsedASGReference{}, fmt.Errorf("resource group must not be empty: %q", resourceID)
	}

	if !strings.EqualFold(parts[5], "providers") {
		return ParsedASGReference{}, fmt.Errorf("expected 'providers' segment, got %q", parts[5])
	}
	if !strings.EqualFold(parts[6], "Microsoft.Network") {
		return ParsedASGReference{}, fmt.Errorf("expected provider 'Microsoft.Network', got %q", parts[6])
	}
	if !strings.EqualFold(parts[7], "applicationSecurityGroups") {
		return ParsedASGReference{}, fmt.Errorf("expected resource type 'applicationSecurityGroups', got %q", parts[7])
	}

	asgName := parts[8]
	if asgName == "" {
		return ParsedASGReference{}, fmt.Errorf("ASG name must not be empty: %q", resourceID)
	}

	fullResourceID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/applicationSecurityGroups/%s",
		sub, rg, asgName)

	return ParsedASGReference{
		SubscriptionID: sub,
		ResourceGroup:  rg,
		ASGName:        asgName,
		FullResourceID: fullResourceID,
	}, nil
}
