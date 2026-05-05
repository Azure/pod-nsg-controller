package controller

import (
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// ---------------------------------------------------------------------------
// T5.3 / T5.4 predicate: PodPredicate
// ---------------------------------------------------------------------------

// TestPodPredicate_PodIPChangeTriggers (T5.3)
// Spec: Pod IP changes → mapping is enqueued (predicate allows update)
func TestPodPredicate_PodIPChangeTriggers(t *testing.T) {
	pred := PodPredicate()

	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.2"},
	}

	ue := event.UpdateEvent{
		ObjectOld: oldPod,
		ObjectNew: newPod,
	}

	if !pred.Update(ue) {
		t.Error("T5.3: PodPredicate should allow update when podIP changes")
	}
}

// TestPodPredicate_LabelChangeTriggers (T5.4)
// Spec: Pod label changes (no longer matches) → predicate allows update
func TestPodPredicate_LabelChangeTriggers(t *testing.T) {
	pred := PodPredicate()

	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "ns",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "ns",
			Labels:    map[string]string{"app": "other"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	ue := event.UpdateEvent{
		ObjectOld: oldPod,
		ObjectNew: newPod,
	}

	if !pred.Update(ue) {
		t.Error("T5.4: PodPredicate should allow update when labels change")
	}
}

// TestPodPredicate_StatusOnlyUpdateFiltered (T5.9)
// Spec: Pod status-only update (no label/IP change) → NOT triggered
func TestPodPredicate_StatusOnlyUpdateFiltered(t *testing.T) {
	pred := PodPredicate()

	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "ns",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
			Phase: corev1.PodRunning,
		},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "ns",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
			Phase: corev1.PodSucceeded, // only status.phase changed
		},
	}

	ue := event.UpdateEvent{
		ObjectOld: oldPod,
		ObjectNew: newPod,
	}

	if pred.Update(ue) {
		t.Error("T5.9: PodPredicate should filter out status-only updates (no label or IP change)")
	}
}

// TestPodPredicate_CreateAllowed
// Pods: create → true
func TestPodPredicate_CreateAllowed(t *testing.T) {
	pred := PodPredicate()
	ce := event.CreateEvent{
		Object: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns"},
		},
	}
	if !pred.Create(ce) {
		t.Error("PodPredicate should allow create events")
	}
}

// TestPodPredicate_DeleteAllowed
// Pods: delete → true
func TestPodPredicate_DeleteAllowed(t *testing.T) {
	pred := PodPredicate()
	de := event.DeleteEvent{
		Object: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns"},
		},
	}
	if !pred.Delete(de) {
		t.Error("PodPredicate should allow delete events")
	}
}

// TestPodPredicate_GenericFiltered
// Pods: generic → false
func TestPodPredicate_GenericFiltered(t *testing.T) {
	pred := PodPredicate()
	ge := event.GenericEvent{
		Object: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns"},
		},
	}
	if pred.Generic(ge) {
		t.Error("PodPredicate should filter generic events")
	}
}

// ---------------------------------------------------------------------------
// T5.5 / T5.6 predicate: MappingPredicate
// ---------------------------------------------------------------------------

// TestMappingPredicate_SpecChangeTriggers (T5.5, T5.6)
// Spec: PodASGMapping spec updated → update allowed
func TestMappingPredicate_SpecChangeTriggers(t *testing.T) {
	pred := MappingPredicate()

	oldMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "ns"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
			},
		},
	}
	newMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "ns"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
					},
				},
			},
		},
	}

	ue := event.UpdateEvent{
		ObjectOld: oldMapping,
		ObjectNew: newMapping,
	}

	if !pred.Update(ue) {
		t.Error("T5.5/T5.6: MappingPredicate should allow update when spec changes")
	}
}

// TestMappingPredicate_StatusOnlyUpdateFiltered
// Spec: Status-only update → NOT triggered
func TestMappingPredicate_StatusOnlyUpdateFiltered(t *testing.T) {
	pred := MappingPredicate()

	oldMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "ns", Generation: 1},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
			},
		},
		Status: v1alpha1.PodASGMappingStatus{MappingCount: 0},
	}
	newMapping := oldMapping.DeepCopy()
	newMapping.Status.MappingCount = 1 // only status changed

	ue := event.UpdateEvent{
		ObjectOld: oldMapping,
		ObjectNew: newMapping,
	}

	if pred.Update(ue) {
		t.Error("MappingPredicate should filter out status-only updates")
	}
}

// TestMappingPredicate_FinalizerOrDeletionTimestampChangeTriggers
// Spec: Finalizer/deletion timestamp changes → update allowed
func TestMappingPredicate_FinalizerOrDeletionTimestampChangeTriggers(t *testing.T) {
	pred := MappingPredicate()

	t.Run("finalizer added", func(t *testing.T) {
		oldMapping := &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "ns"},
			Spec: v1alpha1.PodASGMappingSpec{
				Mappings: []v1alpha1.Mapping{
					{
						PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
						ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
					},
				},
			},
		}
		newMapping := oldMapping.DeepCopy()
		newMapping.Finalizers = []string{CleanupFinalizer}

		ue := event.UpdateEvent{
			ObjectOld: oldMapping,
			ObjectNew: newMapping,
		}

		if !pred.Update(ue) {
			t.Error("MappingPredicate should allow update when finalizers change")
		}
	})

	t.Run("deletion timestamp set", func(t *testing.T) {
		oldMapping := &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "m1",
				Namespace:  "ns",
				Finalizers: []string{CleanupFinalizer},
			},
			Spec: v1alpha1.PodASGMappingSpec{
				Mappings: []v1alpha1.Mapping{
					{
						PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
						ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
					},
				},
			},
		}
		now := metav1.Now()
		newMapping := oldMapping.DeepCopy()
		newMapping.DeletionTimestamp = &now

		ue := event.UpdateEvent{
			ObjectOld: oldMapping,
			ObjectNew: newMapping,
		}

		if !pred.Update(ue) {
			t.Error("MappingPredicate should allow update when deletion timestamp is set")
		}
	})
}

// TestMappingPredicate_CreateDeleteAllowed
func TestMappingPredicate_CreateDeleteAllowed(t *testing.T) {
	pred := MappingPredicate()

	ce := event.CreateEvent{
		Object: &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "ns"},
		},
	}
	if !pred.Create(ce) {
		t.Error("MappingPredicate should allow create events")
	}

	de := event.DeleteEvent{
		Object: &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "ns"},
		},
	}
	if !pred.Delete(de) {
		t.Error("MappingPredicate should allow delete events")
	}
}

// ---------------------------------------------------------------------------
// Internal helper tests
// ---------------------------------------------------------------------------

// TestPodLabelOrIPChanged
func TestPodLabelOrIPChanged(t *testing.T) {
	tests := []struct {
		name   string
		old    *corev1.Pod
		new    *corev1.Pod
		expect bool
	}{
		{
			name: "IP changed",
			old: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			new: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.2"},
			},
			expect: true,
		},
		{
			name: "label changed",
			old: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			new: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			expect: true,
		},
		{
			name: "no change",
			old: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			new: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			expect: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := podLabelOrIPChanged(tc.old, tc.new)
			if got != tc.expect {
				t.Errorf("podLabelOrIPChanged() = %v, want %v", got, tc.expect)
			}
		})
	}
}

// TestMappingPredicate_GenericFiltered
// Spec: MappingPredicate generic → false
func TestMappingPredicate_GenericFiltered(t *testing.T) {
	pred := MappingPredicate()
	ge := event.GenericEvent{
		Object: &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "ns"},
		},
	}
	if pred.Generic(ge) {
		t.Error("MappingPredicate should filter generic events")
	}
}

// TestPodPredicate_LabelAddedTriggers
// Edge case: label added where none existed before → predicate allows
func TestPodPredicate_LabelAddedTriggers(t *testing.T) {
	pred := PodPredicate()

	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "ns",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	ue := event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}
	if !pred.Update(ue) {
		t.Error("PodPredicate should allow update when labels are added from nil")
	}
}

// TestPodPredicate_LabelRemovedTriggers
// Edge case: all labels removed → predicate allows
func TestPodPredicate_LabelRemovedTriggers(t *testing.T) {
	pred := PodPredicate()

	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "ns",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	ue := event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}
	if !pred.Update(ue) {
		t.Error("PodPredicate should allow update when labels are removed to nil")
	}
}

// TestPodPredicate_IPAssignedFromEmpty
// Edge case: pod goes from no IP to having IP → predicate allows (T5.3 boundary)
func TestPodPredicate_IPAssignedFromEmpty(t *testing.T) {
	pred := PodPredicate()

	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: ""},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	ue := event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}
	if !pred.Update(ue) {
		t.Error("PodPredicate should allow update when pod IP transitions from empty to assigned")
	}
}

// TestPodPredicate_BothLabelAndIPChangedTriggers
// Edge case: both label and IP change simultaneously
func TestPodPredicate_BothLabelAndIPChangedTriggers(t *testing.T) {
	pred := PodPredicate()

	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns", Labels: map[string]string{"app": "api"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.2"},
	}

	ue := event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}
	if !pred.Update(ue) {
		t.Error("PodPredicate should allow update when both labels and IP change")
	}
}

// TestMappingSpecOrLifecycleChanged
func TestMappingSpecOrLifecycleChanged(t *testing.T) {
	base := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "ns"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
				},
			},
		},
	}

	t.Run("spec changed", func(t *testing.T) {
		modified := base.DeepCopy()
		modified.Spec.Mappings = append(modified.Spec.Mappings, v1alpha1.Mapping{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"}},
		})
		if !mappingSpecOrLifecycleChanged(base, modified) {
			t.Error("expected true when spec changes")
		}
	})

	t.Run("status only changed", func(t *testing.T) {
		modified := base.DeepCopy()
		modified.Status.MappingCount = 5
		if mappingSpecOrLifecycleChanged(base, modified) {
			t.Error("expected false when only status changes")
		}
	})

	t.Run("finalizer changed", func(t *testing.T) {
		modified := base.DeepCopy()
		modified.Finalizers = []string{CleanupFinalizer}
		if !mappingSpecOrLifecycleChanged(base, modified) {
			t.Error("expected true when finalizers change")
		}
	})
}
