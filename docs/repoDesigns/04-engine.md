# 04 — Engine Design

> Package: `internal/engine` · Desired-state computation and diff algorithm

## Overview

The engine is a pure-function layer with no side effects. It computes the desired set of IP addresses per ASG target from Kubernetes state, and diffs it against the actual state from Azure to produce create/update/delete actions.

## Component Map

```
internal/engine/
├── types.go           # ASGTarget, DesiredPrefixSet, ActualPrefixSet, Action
├── desired_state.go   # ComputeDesiredState()
├── diff.go            # ComputeDiff()
└── target_key.go      # Target identity helpers (case-insensitive)
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
    DeletePrefixSet ActionKind = "DeletePrefixSet"
)

type Action struct {
    Kind       ActionKind
    Target     ASGTarget
    DesiredIPs []string  // sorted; nil for delete
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
) []Action
```

### Algorithm

```
For each desired target:
  if target NOT in actual:
    → CreatePrefixSet(target, desiredIPs)
  else if desiredIPs ≠ actualIPs:
    → UpdatePrefixSet(target, desiredIPs)

For each actual target:
  if target NOT in desired:
    → DeletePrefixSet(target)
```

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
2. Updates second
3. Deletes last
```

Within each kind, actions are sorted by `TargetIdentityKey`.

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
