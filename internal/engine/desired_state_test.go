package engine

import (
	"reflect"
	"sort"
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- helpers ---

func makeASGResourceID(sub, rg, name string) string {
	return "/subscriptions/" + sub + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/applicationSecurityGroups/" + name
}

func makePod(namespace, name string, labels map[string]string, ip string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    labels,
		},
		Status: corev1.PodStatus{
			PodIP: ip,
		},
	}
}

func makeMapping(namespace, name string, rules []v1alpha1.Mapping) v1alpha1.PodASGMapping {
	return v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: rules,
		},
	}
}

func ipsFromDesired(d DesiredPrefixSet) []string {
	out := make([]string, 0, len(d.IPs))
	for ip := range d.IPs {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// --- T3.1: Single mapping, 3 pods match, 1 ASG ---
func TestPhase3_T31_SingleMappingThreePodsOneASG(t *testing.T) {
	asgID := makeASGResourceID("sub-1", "rg-1", "asg-web")
	mapping := makeMapping("default", "web-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgID},
			},
		},
	})
	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("default", "web-2", map[string]string{"app": "web"}, "10.0.0.2"),
		makePod("default", "web-3", map[string]string{"app": "web"}, "10.0.0.3"),
	}

	result := ComputeDesiredState("test-cluster", []v1alpha1.PodASGMapping{mapping}, pods)

	if result == nil {
		t.Fatal("T3.1: ComputeDesiredState returned nil, want non-nil map")
	}
	if len(result) != 1 {
		t.Fatalf("T3.1: got %d targets, want 1", len(result))
	}
	for target, prefixSet := range result {
		if target.ASGName != "asg-web" {
			t.Errorf("T3.1: ASGName = %q, want %q", target.ASGName, "asg-web")
		}
		ips := ipsFromDesired(prefixSet)
		wantIPs := []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32"}
		if len(ips) != 3 {
			t.Fatalf("T3.1: got %d IPs, want 3", len(ips))
		}
		for i, ip := range ips {
			if ip != wantIPs[i] {
				t.Errorf("T3.1: IP[%d] = %q, want %q", i, ip, wantIPs[i])
			}
		}
	}
}

// --- T3.2: Single mapping, pod has no IP yet ---
func TestPhase3_T32_PodWithoutIPExcluded(t *testing.T) {
	asgID := makeASGResourceID("sub-1", "rg-1", "asg-web")
	mapping := makeMapping("default", "web-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgID},
			},
		},
	})
	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("default", "web-2", map[string]string{"app": "web"}, ""), // no IP
	}

	result := ComputeDesiredState("test-cluster", []v1alpha1.PodASGMapping{mapping}, pods)

	if result == nil {
		t.Fatal("T3.2: ComputeDesiredState returned nil, want non-nil map")
	}
	for _, prefixSet := range result {
		ips := ipsFromDesired(prefixSet)
		if len(ips) != 1 {
			t.Fatalf("T3.2: got %d IPs, want 1 (pod without IP should be excluded)", len(ips))
		}
		if ips[0] != "10.0.0.1/32" {
			t.Errorf("T3.2: IP = %q, want %q", ips[0], "10.0.0.1/32")
		}
	}
}

// --- T3.3: Two mappings, overlapping selectors, different ASGs ---
func TestPhase3_T33_TwoMappingsOverlappingSelectorsDifferentASGs(t *testing.T) {
	asgWeb := makeASGResourceID("sub-1", "rg-1", "asg-web")
	asgFrontend := makeASGResourceID("sub-1", "rg-1", "asg-frontend")

	mapping1 := makeMapping("default", "mapping-web", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgWeb},
			},
		},
	})
	mapping2 := makeMapping("default", "mapping-api", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"tier": "frontend"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgFrontend},
			},
		},
	})

	pods := []corev1.Pod{
		makePod("default", "pod-1", map[string]string{"app": "web", "tier": "frontend"}, "10.0.0.1"),
		makePod("default", "pod-2", map[string]string{"app": "web", "tier": "backend"}, "10.0.0.2"),
		makePod("default", "pod-3", map[string]string{"app": "api", "tier": "frontend"}, "10.0.0.3"),
	}

	result := ComputeDesiredState("test-cluster",
		[]v1alpha1.PodASGMapping{mapping1, mapping2}, pods)

	if result == nil {
		t.Fatal("T3.3: ComputeDesiredState returned nil, want non-nil map")
	}
	if len(result) != 2 {
		t.Fatalf("T3.3: got %d targets, want 2 (one per ASG)", len(result))
	}

	gotIPsByASG := make(map[string][]string, len(result))
	for target, prefixSet := range result {
		gotIPsByASG[target.ASGName] = ipsFromDesired(prefixSet)
	}

	wantIPsByASG := map[string][]string{
		"asg-web":      {"10.0.0.1/32", "10.0.0.2/32"},
		"asg-frontend": {"10.0.0.1/32", "10.0.0.3/32"},
	}
	for asgName, wantIPs := range wantIPsByASG {
		gotIPs, exists := gotIPsByASG[asgName]
		if !exists {
			t.Fatalf("T3.3: missing target for %q", asgName)
		}
		if len(gotIPs) != len(wantIPs) {
			t.Fatalf("T3.3: target %q has %d IPs, want %d", asgName, len(gotIPs), len(wantIPs))
		}
		for i, gotIP := range gotIPs {
			if gotIP != wantIPs[i] {
				t.Errorf("T3.3: target %q IP[%d] = %q, want %q", asgName, i, gotIP, wantIPs[i])
			}
		}
	}
}

func TestPhase3_DesiredState_SameASGAcrossDifferentMappingsRemainSeparateOwnedTargets(t *testing.T) {
	asgID := makeASGResourceID("sub-1", "rg-1", "asg-shared")

	mapping1 := makeMapping("default", "mapping-a", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "alpha"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgID},
			},
		},
	})
	mapping2 := makeMapping("default", "mapping-b", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "beta"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgID},
			},
		},
	})

	pods := []corev1.Pod{
		makePod("default", "alpha-1", map[string]string{"app": "alpha"}, "10.0.0.1"),
		makePod("default", "beta-1", map[string]string{"app": "beta"}, "10.0.0.2"),
	}

	result := ComputeDesiredState("test-cluster",
		[]v1alpha1.PodASGMapping{mapping1, mapping2}, pods)

	if result == nil {
		t.Fatal("separate owned targets: ComputeDesiredState returned nil, want non-nil map")
	}
	if len(result) != 2 {
		t.Fatalf("separate owned targets: got %d targets, want 2", len(result))
	}

	gotIPsByPrefixSet := make(map[string][]string, len(result))
	for target, prefixSet := range result {
		gotIPsByPrefixSet[target.PrefixSetName] = ipsFromDesired(prefixSet)
	}

	wantIPsByPrefixSet := map[string][]string{
		"test-cluster-default-mapping-a": {"10.0.0.1/32"},
		"test-cluster-default-mapping-b": {"10.0.0.2/32"},
	}
	for prefixSetName, wantIPs := range wantIPsByPrefixSet {
		gotIPs, exists := gotIPsByPrefixSet[prefixSetName]
		if !exists {
			t.Fatalf("separate owned targets: missing target for prefix set %q", prefixSetName)
		}
		if len(gotIPs) != len(wantIPs) {
			t.Fatalf("separate owned targets: prefix set %q has %d IPs, want %d", prefixSetName, len(gotIPs), len(wantIPs))
		}
		for i, gotIP := range gotIPs {
			if gotIP != wantIPs[i] {
				t.Errorf("separate owned targets: prefix set %q IP[%d] = %q, want %q", prefixSetName, i, gotIP, wantIPs[i])
			}
		}
	}
}

// --- T3.5: Cross-subscription ASG references ---
func TestPhase3_T35_CrossSubscriptionASGReferences(t *testing.T) {
	asgSub1 := makeASGResourceID("sub-1", "rg-1", "asg-frontend")
	asgSub2 := makeASGResourceID("sub-2", "rg-2", "asg-backend")

	mapping := makeMapping("default", "cross-sub-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgSub1},
				{ResourceID: asgSub2},
			},
		},
	})

	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
	}

	result := ComputeDesiredState("test-cluster", []v1alpha1.PodASGMapping{mapping}, pods)

	if result == nil {
		t.Fatal("T3.5: ComputeDesiredState returned nil, want non-nil map")
	}
	if len(result) != 2 {
		t.Fatalf("T3.5: got %d targets, want 2 (one per subscription ASG)", len(result))
	}

	subsSeen := map[string]bool{}
	for target := range result {
		subsSeen[target.SubscriptionID] = true
	}
	if !subsSeen["sub-1"] || !subsSeen["sub-2"] {
		t.Errorf("T3.5: expected targets in sub-1 and sub-2, got subs: %v", subsSeen)
	}
}

// --- T3.6: No matching pods — empty prefix set, not absent ---
func TestPhase3_T36_NoMatchingPodsProducesEmptyOwnedPrefixSet(t *testing.T) {
	asgID := makeASGResourceID("sub-1", "rg-1", "asg-web")
	mapping := makeMapping("default", "web-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgID},
			},
		},
	})
	// No pods at all
	pods := []corev1.Pod{}

	result := ComputeDesiredState("test-cluster", []v1alpha1.PodASGMapping{mapping}, pods)

	if result == nil {
		t.Fatal("T3.6: ComputeDesiredState returned nil, want non-nil map with empty prefix set")
	}
	if len(result) != 1 {
		t.Fatalf("T3.6: got %d targets, want 1 (empty but present)", len(result))
	}
	for target, prefixSet := range result {
		if len(prefixSet.IPs) != 0 {
			t.Errorf("T3.6: target %q has %d IPs, want 0", target.ASGName, len(prefixSet.IPs))
		}
	}
}

func TestPhase3_DesiredState_BoundaryConditions(t *testing.T) {
	logger := zaptest.NewLogger(t)

	t.Run("matched pods without IPs still retain empty target", func(t *testing.T) {
		logger.Debug("running matched pods without IPs boundary test")

		asgID := makeASGResourceID("sub-1", "rg-1", "asg-web")
		mapping := makeMapping("default", "web-mapping", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgID},
				},
			},
		})
		pods := []corev1.Pod{
			makePod("default", "web-1", map[string]string{"app": "web"}, ""),
			makePod("default", "web-2", map[string]string{"app": "web"}, ""),
		}

		result := ComputeDesiredState("test-cluster", []v1alpha1.PodASGMapping{mapping}, pods)

		if len(result) != 1 {
			t.Fatalf("matched pods without IPs: got %d targets, want 1", len(result))
		}
		for target, prefixSet := range result {
			if target.PrefixSetName != "test-cluster-default-web-mapping" {
				t.Errorf("matched pods without IPs: PrefixSetName = %q, want %q", target.PrefixSetName, "test-cluster-default-web-mapping")
			}
			if len(prefixSet.IPs) != 0 {
				t.Errorf("matched pods without IPs: got %d IPs, want 0", len(prefixSet.IPs))
			}
		}
	})

	t.Run("empty selector matches all pods in mapping namespace", func(t *testing.T) {
		logger.Debug("running empty selector boundary test")

		asgID := makeASGResourceID("sub-1", "rg-1", "asg-all-pods")
		mapping := makeMapping("default", "all-pods", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgID},
				},
			},
		})
		pods := []corev1.Pod{
			makePod("default", "pod-1", map[string]string{"app": "web"}, "10.0.0.1"),
			makePod("default", "pod-2", map[string]string{"app": "api"}, "10.0.0.2"),
			makePod("other", "pod-3", map[string]string{"app": "web"}, "10.0.0.3"),
		}

		result := ComputeDesiredState("test-cluster", []v1alpha1.PodASGMapping{mapping}, pods)

		if len(result) != 1 {
			t.Fatalf("empty selector: got %d targets, want 1", len(result))
		}
		for _, prefixSet := range result {
			gotIPs := ipsFromDesired(prefixSet)
			wantIPs := []string{"10.0.0.1/32", "10.0.0.2/32"}
			if len(gotIPs) != len(wantIPs) {
				t.Fatalf("empty selector: got %d IPs, want %d", len(gotIPs), len(wantIPs))
			}
			for i, gotIP := range gotIPs {
				if gotIP != wantIPs[i] {
					t.Errorf("empty selector: IP[%d] = %q, want %q", i, gotIP, wantIPs[i])
				}
			}
		}
	})
}

func TestPhase3_DesiredState_NamespaceOwnershipSeparatesTargets(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Debug("running namespace ownership isolation test")

	asgID := makeASGResourceID("sub-1", "rg-1", "asg-shared")
	mappingTeamA := makeMapping("team-a", "shared-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgID},
			},
		},
	})
	mappingTeamB := makeMapping("team-b", "shared-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgID},
			},
		},
	})
	pods := []corev1.Pod{
		makePod("team-a", "web-a", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("team-b", "web-b", map[string]string{"app": "web"}, "10.0.0.2"),
	}

	result := ComputeDesiredState(
		"test-cluster",
		[]v1alpha1.PodASGMapping{mappingTeamA, mappingTeamB},
		pods,
	)

	if len(result) != 2 {
		t.Fatalf("namespace ownership: got %d targets, want 2", len(result))
	}

	gotIPsByPrefixSet := make(map[string][]string, len(result))
	for target, prefixSet := range result {
		gotIPsByPrefixSet[target.PrefixSetName] = ipsFromDesired(prefixSet)
	}

	wantIPsByPrefixSet := map[string][]string{
		"test-cluster-team-a-shared-mapping": {"10.0.0.1/32"},
		"test-cluster-team-b-shared-mapping": {"10.0.0.2/32"},
	}
	for prefixSetName, wantIPs := range wantIPsByPrefixSet {
		gotIPs, exists := gotIPsByPrefixSet[prefixSetName]
		if !exists {
			t.Fatalf("namespace ownership: missing target for prefix set %q", prefixSetName)
		}
		if len(gotIPs) != len(wantIPs) {
			t.Fatalf("namespace ownership: prefix set %q has %d IPs, want %d", prefixSetName, len(gotIPs), len(wantIPs))
		}
		for i, gotIP := range gotIPs {
			if gotIP != wantIPs[i] {
				t.Errorf("namespace ownership: prefix set %q IP[%d] = %q, want %q", prefixSetName, i, gotIP, wantIPs[i])
			}
		}
	}
}

// --- T3.7: Mapping deleted — no desired targets ---
func TestPhase3_T37_MappingDeletedProducesNoDesiredTargets(t *testing.T) {
	// No mappings passed (simulates deleted mapping)
	mappings := []v1alpha1.PodASGMapping{}
	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
	}

	result := ComputeDesiredState("test-cluster", mappings, pods)

	// Must return an initialized empty map, not nil.
	if result == nil {
		t.Fatal("T3.7: ComputeDesiredState returned nil, want non-nil empty map")
	}
	if len(result) != 0 {
		t.Errorf("T3.7: got %d targets, want 0 (no mappings => no desired state)", len(result))
	}
}

// --- Extra: Namespace isolation ---
func TestPhase3_DesiredState_NamespaceIsolation(t *testing.T) {
	asgID := makeASGResourceID("sub-1", "rg-1", "asg-web")
	mapping := makeMapping("team-a", "web-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgID},
			},
		},
	})
	pods := []corev1.Pod{
		makePod("team-a", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("team-b", "web-2", map[string]string{"app": "web"}, "10.0.0.2"), // wrong namespace
	}

	result := ComputeDesiredState("test-cluster", []v1alpha1.PodASGMapping{mapping}, pods)

	if result == nil {
		t.Fatal("NamespaceIsolation: ComputeDesiredState returned nil")
	}
	for target, prefixSet := range result {
		ips := ipsFromDesired(prefixSet)
		if len(ips) != 1 {
			t.Errorf("NamespaceIsolation: target %q got %d IPs, want 1 (only team-a pod)", target.ASGName, len(ips))
		}
		if len(ips) == 1 && ips[0] != "10.0.0.1/32" {
			t.Errorf("NamespaceIsolation: IP = %q, want %q", ips[0], "10.0.0.1/32")
		}
	}
}

// --- Extra: Invalid resource ID skipped ---
func TestPhase3_DesiredState_InvalidResourceIDSkipped(t *testing.T) {
	validASG := makeASGResourceID("sub-1", "rg-1", "asg-good")
	invalidASG := "not-a-valid-resource-id"

	mapping := makeMapping("default", "mixed-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: validASG},
				{ResourceID: invalidASG},
			},
		},
	})
	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
	}

	result := ComputeDesiredState("test-cluster", []v1alpha1.PodASGMapping{mapping}, pods)

	if result == nil {
		t.Fatal("InvalidResourceID: ComputeDesiredState returned nil, want 1 target (valid ASG only)")
	}
	if len(result) != 1 {
		t.Fatalf("InvalidResourceID: got %d targets, want 1 (invalid ASG skipped)", len(result))
	}
	for target := range result {
		if target.ASGName != "asg-good" {
			t.Errorf("InvalidResourceID: ASGName = %q, want %q", target.ASGName, "asg-good")
		}
	}
}

// --- T3.4: Same ASG referenced by multiple rules in one mapping — IPs unioned ---
func TestPhase3_T34_SameASGReferencedByTwoRulesInSameMappingUnionsIPs(t *testing.T) {
	asgID := makeASGResourceID("sub-1", "rg-1", "asg-shared")

	mapping := makeMapping("default", "multi-rule-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "alpha"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgID},
			},
		},
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "beta"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgID},
			},
		},
	})

	pods := []corev1.Pod{
		makePod("default", "alpha-1", map[string]string{"app": "alpha"}, "10.0.0.1"),
		makePod("default", "beta-1", map[string]string{"app": "beta"}, "10.0.0.2"),
	}

	result := ComputeDesiredState("test-cluster", []v1alpha1.PodASGMapping{mapping}, pods)

	if result == nil {
		t.Fatal("T3.4: ComputeDesiredState returned nil, want non-nil map")
	}
	if len(result) != 1 {
		t.Fatalf("T3.4: got %d targets, want 1", len(result))
	}

	for target, prefixSet := range result {
		if target.ASGName != "asg-shared" {
			t.Errorf("T3.4: ASGName = %q, want %q", target.ASGName, "asg-shared")
		}
		if target.PrefixSetName != "test-cluster-default-multi-rule-mapping" {
			t.Errorf("T3.4: PrefixSetName = %q, want %q", target.PrefixSetName, "test-cluster-default-multi-rule-mapping")
		}

		gotIPs := ipsFromDesired(prefixSet)
		wantIPs := []string{"10.0.0.1/32", "10.0.0.2/32"}
		if len(gotIPs) != len(wantIPs) {
			t.Fatalf("T3.4: got %d IPs, want %d", len(gotIPs), len(wantIPs))
		}
		for i, gotIP := range gotIPs {
			if gotIP != wantIPs[i] {
				t.Errorf("T3.4: IP[%d] = %q, want %q", i, gotIP, wantIPs[i])
			}
		}
	}
}

// Test duplicate pod IPs (same IP on multiple pods) - should deduplicate
func TestPhase3_DesiredState_DuplicatePodIPs(t *testing.T) {
	asgID := makeASGResourceID("sub-1", "rg-1", "asg-web")
	mapping := makeMapping("default", "web-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgID},
			},
		},
	})

	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("default", "web-2", map[string]string{"app": "web"}, "10.0.0.1"), // Same IP
		makePod("default", "web-3", map[string]string{"app": "web"}, "10.0.0.2"),
	}

	result := ComputeDesiredState("test-cluster", []v1alpha1.PodASGMapping{mapping}, pods)

	for _, prefixSet := range result {
		if len(prefixSet.IPs) != 2 {
			t.Errorf("Got %d unique IPs, want 2 (duplicates should be deduplicated)", len(prefixSet.IPs))
		}
	}
}

func TestPhase3_DesiredState_CaseInsensitiveASGIdentityDeduplicatesTargets(t *testing.T) {
	mapping := makeMapping("default", "web-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-web")},
				{ResourceID: "/subscriptions/SUB-1/resourceGroups/RG-1/providers/Microsoft.Network/applicationSecurityGroups/ASG-WEB"},
			},
		},
	})

	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
	}

	result := ComputeDesiredState("test-cluster", []v1alpha1.PodASGMapping{mapping}, pods)

	if len(result) != 1 {
		t.Fatalf("got %d targets, want 1 for case-insensitive ASG identity", len(result))
	}

	for target, prefixSet := range result {
		if target.ResourceGroup != "rg-1" {
			t.Errorf("ResourceGroup = %q, want %q from first-seen target", target.ResourceGroup, "rg-1")
		}
		if target.ASGName != "asg-web" {
			t.Errorf("ASGName = %q, want %q from first-seen target", target.ASGName, "asg-web")
		}

		ips := ipsFromDesired(prefixSet)
		if len(ips) != 1 {
			t.Fatalf("got %d IPs, want 1", len(ips))
		}
		if ips[0] != "10.0.0.1/32" {
			t.Errorf("IP = %q, want %q", ips[0], "10.0.0.1/32")
		}
	}
}

func TestPhase3_DesiredState_DeterministicAcrossInputOrdering(t *testing.T) {
	logger := zaptest.NewLogger(t)

	mappingWeb := makeMapping("default", "web-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-web")},
			},
		},
	})
	mappingAPI := makeMapping("default", "api-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "api"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: makeASGResourceID("sub-2", "rg-2", "asg-api")},
			},
		},
	})

	baseMappings := []v1alpha1.PodASGMapping{mappingWeb, mappingAPI}
	basePods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("default", "api-1", map[string]string{"app": "api"}, "10.0.0.2"),
		makePod("default", "web-2", map[string]string{"app": "web"}, "10.0.0.3"),
	}
	want := ComputeDesiredState("test-cluster", baseMappings, basePods)

	tests := []struct {
		name     string
		mappings []v1alpha1.PodASGMapping
		pods     []corev1.Pod
	}{
		{
			name:     "repeated call with same inputs",
			mappings: baseMappings,
			pods:     basePods,
		},
		{
			name:     "reversed pod order",
			mappings: baseMappings,
			pods: []corev1.Pod{
				basePods[2],
				basePods[1],
				basePods[0],
			},
		},
		{
			name: "reversed mapping order",
			mappings: []v1alpha1.PodASGMapping{
				baseMappings[1],
				baseMappings[0],
			},
			pods: basePods,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger.Debug("running desired-state determinism test")

			got := ComputeDesiredState("test-cluster", tc.mappings, tc.pods)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("deterministic ordering: got %#v, want %#v", got, want)
			}
		})
	}
}
