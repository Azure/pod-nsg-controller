# Pod-NSG-Controller — Development Specification

> **Approach:** Spec-Driven Development using Test-Driven Development (TDD) for each phase.
> Every phase defines acceptance criteria that must be expressed as failing tests **before** implementation begins.

---

## Table of Contents

- [Overview](#overview)
- [Architecture Principles](#architecture-principles)
- [Phase 1: CRD Types, Scaffolding & Scheme Registration](#phase-1-crd-types-scaffolding--scheme-registration)
- [Phase 2: Domain Model — Selectors, Ownership & Resource ID Parsing](#phase-2-domain-model--selectors-ownership--resource-id-parsing)
- [Phase 3: Desired-State Engine](#phase-3-desired-state-engine)
- [Phase 4: Azure Diff & Executor](#phase-4-azure-diff--executor)
- [Phase 5: Controller Wiring — Watches, Enqueue & Resync](#phase-5-controller-wiring--watches-enqueue--resync)
- [Phase 6: Status Reporting](#phase-6-status-reporting)
- [Phase 7: Error Handling, Resilience & Concurrency Control](#phase-7-error-handling-resilience--concurrency-control)
- [Phase 8: Integration & E2E Testing](#phase-8-integration--e2e-testing)
- [Appendix A: Ownership Model](#appendix-a-ownership-model)
- [Appendix B: Test Strategy Summary](#appendix-b-test-strategy-summary)

---

## Overview

The pod-nsg-controller is a Kubernetes controller that maps Pod IPs to Azure Application Security Groups (ASGs) via `addressPrefixSet` child resources. It enables NSG-based security enforcement for Kubernetes workloads using the same ASG model used for Azure VM workloads.

### Core Responsibilities

1. Watch `PodASGMapping` Custom Resources and Pods
2. Compute the **desired state**: which Pod IPs belong in which ASG addressPrefixSets
3. Reconcile desired state against **actual ARM state** (GET → diff → PUT/DELETE)
4. Report reconciliation status back to the `PodASGMapping` CR
5. Support cross-subscription ASG references via managed identity

### Design Reference

See [Comprehensive Design Document and User Guide](Comprehensive%20Design%20Document%20and%20User%20Guide%2020260407.md) for the full feature specification.

---

## Architecture Principles

These principles govern all implementation phases:

1. **Reconciliation-first:** The periodic full reconcile loop is the **primary correctness mechanism**. Event-driven handlers (pod create/delete) enqueue reconciliation — they do not imperatively mutate Azure state on their own.

2. **Desired-state convergence:** On every reconcile cycle the controller:
   - Computes the full desired state from `PodASGMapping` CRs + live Pods
   - Fetches actual ARM state (GET addressPrefixSets)
   - Diffs and applies changes (PUT/DELETE)

3. **Owned prefix sets:** Each controller instance manages **its own addressPrefixSet** per ASG, named deterministically (see [Appendix A](#appendix-a-ownership-model)). Multiple clusters can safely write to the same ASG without overwriting each other.

4. **Cross-subscription from day one:** The ASG resource ID parsing, per-subscription credential routing, and multi-target abstractions are part of the core model — not a later add-on.

5. **Testability by design:** Azure clients expose interfaces so that all controller logic can be tested with fakes/mocks. Credentials and HTTP transports are injectable.

---

## Phase 1: CRD Types, Scaffolding & Scheme Registration

### Goal

Define the `PodASGMapping` API types in Go, generate CRD manifests and deepcopy functions, register the scheme, and set up RBAC manifests.

### Deliverables

| Artifact | Path | Description |
|---|---|---|
| API types | `api/v1alpha1/types.go` | `PodASGMapping`, `PodASGMappingSpec`, `PodASGMappingStatus`, `Mapping`, `ASGReference`, `MappingStatus`, `Condition` |
| GroupVersion | `api/v1alpha1/groupversion_info.go` | `SchemeBuilder`, `GroupVersion` registration |
| DeepCopy | `api/v1alpha1/zz_generated.deepcopy.go` | Auto-generated via `controller-gen` |
| CRD YAML | `config/crd/podasgmapping.yaml` | Generated CRD manifest |
| RBAC | `config/rbac/rbac.yaml` | Updated ClusterRole with CRD verbs |
| Scheme registration | `cmd/main.go` | Register `v1alpha1` scheme |
| Makefile | `Makefile` | Wire `generate` and `manifests` targets to `controller-gen` |

### Spec: PodASGMappingSpec

```go
type PodASGMappingSpec struct {
    Mappings []Mapping `json:"mappings"`
}

type Mapping struct {
    PodSelector              PodSelector    `json:"podSelector"`
    ApplicationSecurityGroups []ASGReference `json:"applicationSecurityGroups"`
}

type PodSelector struct {
    MatchLabels map[string]string `json:"matchLabels"`
}

type ASGReference struct {
    ResourceID     string `json:"resourceId"`
    SubscriptionID string `json:"subscriptionId,omitempty"`
    Description    string `json:"description,omitempty"`
}
```

### Spec: PodASGMappingStatus

```go
type PodASGMappingStatus struct {
    Conditions      []metav1.Condition `json:"conditions,omitempty"`
    MappingStatuses []MappingStatus    `json:"mappingStatuses,omitempty"`
}

type MappingStatus struct {
    SelectorHash string      `json:"selectorHash"`
    MatchedPods  int         `json:"matchedPods"`
    ASGSyncState string      `json:"asgSyncState"` // "Synced", "Pending", "Error"
    LastSyncTime metav1.Time `json:"lastSyncTime,omitempty"`
    Error        string      `json:"error,omitempty"`
}
```

### TDD Acceptance Tests

| Test ID | Test | Assertion |
|---|---|---|
| `T1.1` | Serialize a `PodASGMapping` to JSON and back | Round-trip equality |
| `T1.2` | `PodASGMappingSpec` with zero mappings fails CRD validation | Rejected by `minItems: 1` |
| `T1.3` | `ASGReference.ResourceID` with invalid format fails validation | Rejected by `pattern` |
| `T1.4` | Valid `PodASGMapping` passes CRD validation | Accepted |
| `T1.5` | CRD YAML applies cleanly to an envtest cluster | CRD registered successfully |
| `T1.6` | Scheme registration allows typed client CRUD | Create, Get, List, Delete work |

---

## Phase 2: Domain Model — Selectors, Ownership & Resource ID Parsing

### Goal

Build the pure domain logic that sits between the Kubernetes API and Azure API. No I/O in this phase — purely testable functions.

### Deliverables

| Artifact | Path | Description |
|---|---|---|
| Resource ID parser | `internal/model/resourceid.go` | Parse ASG resource ID → subscription, resource group, ASG name |
| Selector compiler | `internal/model/selector.go` | Compile `PodSelector` into a `labels.Selector` |
| Ownership key | `internal/model/ownership.go` | Deterministic `addressPrefixSetName` from cluster ID + mapping identity |
| Mapping index | `internal/model/index.go` | Build/query index: which pods match which mappings |

### Ownership Model

Each controller instance owns a unique `addressPrefixSet` per ASG, named:

```
{clusterName}-{mappingNamespace}-{mappingName}
```

This ensures multiple clusters can write to the same ASG without conflict. See [Appendix A](#appendix-a-ownership-model) for details.

### Resource ID Parsing

```go
type ParsedASGReference struct {
    SubscriptionID string
    ResourceGroup  string
    ASGName        string
    FullResourceID string
}
```

Parse from: `/subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Network/applicationSecurityGroups/{name}`

### TDD Acceptance Tests

| Test ID | Test | Assertion |
|---|---|---|
| `T2.1` | Parse valid ASG resource ID | Correct subscription, RG, name extracted |
| `T2.2` | Parse invalid resource ID | Returns descriptive error |
| `T2.3` | Parse resource ID with mixed case | Case-insensitive match on provider path |
| `T2.4` | Compile `matchLabels: {app: web, tier: frontend}` | Matches pod with both labels, rejects pod missing one |
| `T2.5` | Compile empty `matchLabels` | Matches all pods (or rejected — decide policy) |
| `T2.6` | Generate ownership key for cluster "c1", mapping "prod/web-mapping" | Returns `c1-prod-web-mapping` |
| `T2.7` | Ownership keys from different clusters are distinct | `c1-prod-web-mapping` ≠ `c2-prod-web-mapping` |
| `T2.8` | Build index from 3 mappings, query with pod labels | Returns correct ASG references for each pod |
| `T2.9` | Index handles overlapping selectors (pod matches 2 mappings) | Returns union of ASG references |
| `T2.10` | Index handles no matching mappings | Returns empty set |

---

## Phase 3: Desired-State Engine

### Goal

Given the set of `PodASGMapping` CRs and live Pods, compute the **desired contents** of each owned `addressPrefixSet`. This is the core business logic of the controller.

### Deliverables

| Artifact | Path | Description |
|---|---|---|
| Desired state calculator | `internal/engine/desired_state.go` | `ComputeDesiredState(mappings, pods) → map[ASGTarget]DesiredPrefixSet` |
| ASG target model | `internal/engine/types.go` | `ASGTarget` (parsed resource ID + ownership key), `DesiredPrefixSet` (set of IPs) |
| Diff calculator | `internal/engine/diff.go` | `ComputeDiff(desired, actual) → []Action` where Action is CreatePrefixSet / UpdatePrefixSet / DeletePrefixSet |

### Desired State Computation Flow

```
For each PodASGMapping CR:
  For each Mapping in CR.Spec.Mappings:
    compiledSelector = compile(Mapping.PodSelector)
    matchedPods = filter(allPods, compiledSelector)
    matchedIPs = extract IPs from matchedPods (skip pods without IPs)
    For each ASGRef in Mapping.ApplicationSecurityGroups:
      target = ASGTarget{parsedResourceID, ownershipKey(clusterID, CR)}
      desiredState[target].IPs = union(desiredState[target].IPs, matchedIPs)
```

### Diff Logic

```
For each target in union(desired.keys, actual.keys):
  if target in desired AND NOT in actual → CreatePrefixSet
  if target in desired AND in actual:
    if desired.IPs ≠ actual.IPs → UpdatePrefixSet
    else → no-op
  if target NOT in desired AND in actual → DeletePrefixSet (cleanup)
```

### TDD Acceptance Tests

| Test ID | Test | Assertion |
|---|---|---|
| `T3.1` | Single mapping, 3 pods match, 1 ASG | Desired state has 3 IPs in one prefix set |
| `T3.2` | Single mapping, pod has no IP yet | Pod excluded from desired state |
| `T3.3` | Two mappings, overlapping selectors, different ASGs | Each ASG gets correct IP set |
| `T3.4` | Same ASG referenced by two mappings with different selectors | IPs are unioned |
| `T3.5` | Cross-subscription: mapping references ASGs in sub-1 and sub-2 | Desired state has entries for both subs |
| `T3.6` | No matching pods | Desired prefix set is empty (not absent — still create/maintain the prefix set) |
| `T3.7` | Mapping deleted (not in input) | No desired state for its targets |
| `T3.8` | Diff: desired={A: [ip1,ip2]}, actual={A: [ip1]} | Action: UpdatePrefixSet A with [ip1,ip2] |
| `T3.9` | Diff: desired={A: [ip1]}, actual={A: [ip1]} | No actions |
| `T3.10` | Diff: desired={}, actual={A: [ip1]} | Action: DeletePrefixSet A |
| `T3.11` | Diff: desired={A: [ip1]}, actual={} | Action: CreatePrefixSet A with [ip1] |
| `T3.12` | Diff: desired={A: [ip1], B: [ip2]}, actual={A: [ip1,ip3], C: [ip4]} | Update A, Create B, Delete C |

---

## Phase 4: Azure Diff & Executor

### Goal

Execute the diff actions against Azure ARM. Handle cross-subscription credential routing, ETag-based optimistic concurrency, and parallel execution.

### Deliverables

| Artifact | Path | Description |
|---|---|---|
| Azure executor interface | `internal/azure/interfaces.go` | `AddressPrefixSetAPI` interface (Get, Put, Delete, List) |
| Executor implementation | `internal/azure/executor.go` | Applies `[]Action` in parallel, respects per-sub credentials |
| Cross-sub client factory | `internal/azure/client_factory.go` | Creates/caches per-subscription clients using managed identity |
| Fake client | `internal/azure/fake/` | In-memory fake implementing `AddressPrefixSetAPI` for tests |

### Interface

```go
type AddressPrefixSetAPI interface {
    Get(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) (*AddressPrefixSet, error)
    Put(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string, ips []string) error
    Delete(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) error
    List(ctx context.Context, subscriptionID, resourceGroup, asgName string) ([]AddressPrefixSet, error)
}
```

### Cross-Subscription Client Routing

```
For each Action:
  subscriptionID = action.Target.SubscriptionID
  client = clientFactory.GetOrCreate(subscriptionID)
  execute action with client
```

All ARM calls for a single reconcile cycle are executed in parallel (bounded concurrency).

### ETag / Concurrency Control

- Every GET returns an ETag
- Every PUT includes `If-Match: <etag>` header
- On ETag conflict (412): re-GET, recompute diff for that target, retry
- Maximum 3 retries per target per cycle

### TDD Acceptance Tests

| Test ID | Test | Assertion |
|---|---|---|
| `T4.1` | Execute CreatePrefixSet action with fake | Fake state updated |
| `T4.2` | Execute UpdatePrefixSet action with fake | Fake state reflects new IPs |
| `T4.3` | Execute DeletePrefixSet action with fake | Fake state entry removed |
| `T4.4` | Execute actions across 2 subscriptions | Correct fake clients used per subscription |
| `T4.5` | ETag conflict on PUT → automatic retry succeeds | PUT retried with fresh ETag, succeeds |
| `T4.6` | ETag conflict exceeds max retries | Action marked as failed with error |
| `T4.7` | Parallel execution of 5 actions | All complete, bounded concurrency respected |
| `T4.8` | One action fails, others succeed | Failed action returns error, others still applied |
| `T4.9` | Client factory caches clients per subscription | Second call for same sub reuses client |
| `T4.10` | PUT with empty IP list | Puts empty prefix set (valid — clears membership) |

---

## Phase 5: Controller Wiring — Watches, Enqueue & Resync

### Goal

Wire the desired-state engine and Azure executor into the controller-runtime reconciliation loop. Watch both `PodASGMapping` CRs and Pods; enqueue affected mappings on any change.

### Deliverables

| Artifact | Path | Description |
|---|---|---|
| Mapping reconciler | `internal/controller/mapping_reconciler.go` | Reconciles a single `PodASGMapping` CR |
| Pod event handler | `internal/controller/pod_handler.go` | Maps Pod events → affected `PodASGMapping` enqueue |
| Controller setup | `internal/controller/setup.go` | Register watches, predicates, resync interval |

### Reconcile Flow (per PodASGMapping)

```
1. Fetch PodASGMapping CR (if deleted → cleanup owned prefix sets → return)
2. List all Pods in the CR's namespace
3. Compute desired state (Phase 3 engine)
4. Fetch actual ARM state for all owned prefix sets (Phase 4 client)
5. Compute diff (Phase 3 engine)
6. Execute diff (Phase 4 executor)
7. Update CR status (Phase 6)
8. Requeue after resync interval (default: 60s)
```

### Watch Configuration

| Resource | Watch Type | Enqueue Logic |
|---|---|---|
| `PodASGMapping` | Primary watch | Enqueue self |
| `Pod` | Secondary watch via `handler.EnqueueRequestsFromMapFunc` | For each PodASGMapping in the pod's namespace, if pod labels match any mapping selector → enqueue that PodASGMapping |

### Event Filtering (Predicates)

- **Pods:** Only trigger on create, delete, and label/IP changes (ignore status-only updates that don't affect labels or IP)
- **PodASGMapping:** Trigger on all spec changes; ignore status-only updates

### Cleanup on CRD Deletion

When a `PodASGMapping` is deleted:

1. List all owned addressPrefixSets across all referenced ASGs
2. Delete each owned addressPrefixSet
3. Remove finalizer

A **finalizer** (`networking.azure.com/pod-asg-cleanup`) is added on first reconcile to ensure cleanup runs before the CR is garbage collected.

### TDD Acceptance Tests

| Test ID | Test | Assertion |
|---|---|---|
| `T5.1` | Pod created matching a mapping → mapping is enqueued | Reconcile triggered for the mapping |
| `T5.2` | Pod deleted → mapping is enqueued | Reconcile triggered, IP removed from desired state |
| `T5.3` | Pod IP changes → mapping is enqueued | Reconcile triggered, old IP absent, new IP present |
| `T5.4` | Pod label changes (no longer matches) → mapping enqueued | IP removed from desired state |
| `T5.5` | PodASGMapping spec updated (new ASG added) | New prefix set created in new ASG |
| `T5.6` | PodASGMapping spec updated (ASG removed) | Owned prefix set deleted from removed ASG |
| `T5.7` | PodASGMapping deleted → finalizer triggers cleanup | All owned prefix sets deleted |
| `T5.8` | Periodic resync fires after interval | Reconcile triggered, drift corrected |
| `T5.9` | Pod status-only update (no label/IP change) | Reconcile NOT triggered (predicate filters it) |
| `T5.10` | Multiple PodASGMappings in same namespace, pod matches one | Only the matching mapping is enqueued |

---

## Phase 6: Status Reporting

### Goal

After each reconcile cycle, update the `PodASGMapping` CR status with conditions and per-mapping sync state.

### Deliverables

| Artifact | Path | Description |
|---|---|---|
| Status reducer | `internal/controller/status.go` | Compute status from reconcile results |
| Condition helpers | `internal/controller/conditions.go` | Set/update `metav1.Condition` entries |

### Conditions

| Type | Meaning |
|---|---|
| `Accepted` | The CRD spec is valid and all resource IDs are parseable |
| `Reconciled` | The last reconcile cycle completed (True = success, False = partial/full failure) |

### MappingStatus Fields

For each mapping in the spec:

- `selectorHash`: deterministic hash of the `matchLabels` (for correlation)
- `matchedPods`: count of pods currently matching the selector
- `asgSyncState`: `"Synced"` | `"Pending"` | `"Error"`
- `lastSyncTime`: timestamp of last successful sync for this mapping
- `error`: error message if `asgSyncState == "Error"`

### Status Transition Rules

- On spec validation failure → `Accepted=False`, all mappings `Error`
- On full reconcile success → `Accepted=True`, `Reconciled=True`, all mappings `Synced`
- On partial failure → `Reconciled=False`, failed mappings `Error`, successful `Synced`
- On first reconcile (before Azure call) → mappings `Pending`

### TDD Acceptance Tests

| Test ID | Test | Assertion |
|---|---|---|
| `T6.1` | All actions succeed | `Reconciled=True`, all mappings `Synced` |
| `T6.2` | One mapping's ASG update fails | `Reconciled=False`, failed mapping `Error` with message, others `Synced` |
| `T6.3` | Invalid resource ID in spec | `Accepted=False` immediately |
| `T6.4` | Status preserves `lastSyncTime` from previous success | Timestamp only updates on success |
| `T6.5` | `matchedPods` count is accurate | Matches actual filtered pod count |
| `T6.6` | `selectorHash` is deterministic | Same labels always produce same hash |
| `T6.7` | Status update does not trigger re-reconcile | Status subresource update only |

---

## Phase 7: Error Handling, Resilience & Concurrency Control

### Goal

Harden the controller for production: retry logic, rate limiting, partial failure handling, and graceful degradation.

### Deliverables

| Artifact | Path | Description |
|---|---|---|
| Retry wrapper | `internal/azure/retry.go` | Exponential backoff with jitter for ARM calls |
| Rate limiter | `internal/azure/ratelimit.go` | Per-subscription ARM call rate limiting |
| Reconcile error handler | `internal/controller/errors.go` | Classify errors, decide requeue strategy |

### Retry Policy

| Error Type | Strategy |
|---|---|
| 429 Too Many Requests | Respect `Retry-After` header, requeue |
| 412 Precondition Failed (ETag) | Re-GET, recompute, retry (max 3) |
| 5xx Server Error | Exponential backoff: 1s, 2s, 4s (max 3 retries) |
| 401/403 Auth Error | Do not retry, report error in status |
| Network timeout | Retry once, then requeue with backoff |

### Rate Limiting

- ARM API calls are rate-limited per subscription (configurable, default: 10 RPS)
- Reconcile requeue uses controller-runtime's rate limiter (exponential backoff capped at 5 minutes)

### Partial Failure Handling

If a reconcile cycle has N actions and M fail:
- The (N-M) successful actions are persisted
- The M failures are reported per-mapping in status
- The reconcile is requeued with backoff
- Next cycle recomputes desired state fresh (convergent)

### TDD Acceptance Tests

| Test ID | Test | Assertion |
|---|---|---|
| `T7.1` | ARM returns 429 | Call retried after `Retry-After` delay |
| `T7.2` | ARM returns 500 three times then succeeds | Retried with backoff, succeeds on 4th |
| `T7.3` | ARM returns 500 four times | Gives up, error reported in status |
| `T7.4` | ARM returns 403 | No retry, error reported immediately |
| `T7.5` | 3 of 5 actions fail | 2 succeed and persist, 3 report errors, reconcile requeued |
| `T7.6` | Rate limiter throttles burst of 20 calls | Calls spread over time, none dropped |
| `T7.7` | ETag conflict during retry loop | Re-GET + recompute succeeds |

---

## Phase 8: Integration & E2E Testing

### Goal

Validate the complete system with envtest-based integration tests and optional live Azure E2E tests.

### Deliverables

| Artifact | Path | Description |
|---|---|---|
| envtest suite | `test/integration/` | Full reconcile loop with CRDs, fake Azure |
| E2E suite | `test/e2e/` | Optional: live Azure with real ASGs (CI-gated) |
| Test fixtures | `test/fixtures/` | Sample CRDs, pod manifests |

### Integration Test Scenarios (envtest + fake Azure)

| Test ID | Scenario | Assertion |
|---|---|---|
| `T8.1` | Create PodASGMapping + deploy 3 pods | All 3 IPs appear in fake ASG prefix set |
| `T8.2` | Scale pods from 3 to 5 | 2 new IPs added |
| `T8.3` | Scale pods from 5 to 2 | 3 IPs removed |
| `T8.4` | Delete PodASGMapping | All owned prefix sets cleaned up |
| `T8.5` | Pod IP changes (recreate) | Old IP removed, new IP added |
| `T8.6` | Selector change excludes pods | Excluded pod IPs removed |
| `T8.7` | Cross-sub mapping: 2 ASGs in different subs | Both fake clients receive correct IPs |
| `T8.8` | Controller restart mid-reconcile | Next reconcile converges to correct state |
| `T8.9` | Drift injection: manually add stale IP to fake ASG | Reconcile removes stale IP |
| `T8.10` | Drift injection: manually remove valid IP | Reconcile re-adds the IP |
| `T8.11` | Two PodASGMappings in same namespace | Each manages its own prefix sets independently |
| `T8.12` | PodASGMapping with overlapping selectors | Pod IPs appear in union of all referenced ASGs |

### E2E Test Scenarios (live Azure — CI-gated)

| Test ID | Scenario | Assertion |
|---|---|---|
| `T8.E1` | Full lifecycle: create mapping → deploy pods → verify ASG → delete | IPs in real ASG, cleanup on delete |
| `T8.E2` | Cross-subscription ASG update | Pod IPs propagated to ASG in different subscription |
| `T8.E3` | Scale to 100 pods | All IPs present within SLO (~10s) |

---

## Appendix A: Ownership Model

### Problem

Multiple Kubernetes clusters may reference the same ASG. Each cluster's controller must manage its own set of Pod IPs without overwriting another cluster's entries.

### Solution

Each controller instance creates a uniquely named `addressPrefixSet` child resource per ASG:

```
addressPrefixSetName = "{clusterName}-{mappingNamespace}-{mappingName}"
```

**Example:**

- Cluster: `aks-prod-eastus2`
- PodASGMapping: `production/workload-asg-mapping`
- ASG: `WebAsg-SubA`

Resulting addressPrefixSet:
```
/subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Network/
  applicationSecurityGroups/WebAsg-SubA/addressPrefixSets/aks-prod-eastus2-production-workload-asg-mapping
```

### Properties

- **Deterministic:** Same inputs always produce the same name
- **Collision-free:** Different clusters/mappings produce different names
- **Discoverable:** A controller can list all prefix sets and identify its own by name prefix
- **Cleanable:** On PodASGMapping deletion, delete all prefix sets matching the ownership key

### Configuration

The cluster name is provided via environment variable `CLUSTER_NAME` (required). The controller refuses to start without it.

---

## Appendix B: Test Strategy Summary

| Phase | Test Type | Tools | Scope |
|---|---|---|---|
| 1 | Unit + envtest | `go test`, `envtest` | CRD validation, scheme registration |
| 2 | Unit | `go test` | Pure functions: parsing, selectors, ownership |
| 3 | Unit | `go test` | Desired-state computation, diff logic |
| 4 | Unit | `go test` + fake client | Azure executor, cross-sub routing, retries |
| 5 | Unit + envtest | `go test`, `envtest`, fake client | Controller watches, enqueue, reconcile flow |
| 6 | Unit | `go test` | Status computation, condition transitions |
| 7 | Unit | `go test` + fake client | Retry logic, rate limiting, partial failure |
| 8 | Integration + E2E | `envtest` + fake, live Azure (gated) | Full system scenarios |

### TDD Workflow (per phase)

```
1. Read the phase spec and acceptance tests
2. Write failing test(s) for the first acceptance criterion
3. Implement the minimum code to pass
4. Refactor
5. Repeat for next acceptance criterion
6. Run full test suite to verify no regressions
7. Move to next phase
```

### Test Naming Convention

```
TestPhaseX_Description (e.g., TestPhase2_ParseValidASGResourceID)
```

Or grouped in test suites:

```
func TestDesiredStateEngine(t *testing.T) {
    t.Run("single mapping with matching pods", func(t *testing.T) { ... })
    t.Run("cross-subscription ASG references", func(t *testing.T) { ... })
}
```
