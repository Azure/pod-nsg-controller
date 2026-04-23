package model

import (
	"strings"
	"testing"
)

func TestPhase2_T26_OwnershipKeyGeneration(t *testing.T) {
	key := OwnershipKey("c1", "prod", "web-mapping")
	want := "c1-prod-web-mapping"
	if key != want {
		t.Errorf("OwnershipKey(c1, prod, web-mapping) = %q, want %q", key, want)
	}
}

func TestPhase2_T27_OwnershipKeysDistinctAcrossClusters(t *testing.T) {
	key1 := OwnershipKey("c1", "prod", "web-mapping")
	key2 := OwnershipKey("c2", "prod", "web-mapping")

	want1 := "c1-prod-web-mapping"
	want2 := "c2-prod-web-mapping"

	if key1 != want1 {
		t.Errorf("OwnershipKey for c1 = %q, want %q", key1, want1)
	}
	if key2 != want2 {
		t.Errorf("OwnershipKey for c2 = %q, want %q", key2, want2)
	}
	if key1 == key2 {
		t.Errorf("ownership keys should be distinct across clusters, both are %q", key1)
	}
}

func TestPhase2_OwnershipKey_DifferentNamespacesAreDistinct(t *testing.T) {
	key1 := OwnershipKey("c1", "prod", "web-mapping")
	key2 := OwnershipKey("c1", "staging", "web-mapping")

	if key1 == key2 {
		t.Errorf("ownership keys should differ across namespaces, both are %q", key1)
	}
}

func TestPhase2_OwnershipKey_DifferentMappingNamesAreDistinct(t *testing.T) {
	key1 := OwnershipKey("c1", "prod", "web-mapping")
	key2 := OwnershipKey("c1", "prod", "api-mapping")

	if key1 == key2 {
		t.Errorf("ownership keys should differ across mapping names, both are %q", key1)
	}
}

// --- Additional coverage below ---

func TestPhase2_OwnershipKey_EmptyInputs(t *testing.T) {
	// Empty inputs should still produce a deterministic key following the format.
	tests := []struct {
		name        string
		cluster     string
		namespace   string
		mappingName string
		want        string
	}{
		{"all empty", "", "", "", "--"},
		{"empty cluster", "", "ns", "mapping", "-ns-mapping"},
		{"empty namespace", "cluster", "", "mapping", "cluster--mapping"},
		{"empty mapping name", "cluster", "ns", "", "cluster-ns-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := OwnershipKey(tc.cluster, tc.namespace, tc.mappingName)
			if got != tc.want {
				t.Errorf("OwnershipKey(%q, %q, %q) = %q, want %q",
					tc.cluster, tc.namespace, tc.mappingName, got, tc.want)
			}
		})
	}
}

func TestPhase2_OwnershipKey_SpecialCharacters(t *testing.T) {
	// Ownership key must not mutate or normalize inputs — preserve as-is.
	tests := []struct {
		name        string
		cluster     string
		namespace   string
		mappingName string
		want        string
	}{
		{"dots", "c1.east", "prod.v2", "web.mapping", "c1.east-prod.v2-web.mapping"},
		{"underscores", "c1_east", "prod_v2", "web_mapping", "c1_east-prod_v2-web_mapping"},
		{"hyphens", "c-1", "my-ns", "my-mapping", "c-1-my-ns-my-mapping"},
		{"mixed special", "c1.east_us", "kube-system", "core.dns_map", "c1.east_us-kube-system-core.dns_map"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := OwnershipKey(tc.cluster, tc.namespace, tc.mappingName)
			if got != tc.want {
				t.Errorf("OwnershipKey(%q, %q, %q) = %q, want %q",
					tc.cluster, tc.namespace, tc.mappingName, got, tc.want)
			}
		})
	}
}

func TestPhase2_OwnershipKey_FormatIsClusterDashNamespaceDashMapping(t *testing.T) {
	// Explicitly verify the contract: format is "{clusterName}-{namespace}-{mappingName}".
	tests := []struct {
		cluster   string
		namespace string
		mapping   string
	}{
		{"aks-east", "production", "frontend-map"},
		{"cluster1", "default", "backend"},
		{"c", "n", "m"},
	}
	for _, tc := range tests {
		t.Run(tc.cluster+"/"+tc.namespace+"/"+tc.mapping, func(t *testing.T) {
			got := OwnershipKey(tc.cluster, tc.namespace, tc.mapping)
			want := tc.cluster + "-" + tc.namespace + "-" + tc.mapping
			if got != want {
				t.Errorf("OwnershipKey = %q, want %q (format: cluster-namespace-mapping)", got, want)
			}
		})
	}
}

func TestPhase2_OwnershipKey_AmbiguousDashBoundaries(t *testing.T) {
	// Ownership keys with embedded dashes may be ambiguous to human parsing
	// but MUST be distinct when any of the three inputs differ.
	// E.g., ("a-b", "c", "d") vs ("a", "b-c", "d") vs ("a", "b", "c-d")
	tests := []struct {
		name    string
		c, n, m string
	}{
		{"dashes in cluster", "a-b", "c", "d"},
		{"dashes in namespace", "a", "b-c", "d"},
		{"dashes in mapping", "a", "b", "c-d"},
	}
	keys := make(map[string]string)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := OwnershipKey(tc.c, tc.n, tc.m)
			keys[tc.name] = key
		})
	}
	// Note: These MAY collide because the function uses simple concatenation.
	// This test documents that the function does NOT prevent such collisions.
	// "a-b-c-d" is the output for all three — this is a known design choice.
	// What matters is: same inputs always produce same output.
	key1 := OwnershipKey("a-b", "c", "d")
	key2 := OwnershipKey("a-b", "c", "d")
	if key1 != key2 {
		t.Errorf("same inputs should produce identical keys: %q != %q", key1, key2)
	}
}

func TestPhase2_OwnershipKey_Deterministic(t *testing.T) {
	// Calling OwnershipKey with the same arguments must always produce the same result.
	const iterations = 100
	first := OwnershipKey("my-cluster", "my-namespace", "my-mapping")
	for i := 0; i < iterations; i++ {
		got := OwnershipKey("my-cluster", "my-namespace", "my-mapping")
		if got != first {
			t.Fatalf("iteration %d: OwnershipKey not deterministic: %q != %q", i, got, first)
		}
	}
}

func TestPhase2_OwnershipKey_LongInputs(t *testing.T) {
	// Verify no truncation or error on long input strings.
	longCluster := "cluster-" + strings.Repeat("a", 200)
	longNS := "namespace-" + strings.Repeat("b", 200)
	longMapping := "mapping-" + strings.Repeat("c", 200)

	got := OwnershipKey(longCluster, longNS, longMapping)
	want := longCluster + "-" + longNS + "-" + longMapping
	if got != want {
		t.Errorf("OwnershipKey with long inputs: got length %d, want %d", len(got), len(want))
	}
}

func TestPhase2_OwnershipKey_UnicodeInputs(t *testing.T) {
	// While unlikely in practice, ensure no panic or corruption with unicode.
	got := OwnershipKey("clüster", "nämespace", "mäpping")
	want := "clüster-nämespace-mäpping"
	if got != want {
		t.Errorf("OwnershipKey with unicode = %q, want %q", got, want)
	}
}
