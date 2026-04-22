package model

import (
	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/labels"
)

// CompileSelector compiles a PodSelector into a labels.Selector.
// Empty or nil MatchLabels returns labels.Everything() (matches all pods).
func CompileSelector(sel v1alpha1.PodSelector) (labels.Selector, error) {
	if len(sel.MatchLabels) == 0 {
		return labels.Everything(), nil
	}
	return labels.SelectorFromSet(labels.Set(sel.MatchLabels)), nil
}
