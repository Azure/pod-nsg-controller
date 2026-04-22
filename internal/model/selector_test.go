package model

import (
	"fmt"
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/labels"
)

func TestPhase2_T24_CompileSelectorMatchLabels_RequiresAllLabels(t *testing.T) {
	sel := v1alpha1.PodSelector{
		MatchLabels: map[string]string{
			"app":  "web",
			"tier": "frontend",
		},
	}
	compiled, err := CompileSelector(sel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if compiled == nil {
		t.Fatal("compiled selector should not be nil")
	}

	// Pod with both labels should match
	podLabels := labels.Set{"app": "web", "tier": "frontend"}
	if !compiled.Matches(podLabels) {
		t.Errorf("selector should match pod with both labels {app:web, tier:frontend}")
	}

	// Pod missing one label should NOT match
	partialLabels := labels.Set{"app": "web"}
	if compiled.Matches(partialLabels) {
		t.Errorf("selector should NOT match pod missing 'tier' label")
	}

	// Pod with wrong value should NOT match
	wrongLabels := labels.Set{"app": "web", "tier": "backend"}
	if compiled.Matches(wrongLabels) {
		t.Errorf("selector should NOT match pod with tier=backend")
	}

	// Pod with no labels should NOT match
	emptyLabels := labels.Set{}
	if compiled.Matches(emptyLabels) {
		t.Errorf("selector should NOT match pod with no labels")
	}

	// Pod with extra labels should still match (superset)
	supersetLabels := labels.Set{"app": "web", "tier": "frontend", "version": "v2"}
	if !compiled.Matches(supersetLabels) {
		t.Errorf("selector should match pod with superset of required labels")
	}
}

func TestPhase2_T25_CompileSelectorEmptyMatchLabelsMatchesAll(t *testing.T) {
	t.Run("nil matchLabels", func(t *testing.T) {
		sel := v1alpha1.PodSelector{
			MatchLabels: nil,
		}
		compiled, err := CompileSelector(sel)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if compiled == nil {
			t.Fatal("compiled selector should not be nil")
		}

		// Should match any pod
		anyLabels := labels.Set{"app": "anything", "env": "prod"}
		if !compiled.Matches(anyLabels) {
			t.Errorf("empty selector should match any pod")
		}

		// Should match empty labels too
		if !compiled.Matches(labels.Set{}) {
			t.Errorf("empty selector should match pod with no labels")
		}
	})

	t.Run("empty map matchLabels", func(t *testing.T) {
		sel := v1alpha1.PodSelector{
			MatchLabels: map[string]string{},
		}
		compiled, err := CompileSelector(sel)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if compiled == nil {
			t.Fatal("compiled selector should not be nil")
		}

		if !compiled.Matches(labels.Set{"app": "anything"}) {
			t.Errorf("empty selector should match any pod")
		}
		if !compiled.Matches(labels.Set{}) {
			t.Errorf("empty selector should match pod with no labels")
		}
	})
}

// --- Additional coverage below ---

func TestPhase2_CompileSelector_SingleLabel(t *testing.T) {
	sel := v1alpha1.PodSelector{
		MatchLabels: map[string]string{"app": "web"},
	}
	compiled, err := CompileSelector(sel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !compiled.Matches(labels.Set{"app": "web"}) {
		t.Errorf("single-label selector should match pod with exact label")
	}
	if !compiled.Matches(labels.Set{"app": "web", "extra": "yes"}) {
		t.Errorf("single-label selector should match pod with superset labels")
	}
	if compiled.Matches(labels.Set{"app": "api"}) {
		t.Errorf("single-label selector should NOT match pod with wrong value")
	}
	if compiled.Matches(labels.Set{}) {
		t.Errorf("single-label selector should NOT match pod with no labels")
	}
	if compiled.Matches(labels.Set(nil)) {
		t.Errorf("single-label selector should NOT match nil labels set")
	}
}

func TestPhase2_CompileSelector_ZeroValueStruct(t *testing.T) {
	// A zero-value PodSelector (MatchLabels is nil by default) should match everything.
	var sel v1alpha1.PodSelector
	compiled, err := CompileSelector(sel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if compiled == nil {
		t.Fatal("compiled selector should not be nil")
	}
	if !compiled.Matches(labels.Set{"any": "label"}) {
		t.Errorf("zero-value PodSelector should match any pod")
	}
	if !compiled.Matches(labels.Set{}) {
		t.Errorf("zero-value PodSelector should match pod with no labels")
	}
}

func TestPhase2_CompileSelector_AlwaysReturnsNilError(t *testing.T) {
	// Per design §3.4B: CompileSelector is a total transform for Phase 2 matchLabels-only model.
	// It always returns nil error.
	tests := []struct {
		name string
		sel  v1alpha1.PodSelector
	}{
		{"nil matchLabels", v1alpha1.PodSelector{MatchLabels: nil}},
		{"empty matchLabels", v1alpha1.PodSelector{MatchLabels: map[string]string{}}},
		{"single label", v1alpha1.PodSelector{MatchLabels: map[string]string{"a": "b"}}},
		{"multiple labels", v1alpha1.PodSelector{MatchLabels: map[string]string{"a": "1", "b": "2", "c": "3"}}},
		{"zero-value struct", v1alpha1.PodSelector{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := CompileSelector(tc.sel)
			if err != nil {
				t.Errorf("CompileSelector returned non-nil error: %v", err)
			}
			if compiled == nil {
				t.Errorf("CompileSelector returned nil selector")
			}
		})
	}
}

func TestPhase2_CompileSelector_EmptyStringLabelValues(t *testing.T) {
	// Labels with empty-string values are valid in Kubernetes.
	// Selector must match on key+value, including empty strings.
	sel := v1alpha1.PodSelector{
		MatchLabels: map[string]string{"app": "", "env": "prod"},
	}
	compiled, err := CompileSelector(sel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Pod with matching empty-string value
	if !compiled.Matches(labels.Set{"app": "", "env": "prod"}) {
		t.Errorf("selector should match pod with app='' and env=prod")
	}
	// Pod with non-empty value for 'app' should not match
	if compiled.Matches(labels.Set{"app": "web", "env": "prod"}) {
		t.Errorf("selector should NOT match pod with app=web (want app='')")
	}
	// Pod missing key entirely should not match
	if compiled.Matches(labels.Set{"env": "prod"}) {
		t.Errorf("selector should NOT match pod missing 'app' key entirely")
	}
}

func TestPhase2_CompileSelector_ManyLabels(t *testing.T) {
	// Test with a large number of labels to ensure no scaling issues.
	matchLabels := make(map[string]string, 20)
	podLabels := make(map[string]string, 25)
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("label-%d", i)
		matchLabels[key] = fmt.Sprintf("val-%d", i)
		podLabels[key] = fmt.Sprintf("val-%d", i)
	}
	// Add extra labels on the pod
	for i := 20; i < 25; i++ {
		podLabels[fmt.Sprintf("extra-%d", i)] = "extra"
	}

	sel := v1alpha1.PodSelector{MatchLabels: matchLabels}
	compiled, err := CompileSelector(sel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Pod with all required labels (and extras) should match
	if !compiled.Matches(labels.Set(podLabels)) {
		t.Errorf("selector with 20 labels should match pod with superset")
	}

	// Pod missing one required label should not match
	partialLabels := make(map[string]string, 19)
	for k, v := range matchLabels {
		partialLabels[k] = v
	}
	delete(partialLabels, "label-0")
	if compiled.Matches(labels.Set(partialLabels)) {
		t.Errorf("selector should NOT match pod missing label-0")
	}
}

func TestPhase2_CompileSelector_SelectorStringRepresentation(t *testing.T) {
	// Verify the String() representation is meaningful (not empty) for debugging.
	sel := v1alpha1.PodSelector{
		MatchLabels: map[string]string{"app": "web", "tier": "frontend"},
	}
	compiled, err := CompileSelector(sel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := compiled.String()
	if len(s) == 0 {
		t.Errorf("compiled selector String() should be non-empty for debugging")
	}

	// Empty selector should also have a meaningful string representation
	emptySel := v1alpha1.PodSelector{}
	emptyCompiled, err := CompileSelector(emptySel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	emptyStr := emptyCompiled.String()
	// labels.Everything() typically has an empty string — that's fine, just ensure no panic
	_ = emptyStr
}

func TestPhase2_CompileSelector_MatchesNilLabelsSet(t *testing.T) {
	// Verify compiled selector handles nil labels.Set (as would come from a pod with nil labels).
	sel := v1alpha1.PodSelector{
		MatchLabels: map[string]string{"app": "web"},
	}
	compiled, err := CompileSelector(sel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A pod with nil labels should not match a non-empty selector.
	if compiled.Matches(labels.Set(nil)) {
		t.Errorf("non-empty selector should NOT match nil labels.Set")
	}

	// An everything selector should match nil labels.Set.
	everything, err := CompileSelector(v1alpha1.PodSelector{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !everything.Matches(labels.Set(nil)) {
		t.Errorf("everything selector should match nil labels.Set")
	}
}
