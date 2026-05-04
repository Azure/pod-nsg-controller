package controller

import (
	"context"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// MapPodToMappings maps a pod event to reconcile requests for matching PodASGMappings.
// Used with handler.EnqueueRequestsFromMapFunc for the secondary pod watch.
func (r *MappingReconciler) MapPodToMappings(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	if pod.Namespace == "" {
		return nil
	}

	if pod.Labels == nil || len(pod.Labels) == 0 {
		keys, err := r.allMappingsInNamespace(ctx, pod.Namespace)
		if err != nil {
			log.FromContext(ctx).Error(err, "listing all mappings for unlabeled pod", "namespace", pod.Namespace, "pod", types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace})
			return nil
		}
		return requestsFromNamespacedNames(keys)
	}

	keys, err := r.matchingMappingsForLabels(ctx, pod.Namespace, pod.Labels)
	if err != nil {
		log.FromContext(ctx).Error(err, "listing matching mappings for pod", "namespace", pod.Namespace, "pod", types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace})
		return nil
	}

	return requestsFromNamespacedNames(keys)
}

// matchingMappingsForLabels intentionally scans namespace-local mappings.
// In production the manager-backed client serves this list from cache, which
// keeps the behavior simple and correct; add a selector index only if this
// becomes a measured hot path.
func (r *MappingReconciler) matchingMappingsForLabels(ctx context.Context, namespace string, podLabels map[string]string) ([]types.NamespacedName, error) {
	var mappingList v1alpha1.PodASGMappingList
	if err := r.List(ctx, &mappingList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var keys []types.NamespacedName
	for i := range mappingList.Items {
		if mappingMatchesLabels(&mappingList.Items[i], podLabels) {
			keys = append(keys, types.NamespacedName{
				Name:      mappingList.Items[i].Name,
				Namespace: mappingList.Items[i].Namespace,
			})
		}
	}
	return keys, nil
}

func (r *MappingReconciler) allMappingsInNamespace(ctx context.Context, namespace string) ([]types.NamespacedName, error) {
	var mappingList v1alpha1.PodASGMappingList
	if err := r.List(ctx, &mappingList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	keys := make([]types.NamespacedName, 0, len(mappingList.Items))
	for i := range mappingList.Items {
		keys = append(keys, types.NamespacedName{
			Name:      mappingList.Items[i].Name,
			Namespace: mappingList.Items[i].Namespace,
		})
	}
	return keys, nil
}

func requestsFromNamespacedNames(keys []types.NamespacedName) []reconcile.Request {
	requests := make([]reconcile.Request, 0, len(keys))
	for _, key := range keys {
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}
	return requests
}

// mappingMatchesLabels returns true if the mapping has at least one selector that matches the given labels.
func mappingMatchesLabels(mapping *v1alpha1.PodASGMapping, podLabels map[string]string) bool {
	for _, rule := range mapping.Spec.Mappings {
		if len(rule.PodSelector.MatchLabels) == 0 {
			return true
		}
		selector := labels.SelectorFromSet(labels.Set(rule.PodSelector.MatchLabels))
		if selector.Matches(labels.Set(podLabels)) {
			return true
		}
	}
	return false
}
