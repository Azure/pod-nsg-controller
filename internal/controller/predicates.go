package controller

import (
	"reflect"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// PodEventPredicate returns a predicate that filters pod events.
// Passes: create, delete, label changes, IP changes.
// Blocks: status-only updates without label/IP change.
func PodEventPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, okOld := e.ObjectOld.(*corev1.Pod)
			newPod, okNew := e.ObjectNew.(*corev1.Pod)
			if !okOld || !okNew {
				return true
			}
			// Pass if labels changed
			if !reflect.DeepEqual(oldPod.Labels, newPod.Labels) {
				return true
			}
			// Pass if PodIP changed
			if oldPod.Status.PodIP != newPod.Status.PodIP {
				return true
			}
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

// MappingEventPredicate returns a predicate that filters PodASGMapping events.
// Passes: create, delete, spec changes, finalizer changes, deletion timestamp changes.
// Blocks: status-only updates.
func MappingEventPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldMapping, okOld := e.ObjectOld.(*v1alpha1.PodASGMapping)
			newMapping, okNew := e.ObjectNew.(*v1alpha1.PodASGMapping)
			if !okOld || !okNew {
				return true
			}
			// Pass if spec changed
			if !equality.Semantic.DeepEqual(oldMapping.Spec, newMapping.Spec) {
				return true
			}
			// Pass if finalizers changed
			if !equality.Semantic.DeepEqual(oldMapping.GetFinalizers(), newMapping.GetFinalizers()) {
				return true
			}
			// Pass if deletion timestamp changed
			oldDT := oldMapping.GetDeletionTimestamp()
			newDT := newMapping.GetDeletionTimestamp()
			if (oldDT == nil) != (newDT == nil) {
				return true
			}
			if oldDT != nil && newDT != nil && !oldDT.Equal(newDT) {
				return true
			}
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}
