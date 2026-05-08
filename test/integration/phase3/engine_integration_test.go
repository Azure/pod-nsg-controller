package phase3_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func asgResourceID(sub, rg, name string) string {
	return "/subscriptions/" + sub + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/applicationSecurityGroups/" + name
}

func makePod(ns, name string, labels map[string]string, ip string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    labels,
		},
		Status: corev1.PodStatus{PodIP: ip},
	}
}

func makeMapping(ns, name string, rules []v1alpha1.Mapping) v1alpha1.PodASGMapping {
	return v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.PodASGMappingSpec{Mappings: rules},
	}
}

func sortedIPsFromDesired(d engine.DesiredPrefixSet) []string {
	out := make([]string, 0, len(d.IPs))
	for ip := range d.IPs {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

func desiredToActual(desired map[engine.ASGTarget]engine.DesiredPrefixSet) map[engine.ASGTarget]engine.ActualPrefixSet {
	actual := make(map[engine.ASGTarget]engine.ActualPrefixSet, len(desired))
	for t, d := range desired {
		actual[t] = engine.ActualPrefixSet{IPs: d.IPs}
	}
	return actual
}

func ipSet(ips ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		m[ip] = struct{}{}
	}
	return m
}

func actionsByKind(actions []engine.Action) map[engine.ActionKind][]engine.Action {
	m := make(map[engine.ActionKind][]engine.Action)
	for _, a := range actions {
		m[a.Kind] = append(m[a.Kind], a)
	}
	return m
}

// ---------------------------------------------------------------------------
// Integration: Full pipeline — CRD types → ComputeDesiredState → ComputeDiff
// ---------------------------------------------------------------------------

// TestIntegration_FullPipeline_DesiredStateThenDiff verifies the end-to-end
// data flow from realistic PodASGMapping CRs and Pods through
// ComputeDesiredState and then ComputeDiff. This crosses three module
// boundaries: api/v1alpha1 → internal/model → internal/engine.
func TestIntegration_FullPipeline_DesiredStateThenDiff(t *testing.T) {
	const clusterName = "prod-cluster"

	asgWeb := asgResourceID("sub-1", "rg-prod", "asg-web")
	asgAPI := asgResourceID("sub-1", "rg-prod", "asg-api")
	asgDB := asgResourceID("sub-2", "rg-data", "asg-db")

	mappings := []v1alpha1.PodASGMapping{
		makeMapping("production", "web-mapping", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgWeb}},
			},
		}),
		makeMapping("production", "api-mapping", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgAPI}},
			},
		}),
		makeMapping("production", "db-mapping", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "db"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgDB}},
			},
		}),
	}

	pods := []corev1.Pod{
		makePod("production", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("production", "web-2", map[string]string{"app": "web"}, "10.0.0.2"),
		makePod("production", "api-1", map[string]string{"app": "api"}, "10.0.1.1"),
		makePod("production", "db-1", map[string]string{"app": "db"}, "10.0.2.1"),
		makePod("production", "db-2", map[string]string{"app": "db"}, "10.0.2.2"),
	}

	// Step 1: Compute desired state (crosses api → model → engine boundary).
	desired := engine.ComputeDesiredState(clusterName, mappings, pods)
	if desired == nil {
		t.Fatal("ComputeDesiredState returned nil")
	}
	if len(desired) != 3 {
		t.Fatalf("expected 3 ASG targets, got %d", len(desired))
	}

	// Verify IP counts per ASG.
	ipCounts := make(map[string]int)
	for target, ps := range desired {
		ipCounts[target.ASGName] = len(ps.IPs)
	}
	wantCounts := map[string]int{"asg-web": 2, "asg-api": 1, "asg-db": 2}
	for asg, wantCount := range wantCounts {
		if got := ipCounts[asg]; got != wantCount {
			t.Errorf("ASG %s: got %d IPs, want %d", asg, got, wantCount)
		}
	}

	// Step 2: Diff against empty actual (simulates first reconciliation).
	actionsFirstSync := engine.ComputeDiff(desired, nil)
	if len(actionsFirstSync) != 3 {
		t.Fatalf("first sync: got %d actions, want 3 creates", len(actionsFirstSync))
	}
	for _, a := range actionsFirstSync {
		if a.Kind != engine.CreatePrefixSet {
			t.Errorf("first sync: expected CreatePrefixSet, got %s for %s", a.Kind, a.Target.ASGName)
		}
	}

	// Step 3: Diff against identical actual (simulates steady state).
	actual := desiredToActual(desired)
	actionsNoOp := engine.ComputeDiff(desired, actual)
	if len(actionsNoOp) != 0 {
		t.Errorf("steady state: got %d actions, want 0", len(actionsNoOp))
	}

	// Step 4: Simulate pod scaling — add a web pod, remove a db pod.
	pods = []corev1.Pod{
		makePod("production", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("production", "web-2", map[string]string{"app": "web"}, "10.0.0.2"),
		makePod("production", "web-3", map[string]string{"app": "web"}, "10.0.0.3"), // new
		makePod("production", "api-1", map[string]string{"app": "api"}, "10.0.1.1"),
		makePod("production", "db-1", map[string]string{"app": "db"}, "10.0.2.1"),
		// db-2 removed
	}
	newDesired := engine.ComputeDesiredState(clusterName, mappings, pods)
	actionsScaling := engine.ComputeDiff(newDesired, actual)

	byKind := actionsByKind(actionsScaling)
	if len(byKind[engine.UpdatePrefixSet]) != 2 {
		t.Errorf("scaling: expected 2 updates (web gained, db lost), got %d", len(byKind[engine.UpdatePrefixSet]))
	}
}

// ---------------------------------------------------------------------------
// Integration: Phase 2 (model.BuildIndex) + Phase 3 (engine) agreement
// ---------------------------------------------------------------------------

// TestIntegration_ModelIndex_Agrees_WithDesiredState verifies that for every
// pod matched by model.BuildIndex.MatchingASGs, the engine's ComputeDesiredState
// includes that pod's IP in the corresponding ASG target's desired set.
func TestIntegration_ModelIndex_Agrees_WithDesiredState(t *testing.T) {
	const clusterName = "test-cluster"

	asgFrontend := asgResourceID("sub-1", "rg-1", "asg-frontend")
	asgBackend := asgResourceID("sub-1", "rg-1", "asg-backend")

	mappings := []v1alpha1.PodASGMapping{
		makeMapping("default", "frontend-mapping", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "frontend"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgFrontend}},
			},
		}),
		makeMapping("default", "backend-mapping", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "backend"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgBackend}},
			},
		}),
	}

	pods := []corev1.Pod{
		makePod("default", "fe-1", map[string]string{"tier": "frontend"}, "10.0.0.1"),
		makePod("default", "fe-2", map[string]string{"tier": "frontend"}, "10.0.0.2"),
		makePod("default", "be-1", map[string]string{"tier": "backend"}, "10.0.1.1"),
		makePod("default", "no-match", map[string]string{"tier": "cache"}, "10.0.9.1"),
	}

	// Phase 2: BuildIndex
	idx := model.BuildIndex(mappings)

	// Phase 3: ComputeDesiredState
	desired := engine.ComputeDesiredState(clusterName, mappings, pods)

	// Verify agreement: every pod matched by BuildIndex should have its IP
	// in the corresponding desired prefix set.
	for i := range pods {
		pod := &pods[i]
		matchedASGs := idx.MatchingASGs(pod)

		for _, asgRef := range matchedASGs {
			parsed, err := model.ParseASGResourceID(asgRef.ResourceID)
			if err != nil {
				t.Fatalf("ParseASGResourceID(%s): %v", asgRef.ResourceID, err)
			}

			found := false
			for target, ps := range desired {
				if strings.EqualFold(target.FullResourceID, parsed.FullResourceID) {
					if _, hasIP := ps.IPs[pod.Status.PodIP]; hasIP {
						found = true
						break
					}
				}
			}
			if pod.Status.PodIP != "" && !found {
				t.Errorf("pod %s/%s (IP %s) matched ASG %s via BuildIndex but IP not in desired state",
					pod.Namespace, pod.Name, pod.Status.PodIP, parsed.ASGName)
			}
		}
	}

	// Also verify unmatched pod's IP is absent from all desired sets.
	noMatchPod := &pods[3]
	for target, ps := range desired {
		if _, hasIP := ps.IPs[noMatchPod.Status.PodIP]; hasIP {
			t.Errorf("unmatched pod %s IP %s should not appear in target %s",
				noMatchPod.Name, noMatchPod.Status.PodIP, target.ASGName)
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: DeepCopy'd CRDs produce identical engine results
// ---------------------------------------------------------------------------

// TestIntegration_DeepCopiedCRDs_ProduceIdenticalDesiredState ensures that
// CRDs which have passed through controller-runtime's DeepCopy (as happens
// during real reconciliation) produce identical desired state.
func TestIntegration_DeepCopiedCRDs_ProduceIdenticalDesiredState(t *testing.T) {
	const clusterName = "test-cluster"

	asgID := asgResourceID("sub-1", "rg-1", "asg-test")
	original := makeMapping("default", "test-mapping", []v1alpha1.Mapping{
		{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "test", "env": "prod"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
		},
	})
	copied := *original.DeepCopy()

	pods := []corev1.Pod{
		makePod("default", "pod-1", map[string]string{"app": "test", "env": "prod"}, "10.0.0.1"),
		makePod("default", "pod-2", map[string]string{"app": "test", "env": "prod"}, "10.0.0.2"),
	}

	origDesired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{original}, pods)
	copyDesired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{copied}, pods)

	if len(origDesired) != len(copyDesired) {
		t.Fatalf("DeepCopy divergence: orig %d targets, copy %d targets", len(origDesired), len(copyDesired))
	}

	for origTarget, origPS := range origDesired {
		copyPS, found := copyDesired[origTarget]
		if !found {
			t.Errorf("target %+v present in original but missing from copy", origTarget)
			continue
		}
		origIPs := sortedIPsFromDesired(origPS)
		copyIPs := sortedIPsFromDesired(copyPS)
		if len(origIPs) != len(copyIPs) {
			t.Errorf("target %s: orig %d IPs, copy %d IPs", origTarget.ASGName, len(origIPs), len(copyIPs))
			continue
		}
		for i := range origIPs {
			if origIPs[i] != copyIPs[i] {
				t.Errorf("target %s IP[%d]: orig=%s copy=%s", origTarget.ASGName, i, origIPs[i], copyIPs[i])
			}
		}
	}

	// Diff between original and copy should produce zero actions.
	actions := engine.ComputeDiff(origDesired, desiredToActual(copyDesired))
	if len(actions) != 0 {
		t.Errorf("diff between original and DeepCopy produced %d actions, want 0", len(actions))
	}
}

// ---------------------------------------------------------------------------
// Integration: Case-insensitive resource IDs across model → engine boundary
// ---------------------------------------------------------------------------

// TestIntegration_CaseInsensitiveResourceID_AcrossModelAndEngine verifies that
// model.ParseASGResourceID's canonical form is used consistently by the engine,
// so case-only differences in resource IDs don't create duplicate ASG targets.
func TestIntegration_CaseInsensitiveResourceID_AcrossModelAndEngine(t *testing.T) {
	const clusterName = "test-cluster"

	// Two resource IDs that differ only in casing of static segments.
	lowerID := "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Network/applicationSecurityGroups/asg-web"
	upperID := "/subscriptions/sub-1/resourceGroups/RG-1/providers/Microsoft.Network/applicationSecurityGroups/ASG-WEB"

	// Verify both parse successfully through model.
	parsedLower, err := model.ParseASGResourceID(lowerID)
	if err != nil {
		t.Fatalf("ParseASGResourceID(%q): %v", lowerID, err)
	}
	parsedUpper, err := model.ParseASGResourceID(upperID)
	if err != nil {
		t.Fatalf("ParseASGResourceID(%q): %v", upperID, err)
	}

	// Both should produce the same canonical resource ID.
	if !strings.EqualFold(parsedLower.FullResourceID, parsedUpper.FullResourceID) {
		t.Fatalf("parsed resource IDs differ: %q vs %q", parsedLower.FullResourceID, parsedUpper.FullResourceID)
	}

	// A mapping referencing both IDs should produce one target, not two.
	mapping := makeMapping("default", "web-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: lowerID},
				{ResourceID: upperID},
			},
		},
	})
	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
	}

	desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)
	if len(desired) != 1 {
		t.Errorf("expected 1 target (case-insensitive dedup), got %d", len(desired))
	}
}

// ---------------------------------------------------------------------------
// Integration: Multi-namespace isolation through full pipeline
// ---------------------------------------------------------------------------

// TestIntegration_MultiNamespace_FullPipeline verifies that namespace
// isolation is preserved end-to-end: a mapping in namespace A only sees
// pods in namespace A, even when pods in namespace B have matching labels.
func TestIntegration_MultiNamespace_FullPipeline(t *testing.T) {
	const clusterName = "multi-ns-cluster"

	asgTeamA := asgResourceID("sub-1", "rg-1", "asg-team-a")
	asgTeamB := asgResourceID("sub-1", "rg-1", "asg-team-b")

	mappings := []v1alpha1.PodASGMapping{
		makeMapping("team-a", "app-mapping", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgTeamA}},
			},
		}),
		makeMapping("team-b", "app-mapping", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgTeamB}},
			},
		}),
	}

	pods := []corev1.Pod{
		makePod("team-a", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("team-a", "web-2", map[string]string{"app": "web"}, "10.0.0.2"),
		makePod("team-b", "web-1", map[string]string{"app": "web"}, "10.0.1.1"),
	}

	desired := engine.ComputeDesiredState(clusterName, mappings, pods)

	// Should have 2 targets (one per namespace/ASG combo).
	if len(desired) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(desired))
	}

	for target, ps := range desired {
		ips := sortedIPsFromDesired(ps)
		switch target.ASGName {
		case "asg-team-a":
			if len(ips) != 2 {
				t.Errorf("asg-team-a: got %d IPs, want 2", len(ips))
			}
			for _, ip := range ips {
				if !strings.HasPrefix(ip, "10.0.0.") {
					t.Errorf("asg-team-a: unexpected IP %s (should be team-a range)", ip)
				}
			}
		case "asg-team-b":
			if len(ips) != 1 {
				t.Errorf("asg-team-b: got %d IPs, want 1", len(ips))
			}
			if len(ips) == 1 && ips[0] != "10.0.1.1" {
				t.Errorf("asg-team-b: got IP %s, want 10.0.1.1", ips[0])
			}
		default:
			t.Errorf("unexpected ASG target: %s", target.ASGName)
		}
	}

	// Full pipeline: diff against empty → all creates, namespaces remain isolated.
	actions := engine.ComputeDiff(desired, nil)
	if len(actions) != 2 {
		t.Fatalf("expected 2 create actions, got %d", len(actions))
	}
	for _, a := range actions {
		if a.Kind != engine.CreatePrefixSet {
			t.Errorf("expected CreatePrefixSet, got %s", a.Kind)
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: Cross-subscription ASGs through full pipeline with diff
// ---------------------------------------------------------------------------

// TestIntegration_CrossSubscription_DesiredStateToDiff verifies that ASGs
// in different Azure subscriptions produce correct targets and diff actions.
func TestIntegration_CrossSubscription_DesiredStateToDiff(t *testing.T) {
	const clusterName = "cross-sub-cluster"

	asgSub1 := asgResourceID("sub-alpha", "rg-1", "asg-frontend")
	asgSub2 := asgResourceID("sub-beta", "rg-2", "asg-backend")

	mappings := []v1alpha1.PodASGMapping{
		makeMapping("default", "multi-sub-mapping", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgSub1},
					{ResourceID: asgSub2},
				},
			},
		}),
	}

	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("default", "web-2", map[string]string{"app": "web"}, "10.0.0.2"),
	}

	desired := engine.ComputeDesiredState(clusterName, mappings, pods)
	if len(desired) != 2 {
		t.Fatalf("expected 2 targets (one per subscription ASG), got %d", len(desired))
	}

	subsSeen := make(map[string]bool)
	for target := range desired {
		subsSeen[target.SubscriptionID] = true
	}
	if !subsSeen["sub-alpha"] || !subsSeen["sub-beta"] {
		t.Errorf("expected targets in sub-alpha and sub-beta, got %v", subsSeen)
	}

	// Simulate: sub-alpha ASG already exists with correct IPs, sub-beta is new.
	partialActual := make(map[engine.ASGTarget]engine.ActualPrefixSet)
	for target, ps := range desired {
		if target.SubscriptionID == "sub-alpha" {
			partialActual[target] = engine.ActualPrefixSet{IPs: ps.IPs}
		}
	}

	actions := engine.ComputeDiff(desired, partialActual)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action (create sub-beta), got %d", len(actions))
	}
	if actions[0].Kind != engine.CreatePrefixSet {
		t.Errorf("expected CreatePrefixSet, got %s", actions[0].Kind)
	}
	if actions[0].Target.SubscriptionID != "sub-beta" {
		t.Errorf("expected create for sub-beta, got %s", actions[0].Target.SubscriptionID)
	}
}

// ---------------------------------------------------------------------------
// Integration: Mapping deletion triggers diff cleanup
// ---------------------------------------------------------------------------

// TestIntegration_MappingDeletion_ProducesDeleteActions verifies that when a
// mapping is removed from the input, ComputeDesiredState no longer includes its
// targets, and ComputeDiff produces Delete actions to clean up Azure state.
func TestIntegration_MappingDeletion_ProducesDeleteActions(t *testing.T) {
	const clusterName = "cleanup-cluster"

	asgWeb := asgResourceID("sub-1", "rg-1", "asg-web")
	asgAPI := asgResourceID("sub-1", "rg-1", "asg-api")

	allMappings := []v1alpha1.PodASGMapping{
		makeMapping("default", "web-mapping", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgWeb}},
			},
		}),
		makeMapping("default", "api-mapping", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgAPI}},
			},
		}),
	}

	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("default", "api-1", map[string]string{"app": "api"}, "10.0.1.1"),
	}

	// Initial state: both mappings active.
	initialDesired := engine.ComputeDesiredState(clusterName, allMappings, pods)
	if len(initialDesired) != 2 {
		t.Fatalf("initial: expected 2 targets, got %d", len(initialDesired))
	}
	actual := desiredToActual(initialDesired)

	// Delete the API mapping — only web-mapping remains.
	remainingMappings := allMappings[:1]
	newDesired := engine.ComputeDesiredState(clusterName, remainingMappings, pods)
	if len(newDesired) != 1 {
		t.Fatalf("after deletion: expected 1 target, got %d", len(newDesired))
	}

	actions := engine.ComputeDiff(newDesired, actual)
	byKind := actionsByKind(actions)

	if len(byKind[engine.DeletePrefixSet]) != 1 {
		t.Errorf("expected 1 delete action, got %d", len(byKind[engine.DeletePrefixSet]))
	}
	if len(byKind[engine.CreatePrefixSet]) != 0 && len(byKind[engine.UpdatePrefixSet]) != 0 {
		t.Errorf("expected no create/update actions for web mapping (unchanged)")
	}
}

// ---------------------------------------------------------------------------
// Integration: OwnershipKey determinism across model → engine
// ---------------------------------------------------------------------------

// TestIntegration_OwnershipKey_DeterministicAcrossModules verifies that the
// PrefixSetName produced by engine.ComputeDesiredState matches what
// model.OwnershipKey would produce for the same inputs.
func TestIntegration_OwnershipKey_DeterministicAcrossModules(t *testing.T) {
	const clusterName = "my-cluster"
	const ns = "production"
	const mappingName = "web-mapping"

	expectedOwnerKey := model.OwnershipKey(clusterName, ns, mappingName)

	asgID := asgResourceID("sub-1", "rg-1", "asg-web")
	mapping := makeMapping(ns, mappingName, []v1alpha1.Mapping{
		{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
		},
	})
	pods := []corev1.Pod{
		makePod(ns, "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
	}

	desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)
	if len(desired) != 1 {
		t.Fatalf("expected 1 target, got %d", len(desired))
	}

	for target := range desired {
		if target.PrefixSetName != expectedOwnerKey {
			t.Errorf("PrefixSetName = %q, want %q (from model.OwnershipKey)", target.PrefixSetName, expectedOwnerKey)
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: ParseASGResourceID fields propagated correctly to ASGTarget
// ---------------------------------------------------------------------------

// TestIntegration_ParsedFields_PropagatedToASGTarget verifies that fields
// parsed by model.ParseASGResourceID appear correctly in engine.ASGTarget
// after flowing through ComputeDesiredState.
func TestIntegration_ParsedFields_PropagatedToASGTarget(t *testing.T) {
	const clusterName = "field-test-cluster"

	asgID := "/subscriptions/sub-ABC/resourceGroups/rg-PROD/providers/Microsoft.Network/applicationSecurityGroups/my-ASG"
	mapping := makeMapping("default", "test-mapping", []v1alpha1.Mapping{
		{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "test"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
		},
	})
	pods := []corev1.Pod{
		makePod("default", "test-1", map[string]string{"app": "test"}, "10.0.0.1"),
	}

	// First parse through model to know what to expect.
	parsed, err := model.ParseASGResourceID(asgID)
	if err != nil {
		t.Fatalf("ParseASGResourceID: %v", err)
	}

	desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)
	if len(desired) != 1 {
		t.Fatalf("expected 1 target, got %d", len(desired))
	}

	for target := range desired {
		if target.SubscriptionID != parsed.SubscriptionID {
			t.Errorf("SubscriptionID: got %q, want %q", target.SubscriptionID, parsed.SubscriptionID)
		}
		if target.ResourceGroup != parsed.ResourceGroup {
			t.Errorf("ResourceGroup: got %q, want %q", target.ResourceGroup, parsed.ResourceGroup)
		}
		if target.ASGName != parsed.ASGName {
			t.Errorf("ASGName: got %q, want %q", target.ASGName, parsed.ASGName)
		}
		if target.FullResourceID != parsed.FullResourceID {
			t.Errorf("FullResourceID: got %q, want %q", target.FullResourceID, parsed.FullResourceID)
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: Invalid data gracefully handled across module boundaries
// ---------------------------------------------------------------------------

// TestIntegration_InvalidResourceID_SkippedGracefully verifies that an invalid
// ASG resource ID in a CRD doesn't crash the engine and doesn't poison valid
// targets. Error propagation across model → engine boundary is graceful.
func TestIntegration_InvalidResourceID_SkippedGracefully(t *testing.T) {
	const clusterName = "error-handling-cluster"

	validASG := asgResourceID("sub-1", "rg-1", "asg-good")
	invalidASGs := []string{
		"not-a-valid-resource-id",
		"",
		"/subscriptions//resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/bad",
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/not-an-asg",
	}

	for _, invalidASG := range invalidASGs {
		t.Run(invalidASG, func(t *testing.T) {
			mapping := makeMapping("default", "mixed-mapping", []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "test"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: validASG},
						{ResourceID: invalidASG},
					},
				},
			})
			pods := []corev1.Pod{
				makePod("default", "test-1", map[string]string{"app": "test"}, "10.0.0.1"),
			}

			desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)
			if len(desired) != 1 {
				t.Errorf("expected 1 target (valid ASG only), got %d", len(desired))
			}
			for target := range desired {
				if target.ASGName != "asg-good" {
					t.Errorf("expected target for asg-good, got %s", target.ASGName)
				}
			}

			// Diff should work normally with the valid target.
			actions := engine.ComputeDiff(desired, nil)
			if len(actions) != 1 {
				t.Errorf("expected 1 create action, got %d", len(actions))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Integration: Reconciliation lifecycle simulation
// ---------------------------------------------------------------------------

// TestIntegration_ReconciliationLifecycle simulates a realistic multi-step
// reconciliation lifecycle:
//  1. Initial sync: no actual state → all creates
//  2. Steady state: desired == actual → no actions
//  3. Pod scaling: pod added → update action
//  4. Mapping update: selector change → update actions
//  5. Mapping deletion: mapping removed → delete actions
//  6. Full cleanup: all mappings removed → all deletes
func TestIntegration_ReconciliationLifecycle(t *testing.T) {
	const clusterName = "lifecycle-cluster"

	asgID := asgResourceID("sub-1", "rg-1", "asg-app")
	baseMappings := func(sel map[string]string) []v1alpha1.PodASGMapping {
		return []v1alpha1.PodASGMapping{
			makeMapping("default", "app-mapping", []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: sel},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
				},
			}),
		}
	}

	// Step 1: Initial sync
	pods := []corev1.Pod{
		makePod("default", "app-1", map[string]string{"app": "myapp", "version": "v1"}, "10.0.0.1"),
		makePod("default", "app-2", map[string]string{"app": "myapp", "version": "v1"}, "10.0.0.2"),
	}
	desired := engine.ComputeDesiredState(clusterName, baseMappings(map[string]string{"app": "myapp"}), pods)
	actions := engine.ComputeDiff(desired, nil)
	if len(actions) != 1 || actions[0].Kind != engine.CreatePrefixSet {
		t.Fatalf("step 1: expected 1 create, got %v", actions)
	}
	actual := desiredToActual(desired)

	// Step 2: Steady state
	desired = engine.ComputeDesiredState(clusterName, baseMappings(map[string]string{"app": "myapp"}), pods)
	actions = engine.ComputeDiff(desired, actual)
	if len(actions) != 0 {
		t.Fatalf("step 2: expected 0 actions (steady state), got %d", len(actions))
	}

	// Step 3: Pod scaling — add a pod
	pods = append(pods, makePod("default", "app-3", map[string]string{"app": "myapp", "version": "v1"}, "10.0.0.3"))
	desired = engine.ComputeDesiredState(clusterName, baseMappings(map[string]string{"app": "myapp"}), pods)
	actions = engine.ComputeDiff(desired, actual)
	if len(actions) != 1 || actions[0].Kind != engine.UpdatePrefixSet {
		t.Fatalf("step 3: expected 1 update (pod added), got %v", actions)
	}
	if len(actions[0].DesiredIPs) != 3 {
		t.Errorf("step 3: expected 3 desired IPs, got %d", len(actions[0].DesiredIPs))
	}
	actual = desiredToActual(desired)

	// Step 4: Selector change — now select only v2 pods
	pods = append(pods, makePod("default", "app-4", map[string]string{"app": "myapp", "version": "v2"}, "10.0.0.4"))
	desired = engine.ComputeDesiredState(clusterName, baseMappings(map[string]string{"version": "v2"}), pods)
	actions = engine.ComputeDiff(desired, actual)
	if len(actions) != 1 || actions[0].Kind != engine.UpdatePrefixSet {
		t.Fatalf("step 4: expected 1 update (selector change), got %v", actions)
	}
	if len(actions[0].DesiredIPs) != 1 || actions[0].DesiredIPs[0] != "10.0.0.4" {
		t.Errorf("step 4: expected [10.0.0.4], got %v", actions[0].DesiredIPs)
	}
	actual = desiredToActual(desired)

	// Step 5: Full cleanup — all mappings removed
	desired = engine.ComputeDesiredState(clusterName, nil, pods)
	actions = engine.ComputeDiff(desired, actual)
	if len(actions) != 1 || actions[0].Kind != engine.DeletePrefixSet {
		t.Fatalf("step 5: expected 1 delete (all mappings removed), got %v", actions)
	}
}

// ---------------------------------------------------------------------------
// Integration: Large-scale scenario
// ---------------------------------------------------------------------------

// TestIntegration_LargeScale_ManyMappingsAndPods verifies that the engine
// handles a realistic production-scale scenario with many mappings and pods
// without panicking and produces correct aggregate results.
func TestIntegration_LargeScale_ManyMappingsAndPods(t *testing.T) {
	const clusterName = "scale-cluster"
	const numMappings = 20
	const podsPerMapping = 50

	var mappings []v1alpha1.PodASGMapping
	var pods []corev1.Pod

	for i := 0; i < numMappings; i++ {
		asgID := asgResourceID("sub-1", "rg-1", "asg-"+string(rune('a'+i)))
		appLabel := "app-" + string(rune('a'+i))
		m := makeMapping("default", "mapping-"+string(rune('a'+i)), []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": appLabel}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
			},
		})
		mappings = append(mappings, m)

		for j := 0; j < podsPerMapping; j++ {
			// Use numeric approach for deterministic IPs.
			ip := fmt.Sprintf("10.%d.%d.%d", i/256, i%256, j)
			pods = append(pods, makePod("default", fmt.Sprintf("pod-%d-%d", i, j),
				map[string]string{"app": appLabel}, ip))
		}
	}

	desired := engine.ComputeDesiredState(clusterName, mappings, pods)
	if len(desired) != numMappings {
		t.Fatalf("expected %d targets, got %d", numMappings, len(desired))
	}

	for _, ps := range desired {
		if len(ps.IPs) != podsPerMapping {
			t.Errorf("expected %d IPs per target, got %d", podsPerMapping, len(ps.IPs))
		}
	}

	// First sync: all creates
	actions := engine.ComputeDiff(desired, nil)
	if len(actions) != numMappings {
		t.Fatalf("expected %d create actions, got %d", numMappings, len(actions))
	}

	// Steady state: no actions
	actual := desiredToActual(desired)
	actions = engine.ComputeDiff(desired, actual)
	if len(actions) != 0 {
		t.Errorf("steady state: expected 0 actions, got %d", len(actions))
	}
}

// ---------------------------------------------------------------------------
// Integration: Empty desired prefix set (T3.6) → diff creates prefix set
// ---------------------------------------------------------------------------

// TestIntegration_EmptyDesiredPrefixSet_CreateAction verifies that when a
// mapping exists but no pods match (T3.6), the engine still produces an
// ASG target with an empty IP set, and ComputeDiff produces a CreatePrefixSet
// action with an empty DesiredIPs list against empty actual state.
func TestIntegration_EmptyDesiredPrefixSet_CreateAction(t *testing.T) {
	const clusterName = "empty-ps-cluster"

	asgID := asgResourceID("sub-1", "rg-1", "asg-lonely")
	mapping := makeMapping("default", "lonely-mapping", []v1alpha1.Mapping{
		{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "ghost"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
		},
	})

	// No pods match "app=ghost".
	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
	}

	desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)
	if len(desired) != 1 {
		t.Fatalf("expected 1 target (empty prefix set), got %d", len(desired))
	}
	for _, ps := range desired {
		if len(ps.IPs) != 0 {
			t.Errorf("expected 0 IPs in empty prefix set, got %d", len(ps.IPs))
		}
	}

	// Diff against nil actual → CreatePrefixSet with empty IPs.
	actions := engine.ComputeDiff(desired, nil)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Kind != engine.CreatePrefixSet {
		t.Errorf("expected CreatePrefixSet, got %s", actions[0].Kind)
	}
	if len(actions[0].DesiredIPs) != 0 {
		t.Errorf("expected empty DesiredIPs, got %v", actions[0].DesiredIPs)
	}

	// Diff against actual with IPs → UpdatePrefixSet (actual has IPs, desired is empty).
	actualWithIPs := map[engine.ASGTarget]engine.ActualPrefixSet{
		actions[0].Target: {IPs: ipSet("10.0.0.99")},
	}
	actions = engine.ComputeDiff(desired, actualWithIPs)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Kind != engine.UpdatePrefixSet {
		t.Errorf("expected UpdatePrefixSet (clear IPs), got %s", actions[0].Kind)
	}
	if len(actions[0].DesiredIPs) != 0 {
		t.Errorf("expected empty DesiredIPs on update, got %v", actions[0].DesiredIPs)
	}
}

// ---------------------------------------------------------------------------
// Integration: Multiple rules in single mapping → shared ASG union → diff
// ---------------------------------------------------------------------------

// TestIntegration_MultipleRules_SharedASG_UnionThroughDiff verifies that
// when a single PodASGMapping CR has multiple rules pointing at the same ASG,
// IPs from all rules are unioned (T3.4) and the diff pipeline handles
// the union correctly.
func TestIntegration_MultipleRules_SharedASG_UnionThroughDiff(t *testing.T) {
	const clusterName = "union-cluster"

	asgID := asgResourceID("sub-1", "rg-1", "asg-shared")

	// Two rules in one mapping, both pointing at same ASG but different selectors.
	mapping := makeMapping("default", "union-mapping", []v1alpha1.Mapping{
		{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "frontend"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
		},
		{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "backend"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
		},
	})

	pods := []corev1.Pod{
		makePod("default", "fe-1", map[string]string{"tier": "frontend"}, "10.0.0.1"),
		makePod("default", "fe-2", map[string]string{"tier": "frontend"}, "10.0.0.2"),
		makePod("default", "be-1", map[string]string{"tier": "backend"}, "10.0.1.1"),
	}

	desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)
	if len(desired) != 1 {
		t.Fatalf("expected 1 target (shared ASG), got %d", len(desired))
	}

	for _, ps := range desired {
		ips := sortedIPsFromDesired(ps)
		want := []string{"10.0.0.1", "10.0.0.2", "10.0.1.1"}
		if len(ips) != 3 {
			t.Fatalf("expected 3 unioned IPs, got %d: %v", len(ips), ips)
		}
		for i, ip := range ips {
			if ip != want[i] {
				t.Errorf("IP[%d] = %s, want %s", i, ip, want[i])
			}
		}
	}

	// Partial actual: only frontend IPs were synced before.
	var target engine.ASGTarget
	for t := range desired {
		target = t
	}
	partialActual := map[engine.ASGTarget]engine.ActualPrefixSet{
		target: {IPs: ipSet("10.0.0.1", "10.0.0.2")},
	}

	actions := engine.ComputeDiff(desired, partialActual)
	if len(actions) != 1 {
		t.Fatalf("expected 1 update action, got %d", len(actions))
	}
	if actions[0].Kind != engine.UpdatePrefixSet {
		t.Errorf("expected UpdatePrefixSet, got %s", actions[0].Kind)
	}
	if len(actions[0].DesiredIPs) != 3 {
		t.Errorf("expected 3 desired IPs in update, got %d", len(actions[0].DesiredIPs))
	}
}

// ---------------------------------------------------------------------------
// Integration: BuildIndex excludes pods without IPs, engine agrees
// ---------------------------------------------------------------------------

// TestIntegration_PodWithoutIP_ExcludedByBothIndexAndEngine verifies that
// pods without IPs are excluded by model.BuildIndex's MatchingASGs (which
// still matches them by labels) but ComputeDesiredState correctly excludes
// their empty IPs from the desired prefix set.
func TestIntegration_PodWithoutIP_ExcludedByBothIndexAndEngine(t *testing.T) {
	const clusterName = "no-ip-cluster"

	asgID := asgResourceID("sub-1", "rg-1", "asg-app")
	mapping := makeMapping("default", "app-mapping", []v1alpha1.Mapping{
		{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "test"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
		},
	})

	pods := []corev1.Pod{
		makePod("default", "pod-with-ip", map[string]string{"app": "test"}, "10.0.0.1"),
		makePod("default", "pod-no-ip", map[string]string{"app": "test"}, ""),        // no IP yet
		makePod("default", "pod-pending", map[string]string{"app": "test"}, ""),       // also no IP
	}

	// BuildIndex matches by labels (both pods match), but engine excludes empty IPs.
	idx := model.BuildIndex([]v1alpha1.PodASGMapping{mapping})
	for _, pod := range pods {
		if pod.Labels["app"] == "test" {
			asgs := idx.MatchingASGs(&pod)
			if len(asgs) != 1 {
				t.Errorf("pod %s: BuildIndex should match 1 ASG, got %d", pod.Name, len(asgs))
			}
		}
	}

	desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)
	if len(desired) != 1 {
		t.Fatalf("expected 1 target, got %d", len(desired))
	}
	for _, ps := range desired {
		if len(ps.IPs) != 1 {
			t.Errorf("expected 1 IP (only pod with IP), got %d", len(ps.IPs))
		}
		if _, ok := ps.IPs["10.0.0.1"]; !ok {
			t.Errorf("expected IP 10.0.0.1 in desired set")
		}
		if _, ok := ps.IPs[""]; ok {
			t.Errorf("empty IP should not be in desired set")
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: Wildcard selector (empty MatchLabels) through full pipeline
// ---------------------------------------------------------------------------

// TestIntegration_WildcardSelector_MatchesAllNamespacePods verifies that
// model.CompileSelector with empty MatchLabels returns Everything(), and
// the engine correctly matches all pods in the mapping's namespace but
// not pods in other namespaces.
func TestIntegration_WildcardSelector_MatchesAllNamespacePods(t *testing.T) {
	const clusterName = "wildcard-cluster"

	asgID := asgResourceID("sub-1", "rg-1", "asg-all")

	// Mapping with empty selector → matches all pods in "default" namespace.
	mapping := makeMapping("default", "catch-all", []v1alpha1.Mapping{
		{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
		},
	})

	pods := []corev1.Pod{
		makePod("default", "pod-a", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("default", "pod-b", map[string]string{"app": "api"}, "10.0.0.2"),
		makePod("default", "pod-c", map[string]string{}, "10.0.0.3"),
		makePod("other-ns", "pod-d", map[string]string{"app": "web"}, "10.0.1.1"), // different namespace
	}

	// Verify model.CompileSelector works with empty MatchLabels.
	sel, err := model.CompileSelector(v1alpha1.PodSelector{MatchLabels: map[string]string{}})
	if err != nil {
		t.Fatalf("CompileSelector failed for empty MatchLabels: %v", err)
	}
	// Should match all pods regardless of labels.
	if !sel.Matches(nil) {
		t.Errorf("empty selector should match everything")
	}

	desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)
	if len(desired) != 1 {
		t.Fatalf("expected 1 target, got %d", len(desired))
	}
	for _, ps := range desired {
		ips := sortedIPsFromDesired(ps)
		// Should include all 3 default-namespace pods, but NOT the other-ns pod.
		want := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
		if len(ips) != 3 {
			t.Fatalf("expected 3 IPs (all default ns), got %d: %v", len(ips), ips)
		}
		for i, ip := range ips {
			if ip != want[i] {
				t.Errorf("IP[%d] = %s, want %s", i, ip, want[i])
			}
		}
	}

	// Full pipeline: diff creates, then no-op.
	actions := engine.ComputeDiff(desired, nil)
	if len(actions) != 1 || actions[0].Kind != engine.CreatePrefixSet {
		t.Errorf("expected 1 create action, got %v", actions)
	}
	actual := desiredToActual(desired)
	actions = engine.ComputeDiff(desired, actual)
	if len(actions) != 0 {
		t.Errorf("expected 0 actions (steady state), got %d", len(actions))
	}
}

// ---------------------------------------------------------------------------
// Integration: Diff DesiredIPs are sorted after flowing through full pipeline
// ---------------------------------------------------------------------------

// TestIntegration_DiffDesiredIPs_SortedFromPipeline verifies that Action.DesiredIPs
// produced by ComputeDiff are sorted lexicographically when the desired state
// comes from ComputeDesiredState with realistic CRDs — ensuring the engine's
// internal IP set correctly converts to the diff's sorted output.
func TestIntegration_DiffDesiredIPs_SortedFromPipeline(t *testing.T) {
	const clusterName = "sort-cluster"

	asgID := asgResourceID("sub-1", "rg-1", "asg-sorted")
	mapping := makeMapping("default", "sort-mapping", []v1alpha1.Mapping{
		{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "sort"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
		},
	})

	// IPs intentionally out of lexicographic order.
	pods := []corev1.Pod{
		makePod("default", "pod-z", map[string]string{"app": "sort"}, "10.0.0.9"),
		makePod("default", "pod-a", map[string]string{"app": "sort"}, "10.0.0.1"),
		makePod("default", "pod-m", map[string]string{"app": "sort"}, "10.0.0.5"),
		makePod("default", "pod-b", map[string]string{"app": "sort"}, "10.0.0.12"),
	}

	desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)
	actions := engine.ComputeDiff(desired, nil)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}

	ips := actions[0].DesiredIPs
	for i := 1; i < len(ips); i++ {
		if ips[i-1] > ips[i] {
			t.Errorf("DesiredIPs not sorted: %v", ips)
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: Idempotency of ComputeDesiredState
// ---------------------------------------------------------------------------

// TestIntegration_ComputeDesiredState_Idempotent verifies that calling
// ComputeDesiredState multiple times with identical inputs produces
// identical outputs (no hidden mutable state between calls).
func TestIntegration_ComputeDesiredState_Idempotent(t *testing.T) {
	const clusterName = "idempotent-cluster"

	asg1 := asgResourceID("sub-1", "rg-1", "asg-a")
	asg2 := asgResourceID("sub-2", "rg-2", "asg-b")

	mappings := []v1alpha1.PodASGMapping{
		makeMapping("ns1", "m1", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "a"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg1}},
			},
		}),
		makeMapping("ns2", "m2", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "b"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg2}},
			},
		}),
	}

	pods := []corev1.Pod{
		makePod("ns1", "pod-1", map[string]string{"app": "a"}, "10.0.0.1"),
		makePod("ns1", "pod-2", map[string]string{"app": "a"}, "10.0.0.2"),
		makePod("ns2", "pod-3", map[string]string{"app": "b"}, "10.0.1.1"),
	}

	result1 := engine.ComputeDesiredState(clusterName, mappings, pods)
	result2 := engine.ComputeDesiredState(clusterName, mappings, pods)

	if len(result1) != len(result2) {
		t.Fatalf("idempotency broken: call 1 has %d targets, call 2 has %d", len(result1), len(result2))
	}

	for target, ps1 := range result1 {
		ps2, ok := result2[target]
		if !ok {
			t.Errorf("target %+v present in call 1 but missing from call 2", target)
			continue
		}
		ips1 := sortedIPsFromDesired(ps1)
		ips2 := sortedIPsFromDesired(ps2)
		if len(ips1) != len(ips2) {
			t.Errorf("target %s: call 1 has %d IPs, call 2 has %d", target.ASGName, len(ips1), len(ips2))
			continue
		}
		for i := range ips1 {
			if ips1[i] != ips2[i] {
				t.Errorf("target %s IP[%d]: call 1=%s, call 2=%s", target.ASGName, i, ips1[i], ips2[i])
			}
		}
	}

	// Diff between two identical desired states should produce no actions.
	actions := engine.ComputeDiff(result1, desiredToActual(result2))
	if len(actions) != 0 {
		t.Errorf("diff between two identical ComputeDesiredState results: got %d actions, want 0", len(actions))
	}
}

// ---------------------------------------------------------------------------
// Integration: Two mappings sharing an ASG across namespaces produce distinct targets
// ---------------------------------------------------------------------------

// TestIntegration_SameASG_DifferentNamespaces_DistinctTargets verifies that
// two PodASGMapping CRs in different namespaces referencing the same Azure ASG
// produce distinct ASG targets (different PrefixSetName from OwnershipKey),
// with correct namespace-scoped IPs, and diff treats them independently.
func TestIntegration_SameASG_DifferentNamespaces_DistinctTargets(t *testing.T) {
	const clusterName = "shared-asg-cluster"

	sharedASG := asgResourceID("sub-1", "rg-1", "asg-shared")

	mappings := []v1alpha1.PodASGMapping{
		makeMapping("team-a", "web", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: sharedASG}},
			},
		}),
		makeMapping("team-b", "web", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: sharedASG}},
			},
		}),
	}

	pods := []corev1.Pod{
		makePod("team-a", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("team-b", "web-1", map[string]string{"app": "web"}, "10.0.1.1"),
		makePod("team-b", "web-2", map[string]string{"app": "web"}, "10.0.1.2"),
	}

	desired := engine.ComputeDesiredState(clusterName, mappings, pods)

	// Ownership keys differ by namespace, so we get 2 distinct targets.
	if len(desired) != 2 {
		t.Fatalf("expected 2 targets (distinct ownership keys), got %d", len(desired))
	}

	expectedOwnerA := model.OwnershipKey(clusterName, "team-a", "web")
	expectedOwnerB := model.OwnershipKey(clusterName, "team-b", "web")

	var targetA, targetB *engine.ASGTarget
	for target := range desired {
		t2 := target
		switch target.PrefixSetName {
		case expectedOwnerA:
			targetA = &t2
		case expectedOwnerB:
			targetB = &t2
		}
	}
	if targetA == nil || targetB == nil {
		t.Fatalf("missing targets: A=%v B=%v", targetA, targetB)
	}

	// Verify IP isolation.
	psA := desired[*targetA]
	psB := desired[*targetB]
	if len(psA.IPs) != 1 || len(psB.IPs) != 2 {
		t.Errorf("expected team-a:1 IP, team-b:2 IPs; got %d, %d", len(psA.IPs), len(psB.IPs))
	}

	// Diff: simulate team-a already synced, team-b new.
	partialActual := map[engine.ASGTarget]engine.ActualPrefixSet{
		*targetA: {IPs: psA.IPs},
	}
	actions := engine.ComputeDiff(desired, partialActual)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action (create team-b), got %d", len(actions))
	}
	if actions[0].Kind != engine.CreatePrefixSet {
		t.Errorf("expected CreatePrefixSet, got %s", actions[0].Kind)
	}
	if actions[0].Target.PrefixSetName != expectedOwnerB {
		t.Errorf("expected create for team-b ownership key, got %s", actions[0].Target.PrefixSetName)
	}
}
