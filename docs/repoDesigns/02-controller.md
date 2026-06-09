# 02 — Controller Design

> Package: `internal/controller` · Core reconciliation loop and supporting components

## Overview

The controller package is the heart of the system. It contains the `MappingReconciler` (the main reconcile loop), status management, pod-to-mapping event mapping, watch predicates, ownership tracking, and error classification.

## Component Map

```
internal/controller/
├── conditions.go            # Condition types, reasons, sync states
├── error_policy.go          # finalizeSystemError(), finalizeStatusWriteError()
├── errors.go                # Error classification and requeue decisions
├── initial_reconcile_initializer.go # InitialReconcileInitializer manager runnable
├── mapping_reconciler.go    # MappingReconciler.Reconcile() — main loop
├── ownership_store.go       # Owned ASG annotation read/write
├── pod_controller.go        # Legacy PodReconciler (unused)
├── pod_handler.go           # PodToMappingEventHandler — pod→mapping enqueue
├── predicates.go            # PodPredicate(), MappingPredicate()
├── reconcile_metrics.go     # Reconcile/CRD metric result classification
├── setup.go                 # SetupWithManager() — controller-runtime wiring
├── status.go                # ComputeStatus() — builds status from inputs
├── status_updater.go        # MappingStatusUpdater — status writes with retry
└── status_updater_convergence_stub.go # Status-write convergence callback hook
```

## MappingReconciler

### Struct

```go
type MappingReconciler struct {
    client.Client
    Scheme *runtime.Scheme

    ClusterName          string
    DefaultSubscription  string
    DefaultResourceGroup string
    ResyncInterval       time.Duration
    PatchThresholdPercent int

    PrefixSetFactory azure.AddressPrefixSetClientFactory
    Executor         Executor
    StatusUpdater    StatusUpdater

    DesiredStateCache  *engine.DesiredStateCache
    MetricsRecorder    *metrics.Recorder
    ConvergenceTracker *metrics.ConvergenceTracker
    InitialTracker     *metrics.InitialReconcileTracker
}
```

### Interfaces

```go
type Executor interface {
    Execute(ctx context.Context, actions []engine.Action) []azure.ActionResult
}

type StatusUpdater interface {
    UpdatePending(ctx context.Context, key types.NamespacedName,
        observedGeneration int64, prefixSetName string,
        matchedPodsByIndex []int) error
    UpdateAfterReconcile(ctx context.Context, key types.NamespacedName,
        observedGeneration int64, prefixSetName string,
        results []azure.ActionResult, reconcileErr error,
        validationIssues []ValidationIssue,
        matchedPodsByIndex []int) error
}
```

Both interfaces are optional (`StatusUpdater` can be nil) for testability.

### Reconcile Flow

```
Reconcile(ctx, req)
│
├─ 1. Fetch PodASGMapping ── not found → return nil (deleted)
│
├─ 2. Deleting?
│     └─ Has finalizer → reconcileDelete() → remove finalizer → return
│
├─ 3. Ensure finalizer ── added → return Requeue:true
│
├─ 4. List pods in namespace
│
├─ 5. Compute matched pods per mapping rule
│     └─ ComputeMatchedPodsByMapping(spec, pods)
│
├─ 6. Validate ASG resource IDs
│     └─ Invalid → log, write status, return nil (no requeue)
│
├─ 7. Write Pending status (if StatusUpdater != nil)
│
├─ 8. Compute desired state
│     └─ engine.ComputeDesiredState(clusterName, mappings, pods)
│     └─ Filter out targets with no desired IPs
│
├─ 9. Load ownership annotation → union with desired → all targets
│
├─10. Fetch actual state from Azure (listActualForTargets)
│
├─11. Compute diff → engine.ComputeDiff(desired, actual)
│
├─12. Pre-update ownership annotation (add desired targets)
│
├─13. Execute actions → Executor.Execute(ctx, actions)
│
├─14. Post-update ownership (remove successful deletes)
│
├─15. Write final status (Synced/Error per mapping row)
│
└─16. Return RequeueAfter(resyncInterval) or error-based requeue
```

### Deletion Path

```
reconcileDelete(ctx, mapping, ownershipKey, logger)
│
├─ Parse ownership annotation
│   └─ Corrupt → return error (keep finalizer)
│
├─ Build cleanup candidates
│   ├─ From annotation (if present)
│   └─ From spec (fallback)
│
├─ For each candidate ASG:
│   ├─ List prefix sets
│   └─ Delete matching ownership key
│       └─ 404 → non-fatal (idempotent)
│
└─ Aggregate cleanup errors → return (keep finalizer on failure)
```

## Status Updater

### `MappingStatusUpdater`

```go
type MappingStatusUpdater struct {
    Client      client.Client
    Logger      logr.Logger
    Now         func() metav1.Time
    MaxAttempts int               // default: 3
}
```

Both `UpdatePending` and `UpdateAfterReconcile` follow the same pattern:

1. **Fetch** the latest `PodASGMapping` from the API server
2. **Reject stale generation** — if `mapping.Generation > observedGeneration`, return `ErrStatusStaleGeneration`
3. **Compute** new status via `ComputeStatus()`
4. **Short-circuit** if existing status is semantically equal (`statusSemanticEqual`)
5. **Write** via `Status().Update()` with conflict retry
6. **Track last error** for inclusion in max-attempts-exceeded message

### `ComputeStatus()`

Builds the complete `PodASGMappingStatus` from inputs:

- Sets `Accepted` condition based on validation issues
- Sets `Reconciled` condition based on phase (Pending/Final) and results
- Builds per-mapping `MappingStatus` rows with `SelectorHash`, `MatchedPods`, `ASGSyncState`
- Preserves `LastSyncTime` from previous status using selector-hash-based matching

### `statusSemanticEqual()`

Compares two status values ignoring condition `LastTransitionTime` fields. Prevents unnecessary `resourceVersion` churn when the computed status is identical to what's already stored.

## Predicates

### `PodPredicate()`

Fires on:
- Pod create/delete
- Pod label changes
- `PodIP` changes

Does NOT fire on annotations, spec, or other status changes.

### `MappingPredicate()`

Fires on:
- Spec changes (`Generation` change)
- Finalizer changes
- `DeletionTimestamp` set

Does NOT fire on status-only updates.

## Pod-to-Mapping Event Handler

`PodToMappingEventHandler` maps pod events to `PodASGMapping` reconcile requests:

1. Lists all `PodASGMapping` resources in the pod's namespace
2. For each mapping, checks if any selector matches the pod's labels
3. Enqueues matching mappings for reconciliation

Includes a cache keyed by namespace + resource version to avoid redundant list calls.

`setup.go` selects the cache-aware pod handler when `DesiredStateCache` is enabled; otherwise it uses the default handler.

## Ownership Store

The ownership annotation `networking.azure.com/owned-asgs.v1` stores a JSON array of `OwnedASGRef`:

```go
type OwnedASGRef struct {
    SubscriptionID string `json:"s"`
    ResourceGroup  string `json:"r"`
    ASGName        string `json:"a"`
}
```

Functions:
- `LoadOwnedASGs()` — parse annotation
- `StoreOwnedASGs()` — write annotation
- `UpdateOwnedASGsAfterResults()` — add desired targets, remove successful deletes
- `TargetsFromOwnedASGs()` — convert refs to `engine.ASGTarget`
- `PrefixSetNameEqualsOwnership()` — case-insensitive name match for cleanup

## Error Policy

`error_policy.go` centralizes post-reconcile error handling:

- `(*MappingReconciler).finalizeSystemError(...)` — logs the system error, computes a policy-driven requeue result, and optionally performs a best-effort final status write
- `(*MappingReconciler).finalizeStatusWriteError(...)` — classifies status-write failures and applies the same requeue policy without recursing into another status update

These helpers keep error → requeue behavior consistent across reconcile exit paths.

## Reconcile Metrics Classification

`reconcile_metrics.go` classifies reconcile outcomes for Prometheus metric emission:

- `ReconcileMetricStage` — enum for reconcile exit paths such as `finalizer-add-early-return`, `validation-terminal`, `steady-state-success`, `system-error`, and `status-write-error`
- `InitialTerminalReason` — reason an object became terminal for initial-reconcile tracking (`steady-state`, `validation`, `delete-complete`, `stale-generation`, `status-not-found`, `mapping-not-found`)
- `ClassifyReconcileMetricResult(stage)` — maps reconcile stages to metric labels such as success, requeue, partial_failure, or error
- `ClassifyCRDResolutionOperation(actions)` — classifies CRD resolution work as `add`, `delete`, or `update`

## Initial Reconcile Initializer

`InitialReconcileInitializer` is a `manager.Runnable` that seeds startup reconciliation tracking:

- `NewInitialReconcileInitializer(...)` constructs the runnable from a reader, tracker, reconcile recorder, and logger
- `Start(ctx)` lists existing `PodASGMapping` objects, initializes the `InitialReconcileTracker`, and immediately emits completion if the startup set is empty
- `NeedLeaderElection() bool` returns `true`, so only the leader performs the startup initialization path

`cmd/main.go` registers it with the manager so initial-reconcile metrics are initialized before steady-state reconciliation proceeds.

## Convergence Commit Callback

`status_updater_convergence_stub.go` provides the hook used to commit convergence metrics after status resolution:

- `StatusWriteOutcome` — status write result enum (`written`, `noop`, `error`)
- `ConvergenceCommitter` — callback signature invoked with the key, observed generation, action results, write outcome, and any status error
- `SetConvergenceCommitter()` — injects the callback into `MappingStatusUpdater`

The status updater calls `notifyConvergence()` after `UpdateAfterReconcile()` completes so the metrics subsystem can commit or discard staged convergence observations.

## Wiring

```go
func (r *MappingReconciler) SetupWithManager(mgr ctrl.Manager) error {
    var podHandler handler.EventHandler
    if r.DesiredStateCache != nil {
        podHandler = NewPodToMappingEventHandlerWithCache(...)
    } else {
        podHandler = NewPodToMappingEventHandler(...)
    }

    opts := controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}
    if r.MetricsRecorder != nil {
        opts.NewQueue = metrics.NewInstrumentedQueueFactory(r.MetricsRecorder.Reconcile)
    }

    return ctrl.NewControllerManagedBy(mgr).
        Named(name).
        WithOptions(opts).
        For(&PodASGMapping{}, WithPredicates(MappingPredicate())).
        Watches(&corev1.Pod{}, podHandler, WithPredicates(PodPredicate())).
        Complete(r)
}
```

`SetupWithManager()` chooses the cache-aware pod handler when desired-state caching is enabled and wires the instrumented workqueue when metrics recording is available.

Controller names include an atomic sequence number to avoid Prometheus metric collisions in envtest (multiple managers in one process).
