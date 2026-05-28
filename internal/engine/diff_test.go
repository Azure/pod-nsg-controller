package engine

import (
	"sort"
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
)

// --- helpers ---

func makeTarget(sub, rg, asg, prefixSetName string) ASGTarget {
	return ASGTarget{
		SubscriptionID: sub,
		ResourceGroup:  rg,
		ASGName:        asg,
		FullResourceID: makeASGResourceIDForDiff(sub, rg, asg),
		PrefixSetName:  prefixSetName,
	}
}

func makeASGResourceIDForDiff(sub, rg, name string) string {
	return "/subscriptions/" + sub + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/applicationSecurityGroups/" + name
}

func ipSet(ips ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		m[ip] = struct{}{}
	}
	return m
}

func desiredToActual(desired map[ASGTarget]DesiredPrefixSet) map[ASGTarget]ActualPrefixSet {
	actual := make(map[ASGTarget]ActualPrefixSet, len(desired))
	for target, prefixSet := range desired {
		actual[target] = ActualPrefixSet{IPs: prefixSet.IPs}
	}
	return actual
}

func actionKinds(actions []Action) []ActionKind {
	out := make([]ActionKind, len(actions))
	for i, a := range actions {
		out[i] = a.Kind
	}
	return out
}

// --- T3.8: Diff — Update when IPs differ ---
func TestPhase3_T38_DiffUpdateWhenIPsDiffer(t *testing.T) {
	target := makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping")

	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: ipSet("10.0.0.1", "10.0.0.2")},
	}
	actual := map[ASGTarget]ActualPrefixSet{
		target: {IPs: ipSet("10.0.0.1")},
	}

	actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)

	if len(actions) != 1 {
		t.Fatalf("T3.8: got %d actions, want 1", len(actions))
	}
	if actions[0].Kind != UpdatePrefixSet && actions[0].Kind != PatchPrefixSet {
		t.Errorf("T3.8: action kind = %q, want UpdatePrefixSet or PatchPrefixSet", actions[0].Kind)
	}
	if actions[0].Target.ASGName != "asg-a" {
		t.Errorf("T3.8: target ASGName = %q, want %q", actions[0].Target.ASGName, "asg-a")
	}
	wantIPs := []string{"10.0.0.1", "10.0.0.2"}
	gotIPs := actions[0].DesiredIPs
	if len(gotIPs) != len(wantIPs) {
		t.Fatalf("T3.8: got %d desired IPs, want %d", len(gotIPs), len(wantIPs))
	}
	for i, ip := range gotIPs {
		if ip != wantIPs[i] {
			t.Errorf("T3.8: DesiredIPs[%d] = %q, want %q", i, ip, wantIPs[i])
		}
	}
}

// --- T3.9: Diff — No actions when IPs equal ---
func TestPhase3_T39_DiffNoActionsWhenIPsEqual(t *testing.T) {
	target := makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping")

	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: ipSet("10.0.0.1")},
	}
	actual := map[ASGTarget]ActualPrefixSet{
		target: {IPs: ipSet("10.0.0.1")},
	}

	actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)

	// Must return a non-nil empty slice, not nil.
	if actions == nil {
		t.Fatal("T3.9: ComputeDiff returned nil, want non-nil empty slice")
	}
	if len(actions) != 0 {
		t.Errorf("T3.9: got %d actions, want 0 (no-op when IPs equal)", len(actions))
	}
}

// --- T3.10: Diff — Delete when only actual exists ---
func TestPhase3_T310_DiffDeleteWhenOnlyActualExists(t *testing.T) {
	target := makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping")

	desired := map[ASGTarget]DesiredPrefixSet{}
	actual := map[ASGTarget]ActualPrefixSet{
		target: {IPs: ipSet("10.0.0.1")},
	}

	actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)

	if len(actions) != 1 {
		t.Fatalf("T3.10: got %d actions, want 1", len(actions))
	}
	if actions[0].Kind != DeletePrefixSet {
		t.Errorf("T3.10: action kind = %q, want %q", actions[0].Kind, DeletePrefixSet)
	}
	if actions[0].DesiredIPs != nil {
		t.Errorf("T3.10: DesiredIPs should be nil for delete, got %v", actions[0].DesiredIPs)
	}
}

// --- T3.11: Diff — Create when only desired exists ---
func TestPhase3_T311_DiffCreateWhenOnlyDesiredExists(t *testing.T) {
	target := makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping")

	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: ipSet("10.0.0.1")},
	}
	actual := map[ASGTarget]ActualPrefixSet{}

	actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)

	if len(actions) != 1 {
		t.Fatalf("T3.11: got %d actions, want 1", len(actions))
	}
	if actions[0].Kind != CreatePrefixSet {
		t.Errorf("T3.11: action kind = %q, want %q", actions[0].Kind, CreatePrefixSet)
	}
	if len(actions[0].DesiredIPs) != 1 || actions[0].DesiredIPs[0] != "10.0.0.1" {
		t.Errorf("T3.11: DesiredIPs = %v, want [10.0.0.1]", actions[0].DesiredIPs)
	}
}

// --- T3.12: Diff — Mixed update, create, delete ---
func TestPhase3_T312_DiffMixedUpdateCreateDelete(t *testing.T) {
	targetA := makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping")
	targetB := makeTarget("sub-1", "rg-1", "asg-b", "cluster-default-mapping")
	targetC := makeTarget("sub-1", "rg-1", "asg-c", "cluster-default-mapping")

	desired := map[ASGTarget]DesiredPrefixSet{
		targetA: {IPs: ipSet("10.0.0.1")}, // actual has [ip1,ip3] => Update
		targetB: {IPs: ipSet("10.0.0.2")}, // no actual => Create
	}
	actual := map[ASGTarget]ActualPrefixSet{
		targetA: {IPs: ipSet("10.0.0.1", "10.0.0.3")}, // differs => Update
		targetC: {IPs: ipSet("10.0.0.4")},             // no desired => Delete
	}

	actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)

	if len(actions) != 3 {
		t.Fatalf("T3.12: got %d actions, want 3 (update + create + delete)", len(actions))
	}

	kindMap := map[ActionKind]int{}
	for _, a := range actions {
		kindMap[a.Kind]++
	}
	if kindMap[UpdatePrefixSet]+kindMap[PatchPrefixSet] != 1 {
		t.Errorf("T3.12: expected 1 UpdatePrefixSet or PatchPrefixSet, got update=%d patch=%d", kindMap[UpdatePrefixSet], kindMap[PatchPrefixSet])
	}
	if kindMap[CreatePrefixSet] != 1 {
		t.Errorf("T3.12: expected 1 CreatePrefixSet, got %d", kindMap[CreatePrefixSet])
	}
	if kindMap[DeletePrefixSet] != 1 {
		t.Errorf("T3.12: expected 1 DeletePrefixSet, got %d", kindMap[DeletePrefixSet])
	}
}

// --- Extra: Deterministic ordering ---
func TestPhase3_Diff_DeterministicOrdering(t *testing.T) {
	targetA := makeTarget("sub-1", "rg-1", "asg-alpha", "key-1")
	targetB := makeTarget("sub-1", "rg-1", "asg-beta", "key-1")
	targetC := makeTarget("sub-1", "rg-1", "asg-gamma", "key-1")

	desired := map[ASGTarget]DesiredPrefixSet{
		targetA: {IPs: ipSet("10.0.0.1")},
		targetB: {IPs: ipSet("10.0.0.2")},
		targetC: {IPs: ipSet("10.0.0.3")},
	}
	actual := map[ASGTarget]ActualPrefixSet{}

	// Run multiple times to verify ordering is deterministic.
	var firstRun []string
	for i := 0; i < 5; i++ {
		actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)
		if len(actions) != 3 {
			t.Fatalf("DeterministicOrdering: run %d got %d actions, want 3", i, len(actions))
		}
		names := make([]string, len(actions))
		for j, a := range actions {
			names[j] = a.Target.ASGName
		}
		if i == 0 {
			firstRun = names
		} else {
			for j := range names {
				if names[j] != firstRun[j] {
					t.Errorf("DeterministicOrdering: run %d order differs at index %d: %v vs %v", i, j, names, firstRun)
					break
				}
			}
		}
	}

	// Verify alphabetical order by ASG name.
	if len(firstRun) == 3 {
		sorted := make([]string, len(firstRun))
		copy(sorted, firstRun)
		sort.Strings(sorted)
		for i, name := range firstRun {
			if name != sorted[i] {
				t.Errorf("DeterministicOrdering: actions not in sorted order: got %v, want %v", firstRun, sorted)
				break
			}
		}
	}
}

// --- Extra: Nil inputs handled as empty ---
func TestPhase3_Diff_NilInputsHandledAsEmpty(t *testing.T) {
	t.Run("both nil", func(t *testing.T) {
		actions := ComputeDiff(nil, nil, DefaultPatchThresholdPercent)
		// Must return a non-nil empty slice, not nil.
		if actions == nil {
			t.Fatal("both nil: ComputeDiff returned nil, want non-nil empty slice")
		}
		if len(actions) != 0 {
			t.Errorf("both nil: got %d actions, want 0", len(actions))
		}
	})

	t.Run("desired nil, actual has entry", func(t *testing.T) {
		target := makeTarget("sub-1", "rg-1", "asg-a", "key")
		actual := map[ASGTarget]ActualPrefixSet{
			target: {IPs: ipSet("10.0.0.1")},
		}
		actions := ComputeDiff(nil, actual, DefaultPatchThresholdPercent)
		if len(actions) != 1 {
			t.Fatalf("desired nil: got %d actions, want 1 (delete)", len(actions))
		}
		if actions[0].Kind != DeletePrefixSet {
			t.Errorf("desired nil: action kind = %q, want %q", actions[0].Kind, DeletePrefixSet)
		}
	})

	t.Run("actual nil, desired has entry", func(t *testing.T) {
		target := makeTarget("sub-1", "rg-1", "asg-a", "key")
		desired := map[ASGTarget]DesiredPrefixSet{
			target: {IPs: ipSet("10.0.0.1")},
		}
		actions := ComputeDiff(desired, nil, DefaultPatchThresholdPercent)
		if len(actions) != 1 {
			t.Fatalf("actual nil: got %d actions, want 1 (create)", len(actions))
		}
		if actions[0].Kind != CreatePrefixSet {
			t.Errorf("actual nil: action kind = %q, want %q", actions[0].Kind, CreatePrefixSet)
		}
	})
}

// Test that targets differing only by resource group casing are treated as the same identity.
// Azure resource IDs are case-insensitive, so "MyRG" and "MYRG" refer to the same resource.
func TestPhase3_Diff_CaseInsensitiveResourceGroupMatch(t *testing.T) {
	// Desired state built with "MyRG" casing.
	target1 := makeTarget("sub-1", "MyRG", "asg-a", "cluster-ns-mapping")

	// Actual state returned by Azure with "MYRG" casing — same resource, different case.
	target2 := makeTarget("sub-1", "MYRG", "asg-a", "cluster-ns-mapping")

	desired := map[ASGTarget]DesiredPrefixSet{
		target1: {IPs: ipSet("10.0.0.1")},
	}
	actual := map[ASGTarget]ActualPrefixSet{
		target2: {IPs: ipSet("10.0.0.1")},
	}

	actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)

	// Case-insensitive identity matching means these two targets resolve to the same key.
	// IPs are identical, so no actions should be produced.
	t.Logf("Number of actions: %d", len(actions))
	for _, action := range actions {
		t.Logf("  %s: %s (RG=%s)", action.Kind, action.Target.ASGName, action.Target.ResourceGroup)
	}

	if len(actions) != 0 {
		t.Errorf("Got %d actions, want 0 (case-insensitive Azure resource IDs must match)", len(actions))
	}
}

// Test empty IP set in desired state (T3.6 requirement - maintain empty prefix sets)
func TestPhase3_Diff_EmptyDesiredIPSet(t *testing.T) {
	target := makeTarget("sub-1", "rg-1", "asg-a", "cluster-ns-mapping")

	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: ipSet()}, // empty
	}
	actual := map[ASGTarget]ActualPrefixSet{
		target: {IPs: ipSet("10.0.0.1")}, // has an IP
	}

	actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)

	// Should produce Update action to empty the prefix set
	if len(actions) != 1 {
		t.Fatalf("Got %d actions, want 1", len(actions))
	}
	if actions[0].Kind != UpdatePrefixSet {
		t.Errorf("Got action %s, want UpdatePrefixSet", actions[0].Kind)
	}
	if len(actions[0].DesiredIPs) != 0 {
		t.Errorf("Got %d desired IPs, want 0 (empty set)", len(actions[0].DesiredIPs))
	}
}

// Test nil IP map (defensive)
func TestPhase3_Diff_NilIPMap(t *testing.T) {
	target := makeTarget("sub-1", "rg-1", "asg-a", "cluster-ns-mapping")

	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: nil}, // nil map
	}
	actual := map[ASGTarget]ActualPrefixSet{
		target: {IPs: ipSet("10.0.0.1")},
	}

	actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)

	// nil map should be treated as empty
	if len(actions) != 1 {
		t.Fatalf("Got %d actions, want 1", len(actions))
	}
	if actions[0].DesiredIPs == nil {
		t.Errorf("DesiredIPs is nil, should be empty slice")
	}
}

func TestPhase3_Diff_CaseInsensitiveTargetIdentity(t *testing.T) {
	tests := []struct {
		name    string
		desired ASGTarget
		actual  ASGTarget
	}{
		{
			name:    "resource group differs only by case",
			desired: makeTarget("sub-1", "MyRG", "asg-a", "cluster-default-mapping"),
			actual:  makeTarget("sub-1", "MYRG", "asg-a", "cluster-default-mapping"),
		},
		{
			name:    "asg name differs only by case",
			desired: makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping"),
			actual:  makeTarget("sub-1", "rg-1", "ASG-A", "cluster-default-mapping"),
		},
		{
			name:    "prefix set name differs only by case",
			desired: makeTarget("sub-1", "rg-1", "asg-a", "prod-default-web"),
			actual:  makeTarget("sub-1", "rg-1", "asg-a", "PROD-default-web"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			desired := map[ASGTarget]DesiredPrefixSet{
				tc.desired: {IPs: ipSet("10.0.0.1")},
			}
			actual := map[ASGTarget]ActualPrefixSet{
				tc.actual: {IPs: ipSet("10.0.0.1")},
			}

			actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)
			if len(actions) != 0 {
				t.Errorf("got %d actions, want 0 for case-insensitive target identity", len(actions))
			}
		})
	}
}

func TestPhase3_Diff_ClusterNameCaseOnlyDifferenceConverges(t *testing.T) {
	mapping := makeMapping("default", "web-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: makeASGResourceIDForDiff("sub-1", "rg-1", "asg-web")},
			},
		},
	})

	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
	}

	lowerDesired := ComputeDesiredState("prod", []v1alpha1.PodASGMapping{mapping}, pods)
	upperDesired := ComputeDesiredState("PROD", []v1alpha1.PodASGMapping{mapping}, pods)

	if len(lowerDesired) != 1 {
		t.Fatalf("lowerDesired has %d targets, want 1", len(lowerDesired))
	}
	if len(upperDesired) != 1 {
		t.Fatalf("upperDesired has %d targets, want 1", len(upperDesired))
	}

	actions := ComputeDiff(lowerDesired, desiredToActual(upperDesired), DefaultPatchThresholdPercent)
	if len(actions) != 0 {
		t.Errorf("got %d actions, want 0 when cluster names differ only by case", len(actions))
	}
}

func TestPhase3_Diff_TargetIdentityFallsBackWithoutFullResourceID(t *testing.T) {
	logger := zaptest.NewLogger(t)

	t.Run("actual target without full resource id still matches desired", func(t *testing.T) {
		logger.Debug("running target key fallback no-op test")

		desiredTarget := makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping")
		actualTarget := makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping")
		actualTarget.FullResourceID = ""

		desired := map[ASGTarget]DesiredPrefixSet{
			desiredTarget: {IPs: ipSet("10.0.0.1")},
		}
		actual := map[ASGTarget]ActualPrefixSet{
			actualTarget: {IPs: ipSet("10.0.0.1")},
		}

		actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)
		if len(actions) != 0 {
			t.Errorf("actual target without full resource id: got %d actions, want 0", len(actions))
		}
	})

	t.Run("desired target without full resource id still updates matching actual", func(t *testing.T) {
		logger.Debug("running target key fallback update test")

		desiredTarget := makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping")
		desiredTarget.FullResourceID = ""
		actualTarget := makeTarget("sub-1", "RG-1", "ASG-A", "cluster-default-mapping")

		desired := map[ASGTarget]DesiredPrefixSet{
			desiredTarget: {IPs: ipSet("10.0.0.1", "10.0.0.2")},
		}
		actual := map[ASGTarget]ActualPrefixSet{
			actualTarget: {IPs: ipSet("10.0.0.1")},
		}

		actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)
		if len(actions) != 1 {
			t.Fatalf("desired target without full resource id: got %d actions, want 1", len(actions))
		}
		if actions[0].Kind != UpdatePrefixSet && actions[0].Kind != PatchPrefixSet {
			t.Errorf("desired target without full resource id: action kind = %q, want UpdatePrefixSet or PatchPrefixSet", actions[0].Kind)
		}
		wantIPs := []string{"10.0.0.1", "10.0.0.2"}
		if len(actions[0].DesiredIPs) != len(wantIPs) {
			t.Fatalf("desired target without full resource id: got %d desired IPs, want %d", len(actions[0].DesiredIPs), len(wantIPs))
		}
		for i, gotIP := range actions[0].DesiredIPs {
			if gotIP != wantIPs[i] {
				t.Errorf("desired target without full resource id: DesiredIPs[%d] = %q, want %q", i, gotIP, wantIPs[i])
			}
		}
	})
}

// --- Phase 4: Incremental Diff and Patch Tests ---

// TestComputeDiff_IncrementalPatch verifies that a small delta produces a PatchPrefixSet
// action with AddIPs and RemoveIPs fields.
// Spec: Desired: {A, B, C, D}, Actual: {A, B, E} → PatchPrefixSet with AddIPs: [C, D], RemoveIPs: [E]
func TestComputeDiff_IncrementalPatch(t *testing.T) {
	target := makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping")

	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: ipSet("10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4")},
	}
	actual := map[ASGTarget]ActualPrefixSet{
		target: {IPs: ipSet("10.0.0.1", "10.0.0.2", "10.0.0.5")},
	}

	actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)

	if len(actions) != 1 {
		t.Fatalf("TestComputeDiff_IncrementalPatch: got %d actions, want 1", len(actions))
	}

	action := actions[0]

	// Delta is 3 ops (add 2 + remove 1) vs denominator 7 (desired 4 + actual 3).
	// 100*3 = 300 <= 50*7 = 350 → should be patch.
	if action.Kind != PatchPrefixSet {
		t.Errorf("TestComputeDiff_IncrementalPatch: action kind = %q, want %q (small delta should produce patch)",
			action.Kind, PatchPrefixSet)
	}

	// Verify AddIPs
	wantAdd := []string{"10.0.0.3", "10.0.0.4"}
	if len(action.AddIPs) != len(wantAdd) {
		t.Fatalf("TestComputeDiff_IncrementalPatch: AddIPs length = %d, want %d", len(action.AddIPs), len(wantAdd))
	}
	sortedAdd := make([]string, len(action.AddIPs))
	copy(sortedAdd, action.AddIPs)
	sort.Strings(sortedAdd)
	for i, ip := range sortedAdd {
		if ip != wantAdd[i] {
			t.Errorf("TestComputeDiff_IncrementalPatch: AddIPs[%d] = %q, want %q", i, ip, wantAdd[i])
		}
	}

	// Verify RemoveIPs
	wantRemove := []string{"10.0.0.5"}
	if len(action.RemoveIPs) != len(wantRemove) {
		t.Fatalf("TestComputeDiff_IncrementalPatch: RemoveIPs length = %d, want %d", len(action.RemoveIPs), len(wantRemove))
	}
	if action.RemoveIPs[0] != "10.0.0.5" {
		t.Errorf("TestComputeDiff_IncrementalPatch: RemoveIPs[0] = %q, want %q", action.RemoveIPs[0], "10.0.0.5")
	}

	// DesiredIPs must still be populated for patch actions (for retry recompute)
	if len(action.DesiredIPs) != 4 {
		t.Errorf("TestComputeDiff_IncrementalPatch: DesiredIPs length = %d, want 4 (patch must carry full desired)", len(action.DesiredIPs))
	}
}

// TestComputeDiff_FallbackToFullReplace verifies that when delta exceeds threshold (>50%),
// the action falls back to UpdatePrefixSet.
// Spec: Desired: {X, Y, Z}, Actual: {A, B, C} → UpdatePrefixSet (full replacement, delta > 50%)
func TestComputeDiff_FallbackToFullReplace(t *testing.T) {
	target := makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping")

	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: ipSet("10.0.1.1", "10.0.1.2", "10.0.1.3")},
	}
	actual := map[ASGTarget]ActualPrefixSet{
		target: {IPs: ipSet("10.0.2.1", "10.0.2.2", "10.0.2.3")},
	}

	actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)

	if len(actions) != 1 {
		t.Fatalf("TestComputeDiff_FallbackToFullReplace: got %d actions, want 1", len(actions))
	}

	// Delta is 6 ops (add 3 + remove 3) vs denominator 6 (desired 3 + actual 3).
	// 100*6 = 600 > 50*6 = 300 → should be full update.
	if actions[0].Kind != UpdatePrefixSet {
		t.Errorf("TestComputeDiff_FallbackToFullReplace: action kind = %q, want %q (large delta should produce full update)",
			actions[0].Kind, UpdatePrefixSet)
	}

	// Full update should NOT have AddIPs/RemoveIPs
	if len(actions[0].AddIPs) != 0 {
		t.Errorf("TestComputeDiff_FallbackToFullReplace: AddIPs should be empty for full update, got %v", actions[0].AddIPs)
	}
	if len(actions[0].RemoveIPs) != 0 {
		t.Errorf("TestComputeDiff_FallbackToFullReplace: RemoveIPs should be empty for full update, got %v", actions[0].RemoveIPs)
	}
}

// TestComputeDiff_NoDiffNoPatch verifies that when desired == actual, no action is emitted.
func TestComputeDiff_NoDiffNoPatch(t *testing.T) {
	target := makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping")

	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: ipSet("10.0.0.1", "10.0.0.2", "10.0.0.3")},
	}
	actual := map[ASGTarget]ActualPrefixSet{
		target: {IPs: ipSet("10.0.0.1", "10.0.0.2", "10.0.0.3")},
	}

	actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)

	if len(actions) != 0 {
		t.Errorf("TestComputeDiff_NoDiffNoPatch: got %d actions, want 0 (no diff should produce no action)", len(actions))
	}
}

// TestComputeDiff_PatchThreshold_Configurable verifies that the threshold parameter
// directly controls patch vs full-update behavior.
func TestComputeDiff_PatchThreshold_Configurable(t *testing.T) {
	target := makeTarget("sub-1", "rg-1", "asg-a", "cluster-default-mapping")

	// Delta: add 2, remove 1 = 3 ops. Denominator: desired 4 + actual 3 = 7.
	// Default 50% threshold: 100*3 = 300 <= 50*7 = 350 → patch.
	// With threshold=40%: 100*3 = 300 > 40*7 = 280 → full update.
	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: ipSet("10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4")},
	}
	actual := map[ASGTarget]ActualPrefixSet{
		target: {IPs: ipSet("10.0.0.1", "10.0.0.2", "10.0.0.5")},
	}

	t.Run("default threshold produces patch", func(t *testing.T) {
		actions := ComputeDiff(desired, actual, DefaultPatchThresholdPercent)
		if len(actions) != 1 {
			t.Fatalf("got %d actions, want 1", len(actions))
		}
		if actions[0].Kind != PatchPrefixSet {
			t.Errorf("default threshold: action kind = %q, want %q", actions[0].Kind, PatchPrefixSet)
		}
	})

	t.Run("low threshold forces full update", func(t *testing.T) {
		actions := ComputeDiff(desired, actual, 40)
		if len(actions) != 1 {
			t.Fatalf("got %d actions, want 1", len(actions))
		}
		if actions[0].Kind != UpdatePrefixSet {
			t.Errorf("threshold=40: action kind = %q, want %q (delta exceeds 40%% threshold)",
				actions[0].Kind, UpdatePrefixSet)
		}
	})

	t.Run("invalid threshold falls back to default 50", func(t *testing.T) {
		actions := ComputeDiff(desired, actual, 0)
		if len(actions) != 1 {
			t.Fatalf("got %d actions, want 1", len(actions))
		}
		// 0 is outside valid range, clamped to default 50 → patch
		if actions[0].Kind != PatchPrefixSet {
			t.Errorf("invalid threshold (0): action kind = %q, want %q (should use default threshold)",
				actions[0].Kind, PatchPrefixSet)
		}
	})
}