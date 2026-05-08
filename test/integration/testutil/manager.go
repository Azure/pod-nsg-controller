package testutil

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// NewEnvtestManager creates a controller-runtime manager with loopback-only test listeners.
func NewEnvtestManager(t *testing.T, cfg *rest.Config, scheme *runtime.Scheme) ctrl.Manager {
	t.Helper()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	return mgr
}
