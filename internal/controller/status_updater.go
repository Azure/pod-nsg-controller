package controller

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
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
	for attempt := 1; attempt <= u.MaxAttempts; attempt++ {
		var mapping v1alpha1.PodASGMapping
		if err := u.Client.Get(ctx, key, &mapping); err != nil {
			if apierrors.IsNotFound(err) {
				return ErrStatusObjectNotFound
			}
			return errors.Wrap(err, "fetching mapping for pending status")
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

		mapping.Status = newStatus
		if err := u.Client.Status().Update(ctx, &mapping); err != nil {
			if apierrors.IsConflict(err) && attempt < u.MaxAttempts {
				u.Logger.V(1).Info("conflict on pending status update, retrying",
					"attempt", attempt,
					"maxAttempts", u.MaxAttempts,
				)
				continue
			}
			return errors.Wrap(err, "updating pending status")
		}
		return nil
	}
	return errors.Errorf("updating pending status: max attempts exceeded")
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
	for attempt := 1; attempt <= u.MaxAttempts; attempt++ {
		var mapping v1alpha1.PodASGMapping
		if err := u.Client.Get(ctx, key, &mapping); err != nil {
			if apierrors.IsNotFound(err) {
				return ErrStatusObjectNotFound
			}
			return errors.Wrap(err, "fetching mapping for final status")
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

		mapping.Status = newStatus
		if err := u.Client.Status().Update(ctx, &mapping); err != nil {
			if apierrors.IsConflict(err) && attempt < u.MaxAttempts {
				u.Logger.V(1).Info("conflict on final status update, retrying",
					"attempt", attempt,
					"maxAttempts", u.MaxAttempts,
				)
				continue
			}
			return errors.Wrap(err, "updating final status")
		}
		return nil
	}
	return errors.Errorf("updating final status: max attempts exceeded")
}
