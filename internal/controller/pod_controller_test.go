package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/pod-nsg-controller/internal/config"
)

func TestGetDesiredASGs_LabelOnly(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				config.LabelASG: "frontend",
			},
		},
	}

	asgs := getDesiredASGs(pod)
	if len(asgs) != 1 || asgs[0] != "frontend" {
		t.Errorf("expected [frontend], got %v", asgs)
	}
}

func TestGetDesiredASGs_AnnotationOnly(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				config.AnnotationASGs: "asg-frontend, asg-shared-services",
			},
		},
	}

	asgs := getDesiredASGs(pod)
	if len(asgs) != 2 {
		t.Fatalf("expected 2 ASGs, got %d: %v", len(asgs), asgs)
	}
	if asgs[0] != "asg-frontend" || asgs[1] != "asg-shared-services" {
		t.Errorf("expected [asg-frontend, asg-shared-services], got %v", asgs)
	}
}

func TestGetDesiredASGs_LabelAndAnnotation(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				config.LabelASG: "frontend",
			},
			Annotations: map[string]string{
				config.AnnotationASGs: "asg-frontend,asg-shared",
			},
		},
	}

	asgs := getDesiredASGs(pod)
	if len(asgs) != 3 {
		t.Fatalf("expected 3 ASGs, got %d: %v", len(asgs), asgs)
	}
}

func TestGetDesiredASGs_Deduplication(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				config.LabelASG: "frontend",
			},
			Annotations: map[string]string{
				config.AnnotationASGs: "frontend,backend",
			},
		},
	}

	asgs := getDesiredASGs(pod)
	if len(asgs) != 2 {
		t.Fatalf("expected 2 ASGs (deduplicated), got %d: %v", len(asgs), asgs)
	}
}

func TestGetDesiredASGs_Empty(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{},
	}

	asgs := getDesiredASGs(pod)
	if len(asgs) != 0 {
		t.Errorf("expected empty ASGs, got %v", asgs)
	}
}

func TestDeduplicate(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected int
	}{
		{"no duplicates", []string{"a", "b", "c"}, 3},
		{"with duplicates", []string{"a", "b", "a", "c", "b"}, 3},
		{"all same", []string{"x", "x", "x"}, 1},
		{"empty", []string{}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := deduplicate(tt.input)
			if len(result) != tt.expected {
				t.Errorf("expected %d items, got %d: %v", tt.expected, len(result), result)
			}
		})
	}
}
