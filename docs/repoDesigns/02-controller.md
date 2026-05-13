# 02 — Controller Design

> Package: `internal/controller` · Core reconciliation loop and supporting components

## Overview

The controller package is the heart of the system. It contains the `MappingReconciler` (the main reconcile loop), status management, pod-to-mapping event mapping, watch predicates, ownership tracking, and error classification.

## Component Map

```
internal/controller/
├── mapping_reconciler.go    # MappingReconciler.Reconcile() — main loop
├── status_updater.go        # MappingStatusUpdater — status writes with retry
├── status.go                # ComputeStatus() — builds status from inputs
├── conditions.go            # Condition types, reasons, sync states
├── predicates.go            # PodPredicate(), MappingPredicate()
├── pod_handler.go           # PodToMappingEventHandler — pod→mapping enqueue
├── ownership_store.go       # Owned ASG annotation read/write
├── errors.go                # Error classification and requeue decisions
├── setup.go                 # SetupWithManager() — controller-runtime wiring
└── pod_controller.go        # Legacy PodReconciler (unused)
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

    PrefixSetFactory azure.AddressPrefixSetClientFactory
    Executor         Executor
    StatusUpdater    StatusUpdater
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

## Wiring

```go
func (r *MappingReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&PodASGMapping{}, WithPredicates(MappingPredicate())).
        Watches(&corev1.Pod{}, podHandler, WithPredicates(PodPredicate())).
        Complete(r)
}
```

Controller names include an atomic sequence number to avoid Prometheus metric collisions in envtest (multiple managers in one process).
