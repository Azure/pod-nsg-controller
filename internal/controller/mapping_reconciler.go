package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	pkgerrors "github.com/pkg/errors"
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
	"github.com/Azure/pod-nsg-controller/internal/metrics"
	"github.com/Azure/pod-nsg-controller/internal/model"
)

// Executor abstracts Azure action execution for testability.
type Executor interface {
	Execute(ctx context.Context, actions []engine.Action) []azure.ActionResult
}

// StatusUpdater abstracts status updates for the reconcile loop.
type StatusUpdater interface {
	UpdatePending(ctx context.Context, key types.NamespacedName, observedGeneration int64, prefixSetName string, matchedPodsByIndex []int) error
	UpdateAfterReconcile(ctx context.Context, key types.NamespacedName, observedGeneration int64, prefixSetName string, results []azure.ActionResult, reconcileErr error, validationIssues []ValidationIssue, matchedPodsByIndex []int) error
}

// MappingReconciler reconciles PodASGMapping objects.
type MappingReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	ClusterName             string
	DefaultSubscription     string
	DefaultResourceGroup    string
	ResyncInterval          time.Duration
	MaxConcurrentReconciles int
	MinReconcileInterval    time.Duration

	// AzureReadSem is a shared semaphore that caps total in-flight Azure GET
	// calls across all concurrent reconciles. Initialised from
	// config.MaxConcurrentAzureReads in cmd/main.go.
	AzureReadSem chan struct{}

	PrefixSetFactory azure.AddressPrefixSetClientFactory
	Executor         Executor
	StatusUpdater    StatusUpdater

	// Metrics instrumentation (all optional; nil disables the metric path).
	MetricsRecorder      *metrics.Recorder
	PodChurnTracker      *metrics.PodChurnTracker
	ConvergenceTracker   *metrics.ConvergenceTracker
	InitialTracker       *metrics.InitialReconcileTracker

	// lastReconcileState tracks per-key debounce state for Phase 3.
	lastReconcileState sync.Map
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
	reconcileStart := time.Now()
	logger := log.FromContext(ctx).WithValues(
		"mapping", req.Name,
		"namespace", req.Namespace,
	)

	var mapping v1alpha1.PodASGMapping
	if err := r.Get(ctx, req.NamespacedName, &mapping); err != nil {
		if apierrors.IsNotFound(err) {
			r.markInitialTerminal(req.NamespacedName, true)
			r.cleanupPerMappingMetricState(req.NamespacedName)
			r.observeReconcile(req, ReconcileStageMappingNotFound, reconcileStart, 0)
			return ctrl.Result{}, nil
		}
		result, retErr, metricStage := r.finalizeSystemError(ctx, req, nil, "", nil, "fetch-mapping", err, false, logger)
		r.observeReconcile(req, metricStage, reconcileStart, 0)
		return result, retErr
	}

	logger = logger.WithValues(
		"generation", mapping.Generation,
		"deleting", mapping.DeletionTimestamp != nil,
	)

	ownershipKey := model.OwnershipKey(r.ClusterName, mapping.Namespace, mapping.Name)

	// Prune convergence tokens from older generations that can never be
	// committed. This prevents unbounded growth of pending state under
	// repeated spec churn where an older generation's reconcile has already
	// exited but its tokens were never cleaned up.
	if r.ConvergenceTracker != nil {
		r.ConvergenceTracker.PruneStaleGenerations(req.NamespacedName, mapping.Generation)
	}

	// Handle deletion: clean up owned prefix sets and remove finalizer.
	if mapping.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(&mapping, CleanupFinalizer) {
			if err := r.reconcileDelete(ctx, &mapping, ownershipKey, logger); err != nil {
				result, retErr, metricStage := r.finalizeSystemError(ctx, req, &mapping, ownershipKey, nil, "delete-cleanup", err, false, logger)
				r.observeReconcile(req, metricStage, reconcileStart, 0)
				return result, retErr
			}
			// Remove CleanupFinalizer on success.
			patch := client.MergeFrom(mapping.DeepCopy())
			controllerutil.RemoveFinalizer(&mapping, CleanupFinalizer)
			if err := r.Patch(ctx, &mapping, patch); err != nil {
				result, retErr, metricStage := r.finalizeSystemError(ctx, req, &mapping, ownershipKey, nil, "delete-remove-finalizer", err, false, logger)
				r.observeReconcile(req, metricStage, reconcileStart, 0)
				return result, retErr
			}
			r.markInitialTerminal(req.NamespacedName, true)
			r.cleanupPerMappingMetricState(req.NamespacedName)
			r.observeReconcile(req, ReconcileStageDeleteComplete, reconcileStart, 0)
			return ctrl.Result{}, nil
		}
		r.markInitialTerminal(req.NamespacedName, true)
		r.cleanupPerMappingMetricState(req.NamespacedName)
		r.observeReconcile(req, ReconcileStageDeleteComplete, reconcileStart, 0)
		return ctrl.Result{}, nil
	}

	// Ensure finalizer. If added, return immediately with Requeue: true.
	added, err := r.ensureFinalizer(ctx, &mapping)
	if err != nil {
		result, retErr, metricStage := r.finalizeSystemError(ctx, req, &mapping, ownershipKey, nil, "ensure-finalizer", err, false, logger)
		r.observeReconcile(req, metricStage, reconcileStart, 0)
		return result, retErr
	}
	if added {
		r.observeReconcile(req, ReconcileStageFinalizerAddEarlyReturn, reconcileStart, 0)
		return ctrl.Result{Requeue: true}, nil
	}

	// Phase 3: Debounce check — skip expensive work if the same generation
	// was successfully reconciled recently. Placed after ensureFinalizer so
	// that cheap invariants (finalizer presence) are always enforced.
	if remaining, shouldDebounce := r.debounceRemaining(req.NamespacedName, mapping.Generation, reconcileStart); shouldDebounce {
		r.observeReconcile(req, ReconcileStageDebounced, reconcileStart, 0)
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	// List pods in mapping namespace.
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(mapping.Namespace)); err != nil {
		result, retErr, metricStage := r.finalizeSystemError(ctx, req, &mapping, ownershipKey, nil, "list-pods", err, true, logger)
		r.observeReconcile(req, metricStage, reconcileStart, 0)
		return result, retErr
	}

	// Compute matched pods per mapping index.
	matchedPodsByIndex := ComputeMatchedPodsByMapping(mapping.Spec, podList.Items)

	// Validate ASG resource IDs.
	validationIssues := validateASGResourceIDs(mapping.Spec.Mappings)
	if len(validationIssues) > 0 {
		validationErr := aggregateValidationErrors(validationIssues)
		logger.Error(validationErr, "spec validation failed, waiting for spec update")
		if r.StatusUpdater != nil {
			statusErr := r.StatusUpdater.UpdateAfterReconcile(ctx, req.NamespacedName, mapping.Generation, ownershipKey, nil, validationErr, validationIssues, matchedPodsByIndex)
			if statusErr != nil {
				if errors.Is(statusErr, ErrStatusObjectNotFound) {
					r.cleanupPerMappingMetricState(req.NamespacedName)
					r.markInitialTerminal(req.NamespacedName, true)
					r.observeReconcile(req, ReconcileStageStatusSentinelTerminal, reconcileStart, 0)
					return ctrl.Result{}, nil
				}
				if errors.Is(statusErr, ErrStatusStaleGeneration) {
					r.pruneConvergenceForGeneration(req.NamespacedName, mapping.Generation)
					r.observeReconcile(req, ReconcileStageStatusSentinelTerminal, reconcileStart, 0)
					return ctrl.Result{}, nil
				}
				logger.Error(statusErr, "failed to update status for validation failure")
			}
		}
		r.markInitialTerminal(req.NamespacedName, true)
		r.observeReconcile(req, ReconcileStageValidationTerminal, reconcileStart, 0)
		return ctrl.Result{}, nil
	}

	// Write pending status before Azure operations.
	if r.StatusUpdater != nil {
		if pendingErr := r.StatusUpdater.UpdatePending(ctx, req.NamespacedName, mapping.Generation, ownershipKey, matchedPodsByIndex); pendingErr != nil {
			if errors.Is(pendingErr, ErrStatusObjectNotFound) {
				r.cleanupPerMappingMetricState(req.NamespacedName)
				r.markInitialTerminal(req.NamespacedName, true)
				r.observeReconcile(req, ReconcileStageStatusSentinelTerminal, reconcileStart, 0)
				return ctrl.Result{}, nil
			}
			if errors.Is(pendingErr, ErrStatusStaleGeneration) {
				r.pruneConvergenceForGeneration(req.NamespacedName, mapping.Generation)
				r.observeReconcile(req, ReconcileStageStatusSentinelTerminal, reconcileStart, 0)
				return ctrl.Result{}, nil
			}
			logger.Error(pendingErr, "failed to update pending status, continuing")
		}
	}

	// Compute desired state for this mapping using the snapshot-aware path.
	// The snapshot provides the authoritative pod set for both NSG reconciliation
	// and pod-churn metrics, eliminating a duplicate selector walk.
	crdResolutionStart := time.Now()
	desired, podSnapshot := engine.ComputeDesiredStateWithSnapshot(r.ClusterName, []v1alpha1.PodASGMapping{mapping}, podList.Items)

	// Observe pod churn from the same authoritative snapshot.
	r.observePodChurnFromSnapshot(req.NamespacedName, podSnapshot)

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
		result, retErr, metricStage := r.finalizeSystemError(ctx, req, &mapping, ownershipKey, matchedPodsByIndex, "parse-owned-annotation", err, true, logger)
		r.observeReconcile(req, metricStage, reconcileStart, 0)
		return result, retErr
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

	// Capture CRD resolution duration before Azure reads. This metric measures
	// the time to resolve a CRD update into desired-state targets, excluding
	// Azure GET latency which would make it misleading under load.
	crdResolutionDuration := time.Since(crdResolutionStart)

	// Fetch actual state from Azure.
	actual, err := r.listActualForTargets(ctx, allTargets)
	if err != nil {
		result, retErr, metricStage := r.finalizeSystemError(ctx, req, &mapping, ownershipKey, matchedPodsByIndex, "list-actual-state", err, true, logger)
		r.observeReconcile(req, metricStage, reconcileStart, 0)
		return result, retErr
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

	// Record CRD resolution metric with the pre-Azure-read duration.
	if r.MetricsRecorder != nil {
		op := ClassifyCRDResolutionOperation(actions)
		r.MetricsRecorder.Reconcile.ObserveCRDResolution(req.Namespace, req.Name, op, crdResolutionDuration)
	}

	logger.V(1).Info("computed diff", "actionCount", len(actions))

	// Detect drift: actual state has IPs not in desired state.
	// Only counted when a correcting action is emitted, not on every poll.
	r.detectAndRecordDrift(desired, actual, actions)

	// Start convergence tracking for targets with changes.
	// Use reconcileStart as the detection time: the reconcile was triggered by
	// a pod IP change, so the entire reconcile duration (including Azure reads
	// and diff computation) is part of the end-to-end convergence latency.
	r.startConvergenceTracking(req.NamespacedName, mapping.Generation, actions, desired, actual, reconcileStart)

	// Pre-update ownership annotation before executing Azure actions. This
	// ensures the resourceVersion bump (from adding newly-desired targets)
	// completes before Azure state becomes visible, preventing races with
	// external spec updates that observe the Azure result.
	preRefs := UpdateOwnedASGsAfterResults(ownedRefs, desired, nil)
	if ownershipAnnotationChanged(ownedRefs, preRefs) {
		patch := client.MergeFrom(mapping.DeepCopy())
		if err := StoreOwnedASGs(&mapping, preRefs); err != nil {
			result, retErr, metricStage := r.finalizeSystemError(ctx, req, &mapping, ownershipKey, matchedPodsByIndex, "store-owned-pre", err, true, logger)
			r.observeReconcile(req, metricStage, reconcileStart, 0)
			return result, retErr
		}
		if err := r.Patch(ctx, &mapping, patch); err != nil {
			result, retErr, metricStage := r.finalizeSystemError(ctx, req, &mapping, ownershipKey, matchedPodsByIndex, "patch-owned-pre", err, true, logger)
			r.observeReconcile(req, metricStage, reconcileStart, 0)
			return result, retErr
		}
	}

	var results []azure.ActionResult
	if len(actions) > 0 {
		results = r.Executor.Execute(ctx, actions)
	}

	// Record per-action outcomes.
	if r.MetricsRecorder != nil {
		for _, res := range results {
			// Skip no-ops: a 412-retry recompute that found the target already
			// converged is not an ARM mutation and must not inflate action counts.
			if res.NoOp {
				continue
			}
			resultLabel := "success"
			if !res.Success {
				resultLabel = "failure"
			}
			// Use FinalActionKind for the label so recomputed Create<->Update
			// transitions are recorded under the terminal operation.
			kind := res.FinalActionKind
			if kind == "" {
				kind = res.Action.Kind
			}
			r.MetricsRecorder.Convergence.RecordPrefixSetAction(actionKindToOperationLabel(kind), resultLabel)
		}
	}

	// Stage convergence for successful ARM actions.
	r.stageConvergenceResults(req.NamespacedName, mapping.Generation, results)

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
				result, retErr, metricStage := r.finalizeSystemError(ctx, req, &mapping, ownershipKey, matchedPodsByIndex, "store-owned-post", err, true, logger, results)
				r.observeReconcile(req, metricStage, reconcileStart, 0)
				return result, retErr
			}
			if err := r.Patch(ctx, &mapping, patch); err != nil {
				result, retErr, metricStage := r.finalizeSystemError(ctx, req, &mapping, ownershipKey, matchedPodsByIndex, "patch-owned-post", err, true, logger, results)
				r.observeReconcile(req, metricStage, reconcileStart, 0)
				return result, retErr
			}
		}
	}

	// Write final status after Azure operations.
	var statusErr error
	if r.StatusUpdater != nil {
		statusErr = r.StatusUpdater.UpdateAfterReconcile(ctx, req.NamespacedName, mapping.Generation, ownershipKey, results, reconcileErr, nil, matchedPodsByIndex)
		if statusErr != nil {
			if errors.Is(statusErr, ErrStatusObjectNotFound) {
				r.cleanupPerMappingMetricState(req.NamespacedName)
				r.markInitialTerminal(req.NamespacedName, true)
				r.observeReconcile(req, ReconcileStageStatusSentinelTerminal, reconcileStart, 0)
				return ctrl.Result{}, nil
			}
			if errors.Is(statusErr, ErrStatusStaleGeneration) {
				r.pruneConvergenceForGeneration(req.NamespacedName, mapping.Generation)
				r.observeReconcile(req, ReconcileStageStatusSentinelTerminal, reconcileStart, 0)
				return ctrl.Result{}, nil
			}
		}
	}

	// Status write errors take precedence over action-failure requeue
	// decisions so they are never masked (Phase 7 contract).
	if statusErr != nil {
		result, retErr := r.finalizeStatusWriteError(statusErr, logger)
		r.observeReconcile(req, ReconcileStageStatusWriteError, reconcileStart, len(actions))
		return result, retErr
	}

	// Use policy-driven requeue for action failures.
	if reconcileErr != nil {
		summary := ClassifyActionResults(results)
		policy := DefaultRequeuePolicy(r.ResyncInterval)
		result := DecideRequeueFromActionSummary(summary, policy)
		logger.V(0).Info("action failures handled via policy",
			"decisionSource", "action-summary",
			"requeueAfter", result.RequeueAfter,
		)
		stage := ReconcileStageActionMixedFailure
		if summary.NonRetriableCount+summary.RetriableCount == len(results) {
			stage = ReconcileStageActionAllFailure
		}
		r.observeReconcile(req, stage, reconcileStart, len(actions))
		return result, nil
	}

	// Mark initial-reconcile terminal on steady-state success.
	r.markInitialTerminal(req.NamespacedName, true)

	// When no targets exist, schedule a short follow-up while pods/IPs or informer
	// state settle instead of immediately requeueing in a tight loop.
	if len(allTargets) == 0 {
		promptWindow := promptFollowUpWindow(r.ResyncInterval)

		// Pods matched selectors but have no IPs → IP assignment event will trigger.
		if hasPendingIPPods(mapping.Spec.Mappings, podList.Items) &&
			withinPromptFollowUpWindow(mapping.CreationTimestamp.Time, promptWindow) {
			r.observeReconcile(req, ReconcileStageSteadyStateSuccess, reconcileStart, len(actions))
			return ctrl.Result{RequeueAfter: promptRequeueAfter(r.ResyncInterval)}, nil
		}
		if withinPromptFollowUpWindow(mapping.CreationTimestamp.Time, promptWindow) {
			r.observeReconcile(req, ReconcileStageSteadyStateSuccess, reconcileStart, len(actions))
			return ctrl.Result{RequeueAfter: promptRequeueAfter(r.ResyncInterval)}, nil
		}
	}

	r.markReconcileSuccess(req.NamespacedName, mapping.Generation, time.Now())
	r.observeReconcile(req, ReconcileStageSteadyStateSuccess, reconcileStart, len(actions))
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}

// observeReconcile emits reconcile duration, total counter, and actions-per-cycle metrics.
func (r *MappingReconciler) observeReconcile(req ctrl.Request, stage ReconcileMetricStage, startTime time.Time, actionCount int) {
	if r.MetricsRecorder == nil {
		return
	}
	result := ClassifyReconcileMetricResult(stage)
	duration := time.Since(startTime)
	r.MetricsRecorder.Reconcile.ObserveReconcile(req.Namespace, req.Name, result, duration)
	r.MetricsRecorder.Reconcile.ObserveActionsPerCycle(req.Namespace, req.Name, actionCount)
}

// markInitialTerminal marks a key as terminal in the initial-reconcile tracker.
// If the tracker was never initialized (e.g. startup listing failed), it
// performs a lazy fallback initialization using the K8s client so that
// reconcile-path MarkTerminal calls are not permanently ignored.
func (r *MappingReconciler) markInitialTerminal(key types.NamespacedName, terminal bool) {
	if r.InitialTracker == nil || r.MetricsRecorder == nil {
		return
	}
	if !r.InitialTracker.IsInitialized() {
		r.ensureInitialTrackerFallback()
	}
	r.InitialTracker.MarkTerminal(r.MetricsRecorder.Reconcile, key, terminal)
}

// ensureInitialTrackerFallback lazily initializes the initial-reconcile tracker
// from the reconcile goroutine when the startup Runnable failed.
func (r *MappingReconciler) ensureInitialTrackerFallback() {
	listFn := func(ctx context.Context) ([]types.NamespacedName, error) {
		var mappingList v1alpha1.PodASGMappingList
		if err := r.List(ctx, &mappingList); err != nil {
			return nil, err
		}
		keys := make([]types.NamespacedName, len(mappingList.Items))
		for i, m := range mappingList.Items {
			keys[i] = types.NamespacedName{Namespace: m.Namespace, Name: m.Name}
		}
		return keys, nil
	}
	// Best-effort: ignore error — will retry on next reconcile.
	_ = r.InitialTracker.EnsureInitialized(context.Background(), listFn)
}

// cleanupPerMappingMetricState removes all per-mapping state on terminal cleanup:
// debounce tracking, convergence trackers, pod churn windows, and per-key time
// series. This prevents unbounded cardinality growth and stale debounce state
// when PodASGMapping resources are deleted.
func (r *MappingReconciler) cleanupPerMappingMetricState(key types.NamespacedName) {
	r.clearDebounceState(key)
	if r.ConvergenceTracker != nil {
		r.ConvergenceTracker.Forget(key)
	}
	if r.PodChurnTracker != nil && r.MetricsRecorder != nil {
		r.PodChurnTracker.ForgetWithDelete(r.MetricsRecorder.PodChurn, key)
		r.MetricsRecorder.PodChurn.DeleteForMapping(key.Namespace, key.Name)
	}
	if r.MetricsRecorder != nil {
		r.MetricsRecorder.Reconcile.DeleteForMapping(key.Namespace, key.Name)
	}
}

// pruneConvergenceForGeneration removes convergence state for a specific generation
// on stale-generation exits. This prevents unbounded accumulation of pending tokens
// when repeated spec churn causes generation-scoped entries to be abandoned.
func (r *MappingReconciler) pruneConvergenceForGeneration(key types.NamespacedName, generation int64) {
	if r.ConvergenceTracker != nil {
		r.ConvergenceTracker.ForgetGeneration(key, generation)
	}
}

// observePodChurnFromSnapshot records pod churn metrics using the engine's
// authoritative PodSnapshot directly, without conversion.
func (r *MappingReconciler) observePodChurnFromSnapshot(key types.NamespacedName, snap engine.PodSnapshot) {
	if r.MetricsRecorder == nil || r.PodChurnTracker == nil {
		return
	}
	r.PodChurnTracker.ObserveSnapshot(r.MetricsRecorder.PodChurn, key, snap, time.Now())
}

// startConvergenceTracking starts tracking convergence for targets that have changes.
// detectedAt is the time the change was first detected (reconcileStart), ensuring
// convergence duration includes Azure read and diff computation time.
func (r *MappingReconciler) startConvergenceTracking(key types.NamespacedName, observedGeneration int64, actions []engine.Action, desired map[engine.ASGTarget]engine.DesiredPrefixSet, actual map[engine.ASGTarget]engine.ActualPrefixSet, detectedAt time.Time) {
	if r.ConvergenceTracker == nil {
		return
	}
	for _, action := range actions {
		delta := metrics.TargetDelta{}
		if d, ok := desired[action.Target]; ok {
			for ip := range d.IPs {
				if a, aOK := actual[action.Target]; !aOK || !containsIP(a.IPs, ip) {
					if delta.AddedIPs == nil {
						delta.AddedIPs = make(map[string]struct{})
					}
					delta.AddedIPs[ip] = struct{}{}
				}
			}
		}
		if a, ok := actual[action.Target]; ok {
			for ip := range a.IPs {
				if d, dOK := desired[action.Target]; !dOK || !containsIP(d.IPs, ip) {
					if delta.RemovedIPs == nil {
						delta.RemovedIPs = make(map[string]struct{})
					}
					delta.RemovedIPs[ip] = struct{}{}
				}
			}
		}
		if op, hasOp := metrics.ClassifyConvergenceOperation(delta); hasOp {
			r.ConvergenceTracker.StartOrKeep(key, action.Target, observedGeneration, op, detectedAt)
		}
	}
}

func containsIP(ips map[string]struct{}, ip string) bool {
	_, ok := ips[ip]
	return ok
}

// actionKindToOperationLabel maps engine.ActionKind to spec-mandated lowercase
// operation labels for prefix_set_actions_total: "create", "update", "delete".
func actionKindToOperationLabel(kind engine.ActionKind) string {
	switch kind {
	case engine.CreatePrefixSet:
		return "create"
	case engine.UpdatePrefixSet:
		return "update"
	case engine.DeletePrefixSet:
		return "delete"
	default:
		return "unknown"
	}
}

// stageConvergenceResults stages successful ARM actions for convergence measurement.
// Each result carries its own CompletedAt timestamp captured at execution time.
// No-ops (412-retry recompute found target already converged) are excluded.
func (r *MappingReconciler) stageConvergenceResults(key types.NamespacedName, observedGeneration int64, results []azure.ActionResult) {
	if r.ConvergenceTracker == nil {
		return
	}
	for _, res := range results {
		if !res.Success || res.NoOp {
			continue
		}
		// Use FinalActionKind for the convergence operation label.
		kind := res.FinalActionKind
		if kind == "" {
			kind = res.Action.Kind
		}
		op := metrics.ConvergenceOpUpdate
		switch kind {
		case engine.CreatePrefixSet:
			op = metrics.ConvergenceOpAdd
		case engine.DeletePrefixSet:
			op = metrics.ConvergenceOpDelete
		}
		completedAt := res.CompletedAt
		if completedAt.IsZero() {
			completedAt = time.Now()
		}
		r.ConvergenceTracker.StageSuccessfulAction(key, res.Action.Target, observedGeneration, op, completedAt)
	}
}

// detectAndRecordDrift increments prefix_set_drift_corrections_total only for
// targets where actual state has stale IPs (not in desired) AND a correcting
// action was emitted by the diff engine. This ensures the counter increments
// once per correction cycle, not on every poll while drift persists.
func (r *MappingReconciler) detectAndRecordDrift(desired map[engine.ASGTarget]engine.DesiredPrefixSet, actual map[engine.ASGTarget]engine.ActualPrefixSet, actions []engine.Action) {
	if r.MetricsRecorder == nil {
		return
	}
	// Build set of targets with correcting actions (Update or Delete).
	correctedTargets := make(map[engine.ASGTarget]struct{}, len(actions))
	for _, a := range actions {
		if a.Kind == engine.UpdatePrefixSet || a.Kind == engine.DeletePrefixSet {
			correctedTargets[a.Target] = struct{}{}
		}
	}
	for target, actualPS := range actual {
		if _, corrected := correctedTargets[target]; !corrected {
			continue
		}
		desiredPS, inDesired := desired[target]
		for ip := range actualPS.IPs {
			if !inDesired || !containsIP(desiredPS.IPs, ip) {
				r.MetricsRecorder.Convergence.IncrementDriftCorrections(target)
				break
			}
		}
	}
}

// ensureFinalizer adds the cleanup finalizer if not present.
func (r *MappingReconciler) ensureFinalizer(ctx context.Context, m *v1alpha1.PodASGMapping) (bool, error) {
	if controllerutil.ContainsFinalizer(m, CleanupFinalizer) {
		return false, nil
	}
	patch := client.MergeFrom(m.DeepCopy())
	controllerutil.AddFinalizer(m, CleanupFinalizer)
	if err := r.Patch(ctx, m, patch); err != nil {
		return false, fmt.Errorf("adding finalizer: %w", err)
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
		return fmt.Errorf("parsing owned annotation during delete: %w", err)
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
	if len(targets) == 0 {
		return make(map[engine.ASGTarget]engine.ActualPrefixSet), nil
	}

	// Build deterministic target slice for stable ordering.
	targetList := make([]engine.ASGTarget, 0, len(targets))
	for t := range targets {
		targetList = append(targetList, t)
	}
	sort.Slice(targetList, func(i, j int) bool {
		return engine.TargetKey(targetList[i]) < engine.TargetKey(targetList[j])
	})

	// Resolve subscription clients (sequential; typically 1 subscription).
	clientsBySubscription := make(map[string]azure.AddressPrefixSetAPI)
	failedSubscriptions := make(map[string]error)
	var subErrors []listActualTargetError
	for _, t := range targetList {
		if _, ok := clientsBySubscription[t.SubscriptionID]; ok {
			continue
		}
		if _, ok := failedSubscriptions[t.SubscriptionID]; ok {
			continue
		}
		c, err := r.PrefixSetFactory.ForSubscription(t.SubscriptionID)
		if err != nil {
			failedSubscriptions[t.SubscriptionID] = err
			// Record error for all targets in this subscription.
			for _, tt := range targetList {
				if tt.SubscriptionID == t.SubscriptionID {
					subErrors = append(subErrors, listActualTargetError{
						target: tt,
						err:    pkgerrors.Wrapf(err, "getting client for subscription %s", t.SubscriptionID),
					})
				}
			}
		} else {
			clientsBySubscription[t.SubscriptionID] = c
		}
	}

	// Use the shared reconciler-wide semaphore to cap total in-flight Azure
	// reads across all concurrent reconciles. Fall back to unbounded if not set.
	sem := r.AzureReadSem

	var (
		mu       sync.Mutex
		actual   = make(map[engine.ASGTarget]engine.ActualPrefixSet)
		failures []listActualTargetError
		wg       sync.WaitGroup
	)
	failures = append(failures, subErrors...)

	for _, target := range targetList {
		apiClient, ok := clientsBySubscription[target.SubscriptionID]
		if !ok {
			// Already recorded subscription error above.
			continue
		}

		wg.Add(1)
		go func(t engine.ASGTarget, c azure.AddressPrefixSetAPI) {
			defer wg.Done()

			// Acquire semaphore with context awareness (skip if no semaphore configured).
			if sem != nil {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					mu.Lock()
					failures = append(failures, listActualTargetError{
						target: t,
						err:    pkgerrors.Wrap(ctx.Err(), "context canceled waiting for semaphore"),
					})
					mu.Unlock()
					return
				}
				defer func() { <-sem }()
			}

			ps, err := c.Get(ctx, t.SubscriptionID, t.ResourceGroup, t.ASGName, t.PrefixSetName)
			if err != nil {
				if azure.IsNotFound(err) {
					return
				}
				mu.Lock()
				failures = append(failures, listActualTargetError{
					target: t,
					err:    err,
				})
				mu.Unlock()
				return
			}

			ips := make(map[string]struct{})
			if ps.Properties != nil {
				for _, ip := range ps.Properties.AddressPrefixes {
					ips[ip] = struct{}{}
				}
			}
			mu.Lock()
			actual[t] = engine.ActualPrefixSet{IPs: ips}
			mu.Unlock()
		}(target, apiClient)
	}

	wg.Wait()

	if len(failures) == 0 {
		return actual, nil
	}
	return actual, aggregateListActualTargetErrors(failures)
}

// listActualTargetError records a per-target failure from listActualForTargets.
type listActualTargetError struct {
	target engine.ASGTarget
	err    error
}

// listActualAggregateError is the aggregated error returned when multiple
// target GETs fail. It unwraps to a policy-primary cause so that
// azure.IsRetriableARM and azure.ExtractRetryAfterHint work correctly.
type listActualAggregateError struct {
	primary error
	items   []listActualTargetError
	msg     string
}

func (e *listActualAggregateError) Error() string { return e.msg }
func (e *listActualAggregateError) Unwrap() error { return e.primary }

// aggregateListActualTargetErrors builds a deterministic aggregated error
// from per-target failures. The aggregated error unwraps to the primary cause
// selected by selectListActualPrimaryCause.
func aggregateListActualTargetErrors(items []listActualTargetError) error {
	if len(items) == 0 {
		return nil
	}

	// Sort for deterministic message.
	sort.Slice(items, func(i, j int) bool {
		return engine.TargetKey(items[i].target) < engine.TargetKey(items[j].target)
	})

	primary := selectListActualPrimaryCause(items)

	var parts []string
	for _, item := range items {
		key := engine.TargetKey(item.target)
		parts = append(parts, fmt.Sprintf("%s: %v", key, item.err))
	}
	msg := "list-actual-state failures: " + strings.Join(parts, "; ")

	return &listActualAggregateError{
		primary: primary,
		items:   items,
		msg:     msg,
	}
}

// selectListActualPrimaryCause picks the primary underlying error for policy
// classification. If any failure is retriable (429, 5xx, 408, network), select
// the retriable error with the highest RetryAfter (tie-break by target key).
// Otherwise select the first deterministic non-retriable failure.
func selectListActualPrimaryCause(items []listActualTargetError) error {
	var bestRetriable *listActualTargetError
	var bestRetryAfter time.Duration

	for i := range items {
		if azure.IsRetriableARM(items[i].err) {
			hint := azure.ExtractRetryAfterHint(items[i].err)
			if bestRetriable == nil || hint > bestRetryAfter ||
				(hint == bestRetryAfter && engine.TargetKey(items[i].target) < engine.TargetKey(bestRetriable.target)) {
				bestRetriable = &items[i]
				bestRetryAfter = hint
			}
		}
	}

	if bestRetriable != nil {
		return bestRetriable.err
	}
	// No retriable errors; use first deterministic non-retriable.
	return items[0].err
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

// validateASGResourceIDs validates all ASG resource IDs in the spec mappings.
func validateASGResourceIDs(mappings []v1alpha1.Mapping) []ValidationIssue {
	var issues []ValidationIssue
	for i, m := range mappings {
		for j, asgRef := range m.ApplicationSecurityGroups {
			if _, err := model.ParseASGResourceID(asgRef.ResourceID); err != nil {
				issues = append(issues, ValidationIssue{
					MappingIndex: i,
					ASGIndex:     j,
					ResourceID:   asgRef.ResourceID,
					Err:          err,
				})
			}
		}
	}
	return issues
}

// aggregateValidationErrors combines validation issues into a single error.
func aggregateValidationErrors(issues []ValidationIssue) error {
	if len(issues) == 0 {
		return nil
	}
	var parts []string
	for _, vi := range issues {
		parts = append(parts, fmt.Sprintf("mapping[%d].asg[%d] %q: %v", vi.MappingIndex, vi.ASGIndex, vi.ResourceID, vi.Err))
	}
	return fmt.Errorf("validation failed: %s", strings.Join(parts, "; "))
}

// ---------------------------------------------------------------------------
// Phase 3: Debounce helpers
// ---------------------------------------------------------------------------

// debounceState holds per-key debounce tracking state.
type debounceState struct {
	lastSuccessfulAt time.Time
	generation       int64
}

// debounceRemaining returns the remaining time until the next reconcile is allowed
// for the given key and generation. Returns (0, false) if no debounce is needed.
func (r *MappingReconciler) debounceRemaining(key types.NamespacedName, generation int64, now time.Time) (time.Duration, bool) {
	if r.MinReconcileInterval <= 0 {
		return 0, false
	}
	val, ok := r.lastReconcileState.Load(key)
	if !ok {
		return 0, false
	}
	state := val.(debounceState)
	if state.generation != generation {
		return 0, false
	}
	elapsed := now.Sub(state.lastSuccessfulAt)
	remaining := r.MinReconcileInterval - elapsed
	if remaining <= 0 {
		return 0, false
	}
	return remaining, true
}

// markReconcileSuccess records a successful reconcile timestamp for debounce tracking.
func (r *MappingReconciler) markReconcileSuccess(key types.NamespacedName, generation int64, now time.Time) {
	if r.MinReconcileInterval <= 0 {
		return
	}
	r.lastReconcileState.Store(key, debounceState{
		lastSuccessfulAt: now,
		generation:       generation,
	})
}

// clearDebounceState removes debounce tracking for a key (on delete/not-found).
func (r *MappingReconciler) clearDebounceState(key types.NamespacedName) {
	r.lastReconcileState.Delete(key)
}
