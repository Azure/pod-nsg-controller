package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
)

var (
	ErrStatusObjectNotFound  = errors.New("status object not found")
	ErrStatusStaleGeneration = errors.New("status stale generation")
)

// MappingStatusUpdater performs status subresource updates with retry and race handling.
type MappingStatusUpdater struct {
	Client      client.Client
	Logger      logr.Logger
	Now         func() metav1.Time
	MaxAttempts int

	// convergenceCommitter is invoked after a successful status write or
	// semantic no-op to commit convergence metrics. Set via SetConvergenceCommitter.
	convergenceCommitter ConvergenceCommitter
}

// NewMappingStatusUpdater creates a MappingStatusUpdater with sensible defaults.
func NewMappingStatusUpdater(c client.Client, logger logr.Logger) *MappingStatusUpdater {
	return &MappingStatusUpdater{
		Client:      c,
		Logger:      logger,
		Now:         metav1.Now,
		MaxAttempts: 3,
	}
}

// UpdatePending writes an initial Pending status before Azure operations.
func (u *MappingStatusUpdater) UpdatePending(
	ctx context.Context,
	key types.NamespacedName,
	observedGeneration int64,
	prefixSetName string,
	matchedPodsByIndex []int,
) error {
	var lastErr error
	for attempt := 1; attempt <= u.MaxAttempts; attempt++ {
		var mapping v1alpha1.PodASGMapping
		if err := u.Client.Get(ctx, key, &mapping); err != nil {
			if apierrors.IsNotFound(err) {
				return ErrStatusObjectNotFound
			}
			return fmt.Errorf("fetching mapping for pending status: %w", err)
		}

		if mapping.Generation > observedGeneration {
			return ErrStatusStaleGeneration
		}

		newStatus := ComputeStatus(ComputeStatusInput{
			Spec:               mapping.Spec,
			PreviousStatus:     mapping.Status,
			PrefixSetName:      prefixSetName,
			MatchedPodsByIndex: matchedPodsByIndex,
			ObservedGeneration: observedGeneration,
			Phase:              StatusPhasePending,
			Now:                u.Now(),
		})

		if statusSemanticEqual(mapping.Status, newStatus) {
			return nil
		}

		mapping.Status = newStatus
		if err := u.Client.Status().Update(ctx, &mapping); err != nil {
			if apierrors.IsNotFound(err) {
				return ErrStatusObjectNotFound
			}
			lastErr = err
			if apierrors.IsConflict(err) && attempt < u.MaxAttempts {
				u.Logger.V(1).Info("conflict on pending status update, retrying",
					"attempt", attempt,
					"maxAttempts", u.MaxAttempts,
				)
				continue
			}
			return fmt.Errorf("updating pending status: %w", err)
		}
		return nil
	}
	return fmt.Errorf("updating pending status: max attempts exceeded: %w", lastErr)
}

// UpdateAfterReconcile writes the final status after Azure operations complete.
func (u *MappingStatusUpdater) UpdateAfterReconcile(
	ctx context.Context,
	key types.NamespacedName,
	observedGeneration int64,
	prefixSetName string,
	results []azure.ActionResult,
	reconcileErr error,
	validationIssues []ValidationIssue,
	matchedPodsByIndex []int,
) error {
	var lastErr error
	for attempt := 1; attempt <= u.MaxAttempts; attempt++ {
		var mapping v1alpha1.PodASGMapping
		if err := u.Client.Get(ctx, key, &mapping); err != nil {
			if apierrors.IsNotFound(err) {
				return ErrStatusObjectNotFound
			}
			return fmt.Errorf("fetching mapping for final status: %w", err)
		}

		if mapping.Generation > observedGeneration {
			return ErrStatusStaleGeneration
		}

		newStatus := ComputeStatus(ComputeStatusInput{
			Spec:               mapping.Spec,
			PreviousStatus:     mapping.Status,
			PrefixSetName:      prefixSetName,
			Results:            results,
			MatchedPodsByIndex: matchedPodsByIndex,
			ValidationIssues:   validationIssues,
			ReconcileErr:       reconcileErr,
			ObservedGeneration: observedGeneration,
			Phase:              StatusPhaseFinal,
			Now:                u.Now(),
		})

		if statusSemanticEqual(mapping.Status, newStatus) {
			u.notifyConvergence(key, observedGeneration, results, StatusWriteOutcomeNoop, nil)
			return nil
		}

		mapping.Status = newStatus
		if err := u.Client.Status().Update(ctx, &mapping); err != nil {
			if apierrors.IsNotFound(err) {
				return ErrStatusObjectNotFound
			}
			lastErr = err
			if apierrors.IsConflict(err) {
				if attempt < u.MaxAttempts {
					u.Logger.V(1).Info("conflict on final status update, retrying",
						"attempt", attempt,
						"maxAttempts", u.MaxAttempts,
					)
					continue
				}
				// Terminal conflict: break to post-loop re-fetch for stale-generation detection.
				break
			}
			u.notifyConvergence(key, observedGeneration, results, StatusWriteOutcomeError, err)
			return fmt.Errorf("updating final status: %w", err)
		}
		u.notifyConvergence(key, observedGeneration, results, StatusWriteOutcomeWritten, nil)
		return nil
	}
	// Final conflict after all attempts: re-fetch to detect generation advancement.
	if apierrors.IsConflict(lastErr) {
		var check v1alpha1.PodASGMapping
		if getErr := u.Client.Get(ctx, key, &check); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return ErrStatusObjectNotFound
			}
		} else if check.Generation > observedGeneration {
			return ErrStatusStaleGeneration
		}
	}
	u.notifyConvergence(key, observedGeneration, results, StatusWriteOutcomeError, lastErr)
	return fmt.Errorf("updating final status: max attempts exceeded: %w", lastErr)
}

// notifyConvergence invokes the convergence committer callback with the status
// write outcome. This is the instrumentation boundary for convergence metrics:
// the committer decides whether to commit based on the outcome.
func (u *MappingStatusUpdater) notifyConvergence(
	key types.NamespacedName,
	observedGeneration int64,
	results []azure.ActionResult,
	outcome StatusWriteOutcome,
	statusErr error,
) {
	if u.convergenceCommitter != nil {
		u.convergenceCommitter(key, observedGeneration, results, outcome, statusErr)
	}
}
