package controller

import (
	"context"
	stderrors "errors"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// writeEarlyPhaseStatus writes status for early reconcile phases (validation/bootstrap).
// It handles conflict errors by refetching the mapping and comparing generations.
func (r *MappingReconciler) writeEarlyPhaseStatus(
	ctx context.Context,
	key types.NamespacedName,
	mapping *v1alpha1.PodASGMapping,
	input ReconcileStatusInput,
	logger logr.Logger,
) (ctrl.Result, error) {
	statusErr := r.StatusUpdater.UpdateAfterReconcile(ctx, mapping, input)
	if statusErr == nil {
		return ctrl.Result{Requeue: true}, nil
	}

	// Check for generation drift error.
	var driftErr *StatusGenerationDriftError
	if stderrors.As(statusErr, &driftErr) {
		logger.V(1).Info("stale status write skipped",
			"processedGeneration", driftErr.ProcessedGeneration,
			"liveGeneration", driftErr.LiveGeneration,
			"phase", string(input.Phase),
			"staleStatusWriteSkipped", true,
			"requeue", true,
		)
		return ctrl.Result{Requeue: true}, nil
	}

	// Check for conflict error.
	if apierrors.IsConflict(statusErr) {
		return r.handleEarlyPhaseStatusConflict(ctx, key, input, logger)
	}

	// Non-conflict, non-drift error: propagate.
	return ctrl.Result{}, errors.Wrap(statusErr, "early-phase status write")
}

// handleEarlyPhaseStatusConflict disambiguates 409 Conflict errors for early-phase
// status writes by refetching the live object and comparing generations.
// If generation changed → stale write → requeue.
// If same generation → retry status write once.
func (r *MappingReconciler) handleEarlyPhaseStatusConflict(
	ctx context.Context,
	key types.NamespacedName,
	input ReconcileStatusInput,
	logger logr.Logger,
) (ctrl.Result, error) {
	// Refetch live mapping.
	var live v1alpha1.PodASGMapping
	if err := r.Get(ctx, key, &live); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "refetching mapping for conflict resolution")
	}

	// If generation changed, classify as stale write.
	if live.Generation != input.ProcessedGen {
		logger.V(1).Info("stale status write skipped (conflict with generation change)",
			"processedGeneration", input.ProcessedGen,
			"liveGeneration", live.Generation,
			"phase", string(input.Phase),
			"statusWriteConflict", true,
			"staleStatusWriteSkipped", true,
			"requeue", true,
		)
		return ctrl.Result{Requeue: true}, nil
	}

	// Same generation: retry status write once against live object.
	logger.V(1).Info("retrying early-phase status write after conflict",
		"phase", string(input.Phase),
		"statusWriteAttempt", 2,
		"statusWriteConflict", true,
	)

	retryErr := r.StatusUpdater.UpdateAfterReconcile(ctx, &live, input)
	if retryErr == nil {
		return ctrl.Result{Requeue: true}, nil
	}

	// Retry failed: return wrapped error.
	return ctrl.Result{}, errors.Wrap(retryErr, "early-phase status write retry")
}
