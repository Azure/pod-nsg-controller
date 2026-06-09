# 04 — Engine Design

> Package: `internal/engine` · Desired-state computation and diff algorithm

## Overview

The engine package contains the desired-state computation and diff logic, plus cache and snapshot helpers used by the reconciler. The core computation functions remain side-effect free, while the cache provides incremental, concurrency-safe reuse of desired-state results between reconciles. Together, they compute the desired set of IP addresses per ASG target from Kubernetes state and diff it against Azure to produce create/update/delete/patch actions.

## Component Map

```
internal/engine/
├── types.go                   # ASGTarget, DesiredPrefixSet, ActualPrefixSet, Action
├── desired_state.go           # ComputeDesiredState()
├── desired_state_cache.go     # Incremental desired-state cache with CAS-style versioning
├── desired_state_snapshot.go  # PodSnapshot type and snapshot-based desired state computation
├── diff.go                    # ComputeDiff()
└── target_key.go              # Target identity helpers (case-insensitive)
```

## Types

### `ASGTarget`

Identifies a unique address prefix set within an Azure ASG:

```go
type ASGTarget struct {
    SubscriptionID string
    ResourceGroup  string
    ASGName        string
    FullResourceID string    // canonical ARM resource ID
    PrefixSetName  string   // ownership key (case-insensitive identity)
}
```

### `DesiredPrefixSet` / `ActualPrefixSet`

```go
type DesiredPrefixSet struct {
    IPs map[string]struct{}
}

type ActualPrefixSet struct {
    IPs map[string]struct{}
}
```

Both use sets for efficient membership testing and diff computation.

### `Action`

```go
type ActionKind string

const (
    CreatePrefixSet ActionKind = "CreatePrefixSet"
    UpdatePrefixSet ActionKind = "UpdatePrefixSet"
    PatchPrefixSet  ActionKind = "PatchPrefixSet"
    DeletePrefixSet ActionKind = "DeletePrefixSet"
)

type Action struct {
    Kind       ActionKind
    Target     ASGTarget
    DesiredIPs []string  // sorted; nil for delete
    AddIPs     []string  // patch-only: IPs to add
    RemoveIPs  []string  // patch-only: IPs to remove
}
```

## ComputeDesiredState

```go
func ComputeDesiredState(
    clusterName string,
    mappings []v1alpha1.PodASGMapping,
    pods []corev1.Pod,
) map[ASGTarget]DesiredPrefixSet
```

### Algorithm

```
For each PodASGMapping:
  ownershipKey = model.OwnershipKey(clusterName, namespace, name)

  For each mapping rule:
    selector = model.CompileSelector(rule.PodSelector)

    For each pod in same namespace:
      if selector.Matches(pod.Labels) && pod.Status.PodIP != "":
        matchedIPs += pod.Status.PodIP

    For each ASG reference:
      parsed = model.ParseASGResourceID(ref.ResourceID)
      target = ASGTarget{..., PrefixSetName: ownershipKey}
      desired[target].IPs ∪= matchedIPs
```

### Key Properties

1. **Deduplication** — Multiple mappings targeting the same ASG merge their IPs
2. **Namespace-scoped** — Pod matching only considers pods in the mapping's namespace
3. **No-IP filtering** — Pods without `PodIP` are silently skipped
4. **Ownership key** — The `PrefixSetName` encodes `cluster__namespace__name`

## ComputeDiff

```go
func ComputeDiff(
    desired map[ASGTarget]DesiredPrefixSet,
    actual map[ASGTarget]ActualPrefixSet,
    patchThresholdPercent int,
) []Action
```

### Algorithm

```
For each desired target:
  if target NOT in actual:
    → CreatePrefixSet(target, desiredIPs)
  else if desiredIPs ≠ actualIPs:
    if delta is small enough:
      → PatchPrefixSet(target, addIPs, removeIPs)
    else:
      → UpdatePrefixSet(target, desiredIPs)

For each actual target:
  if target NOT in desired:
    → DeletePrefixSet(target)
```

### Patch vs Update Selection

When the diff detects that an existing prefix set needs modification, it decides between a full `UpdatePrefixSet` (replace all IPs) and a `PatchPrefixSet` (add/remove individual IPs).

In the implementation, the delta size is computed from the number of add/remove operations relative to the combined desired and actual set sizes:

```text
deltaOps = len(added) + len(removed)
denominator = len(desired) + len(actual)

if 100 * deltaOps <= patchThresholdPercent * denominator:
    → PatchPrefixSet(target, addIPs, removeIPs)
else:
    → UpdatePrefixSet(target, desiredIPs)
```

The default threshold is 50% (`DefaultPatchThresholdPercent = 50`), configurable via `POD_NSG_PATCH_THRESHOLD_PERCENT`. Patch operations reduce ARM payload size for large prefix sets with small changes.

### Target Identity

Targets are compared using a **case-insensitive** identity key that combines the full ARM resource ID and prefix set name:

```go
func TargetIdentityKey(fullResourceID, prefixSetName string) string {
    return strings.ToLower(fullResourceID) + "|" + strings.ToLower(prefixSetName)
}
```

This matches Azure's own case-insensitive resource handling.

### IP Set Comparison

```go
func ipSetsEqual(a map[string]struct{}, b map[string]struct{}) bool
```

Two IP sets are equal if they have the same size and every IP in `a` exists in `b`.

### Action Ordering

Actions are sorted deterministically for reproducible behaviour:

```
1. Creates first
2. Updates and patches second
3. Deletes last
```

Within each kind, actions are sorted by `TargetIdentityKey`.

## Desired State Cache

`desired_state_cache.go` provides an incremental cache that avoids full recomputation on every reconcile.

### Key Types

```go
type CachedDesiredState struct {
    Desired            map[ASGTarget]DesiredPrefixSet
    Snapshot           PodSnapshot
    MatchedPodsByIndex []int
    HasPendingIPPods   bool
}

type DesiredStateCache struct {
    // Thread-safe, mapping-scoped cache with version and lifecycle fences
}
```

The cache also exposes version fences for CAS-style publish:

```go
func (c *DesiredStateCache) GetWithVersion(
    mapping *v1alpha1.PodASGMapping,
) (CachedDesiredState, uint64, uint64, bool)
```

### How It Works

1. **Version fencing** — Each mapping key has a monotonic mutation version. Stale writes are rejected by `SetFromRecomputeIfVersion()`.
2. **Epoch/generation tracking** — Namespace invalidation bumps a namespace epoch, deletes bump a lifecycle epoch, and committed generations are tracked so older generations cannot repopulate the cache.
3. **Incremental pod handlers**:
   - `OnPodAdd(mapping, pod)` — adds a matched pod's CIDR to the affected ASG targets
   - `OnPodDelete(mapping, pod)` — removes a pod's contribution, scoped by the rules it matched
   - `OnPodUpdate(mapping, oldPod, newPod)` — handles selector, label, and IP changes incrementally
4. **CAS publish** — `SetFromRecomputeIfVersion()` only writes if the captured version and lifecycle fences still match, preventing lost updates from concurrent reconciles.
5. **Full recompute fallback** — `SetFromRecomputeArtifactsIfVersion()` accepts precomputed artifacts for an atomic cache replace without re-deriving pod rule membership under lock.

### Cache Miss Flow

When the cache is empty, when a generation is stale, or when a fence has advanced, the reconciler falls back to a full recompute via `ComputeDesiredStateWithSnapshot()` / `ComputeDesiredStateRecomputeArtifacts()` and then repopulates the cache for subsequent cycles.

## Desired State Snapshots

`desired_state_snapshot.go` adds snapshot-aware desired-state computation helpers that pair the computed state with lightweight matched-pod membership data.

### PodSnapshot

A point-in-time view of matched pods is captured as a map keyed by pod identity:

```go
type PodIdentity struct {
    Namespace string
    Name      string
    UID       string
}

type PodMembership struct {
    PodIP string
}

type PodSnapshot struct {
    Pods map[PodIdentity]PodMembership
}
```

### Snapshot-Based Computation

`ComputeDesiredStateWithSnapshot()` computes desired state from `[]corev1.Pod` and returns both the desired map and a `PodSnapshot`, allowing later cache operations to reuse compact pod membership data instead of re-walking full pod objects.

```go
func ComputeDesiredStateWithSnapshot(
    clusterName string,
    mappings []v1alpha1.PodASGMapping,
    pods []corev1.Pod,
) (map[ASGTarget]DesiredPrefixSet, PodSnapshot)
```

### Recompute Artifacts

`DesiredStateRecomputeArtifacts` bundles the outputs of a single recompute pass so the cache can be updated atomically without redoing selector matching:

```go
type DesiredStateRecomputeArtifacts struct {
    Desired            map[ASGTarget]DesiredPrefixSet
    Snapshot           PodSnapshot
    MatchedPodsByIndex []int
    HasPendingIPPods   bool
    PodRules           map[PodIdentity]map[int]struct{}
    PendingIPCount     int
}

func ComputeDesiredStateRecomputeArtifacts(
    clusterName string,
    mapping v1alpha1.PodASGMapping,
    pods []corev1.Pod,
) DesiredStateRecomputeArtifacts
```

The artifacts are computed once and can then be published into the cache atomically via `SetFromRecomputeArtifactsIfVersion()`.

## Data Flow

```
Kubernetes State                    Azure State
      │                                  │
      ▼                                  ▼
ComputeDesiredState()          listActualForTargets()
      │                                  │
      ▼                                  ▼
map[ASGTarget]DesiredPrefixSet   map[ASGTarget]ActualPrefixSet
      │                                  │
      └──────────┬───────────────────────┘
                 │
                 ▼
          ComputeDiff()
                 │
                 ▼
           []Action
                 │
                 ▼
        Executor.Execute()
```
