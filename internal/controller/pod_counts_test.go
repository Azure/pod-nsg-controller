package controller

import (
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- T6.5: matchedPods count is accurate ---

func TestComputePodCountsFromPods_T65_UsesSelectorSemantics(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "api", "env": "prod"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{}, // empty = matches all
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg3"},
				},
			},
		},
	}

	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "web-pod-1",
				Labels: map[string]string{"app": "web"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "web-pod-2",
				Labels: map[string]string{"app": "web", "version": "v2"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "api-pod-1",
				Labels: map[string]string{"app": "api", "env": "prod"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "api-pod-2",
				Labels: map[string]string{"app": "api", "env": "staging"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "other-pod",
				Labels: map[string]string{"app": "other"},
			},
		},
	}

	counts := ComputePodCountsFromPods(spec, pods)

	t.Run("Selector app=web matches 2 pods", func(t *testing.T) {
		hash := SelectorHash(map[string]string{"app": "web"})
		got := counts[hash]
		if got != 2 {
			t.Errorf("count for app=web selector = %d, want 2", got)
		}
	})

	t.Run("Selector app=api,env=prod matches 1 pod", func(t *testing.T) {
		hash := SelectorHash(map[string]string{"app": "api", "env": "prod"})
		got := counts[hash]
		if got != 1 {
			t.Errorf("count for app=api,env=prod selector = %d, want 1", got)
		}
	})

	t.Run("Empty selector matches all 5 pods", func(t *testing.T) {
		hash := SelectorHash(map[string]string{})
		got := counts[hash]
		if got != 5 {
			t.Errorf("count for empty selector = %d, want 5 (matches all pods)", got)
		}
	})
}

func TestComputePodCountsFromPods_CountsPodsWithoutPodIP(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
		},
	}

	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "pod-with-ip",
				Labels: map[string]string{"app": "web"},
			},
			Status: corev1.PodStatus{
				PodIP: "10.0.0.1",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "pod-without-ip",
				Labels: map[string]string{"app": "web"},
			},
			Status: corev1.PodStatus{
				PodIP: "", // No IP assigned yet
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "pod-pending",
				Labels: map[string]string{"app": "web"},
			},
			// No status at all
		},
	}

	counts := ComputePodCountsFromPods(spec, pods)

	t.Run("Counts by labels only, not PodIP", func(t *testing.T) {
		hash := SelectorHash(map[string]string{"app": "web"})
		got := counts[hash]
		// All 3 pods match by labels, regardless of PodIP assignment.
		if got != 3 {
			t.Errorf("count = %d, want 3 (counts pods without PodIP too)", got)
		}
	})
}

func TestComputePodCountsFromPods_DuplicateSelectorsShareSelectorHashEntry(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "api"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg3"},
				},
			},
		},
	}

	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Labels: map[string]string{"app": "web"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "web-2", Labels: map[string]string{"app": "web", "version": "v2"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Labels: map[string]string{"app": "api"}}},
	}

	counts := ComputePodCountsFromPods(spec, pods)

	t.Run("Duplicate selectors collapse to unique selector hashes", func(t *testing.T) {
		if len(counts) != 2 {
			t.Errorf("len(counts) = %d, want 2 unique selector hashes", len(counts))
		}
	})

	t.Run("Duplicate selector hash stores the shared pod count", func(t *testing.T) {
		hash := SelectorHash(map[string]string{"app": "web"})
		if counts[hash] != 2 {
			t.Errorf("counts[%q] = %d, want 2", hash, counts[hash])
		}
	})

	t.Run("Other unique selector hashes still have their own counts", func(t *testing.T) {
		hash := SelectorHash(map[string]string{"app": "api"})
		if counts[hash] != 1 {
			t.Errorf("counts[%q] = %d, want 1", hash, counts[hash])
		}
	})
}

func TestPodCountHelpers_NilMatchLabelsBehaveLikeEverythingSelector(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: nil,
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
		},
	}

	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-with-labels", Labels: map[string]string{"app": "web"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-with-nil-labels"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-with-other-labels", Labels: map[string]string{"app": "api"}}},
	}

	hash := SelectorHash(nil)
	counts := ComputePodCountsFromPods(spec, pods)
	zeroCounts := ZeroPodCountsForSpec(spec)

	t.Run("ComputePodCountsFromPods counts all pods for nil MatchLabels", func(t *testing.T) {
		if counts[hash] != 3 {
			t.Errorf("counts[%q] = %d, want 3", hash, counts[hash])
		}
	})

	t.Run("ZeroPodCountsForSpec uses the nil-selector hash", func(t *testing.T) {
		if zeroCounts[hash] != 0 {
			t.Errorf("zeroCounts[%q] = %d, want 0", hash, zeroCounts[hash])
		}
	})
}

func TestComputePodCountsFromPods_NoPods(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
		},
	}

	counts := ComputePodCountsFromPods(spec, []corev1.Pod{})

	t.Run("Returns zero count when no pods match", func(t *testing.T) {
		hash := SelectorHash(map[string]string{"app": "web"})
		got := counts[hash]
		if got != 0 {
			t.Errorf("count = %d, want 0 (no pods available)", got)
		}
	})

	t.Run("Result map is not nil", func(t *testing.T) {
		if counts == nil {
			t.Error("ComputePodCountsFromPods returned nil, want non-nil map")
		}
	})
}

func TestZeroPodCountsForSpec_ReturnsAllSelectorHashes(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "api", "env": "prod"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg3"},
				},
			},
		},
	}

	counts := ZeroPodCountsForSpec(spec)

	t.Run("Returns non-nil map", func(t *testing.T) {
		if counts == nil {
			t.Fatal("ZeroPodCountsForSpec returned nil, want non-nil map")
		}
	})

	t.Run("Map has entry for each mapping", func(t *testing.T) {
		if len(counts) != 3 {
			t.Errorf("map length = %d, want 3 (one per mapping)", len(counts))
		}
	})

	t.Run("All counts are zero", func(t *testing.T) {
		for hash, count := range counts {
			if count != 0 {
				t.Errorf("counts[%q] = %d, want 0", hash, count)
			}
		}
	})

	t.Run("Contains hash for app=web", func(t *testing.T) {
		hash := SelectorHash(map[string]string{"app": "web"})
		if _, ok := counts[hash]; !ok {
			t.Errorf("missing expected key %q for app=web selector", hash)
		}
	})

	t.Run("Contains hash for app=api,env=prod", func(t *testing.T) {
		hash := SelectorHash(map[string]string{"app": "api", "env": "prod"})
		if _, ok := counts[hash]; !ok {
			t.Errorf("missing expected key %q for app=api,env=prod selector", hash)
		}
	})

	t.Run("Contains hash for empty selector", func(t *testing.T) {
		hash := SelectorHash(map[string]string{})
		if _, ok := counts[hash]; !ok {
			t.Errorf("missing expected key %q for empty selector", hash)
		}
	})
}
