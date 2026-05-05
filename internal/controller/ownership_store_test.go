package controller

import (
	"encoding/json"
	"strings"
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// TestLoadOwnedASGs_MissingAnnotationReturnsEmpty
// No annotation → empty slice, no error
// ---------------------------------------------------------------------------
func TestLoadOwnedASGs_MissingAnnotationReturnsEmpty(t *testing.T) {
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "m1",
			Namespace: "ns",
		},
	}

	refs, err := LoadOwnedASGs(mapping)
	if err != nil {
		t.Fatalf("expected no error for missing annotation, got: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected empty refs for missing annotation, got %d refs", len(refs))
	}
}

// ---------------------------------------------------------------------------
// TestLoadOwnedASGs_InvalidJSONReturnsError
// Malformed annotation → error returned
// ---------------------------------------------------------------------------
func TestLoadOwnedASGs_InvalidJSONReturnsError(t *testing.T) {
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "m1",
			Namespace: "ns",
			Annotations: map[string]string{
				OwnedASGsAnnotationKey: "not-valid-json{{{",
			},
		},
	}

	_, err := LoadOwnedASGs(mapping)
	if err == nil {
		t.Error("expected error for invalid JSON annotation, got nil")
	}
}

// ---------------------------------------------------------------------------
// TestLoadOwnedASGs_ValidJSON
// Valid annotation → correctly parsed refs
// ---------------------------------------------------------------------------
func TestLoadOwnedASGs_ValidJSON(t *testing.T) {
	refs := []OwnedASGRef{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1"},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2"},
	}
	data, _ := json.Marshal(refs)

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "m1",
			Namespace: "ns",
			Annotations: map[string]string{
				OwnedASGsAnnotationKey: string(data),
			},
		},
	}

	loaded, err := LoadOwnedASGs(mapping)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(loaded))
	}
	if loaded[0].ASGName != "asg1" {
		t.Errorf("expected first ref ASGName=asg1, got %s", loaded[0].ASGName)
	}
	if loaded[1].ASGName != "asg2" {
		t.Errorf("expected second ref ASGName=asg2, got %s", loaded[1].ASGName)
	}
}

// ---------------------------------------------------------------------------
// TestStoreOwnedASGs_SetsAnnotation
// StoreOwnedASGs should serialize refs to annotation
// ---------------------------------------------------------------------------
func TestStoreOwnedASGs_SetsAnnotation(t *testing.T) {
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "m1",
			Namespace: "ns",
		},
	}

	refs := []OwnedASGRef{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1"},
	}

	err := StoreOwnedASGs(mapping, refs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	annotation, ok := mapping.Annotations[OwnedASGsAnnotationKey]
	if !ok {
		t.Fatal("expected ownership annotation to be set after StoreOwnedASGs")
	}

	var loaded []OwnedASGRef
	if err := json.Unmarshal([]byte(annotation), &loaded); err != nil {
		t.Fatalf("annotation is not valid JSON: %v", err)
	}
	if len(loaded) != 1 || loaded[0].ASGName != "asg1" {
		t.Errorf("expected [{asg1}], got %+v", loaded)
	}
}

// ---------------------------------------------------------------------------
// TestTargetsFromOwnedASGs_ConvertsToASGTargets
// ---------------------------------------------------------------------------
func TestTargetsFromOwnedASGs_ConvertsToASGTargets(t *testing.T) {
	refs := []OwnedASGRef{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1"},
		{SubscriptionID: "sub2", ResourceGroup: "rg2", ASGName: "asg2"},
	}

	targets := TargetsFromOwnedASGs(refs, "my-ownership-key")

	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(targets))
	}
}

// ---------------------------------------------------------------------------
// TestBuildOwnedASGsFromDesired
// ---------------------------------------------------------------------------
func TestBuildOwnedASGsFromDesired(t *testing.T) {
	desired := map[engine.ASGTarget]engine.DesiredPrefixSet{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "key"}: {
			IPs: map[string]struct{}{"10.0.0.1": {}},
		},
		{SubscriptionID: "sub2", ResourceGroup: "rg2", ASGName: "asg2", PrefixSetName: "key"}: {
			IPs: map[string]struct{}{"10.0.0.2": {}},
		},
	}

	refs := BuildOwnedASGsFromDesired(desired)
	if len(refs) != 2 {
		t.Fatalf("expected 2 owned refs, got %d", len(refs))
	}
}

// ---------------------------------------------------------------------------
// TestUpdateOwnedASGsAfterResults_PartialFailureRetainsOwnedRefs
// If some actions fail, the owned ASGs list must still include the failed targets
// ---------------------------------------------------------------------------
func TestUpdateOwnedASGsAfterResults_PartialFailureRetainsOwnedRefs(t *testing.T) {
	existing := []OwnedASGRef{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1"},
	}
	desired := map[engine.ASGTarget]engine.DesiredPrefixSet{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "key"}: {
			IPs: map[string]struct{}{"10.0.0.1": {}},
		},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2", PrefixSetName: "key"}: {
			IPs: map[string]struct{}{"10.0.0.2": {}},
		},
	}
	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: engine.ASGTarget{ASGName: "asg1"}},
			Success: true,
		},
		{
			Action:  engine.Action{Kind: engine.CreatePrefixSet, Target: engine.ASGTarget{ASGName: "asg2"}},
			Success: false,
			Err:     azure.ErrNotFound,
		},
	}

	updated := UpdateOwnedASGsAfterResults(existing, desired, results)

	// Must retain both: asg1 succeeded, asg2 failed but should still be tracked
	if len(updated) < 2 {
		t.Errorf("expected at least 2 owned refs (partial failure retains), got %d: %+v", len(updated), updated)
	}
}

// ---------------------------------------------------------------------------
// TestPrefixSetNameEqualsOwnership
// ---------------------------------------------------------------------------
func TestPrefixSetNameEqualsOwnership(t *testing.T) {
	ownerKey := "cluster-ns-mapping"
	name := "cluster-ns-mapping"

	ps := azure.AddressPrefixSet{
		Name: &name,
	}

	if !PrefixSetNameEqualsOwnership(ps, ownerKey) {
		t.Error("expected PrefixSetNameEqualsOwnership to return true for matching name")
	}

	otherName := "other-key"
	ps2 := azure.AddressPrefixSet{
		Name: &otherName,
	}
	if PrefixSetNameEqualsOwnership(ps2, ownerKey) {
		t.Error("expected PrefixSetNameEqualsOwnership to return false for non-matching name")
	}
}

// ---------------------------------------------------------------------------
// TestUpdateOwnedASGsAfterResults_SuccessfulDeleteRemovesStaleRef
// Design §5.4: Successful delete removes target ref from owned list
// ---------------------------------------------------------------------------
func TestUpdateOwnedASGsAfterResults_SuccessfulDeleteRemovesStaleRef(t *testing.T) {
	// existing owned: asg1, asg2
	existing := []OwnedASGRef{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1"},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2"},
	}
	// desired only has asg1 (asg2 removed from spec)
	desired := map[engine.ASGTarget]engine.DesiredPrefixSet{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "key"}: {
			IPs: map[string]struct{}{"10.0.0.1": {}},
		},
	}
	// asg2 was successfully deleted
	results := []azure.ActionResult{
		{
			Action: engine.Action{
				Kind:   engine.DeletePrefixSet,
				Target: engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2", PrefixSetName: "key"},
			},
			Success: true,
		},
	}

	updated := UpdateOwnedASGsAfterResults(existing, desired, results)

	// asg2 was successfully deleted → must NOT appear in owned refs
	for _, ref := range updated {
		if ref.ASGName == "asg2" {
			t.Errorf("expected asg2 to be removed from owned refs after successful delete, but found it: %+v", updated)
		}
	}
	// asg1 must still be present
	foundASG1 := false
	for _, ref := range updated {
		if ref.ASGName == "asg1" {
			foundASG1 = true
		}
	}
	if !foundASG1 {
		t.Errorf("expected asg1 to remain in owned refs, got: %+v", updated)
	}
	if len(updated) != 1 {
		t.Errorf("expected exactly 1 owned ref after successful delete of asg2, got %d: %+v", len(updated), updated)
	}
}

// ---------------------------------------------------------------------------
// TestUpdateOwnedASGsAfterResults_NoActionDesiredTargetStillTracked
// Design §5.4 rule 3: Preserve desired targets with no action (already converged)
// ---------------------------------------------------------------------------
func TestUpdateOwnedASGsAfterResults_NoActionDesiredTargetStillTracked(t *testing.T) {
	existing := []OwnedASGRef{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1"},
	}
	// desired has asg1 and asg2; asg2 is new but already converged (no action)
	desired := map[engine.ASGTarget]engine.DesiredPrefixSet{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "key"}: {
			IPs: map[string]struct{}{"10.0.0.1": {}},
		},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2", PrefixSetName: "key"}: {
			IPs: map[string]struct{}{"10.0.0.2": {}},
		},
	}
	// No actions at all (both already converged)
	results := []azure.ActionResult{}

	updated := UpdateOwnedASGsAfterResults(existing, desired, results)

	// Both asg1 and asg2 must be in owned refs
	if len(updated) != 2 {
		t.Fatalf("expected 2 owned refs for converged desired targets, got %d: %+v", len(updated), updated)
	}
	foundASG1 := false
	foundASG2 := false
	for _, ref := range updated {
		if ref.ASGName == "asg1" {
			foundASG1 = true
		}
		if ref.ASGName == "asg2" {
			foundASG2 = true
		}
	}
	if !foundASG1 {
		t.Error("expected asg1 to be tracked in owned refs (already converged)")
	}
	if !foundASG2 {
		t.Error("expected asg2 to be tracked in owned refs (desired, no action needed)")
	}
}

// ---------------------------------------------------------------------------
// TestOwnedASGRefSorting_UsesFullTargetKey
// Cross-subscription/resource-group refs with the same ASG name must sort stably.
// ---------------------------------------------------------------------------
func TestOwnedASGRefSorting_UsesFullTargetKey(t *testing.T) {
	t.Run("BuildOwnedASGsFromDesired", func(t *testing.T) {
		desired := map[engine.ASGTarget]engine.DesiredPrefixSet{
			{SubscriptionID: "sub-b", ResourceGroup: "rg-b", ASGName: "shared", PrefixSetName: "key"}: {
				IPs: map[string]struct{}{"10.0.0.2": {}},
			},
			{SubscriptionID: "sub-a", ResourceGroup: "rg-c", ASGName: "shared", PrefixSetName: "key"}: {
				IPs: map[string]struct{}{"10.0.0.3": {}},
			},
			{SubscriptionID: "sub-a", ResourceGroup: "rg-a", ASGName: "shared", PrefixSetName: "key"}: {
				IPs: map[string]struct{}{"10.0.0.1": {}},
			},
		}

		refs := BuildOwnedASGsFromDesired(desired)
		got := ownedRefKeys(refs)
		want := []string{
			"sub-a/rg-a/shared",
			"sub-a/rg-c/shared",
			"sub-b/rg-b/shared",
		}

		if len(got) != len(want) {
			t.Fatalf("got %d refs, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("BuildOwnedASGsFromDesired order[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("UpdateOwnedASGsAfterResults", func(t *testing.T) {
		existing := []OwnedASGRef{
			{SubscriptionID: "sub-b", ResourceGroup: "rg-b", ASGName: "shared"},
			{SubscriptionID: "sub-a", ResourceGroup: "rg-c", ASGName: "shared"},
		}
		desired := map[engine.ASGTarget]engine.DesiredPrefixSet{
			{SubscriptionID: "sub-a", ResourceGroup: "rg-a", ASGName: "shared", PrefixSetName: "key"}: {
				IPs: map[string]struct{}{"10.0.0.1": {}},
			},
			{SubscriptionID: "sub-a", ResourceGroup: "rg-c", ASGName: "shared", PrefixSetName: "key"}: {
				IPs: map[string]struct{}{"10.0.0.2": {}},
			},
		}

		refs := UpdateOwnedASGsAfterResults(existing, desired, nil)
		got := ownedRefKeys(refs)
		want := []string{
			"sub-a/rg-a/shared",
			"sub-a/rg-c/shared",
			"sub-b/rg-b/shared",
		}

		if len(got) != len(want) {
			t.Fatalf("got %d refs, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("UpdateOwnedASGsAfterResults order[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})
}

func ownedRefKeys(refs []OwnedASGRef) []string {
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, strings.ToLower(ref.SubscriptionID+"/"+ref.ResourceGroup+"/"+ref.ASGName))
	}
	return keys
}
