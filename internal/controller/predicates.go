package controller

import (
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
)

// PodPredicate returns a predicate that filters pod events.
// Create/Delete: true. Update: only if labels or podIP changed. Generic: false.
func PodPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return true },
		DeleteFunc: func(e event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, ok1 := e.ObjectOld.(*corev1.Pod)
			newPod, ok2 := e.ObjectNew.(*corev1.Pod)
			if !ok1 || !ok2 {
				return false
			}
			return podLabelOrIPChanged(oldPod, newPod)
		},
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}
}

// MappingPredicate returns a predicate that filters PodASGMapping events.
// Create/Delete: true. Update: only if spec, finalizers, or deletion timestamp changed.
func MappingPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return true },
		DeleteFunc: func(e event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldM, ok1 := e.ObjectOld.(*v1alpha1.PodASGMapping)
			newM, ok2 := e.ObjectNew.(*v1alpha1.PodASGMapping)
			if !ok1 || !ok2 {
				return false
			}
			return mappingSpecOrLifecycleChanged(oldM, newM)
		},
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}
}

// podLabelOrIPChanged returns true if labels or podIP changed.
func podLabelOrIPChanged(oldPod, newPod *corev1.Pod) bool {
	if oldPod.Status.PodIP != newPod.Status.PodIP {
		return true
	}
	if !reflect.DeepEqual(oldPod.Labels, newPod.Labels) {
		return true
	}
	return false
}

// mappingSpecOrLifecycleChanged returns true if spec, finalizers, or deletion timestamp changed.
func mappingSpecOrLifecycleChanged(oldM, newM *v1alpha1.PodASGMapping) bool {
	if !reflect.DeepEqual(oldM.Spec, newM.Spec) {
		return true
	}
	if !reflect.DeepEqual(oldM.Finalizers, newM.Finalizers) {
		return true
	}
	oldDel := oldM.DeletionTimestamp
	newDel := newM.DeletionTimestamp
	if (oldDel == nil) != (newDel == nil) {
		return true
	}
	if oldDel != nil && newDel != nil && !oldDel.Equal(newDel) {
		return true
	}
	return false
}
