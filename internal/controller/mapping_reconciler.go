package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	MappingCleanupFinalizer = "networking.azure.com/pod-asg-cleanup"
	OwnedTargetsAnnotation  = "networking.azure.com/owned-targets"
)

// ActionExecutor executes engine actions against Azure.
type ActionExecutor interface {
	Execute(ctx context.Context, actions []engine.Action) []azure.ActionResult
}

// StatusPlaceholderUpdater captures the Phase 6 status-update seam.
type StatusPlaceholderUpdater func(context.Context, *v1alpha1.PodASGMapping, []azure.ActionResult) error

// MappingReconciler reconciles PodASGMapping objects.
type MappingReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	ClusterName    string
	ResyncInterval time.Duration
	Factory        azure.AddressPrefixSetClientFactory
	Executor       ActionExecutor
	StatusUpdater  StatusPlaceholderUpdater
}

// Reconcile handles a single reconcile request for a PodASGMapping.
func (r *MappingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("mapping", req.NamespacedName)

	var mapping v1alpha1.PodASGMapping
	if err := r.Get(ctx, req.NamespacedName, &mapping); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching mapping: %w", err)
	}

	// Deletion path
	if !mapping.DeletionTimestamp.IsZero() {
		logger.V(1).Info("reconciling deleting mapping")
		if err := r.reconcileDeleting(ctx, &mapping); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Active path
	if err := r.ensureFinalizer(ctx, req.NamespacedName); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileActive(ctx, &mapping); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}

func (r *MappingReconciler) reconcileActive(ctx context.Context, mapping *v1alpha1.PodASGMapping) error {
	// Re-fetch mapping to get latest state after finalizer addition
	var current v1alpha1.PodASGMapping
	if err := r.Get(ctx, types.NamespacedName{Name: mapping.Name, Namespace: mapping.Namespace}, &current); err != nil {
		return fmt.Errorf("re-fetching mapping: %w", err)
	}
	mapping = &current

	// List namespace pods
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(mapping.Namespace)); err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}

	// Compute desired state from single mapping + namespace pods
	desired := engine.ComputeDesiredState(r.ClusterName, []v1alpha1.PodASGMapping{*mapping}, podList.Items)

	// Remove targets with empty IP sets — these should be deleted, not updated to empty
	for target, dps := range desired {
		if len(dps.IPs) == 0 {
			delete(desired, target)
		}
	}

	// Parse persisted owned-target annotation
	var persistedTargets []OwnedTarget
	if raw, ok := mapping.Annotations[OwnedTargetsAnnotation]; ok && raw != "" {
		var err error
		persistedTargets, err = ParseOwnedTargets(raw)
		if err != nil {
			return fmt.Errorf("parsing owned targets annotation: %w", err)
		}
	}

	// Compute desired targets
	desiredTargets := OwnedTargetsFromDesired(desired)

	// Active reconcile must read both the currently desired targets and any
	// previously persisted owned targets so ComputeDiff can still discover
	// delete candidates after pods or mapping rules stop matching.
	readScopeTargets := UnionOwnedTargets(desiredTargets, persistedTargets)

	// Convert to ASGTargets for fetching actual state
	asgTargets, err := OwnedTargetsToASGTargets(readScopeTargets)
	if err != nil {
		return fmt.Errorf("converting owned targets: %w", err)
	}

	// Fetch actual state
	actual, err := r.collectActualState(ctx, asgTargets)
	if err != nil {
		return fmt.Errorf("collecting actual state: %w", err)
	}

	// Compute diff
	actions := engine.ComputeDiff(desired, actual)

	if len(actions) == 0 {
		// No changes needed, update annotation if necessary
		if err := r.updateOwnedTargets(ctx, types.NamespacedName{Name: mapping.Name, Namespace: mapping.Namespace}, desiredTargets); err != nil {
			return fmt.Errorf("updating owned targets: %w", err)
		}
		if err := r.runStatusPlaceholderUpdate(ctx, mapping, nil); err != nil {
			return fmt.Errorf("updating status placeholder: %w", err)
		}
		return nil
	}

	// Execute actions
	results := r.Executor.Execute(ctx, actions)

	// Check for failures
	for _, result := range results {
		if !result.Success {
			return fmt.Errorf("action %s on %s/%s failed: %w",
				result.Action.Kind, result.Action.Target.ASGName, result.Action.Target.PrefixSetName, result.Err)
		}
	}

	// Persist annotation as canonical desired-only targets
	if err := r.updateOwnedTargets(ctx, types.NamespacedName{Name: mapping.Name, Namespace: mapping.Namespace}, desiredTargets); err != nil {
		return fmt.Errorf("updating owned targets: %w", err)
	}

	if err := r.runStatusPlaceholderUpdate(ctx, mapping, results); err != nil {
		return fmt.Errorf("updating status placeholder: %w", err)
	}

	return nil
}

func (r *MappingReconciler) reconcileDeleting(ctx context.Context, mapping *v1alpha1.PodASGMapping) error {
	if !controllerutil.ContainsFinalizer(mapping, MappingCleanupFinalizer) {
		return nil
	}

	// Parse specTargets from mapping
	specTargets, err := OwnedTargetsFromSpec(r.ClusterName, mapping)
	if err != nil {
		return fmt.Errorf("computing spec targets: %w", err)
	}

	// Parse persistedTargets from annotation
	var persistedTargets []OwnedTarget
	if raw, ok := mapping.Annotations[OwnedTargetsAnnotation]; ok && raw != "" {
		persistedTargets, err = ParseOwnedTargets(raw)
		if err != nil {
			return fmt.Errorf("parsing owned targets annotation for deletion: %w", err)
		}
	}

	// cleanupTargets = specTargets ∪ persistedTargets
	cleanupTargets := UnionOwnedTargets(specTargets, persistedTargets)

	// For each target, delete the prefix set
	for _, target := range cleanupTargets {
		if err := r.deleteTargetPrefixSet(ctx, target); err != nil {
			return fmt.Errorf("deleting prefix set for target %s/%s: %w", target.ResourceID, target.PrefixSetName, err)
		}
	}

	// Remove finalizer
	key := types.NamespacedName{Name: mapping.Name, Namespace: mapping.Namespace}
	return r.removeFinalizer(ctx, key)
}

func (r *MappingReconciler) deleteTargetPrefixSet(ctx context.Context, target OwnedTarget) error {
	parsed, err := model.ParseASGResourceID(target.ResourceID)
	if err != nil {
		return fmt.Errorf("parsing ASG resource ID: %w", err)
	}

	azClient, err := r.Factory.ForSubscription(parsed.SubscriptionID)
	if err != nil {
		return fmt.Errorf("getting client for subscription %s: %w", parsed.SubscriptionID, err)
	}

	deleteSetName := target.PrefixSetName

	// Fast path: a direct GET can recover Azure's case-preserved name without
	// listing every prefix set under the ASG.
	prefixSet, getErr := azClient.Get(ctx, parsed.SubscriptionID, parsed.ResourceGroup, parsed.ASGName, target.PrefixSetName)
	if getErr == nil {
		if prefixSet != nil && prefixSet.Name != nil && *prefixSet.Name != "" {
			deleteSetName = *prefixSet.Name
		}
	} else if !azure.IsNotFound(getErr) {
		return fmt.Errorf("getting prefix set: %w", getErr)
	} else {
		// Fallback: when GET cannot resolve the name, list once to recover the
		// case-preserved value or confirm the prefix set is already gone.
		prefixSets, listErr := azClient.List(ctx, parsed.SubscriptionID, parsed.ResourceGroup, parsed.ASGName)
		if listErr != nil {
			return fmt.Errorf("listing prefix sets: %w", listErr)
		}

		found := false
		for _, ps := range prefixSets {
			if ps.Name != nil && strings.EqualFold(*ps.Name, target.PrefixSetName) {
				deleteSetName = *ps.Name
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}

	// Delete the prefix set (tolerate NotFound)
	if err := azClient.Delete(ctx, parsed.SubscriptionID, parsed.ResourceGroup, parsed.ASGName, deleteSetName); err != nil {
		if !azure.IsNotFound(err) {
			return fmt.Errorf("deleting prefix set: %w", err)
		}
	}

	return nil
}

func (r *MappingReconciler) ensureFinalizer(ctx context.Context, key types.NamespacedName) error {
	var mapping v1alpha1.PodASGMapping
	if err := r.Get(ctx, key, &mapping); err != nil {
		return fmt.Errorf("getting mapping for finalizer: %w", err)
	}
	if controllerutil.ContainsFinalizer(&mapping, MappingCleanupFinalizer) {
		return nil
	}
	controllerutil.AddFinalizer(&mapping, MappingCleanupFinalizer)
	if err := r.Update(ctx, &mapping); err != nil {
		return fmt.Errorf("adding finalizer: %w", err)
	}
	return nil
}

func (r *MappingReconciler) removeFinalizer(ctx context.Context, key types.NamespacedName) error {
	var mapping v1alpha1.PodASGMapping
	if err := r.Get(ctx, key, &mapping); err != nil {
		return fmt.Errorf("getting mapping for finalizer removal: %w", err)
	}
	if !controllerutil.ContainsFinalizer(&mapping, MappingCleanupFinalizer) {
		return nil
	}
	controllerutil.RemoveFinalizer(&mapping, MappingCleanupFinalizer)
	if err := r.Update(ctx, &mapping); err != nil {
		return fmt.Errorf("removing finalizer: %w", err)
	}
	return nil
}

func (r *MappingReconciler) collectActualState(ctx context.Context, targets []engine.ASGTarget) (map[engine.ASGTarget]engine.ActualPrefixSet, error) {
	actual := make(map[engine.ASGTarget]engine.ActualPrefixSet)

	for _, target := range targets {
		azClient, err := r.Factory.ForSubscription(target.SubscriptionID)
		if err != nil {
			return nil, fmt.Errorf("getting client for subscription %s: %w", target.SubscriptionID, err)
		}

		result, err := azClient.Get(ctx, target.SubscriptionID, target.ResourceGroup, target.ASGName, target.PrefixSetName)
		if err != nil {
			if azure.IsNotFound(err) {
				// Absent — don't add to actual map
				continue
			}
			return nil, fmt.Errorf("getting prefix set %s/%s: %w", target.ASGName, target.PrefixSetName, err)
		}

		ips := make(map[string]struct{})
		if result != nil && result.Properties != nil {
			for _, ip := range result.Properties.AddressPrefixes {
				ips[ip] = struct{}{}
			}
		}
		actual[target] = engine.ActualPrefixSet{IPs: ips}
	}

	return actual, nil
}

func (r *MappingReconciler) updateOwnedTargets(ctx context.Context, key types.NamespacedName, targets []OwnedTarget) error {
	var mapping v1alpha1.PodASGMapping
	if err := r.Get(ctx, key, &mapping); err != nil {
		return fmt.Errorf("getting mapping for annotation update: %w", err)
	}

	var annotationValue string
	if len(targets) > 0 {
		data, err := json.Marshal(CanonicalizeOwnedTargets(targets))
		if err != nil {
			return fmt.Errorf("marshaling owned targets: %w", err)
		}
		annotationValue = string(data)
	}

	if mapping.Annotations == nil {
		mapping.Annotations = make(map[string]string)
	}

	if mapping.Annotations[OwnedTargetsAnnotation] == annotationValue {
		return nil
	}

	if annotationValue == "" {
		delete(mapping.Annotations, OwnedTargetsAnnotation)
	} else {
		mapping.Annotations[OwnedTargetsAnnotation] = annotationValue
	}

	if err := r.Update(ctx, &mapping); err != nil {
		return fmt.Errorf("updating owned targets annotation: %w", err)
	}
	return nil
}

func noopStatusPlaceholderUpdate(_ context.Context, _ *v1alpha1.PodASGMapping, _ []azure.ActionResult) error {
	// Phase 6 insertion point
	return nil
}

func (r *MappingReconciler) runStatusPlaceholderUpdate(ctx context.Context, mapping *v1alpha1.PodASGMapping, results []azure.ActionResult) error {
	updater := r.StatusUpdater
	if updater == nil {
		updater = noopStatusPlaceholderUpdate
	}
	return updater(ctx, mapping, results)
}
