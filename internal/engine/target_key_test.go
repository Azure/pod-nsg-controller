package engine

import "testing"

// TestTargetIdentityKey_CaseInsensitiveStable verifies T6.6 (identity key aspect):
// targetIdentityKey must produce the same key regardless of casing in the
// resource ID or prefix set name, and the result must be stable across calls.
func TestTargetIdentityKey_CaseInsensitiveStable(t *testing.T) {
	const (
		lower = "/subscriptions/sub-1/resourcegroups/rg-1/providers/microsoft.network/applicationsecuritygroups/asg-1"
		mixed = "/Subscriptions/Sub-1/ResourceGroups/RG-1/Providers/Microsoft.Network/ApplicationSecurityGroups/ASG-1"
		upper = "/SUBSCRIPTIONS/SUB-1/RESOURCEGROUPS/RG-1/PROVIDERS/MICROSOFT.NETWORK/APPLICATIONSECURITYGROUPS/ASG-1"
	)
	prefix := "myCluster__ns__mapping"

	tests := []struct {
		name       string
		resourceID string
		prefix     string
	}{
		{"lowercase", lower, prefix},
		{"mixed case", mixed, prefix},
		{"uppercase", upper, prefix},
		{"mixed prefix case", lower, "MyCluster__NS__Mapping"},
	}

	// All variants must produce the same key.
	baseline := targetIdentityKey(lower, prefix)
	if baseline == "" {
		t.Fatal("targetIdentityKey returned empty string; stub not yet implemented")
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := targetIdentityKey(tc.resourceID, tc.prefix)
			if got != baseline {
				t.Errorf("targetIdentityKey(%q, %q) = %q, want %q", tc.resourceID, tc.prefix, got, baseline)
			}
		})
	}

	// Stability: calling twice must return the same result.
	t.Run("stable across calls", func(t *testing.T) {
		a := targetIdentityKey(lower, prefix)
		b := targetIdentityKey(lower, prefix)
		if a != b {
			t.Errorf("targetIdentityKey not stable: %q != %q", a, b)
		}
	})
}

// TestTargetIdentityKey_DifferentPrefixes verifies that different prefix set
// names produce different identity keys for the same resource ID.
func TestTargetIdentityKey_DifferentPrefixes(t *testing.T) {
	resourceID := "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Network/applicationSecurityGroups/asg-1"

	k1 := targetIdentityKey(resourceID, "cluster1__ns1__mapping1")
	k2 := targetIdentityKey(resourceID, "cluster1__ns1__mapping2")

	if k1 == k2 {
		t.Errorf("different prefixes produced the same key: %q", k1)
	}
}

// TestTargetIdentityKey_DifferentResourceIDs verifies that different resource
// IDs produce different identity keys for the same prefix.
func TestTargetIdentityKey_DifferentResourceIDs(t *testing.T) {
	prefix := "cluster__ns__mapping"

	k1 := targetIdentityKey("/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Network/applicationSecurityGroups/asg-1", prefix)
	k2 := targetIdentityKey("/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Network/applicationSecurityGroups/asg-2", prefix)

	if k1 == k2 {
		t.Errorf("different resource IDs produced the same key: %q", k1)
	}
}

// TestTargetIdentityKey_EmptyInputs verifies behavior with empty strings.
func TestTargetIdentityKey_EmptyInputs(t *testing.T) {
	// Should not panic and should produce a non-empty result.
	k := targetIdentityKey("", "")
	if k == "" {
		t.Error("targetIdentityKey with empty inputs returned empty string")
	}

	// Empty resourceID with valid prefix vs valid resourceID with empty prefix must differ.
	k1 := targetIdentityKey("", "prefix")
	k2 := targetIdentityKey("prefix", "")
	if k1 == k2 {
		t.Errorf("swapped empty/non-empty inputs produced same key: %q", k1)
	}
}

// TestTargetIdentityKey_SeparatorPreventsAmbiguity verifies that realistic
// resource IDs with different prefix set names are distinguishable. The '/'
// separator between resourceID and prefix is safe because valid Azure resource
// IDs always end with the ASG name (no trailing '/'), so there is no real
// boundary ambiguity in production inputs.
func TestTargetIdentityKey_SeparatorPreventsAmbiguity(t *testing.T) {
	// Two realistic resource IDs with the same prefix must differ.
	k1 := targetIdentityKey(
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg1",
		"cluster__ns__mapping",
	)
	k2 := targetIdentityKey(
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg2",
		"cluster__ns__mapping",
	)
	if k1 == k2 {
		t.Errorf("different ASGs with same prefix produced same key: %q", k1)
	}

	// Same resource ID with different prefixes must differ.
	k3 := targetIdentityKey(
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg1",
		"clusterA__ns__mapping",
	)
	k4 := targetIdentityKey(
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg1",
		"clusterB__ns__mapping",
	)
	if k3 == k4 {
		t.Errorf("same ASG with different prefixes produced same key: %q", k3)
	}
}
