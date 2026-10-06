//go:build e2e

package e2e_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestValidateControllerContinuity(t *testing.T) {
	const containerName = "manager"
	baselinePods := []corev1.Pod{
		controllerPod("controller-1", "uid-1", containerName, 0),
		controllerPod("controller-2", "uid-2", containerName, 1),
	}
	baseline, err := captureControllerContinuity(baselinePods, containerName)
	if err != nil {
		t.Fatalf("capture baseline: %v", err)
	}

	tests := []struct {
		name    string
		pods    []corev1.Pod
		wantErr string
	}{
		{
			name: "stable state",
			pods: []corev1.Pod{
				controllerPod("controller-1", "uid-1", containerName, 0),
				controllerPod("controller-2", "uid-2", containerName, 1),
			},
		},
		{
			name: "increased restart count",
			pods: []corev1.Pod{
				controllerPod("controller-1", "uid-1", containerName, 1),
				controllerPod("controller-2", "uid-2", containerName, 1),
			},
			wantErr: "restart count changed from 0 to 1",
		},
		{
			name: "replacement pod",
			pods: []corev1.Pod{
				controllerPod("controller-1", "uid-replacement", containerName, 0),
				controllerPod("controller-2", "uid-2", containerName, 1),
			},
			wantErr: "was replaced",
		},
		{
			name: "missing pod",
			pods: []corev1.Pod{
				controllerPod("controller-1", "uid-1", containerName, 0),
			},
			wantErr: "controller pod controller-2 (UID uid-2) is missing",
		},
		{
			name: "extra pod",
			pods: []corev1.Pod{
				controllerPod("controller-1", "uid-1", containerName, 0),
				controllerPod("controller-2", "uid-2", containerName, 1),
				controllerPod("controller-3", "uid-3", containerName, 0),
			},
			wantErr: "unexpected controller pod controller-3 (UID uid-3)",
		},
		{
			name: "missing configured container",
			pods: []corev1.Pod{
				controllerPod("controller-1", "uid-1", "other", 0),
				controllerPod("controller-2", "uid-2", containerName, 1),
			},
			wantErr: "missing container status for manager",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateControllerContinuity(baseline, tt.pods, containerName)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate continuity: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate continuity succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate continuity error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestCaptureControllerContinuityRequiresConfiguredContainer(t *testing.T) {
	_, err := captureControllerContinuity(
		[]corev1.Pod{controllerPod("controller-1", "uid-1", "other", 0)},
		"manager",
	)
	if err == nil {
		t.Fatal("capture baseline succeeded without configured container status")
	}
	if !strings.Contains(err.Error(), "missing container status for manager") {
		t.Fatalf("capture baseline error = %q", err)
	}
}

func controllerPod(name, uid, containerName string, restartCount int32) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: releaseControllerNamespace,
			UID:       types.UID(uid),
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         containerName,
				RestartCount: restartCount,
			}},
		},
	}
}
