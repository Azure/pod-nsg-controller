package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/model"
)

// Executor abstracts Azure action execution for testability.
type Executor interface {
	Execute(ctx context.Context, actions []engine.Action) []azure.ActionResult
}

// StatusUpdater abstracts status updates (Phase 6 hook).
type StatusUpdater interface {
	UpdateAfterReconcile(
		ctx context.Context,
		mapping *v1alpha1.PodASGMapping,
		results []azure.ActionResult,
		reconcileErr error,
	) error
}

// MappingReconciler reconciles PodASGMapping objects.
type MappingReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	ClusterName          string
	DefaultSubscription  string
	DefaultResourceGroup string
	ResyncInterval       time.Duration

	PrefixSetFactory azure.AddressPrefixSetClientFactory
	Executor         Executor
	StatusUpdater    StatusUpdater
}

const (
	CleanupFinalizer       = "networking.azure.com/pod-asg-cleanup"
	OwnedASGsAnnotationKey = "networking.azure.com/owned-asgs.v1"
)

// +kubebuilder:rbac:groups=networking.azure.com,resources=podasgmappings,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=networking.azure.com,resources=podasgmappings/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=networking.azure.com,resources=podasgmappings/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile is the main reconcile loop for PodASGMapping objects.
func (r *MappingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues(
		"mapping", req.Name,
		"namespace", req.Namespace,
	)

	var mapping v1alpha1.PodASGMapping
	if err := r.Get(ctx, req.NamespacedName, &mapping); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, errors.Wrap(err, "fetching PodASGMapping")
	}

	logger = logger.WithValues(
		"generation", mapping.Generation,
		"deleting", mapping.DeletionTimestamp != nil,
	)

	ownershipKey := model.OwnershipKey(r.ClusterName, mapping.Namespace, mapping.Name)

	// Handle deletion: clean up owned prefix sets and remove finalizer.
	if mapping.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(&mapping, CleanupFinalizer) {
			if err := r.reconcileDelete(ctx, &mapping, ownershipKey, logger); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "reconcile delete")
			}
			// Remove CleanupFinalizer on success.
			patch := client.MergeFrom(mapping.DeepCopy())
			controllerutil.RemoveFinalizer(&mapping, CleanupFinalizer)
			if err := r.Patch(ctx, &mapping, patch); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "removing cleanup finalizer")
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer. If added, return immediately with Requeue: true.
	added, err := r.ensureFinalizer(ctx, &mapping)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "ensuring finalizer")
	}
	if added {
		return ctrl.Result{Requeue: true}, nil
	}

	// List pods in mapping namespace.
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(mapping.Namespace)); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "listing pods")
	}

	// Compute desired state for this mapping.
	desired := engine.ComputeDesiredState(r.ClusterName, []v1alpha1.PodASGMapping{mapping}, podList.Items)

	// Filter out targets with no desired IPs to avoid creating empty prefix sets
	// (e.g., when pods haven't received IPs yet). Owned-but-empty targets are
	// cleaned up via the ownership-based diff path.
	for target, dps := range desired {
		if len(dps.IPs) == 0 {
			delete(desired, target)
		}
	}

	logger.V(1).Info("computed desired state", "desiredTargetCount", len(desired))

	// Load ownership annotation (fail closed on parse error).
	ownedRefs, err := LoadOwnedASGs(&mapping)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "parsing owned ASGs annotation")
	}

	ownedTargets := TargetsFromOwnedASGs(ownedRefs, ownershipKey)

	// Build target set = desired targets ∪ owned targets.
	allTargets := make(map[engine.ASGTarget]struct{})
	for t := range desired {
		allTargets[t] = struct{}{}
	}
	for t := range ownedTargets {
		allTargets[t] = struct{}{}
	}

	logger.V(1).Info("target set computed",
		"ownedTargetCount", len(ownedTargets),
		"desiredTargetCount", len(desired),
	)

	// Fetch actual state from Azure.
	actual, err := r.listActualForTargets(ctx, allTargets)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "listing actual state")
	}

	// Ensure owned-but-no-longer-desired targets appear in actual so the diff
	// engine generates Delete actions for them even when Azure returns 404.
	for t := range ownedTargets {
		if _, inDesired := desired[t]; !inDesired {
			if _, inActual := actual[t]; !inActual {
				actual[t] = engine.ActualPrefixSet{IPs: make(map[string]struct{})}
			}
		}
	}

	logger.V(1).Info("fetched actual state", "actualTargetCount", len(actual))

	// Compute diff and execute.
	actions := engine.ComputeDiff(desired, actual)

	logger.V(1).Info("computed diff", "actionCount", len(actions))

	// Pre-update ownership annotation before executing Azure actions. This
	// ensures the resourceVersion bump (from adding newly-desired targets)
	// completes before Azure state becomes visible, preventing races with
	// external spec updates that observe the Azure result.
	preRefs := UpdateOwnedASGsAfterResults(ownedRefs, desired, nil)
	if ownershipAnnotationChanged(ownedRefs, preRefs) {
		patch := client.MergeFrom(mapping.DeepCopy())
		if err := StoreOwnedASGs(&mapping, preRefs); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "storing owned ASGs annotation")
		}
		if err := r.Patch(ctx, &mapping, patch); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "patching ownership annotation")
		}
	}

	var results []azure.ActionResult
	if len(actions) > 0 {
		results = r.Executor.Execute(ctx, actions)
	}

	// Aggregate failures.
	reconcileErr := aggregateActionFailures(results)
	if reconcileErr != nil {
		failCount := 0
		for _, res := range results {
			if !res.Success {
				failCount++
			}
		}
		logger.Error(reconcileErr, "some actions failed",
			"actionCount", len(actions),
			"failedActionCount", failCount,
		)
	}

	// Post-update annotation for successful deletions (remove targets no longer owned).
	if len(results) > 0 {
		postRefs := UpdateOwnedASGsAfterResults(preRefs, desired, results)
		if ownershipAnnotationChanged(preRefs, postRefs) {
			patch := client.MergeFrom(mapping.DeepCopy())
			if err := StoreOwnedASGs(&mapping, postRefs); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "storing post-action owned ASGs")
			}
			if err := r.Patch(ctx, &mapping, patch); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "patching post-action ownership annotation")
			}
		}
	}

	// Refetch mapping after metadata patch before optional status update.
	if r.StatusUpdater != nil {
		if err := r.Get(ctx, types.NamespacedName{Name: mapping.Name, Namespace: mapping.Namespace}, &mapping); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "refetching mapping after patch")
		}
		if err := r.StatusUpdater.UpdateAfterReconcile(ctx, &mapping, results, reconcileErr); err != nil {
			logger.Error(err, "failed to update status")
		}
	}

	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}

	// When no targets exist, schedule a short follow-up while pods/IPs or informer
	// state settle instead of immediately requeueing in a tight loop.
	if len(allTargets) == 0 {
		promptWindow := promptFollowUpWindow(r.ResyncInterval)

		// Pods matched selectors but have no IPs → IP assignment event will trigger.
		if hasPendingIPPods(mapping.Spec.Mappings, podList.Items) &&
			withinPromptFollowUpWindow(mapping.CreationTimestamp.Time, promptWindow) {
			return ctrl.Result{RequeueAfter: promptRequeueAfter(r.ResyncInterval)}, nil
		}
		// Mapping was recently created and cache may not have synced pods yet.
		// Use a short follow-up delay to give informer state time to catch up
		// without creating immediate reconcile churn.
		if withinPromptFollowUpWindow(mapping.CreationTimestamp.Time, promptWindow) {
			return ctrl.Result{RequeueAfter: promptRequeueAfter(r.ResyncInterval)}, nil
		}
	}

	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}

// ensureFinalizer adds the cleanup finalizer if not present.
func (r *MappingReconciler) ensureFinalizer(ctx context.Context, m *v1alpha1.PodASGMapping) (bool, error) {
	if controllerutil.ContainsFinalizer(m, CleanupFinalizer) {
		return false, nil
	}
	patch := client.MergeFrom(m.DeepCopy())
	controllerutil.AddFinalizer(m, CleanupFinalizer)
	if err := r.Patch(ctx, m, patch); err != nil {
		return false, errors.Wrap(err, "adding finalizer")
	}
	return true, nil
}

// deleteCleanupError records a single cleanup operation failure.
type deleteCleanupError struct {
	SubscriptionID string
	ResourceGroup  string
	ASGName        string
	Op             string // forSubscription | list | delete
	PrefixSetName  string // for delete op
	Err            error
}

// aggregateDeleteCleanupErrors combines cleanup errors into a single error.
func aggregateDeleteCleanupErrors(items []deleteCleanupError) error {
	if len(items) == 0 {
		return nil
	}
	var parts []string
	for _, e := range items {
		if e.PrefixSetName != "" {
			parts = append(parts, fmt.Sprintf("%s %s/%s/%s/%s: %v", e.Op, e.SubscriptionID, e.ResourceGroup, e.ASGName, e.PrefixSetName, e.Err))
		} else {
			parts = append(parts, fmt.Sprintf("%s %s/%s/%s: %v", e.Op, e.SubscriptionID, e.ResourceGroup, e.ASGName, e.Err))
		}
	}
	return fmt.Errorf("cleanup errors: %s", strings.Join(parts, "; "))
}

// reconcileDelete handles the deletion path: clean up owned prefix sets and remove finalizer.
func (r *MappingReconciler) reconcileDelete(ctx context.Context, m *v1alpha1.PodASGMapping, ownershipKey string, logger logr.Logger) error {
	// Parse ownership annotation for cleanup candidates.
	ownedRefs, err := LoadOwnedASGs(m)
	if err != nil {
		// Owned annotation present but corrupt → return error immediately, keep finalizer.
		return errors.Wrap(err, "parsing owned annotation during delete")
	}

	// Build candidate targets.
	candidates := make(map[engine.ASGTarget]struct{})

	if ownedRefs != nil {
		// Owned annotation present + valid → use owned refs.
		for t := range TargetsFromOwnedASGs(ownedRefs, ownershipKey) {
			candidates[t] = struct{}{}
		}
	} else {
		// Owned annotation absent → fallback to spec-derived candidates.
		for _, rule := range m.Spec.Mappings {
			for _, asgRef := range rule.ApplicationSecurityGroups {
				parsed, parseErr := model.ParseASGResourceID(asgRef.ResourceID)
				if parseErr != nil {
					continue
				}
				candidates[engine.ASGTarget{
					SubscriptionID: parsed.SubscriptionID,
					ResourceGroup:  parsed.ResourceGroup,
					ASGName:        parsed.ASGName,
					FullResourceID: parsed.FullResourceID,
					PrefixSetName:  ownershipKey,
				}] = struct{}{}
			}
		}
	}

	logger.Info("cleaning up prefix sets",
		"ownershipKey", ownershipKey,
		"candidateASGCount", len(candidates),
	)

	var cleanupErrors []deleteCleanupError
	deletedCount := 0

	for target := range candidates {
		apiClient, clientErr := r.PrefixSetFactory.ForSubscription(target.SubscriptionID)
		if clientErr != nil {
			cleanupErrors = append(cleanupErrors, deleteCleanupError{
				SubscriptionID: target.SubscriptionID,
				ResourceGroup:  target.ResourceGroup,
				ASGName:        target.ASGName,
				Op:             "forSubscription",
				Err:            clientErr,
			})
			continue
		}

		prefixSets, listErr := apiClient.List(ctx, target.SubscriptionID, target.ResourceGroup, target.ASGName)
		if listErr != nil {
			cleanupErrors = append(cleanupErrors, deleteCleanupError{
				SubscriptionID: target.SubscriptionID,
				ResourceGroup:  target.ResourceGroup,
				ASGName:        target.ASGName,
				Op:             "list",
				Err:            listErr,
			})
			continue
		}

		for _, ps := range prefixSets {
			if PrefixSetNameEqualsOwnership(ps, ownershipKey) {
				if delErr := apiClient.Delete(ctx, target.SubscriptionID, target.ResourceGroup, target.ASGName, *ps.Name); delErr != nil {
					if !azure.IsNotFound(delErr) {
						cleanupErrors = append(cleanupErrors, deleteCleanupError{
							SubscriptionID: target.SubscriptionID,
							ResourceGroup:  target.ResourceGroup,
							ASGName:        target.ASGName,
							Op:             "delete",
							PrefixSetName:  *ps.Name,
							Err:            delErr,
						})
					}
				} else {
					deletedCount++
				}
			}
		}
	}

	logger.Info("cleanup complete",
		"deletedPrefixSetCount", deletedCount,
		"cleanupErrorCount", len(cleanupErrors),
	)

	// If any cleanup errors, return aggregated error; caller retains finalizer.
	if aggErr := aggregateDeleteCleanupErrors(cleanupErrors); aggErr != nil {
		return aggErr
	}

	return nil
}

// listActualForTargets fetches actual prefix set state from Azure for all targets.
func (r *MappingReconciler) listActualForTargets(ctx context.Context, targets map[engine.ASGTarget]struct{}) (map[engine.ASGTarget]engine.ActualPrefixSet, error) {
	actual := make(map[engine.ASGTarget]engine.ActualPrefixSet)
	clientsBySubscription := make(map[string]azure.AddressPrefixSetAPI)

	for target := range targets {
		apiClient, ok := clientsBySubscription[target.SubscriptionID]
		if !ok {
			var err error
			apiClient, err = r.PrefixSetFactory.ForSubscription(target.SubscriptionID)
			if err != nil {
				return nil, errors.Wrapf(err, "getting client for subscription %s", target.SubscriptionID)
			}
			clientsBySubscription[target.SubscriptionID] = apiClient
		}

		ps, err := apiClient.Get(ctx, target.SubscriptionID, target.ResourceGroup, target.ASGName, target.PrefixSetName)
		if err != nil {
			if azure.IsNotFound(err) {
				continue
			}
			return nil, errors.Wrapf(err, "getting prefix set %s/%s/%s/%s",
				target.SubscriptionID, target.ResourceGroup, target.ASGName, target.PrefixSetName)
		}

		ips := make(map[string]struct{})
		if ps.Properties != nil {
			for _, ip := range ps.Properties.AddressPrefixes {
				ips[ip] = struct{}{}
			}
		}
		actual[target] = engine.ActualPrefixSet{IPs: ips}
	}

	return actual, nil
}

// aggregateActionFailures combines failures from action results into a single error.
func aggregateActionFailures(results []azure.ActionResult) error {
	var errs []string
	for _, r := range results {
		if !r.Success && r.Err != nil {
			errs = append(errs, fmt.Sprintf("%s %s: %v", r.Action.Kind, r.Action.Target.ASGName, r.Err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("action failures: %s", strings.Join(errs, "; "))
}

// hasPendingIPPods returns true if any pod matches a mapping selector but lacks a PodIP.
func hasPendingIPPods(rules []v1alpha1.Mapping, pods []corev1.Pod) bool {
	for _, rule := range rules {
		selector, err := model.CompileSelector(rule.PodSelector)
		if err != nil {
			continue
		}
		for i := range pods {
			if selector.Matches(labels.Set(pods[i].Labels)) && pods[i].Status.PodIP == "" {
				return true
			}
		}
	}
	return false
}

func promptRequeueAfter(resyncInterval time.Duration) time.Duration {
	const defaultDelay = time.Second

	if resyncInterval <= 0 {
		return defaultDelay
	}
	if resyncInterval <= 2*defaultDelay {
		return resyncInterval
	}
	return defaultDelay
}

func promptFollowUpWindow(resyncInterval time.Duration) time.Duration {
	const maxWindow = 15 * time.Second

	if resyncInterval <= 0 {
		return 0
	}
	if resyncInterval < maxWindow {
		return resyncInterval
	}
	return maxWindow
}

func withinPromptFollowUpWindow(createdAt time.Time, promptWindow time.Duration) bool {
	if promptWindow <= 0 || createdAt.IsZero() {
		return false
	}
	return time.Since(createdAt) < promptWindow
}
