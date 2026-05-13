package controller

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
)

// finalizeSystemError centralizes all system-error exits from Reconcile.
// It derives a policy-driven ctrl.Result and optionally writes a best-effort
// status update. If the status write itself fails with a non-sentinel error,
// that error takes precedence for the requeue decision so it is not silently
// masked. It always returns (result, nil) — never a raw error.
func (r *MappingReconciler) finalizeSystemError(
	ctx context.Context,
	req ctrl.Request,
	mapping *v1alpha1.PodASGMapping,
	ownershipKey string,
	matchedPodsByIndex []int,
	stage string,
	systemErr error,
	allowStatusWrite bool,
	logger logr.Logger,
) (ctrl.Result, error) {
	policy := DefaultRequeuePolicy(r.ResyncInterval)
	result := DecideRequeueFromSystemError(systemErr, policy)

	logger.Error(systemErr, "system error during reconcile",
		"stage", stage,
		"decisionSource", "system-error",
		"requeueAfter", result.RequeueAfter,
	)

	if allowStatusWrite && r.StatusUpdater != nil && mapping != nil {
		statusErr := r.StatusUpdater.UpdateAfterReconcile(
			ctx, req.NamespacedName, mapping.Generation, ownershipKey,
			nil, systemErr, nil, matchedPodsByIndex,
		)
		if statusErr != nil {
			if errors.Is(statusErr, ErrStatusObjectNotFound) || errors.Is(statusErr, ErrStatusStaleGeneration) {
				return ctrl.Result{}, nil
			}
			// Status write errors take precedence over the system-error
			// requeue decision so they are never silently masked.
			return r.finalizeStatusWriteError(statusErr, logger)
		}
	}

	return result, nil
}

// finalizeStatusWriteError handles errors from status-update calls.
// It returns a policy-driven result without attempting another status write
// (preventing recursion).
func (r *MappingReconciler) finalizeStatusWriteError(
	statusErr error,
	logger logr.Logger,
) (ctrl.Result, error) {
	if errors.Is(statusErr, ErrStatusObjectNotFound) || errors.Is(statusErr, ErrStatusStaleGeneration) {
		return ctrl.Result{}, nil
	}

	policy := DefaultRequeuePolicy(r.ResyncInterval)
	result := DecideRequeueFromSystemError(statusErr, policy)

	logger.Error(statusErr, "status write error",
		"decisionSource", "status-write-error",
		"requeueAfter", result.RequeueAfter,
	)

	return result, nil
}
