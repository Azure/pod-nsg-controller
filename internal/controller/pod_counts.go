package controller

import (
	"context"
	"fmt"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ClientPodCountSource lists pods via a Kubernetes client and returns selector-hash counts.
type ClientPodCountSource struct {
	client client.Client
}

// NewClientPodCountSource creates a pod counter backed by a Kubernetes client.
func NewClientPodCountSource(c client.Client) *ClientPodCountSource {
	return &ClientPodCountSource{client: c}
}

// CountBySelectorHash lists pods in the namespace and counts matches per selector.
func (s *ClientPodCountSource) CountBySelectorHash(ctx context.Context, namespace string, spec v1alpha1.PodASGMappingSpec) (map[string]int, error) {
	var podList corev1.PodList
	if err := s.client.List(ctx, &podList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing pods in namespace %s: %w", namespace, err)
	}
	return ComputePodCountsFromPods(spec, podList.Items), nil
}

// ComputePodCountsFromPods counts pods matching each mapping's selector.
// Keys are SelectorHash values. Uses model.CompileSelector semantics.
// Counts by labels only — PodIP is not required.
func ComputePodCountsFromPods(spec v1alpha1.PodASGMappingSpec, pods []corev1.Pod) map[string]int {
	counts := make(map[string]int, len(spec.Mappings))

	for _, mapping := range spec.Mappings {
		hash := SelectorHash(mapping.PodSelector.MatchLabels)
		selector, _ := model.CompileSelector(mapping.PodSelector)

		count := 0
		for i := range pods {
			if selector.Matches(labels.Set(pods[i].Labels)) {
				count++
			}
		}
		counts[hash] = count
	}

	return counts
}

// ZeroPodCountsForSpec returns a map with all selector hashes for the spec set to zero.
func ZeroPodCountsForSpec(spec v1alpha1.PodASGMappingSpec) map[string]int {
	counts := make(map[string]int, len(spec.Mappings))
	for _, mapping := range spec.Mappings {
		hash := SelectorHash(mapping.PodSelector.MatchLabels)
		counts[hash] = 0
	}
	return counts
}
