# Pod-NSG-Controller — Implementation Specification

> **Development Approach:** Spec-Driven Development using Test-Driven Development (TDD).
> Every phase defines acceptance criteria expressed as failing tests **before** implementation begins.

---

## Table of Contents

- [Gap Analysis: Current State vs. Design](#gap-analysis-current-state-vs-design)
- [Architecture Principles](#architecture-principles)
- [Phase 1: CRD Types, Scaffolding & Scheme Registration](#phase-1-crd-types-scaffolding--scheme-registration)
- [Phase 2: Domain Model — Selectors, Ownership & Resource ID Parsing](#phase-2-domain-model--selectors-ownership--resource-id-parsing)
- [Phase 3: Desired-State Engine](#phase-3-desired-state-engine)
- [Phase 4: Azure Client Interfaces & Executor](#phase-4-azure-client-interfaces--executor)
- [Phase 5: Controller Wiring — Watches, Enqueue & Resync](#phase-5-controller-wiring--watches-enqueue--resync)
- [Phase 6: Status Reporting](#phase-6-status-reporting)
- [Phase 7: Error Handling, Resilience & Concurrency Control](#phase-7-error-handling-resilience--concurrency-control)
- [Phase 8: Integration & E2E Testing](#phase-8-integration--e2e-testing)
- [Appendix A: Ownership Model](#appendix-a-ownership-model)
- [Appendix B: Test Strategy Summary](#appendix-b-test-strategy-summary)
- [Appendix C: Configuration Reference](#appendix-c-configuration-reference)

---

## Gap Analysis: Current State vs. Design

The current codebase implements a basic Pod-watching controller. The design document requires a
CRD-driven, addressPrefixSet-based controller with cross-subscription support. Below is a
detailed gap analysis.

### What Exists Today

| Component | File | Description |
|---|---|---|
| Pod Reconciler | `internal/controller/pod_controller.go` | Watches Pods, reads ASG names from labels/annotations, resolves ASGs by name in a single resource group. NIC resolution and ASG update logic are TODO stubs. |
| ASG Client | `internal/azure/asg_client.go` | CRUD operations on ASGs (the parent resource). Uses `armnetwork.ApplicationSecurityGroupsClient`. Single subscription/RG. |
| NIC Client | `internal/azure/nic_client.go` | Basic NIC Get/Update/List. Single subscription/RG. Not used by the reconciler yet. |
| AddressPrefixSet Client | `internal/azure/address_prefix_set_client.go` | Direct REST client for `addressPrefixSets` child resources (GET/PUT/DELETE/List) against API version `2026-01-01`. Single subscription/RG. |
| Config | `internal/config/config.go` | Reads `AZURE_SUBSCRIPTION_ID`, `AZURE_RESOURCE_GROUP`, `AZURE_NSG_NAME` from env vars. |
| Main | `cmd/main.go` | Bootstraps controller-runtime manager, registers `PodReconciler`. No CRD scheme registration. |
| Makefile | `Makefile` | Build/test/deploy targets. `generate` and `manifests` are stubs. |

### What the Design Requires (Not Yet Implemented)

| Requirement | Design Reference | Gap |
|---|---|---|
| **PodASGMapping CRD** | Design §3.4, SPEC Phase 1 | No CRD types, no scheme registration, no CRD YAML |
| **CRD-based reconciliation** | Design §4 | Current reconciler watches Pods with label/annotation-based ASG selection; design requires watching PodASGMapping CRs |
| **addressPrefixSet management** | Design §1.1, §4.2 | The REST client exists but is not wired into the reconciliation loop |
| **Cross-subscription support** | Design §2 | All Azure clients are single-subscription; design requires per-subscription client routing |
| **Ownership model** | SPEC Appendix A | No concept of `{clusterName}-{namespace}-{mappingName}` prefix set naming |
| **Domain model** | SPEC Phase 2 | No resource ID parser, selector compiler, ownership key generator, or mapping index |
| **Desired-state engine** | SPEC Phase 3 | No desired-state computation or diff logic |
| **Azure executor** | SPEC Phase 4 | No action-based executor, no ETag concurrency, no parallel execution |
| **Controller wiring** | SPEC Phase 5 | No PodASGMapping watch, no pod→mapping enqueue, no finalizer, no cleanup |
| **Status reporting** | SPEC Phase 6 | No status subresource, no conditions, no per-mapping sync state |
| **Error handling** | SPEC Phase 7 | No retry with backoff, no rate limiting, no partial failure handling |
| **Integration tests** | SPEC Phase 8 | No envtest integration suite, no E2E tests |
| **CLUSTER_NAME config** | SPEC Appendix A | Not in current config; required for ownership model |
| **Makefile code generation** | SPEC Phase 1 | `generate` and `manifests` targets are stubs |

---

## Architecture Principles

These principles govern all implementation phases:

1. **Reconciliation-first:** The periodic full reconcile loop is the primary correctness mechanism. Event-driven handlers enqueue reconciliation — they do not imperatively mutate Azure state on their own.

2. **Desired-state convergence:** On every reconcile cycle the controller:
   - Computes the full desired state from `PodASGMapping` CRs + live Pods
   - Fetches actual ARM state (GET addressPrefixSets)
   - Diffs and applies changes (PUT/DELETE)

3. **Owned prefix sets:** Each controller instance manages its own `addressPrefixSet` per ASG, named deterministically (see [Appendix A](#appendix-a-ownership-model)). Multiple clusters can safely write to the same ASG without overwriting each other.

4. **Cross-subscription from day one:** ASG resource ID parsing, per-subscription credential routing, and multi-target abstractions are part of the core model.

5. **Testability by design:** Azure clients expose interfaces so all controller logic can be tested with fakes/mocks. Credentials and HTTP transports are injectable.

6. **Structured logging via `go.uber.org/zap`:** All components use the [Uber Zap](https://github.com/uber-go/zap) structured logger as the single logging backend. The controller-runtime integration uses `zapr.NewLogger(zapLog)` to bridge `logr.Logger` calls to Zap. Direct Zap loggers (`*zap.Logger` or `*zap.SugaredLogger`) are used in non-controller-runtime code (Azure clients, domain model utilities). All log output is structured JSON with fields: `timestamp` (ISO 8601), `level`, `msg`, and context-specific keys (`namespace`, `pod`, `asg`, `subscriptionID`, `operation`, `duration`).

---

## Logging Standard

All code in this repository **must** use [`go.uber.org/zap`](https://github.com/uber-go/zap) for structured logging.

### Logger Setup (Entry Point)

The `cmd/main.go` entry point creates a production Zap logger and bridges it to controller-runtime via `zapr`:

```go
import (
    "github.com/go-logr/zapr"
    "go.uber.org/zap"
    "go.uber.org/zap/zapcore"
)

zapCfg := zap.NewProductionConfig()
zapCfg.EncoderConfig.TimeKey = "timestamp"
zapCfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
zapLog, _ := zapCfg.Build()
logger := zapr.NewLogger(zapLog)
ctrl.SetLogger(logger)
```

### Usage by Component Type

| Component | Logger Type | How to Obtain |
|---|---|---|
| **Controller / Reconciler** | `logr.Logger` (backed by Zap) | `log.FromContext(ctx)` or injected via struct field |
| **Azure clients** | `*zap.Logger` | Passed via constructor (e.g., `NewExecutor(logger *zap.Logger, ...)`) |
| **Domain model / engine** | `*zap.Logger` | Passed via function parameter or struct field |
| **Tests** | `zap.NewNop()` or `zaptest.NewLogger(t)` | Use `go.uber.org/zap/zaptest` for test-scoped loggers |

### Required Log Fields

Every log line must include context-appropriate structured fields:

| Field | Type | When to Include |
|---|---|---|
| `namespace` | string | Any operation scoped to a Kubernetes namespace |
| `pod` | string | Any operation involving a specific pod |
| `mapping` | string | Any operation involving a PodASGMapping CR |
| `asg` | string | Any operation involving an ASG |
| `subscriptionID` | string | Any Azure ARM call |
| `resourceGroup` | string | Any Azure ARM call |
| `prefixSetName` | string | Any addressPrefixSet operation |
| `operation` | string | The ARM verb: `GET`, `PUT`, `DELETE` |
| `duration` | duration | Any remote call (Azure, Kubernetes API) |
| `error` | error | Any failed operation (use `zap.Error(err)`) |

### Log Levels

| Level | Usage |
|---|---|
| `Info` | Normal operations: reconcile start/end, ASG updates, status changes |
| `Debug` (`V(1)` via logr) | Verbose: cache hits, predicate evaluations, skipped pods |
| `Error` | Failed operations that will be retried or reported in status |
| `Warn` (via `zap.Warn`) | Degraded state: ASG not found, invalid annotation (non-fatal) |

### Rules

1. **No `fmt.Printf` or `log.Println`** — all logging goes through Zap.
2. **No `logr` without Zap backend** — `logr.Logger` instances must be backed by `zapr.NewLogger()`.
3. **Always defer `zapLog.Sync()`** in `main()` to flush buffered logs.
4. **Use `zap.String`, `zap.Int`, `zap.Error`** typed field constructors (not `zap.Any`) for performance.
5. **In tests**, use `zaptest.NewLogger(t)` to capture logs and fail on unexpected errors.

---

## Phase 1: CRD Types, Scaffolding & Scheme Registration

### Goal

Define the `PodASGMapping` API types in Go, generate CRD manifests and deepcopy functions, register the scheme, and set up RBAC manifests.

### Step-by-Step Actions

1. **Create API types directory**: `api/v1alpha1/`
2. **Define types** in `api/v1alpha1/types.go`:
   - `PodASGMapping`, `PodASGMappingList` (top-level resources)
   - `PodASGMappingSpec` with `Mappings []Mapping`
   - `Mapping` with `PodSelector` and `ApplicationSecurityGroups []ASGReference`
   - `PodSelector` with `MatchLabels map[string]string`
   - `ASGReference` with `ResourceID`, `SubscriptionID` (optional), `Description` (optional)
   - `PodASGMappingStatus` with `Conditions []metav1.Condition` and `MappingStatuses []MappingStatus`
   - `MappingStatus` with `SelectorHash`, `MatchedPods`, `ASGSyncState`, `LastSyncTime`, `Error`
3. **Create GroupVersion registration** in `api/v1alpha1/groupversion_info.go`:
   - Group: `networking.azure.com`
   - Version: `v1alpha1`
   - `SchemeBuilder` and `AddToScheme` function
4. **Add controller-gen markers** on types for CRD generation and deepcopy
5. **Wire Makefile targets**:
   - `generate`: run `controller-gen object paths=./api/...` for deepcopy
   - `manifests`: run `controller-gen crd paths=./api/... output:crd:dir=config/crd`
6. **Generate deepcopy**: `api/v1alpha1/zz_generated.deepcopy.go`
7. **Generate CRD YAML**: `config/crd/podasgmapping.yaml`
8. **Update RBAC**: `config/rbac/rbac.yaml` — add verbs for `podasgmappings` resource
9. **Register scheme in `cmd/main.go`**: Import `api/v1alpha1` and call `AddToScheme(scheme)`
10. **Validate with envtest**: CRD applies cleanly, typed client CRUD works

### Spec: PodASGMappingSpec

```go
// api/v1alpha1/types.go

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mappings",type=integer,JSONPath=`.spec.mappings`,description="Number of mapping rules"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PodASGMapping struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec              PodASGMappingSpec   `json:"spec,omitempty"`
    Status            PodASGMappingStatus `json:"status,omitempty"`
}

type PodASGMappingSpec struct {
    // +kubebuilder:validation:MinItems=1
    Mappings []Mapping `json:"mappings"`
}

type Mapping struct {
    PodSelector               PodSelector    `json:"podSelector"`
    ApplicationSecurityGroups []ASGReference `json:"applicationSecurityGroups"`
}

type PodSelector struct {
    MatchLabels map[string]string `json:"matchLabels"`
}

type ASGReference struct {
    // +kubebuilder:validation:Pattern=`^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Network/applicationSecurityGroups/[^/]+$`
    ResourceID     string `json:"resourceId"`
    SubscriptionID string `json:"subscriptionId,omitempty"`
    Description    string `json:"description,omitempty"`
}

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

### Test File

`api/v1alpha1/types_test.go`

---

## Phase 2: Domain Model — Selectors, Ownership & Resource ID Parsing

### Goal

Build the pure domain logic that sits between the Kubernetes API and Azure API. No I/O in this phase — purely testable functions.

### Step-by-Step Actions

1. **Create domain model package**: `internal/model/`
2. **Implement Resource ID parser** in `internal/model/resourceid.go`:
   - `ParseASGResourceID(string) (ParsedASGReference, error)`
   - Extract: subscriptionID, resourceGroup, ASG name from full ARM resource ID
   - Case-insensitive matching on provider path segments
3. **Implement Selector compiler** in `internal/model/selector.go`:
   - `CompileSelector(PodSelector) (labels.Selector, error)`
   - Convert `matchLabels` map into a `labels.Selector` from `k8s.io/apimachinery`
4. **Implement Ownership key generator** in `internal/model/ownership.go`:
   - `OwnershipKey(clusterName, namespace, mappingName string) string`
   - Returns `"{clusterName}-{namespace}-{mappingName}"`
   - This is the `addressPrefixSetName` used per ASG
5. **Implement Mapping index** in `internal/model/index.go`:
   - `BuildIndex(mappings []PodASGMapping) *MappingIndex`
   - `(*MappingIndex).MatchingASGs(pod *corev1.Pod) []ASGReference`
   - Returns union of ASG references from all matching mappings

### Key Types

```go
// internal/model/resourceid.go
type ParsedASGReference struct {
    SubscriptionID string
    ResourceGroup  string
    ASGName        string
    FullResourceID string
}
```

### TDD Acceptance Tests

| Test ID | Test | Assertion |
|---|---|---|
| `T2.1` | Parse valid ASG resource ID | Correct subscription, RG, name extracted |
| `T2.2` | Parse invalid resource ID (wrong provider) | Returns descriptive error |
| `T2.3` | Parse resource ID with mixed case | Case-insensitive match on provider path |
| `T2.4` | Compile `matchLabels: {app: web, tier: frontend}` | Matches pod with both labels, rejects pod missing one |
| `T2.5` | Compile empty `matchLabels` | Matches all pods (policy decision: document behavior) |
| `T2.6` | Generate ownership key for cluster "c1", mapping "prod/web-mapping" | Returns `c1-prod-web-mapping` |
| `T2.7` | Ownership keys from different clusters are distinct | `c1-prod-web-mapping` ≠ `c2-prod-web-mapping` |
| `T2.8` | Build index from 3 mappings, query with pod labels | Returns correct ASG references for each pod |
| `T2.9` | Index handles overlapping selectors (pod matches 2 mappings) | Returns union of ASG references |
| `T2.10` | Index handles no matching mappings | Returns empty set |

### Test Files

- `internal/model/resourceid_test.go`
- `internal/model/selector_test.go`
- `internal/model/ownership_test.go`
- `internal/model/index_test.go`

---

## Phase 3: Desired-State Engine

### Goal

Given the set of `PodASGMapping` CRs and live Pods, compute the **desired contents** of each owned `addressPrefixSet`. This is the core business logic of the controller.

### Step-by-Step Actions

1. **Create engine package**: `internal/engine/`
2. **Define types** in `internal/engine/types.go`:
   - `ASGTarget`: parsed resource ID + ownership key (the unique identifier for an addressPrefixSet)
   - `DesiredPrefixSet`: set of IP strings
   - `Action` enum: `CreatePrefixSet`, `UpdatePrefixSet`, `DeletePrefixSet`
3. **Implement desired state calculator** in `internal/engine/desired_state.go`:
   - `ComputeDesiredState(clusterName string, mappings []PodASGMapping, pods []corev1.Pod) map[ASGTarget]DesiredPrefixSet`
   - Algorithm:
     ```
     For each PodASGMapping CR:
       For each Mapping in CR.Spec.Mappings:
         compiledSelector = compile(Mapping.PodSelector)
         matchedPods = filter(allPods, compiledSelector)
         matchedIPs = extract pod IPs (skip pods without IPs)
         For each ASGRef in Mapping.ApplicationSecurityGroups:
           target = ASGTarget{parsedRef, ownershipKey(clusterName, CR)}
           desiredState[target].IPs = union(existing, matchedIPs)
     ```
4. **Implement diff calculator** in `internal/engine/diff.go`:
   - `ComputeDiff(desired map[ASGTarget]DesiredPrefixSet, actual map[ASGTarget]ActualPrefixSet) []Action`
   - Logic:
     ```
     For each target in union(desired.keys, actual.keys):
       in desired AND NOT in actual → CreatePrefixSet
       in desired AND in actual → if IPs differ → UpdatePrefixSet; else → no-op
       NOT in desired AND in actual → DeletePrefixSet
     ```

### TDD Acceptance Tests

| Test ID | Test | Assertion |
|---|---|---|
| `T3.1` | Single mapping, 3 pods match, 1 ASG | Desired state has 3 IPs in one prefix set |
| `T3.2` | Single mapping, pod has no IP yet | Pod excluded from desired state |
| `T3.3` | Two mappings, overlapping selectors, different ASGs | Each ASG gets correct IP set |
| `T3.4` | Same ASG referenced by two mappings with different selectors | IPs are unioned |
| `T3.5` | Cross-subscription: mapping references ASGs in sub-1 and sub-2 | Desired state has entries for both subs |
| `T3.6` | No matching pods | Desired prefix set is empty (not absent — still maintain) |
| `T3.7` | Mapping deleted (not in input) | No desired state for its targets |
| `T3.8` | Diff: desired={A: [ip1,ip2]}, actual={A: [ip1]} | Action: UpdatePrefixSet A |
| `T3.9` | Diff: desired={A: [ip1]}, actual={A: [ip1]} | No actions (no-op) |
| `T3.10` | Diff: desired={}, actual={A: [ip1]} | Action: DeletePrefixSet A |
| `T3.11` | Diff: desired={A: [ip1]}, actual={} | Action: CreatePrefixSet A |
| `T3.12` | Diff: desired={A: [ip1], B: [ip2]}, actual={A: [ip1,ip3], C: [ip4]} | Update A, Create B, Delete C |

### Test Files

- `internal/engine/desired_state_test.go`
- `internal/engine/diff_test.go`

---

## Phase 4: Azure Client Interfaces & Executor

### Goal

Execute the diff actions against Azure ARM. Handle cross-subscription credential routing, ETag-based optimistic concurrency, and parallel execution.

### Step-by-Step Actions

1. **Define interface** in `internal/azure/interfaces.go`:
   ```go
   type AddressPrefixSetAPI interface {
       Get(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) (*AddressPrefixSet, error)
       Put(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string, ips []string) error
       Delete(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) error
       List(ctx context.Context, subscriptionID, resourceGroup, asgName string) ([]AddressPrefixSet, error)
   }
   ```
2. **Refactor existing `AddressPrefixSetClient`** to accept subscription/RG per call (not constructor-time)
3. **Implement cross-subscription client factory** in `internal/azure/client_factory.go`:
   - `ClientFactory` creates/caches per-subscription `AddressPrefixSetAPI` clients
   - Uses managed identity / DefaultAzureCredential
4. **Implement executor** in `internal/azure/executor.go`:
   - `Execute(ctx context.Context, actions []Action) []ActionResult`
   - Executes actions in parallel (bounded concurrency via semaphore)
   - Routes each action to the correct per-subscription client
   - Returns per-action success/failure
5. **Implement ETag concurrency control**:
   - Every GET returns an ETag
   - Every PUT includes `If-Match: <etag>` header
   - On 412 conflict: re-GET, recompute diff for that target, retry (max 3)
6. **Create fake client** in `internal/azure/fake/`:
   - In-memory implementation of `AddressPrefixSetAPI`
   - Supports ETag simulation, error injection

### TDD Acceptance Tests

| Test ID | Test | Assertion |
|---|---|---|
| `T4.1` | Execute CreatePrefixSet action with fake | Fake state updated correctly |
| `T4.2` | Execute UpdatePrefixSet action with fake | Fake state reflects new IPs |
| `T4.3` | Execute DeletePrefixSet action with fake | Fake state entry removed |
| `T4.4` | Execute actions across 2 subscriptions | Correct fake clients used per subscription |
| `T4.5` | ETag conflict on PUT → automatic retry succeeds | PUT retried with fresh ETag, succeeds |
| `T4.6` | ETag conflict exceeds max retries (3) | Action marked as failed with error |
| `T4.7` | Parallel execution of 5 actions | All complete, bounded concurrency respected |
| `T4.8` | One action fails, others succeed | Failed action returns error, others still applied |
| `T4.9` | Client factory caches clients per subscription | Second call for same sub reuses client |
| `T4.10` | PUT with empty IP list | Puts empty prefix set (valid — clears membership) |

### Test Files

- `internal/azure/executor_test.go`
- `internal/azure/client_factory_test.go`
- `internal/azure/fake/fake_client.go`
- `internal/azure/fake/fake_client_test.go`

---

## Phase 5: Controller Wiring — Watches, Enqueue & Resync

### Goal

Wire the desired-state engine and Azure executor into the controller-runtime reconciliation loop. Watch both `PodASGMapping` CRs and Pods; enqueue affected mappings on any change.

### Step-by-Step Actions

1. **Create new reconciler** in `internal/controller/mapping_reconciler.go`:
   - Replace (or wrap) the existing `PodReconciler`
   - `MappingReconciler` reconciles `PodASGMapping` CRs (not individual Pods)
   - Reconcile flow:
     ```
     1. Fetch PodASGMapping CR (if deleted → cleanup owned prefix sets → return)
     2. Ensure finalizer is present
     3. List all Pods in the CR's namespace
     4. Compute desired state (Phase 3 engine)
     5. Fetch actual ARM state for all owned prefix sets (Phase 4 client)
     6. Compute diff (Phase 3 engine)
     7. Execute diff (Phase 4 executor)
     8. Update CR status (Phase 6)
     9. Requeue after resync interval (default: 60s)
     ```
2. **Implement pod event handler** in `internal/controller/pod_handler.go`:
   - `handler.EnqueueRequestsFromMapFunc`: for each pod event, find all PodASGMappings in the pod's namespace whose selectors match the pod's labels → enqueue those mappings
3. **Implement controller setup** in `internal/controller/setup.go`:
   - Primary watch: `PodASGMapping` → enqueue self
   - Secondary watch: `Pod` → via `pod_handler.go` → enqueue matching `PodASGMapping`s
4. **Implement predicates** for event filtering:
   - Pods: only trigger on create, delete, and label/IP changes
   - PodASGMapping: trigger on spec changes, ignore status-only updates
5. **Implement finalizer** (`networking.azure.com/pod-asg-cleanup`):
   - Added on first reconcile
   - On deletion: list all owned addressPrefixSets, delete them, remove finalizer
6. **Update `cmd/main.go`**: register `MappingReconciler` instead of/alongside `PodReconciler`
7. **Configure resync interval**: default 60s, configurable via env var `RESYNC_INTERVAL_SECONDS`

### Watch Configuration

| Resource | Watch Type | Enqueue Logic |
|---|---|---|
| `PodASGMapping` | Primary watch (`.For()`) | Enqueue self |
| `Pod` | Secondary watch (`.Watches()` with `EnqueueRequestsFromMapFunc`) | For each PodASGMapping in the pod's namespace, if pod labels match any mapping selector → enqueue that PodASGMapping |

### TDD Acceptance Tests

| Test ID | Test | Assertion |
|---|---|---|
| `T5.1` | Pod created matching a mapping → mapping is enqueued | Reconcile triggered for the correct mapping |
| `T5.2` | Pod deleted → mapping is enqueued | Reconcile triggered, IP removed from desired state |
| `T5.3` | Pod IP changes → mapping is enqueued | Old IP absent, new IP present in desired state |
| `T5.4` | Pod label changes (no longer matches) → mapping enqueued | IP removed from desired state |
| `T5.5` | PodASGMapping spec updated (new ASG added) | New prefix set created in new ASG |
| `T5.6` | PodASGMapping spec updated (ASG removed) | Owned prefix set deleted from removed ASG |
| `T5.7` | PodASGMapping deleted → finalizer triggers cleanup | All owned prefix sets deleted |
| `T5.8` | Periodic resync fires after interval | Reconcile triggered, drift corrected |
| `T5.9` | Pod status-only update (no label/IP change) | Reconcile NOT triggered (predicate filters it) |
| `T5.10` | Multiple PodASGMappings in same namespace, pod matches one | Only the matching mapping is enqueued |

### Test Files

- `internal/controller/mapping_reconciler_test.go`
- `internal/controller/pod_handler_test.go`

---

## Phase 6: Status Reporting

### Goal

After each reconcile cycle, update the `PodASGMapping` CR status with conditions and per-mapping sync state.

### Step-by-Step Actions

1. **Implement status reducer** in `internal/controller/status.go`:
   - `ComputeStatus(spec PodASGMappingSpec, reconcileResults []ActionResult, podCounts map[string]int) PodASGMappingStatus`
2. **Implement condition helpers** in `internal/controller/conditions.go`:
   - `SetCondition(status *PodASGMappingStatus, condType string, value metav1.ConditionStatus, reason, message string)`
   - Standard conditions: `Accepted`, `Reconciled`
3. **Wire status update** into the reconcile loop (after Phase 5 step 7):
   - Use status subresource update (`client.Status().Update()`)
4. **Implement selector hash** for `selectorHash` field:
   - Deterministic hash of the `matchLabels` map (sorted keys, SHA256, truncated)

### Conditions

| Type | Meaning |
|---|---|
| `Accepted` | The CRD spec is valid and all resource IDs are parseable |
| `Reconciled` | The last reconcile cycle completed (`True` = success, `False` = partial/full failure) |

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

### Test File

- `internal/controller/status_test.go`
- `internal/controller/conditions_test.go`

---

## Phase 7: Error Handling, Resilience & Concurrency Control

### Goal

Harden the controller for production: retry logic, rate limiting, partial failure handling, and graceful degradation.

### Step-by-Step Actions

1. **Implement retry wrapper** in `internal/azure/retry.go`:
   - Exponential backoff with jitter for ARM calls
   - Configurable max retries (default: 3)
   - Respect `Retry-After` header for 429 responses
2. **Implement rate limiter** in `internal/azure/ratelimit.go`:
   - Per-subscription ARM call rate limiting (default: 10 RPS, configurable)
   - Uses `golang.org/x/time/rate` token bucket
3. **Implement reconcile error handler** in `internal/controller/errors.go`:
   - Classify errors: retriable (429, 5xx, network), non-retriable (401, 403, 404-ASG-not-found)
   - Decide requeue strategy per error type
4. **Wire retry into executor** (Phase 4 executor):
   - Wrap each ARM call with the retry wrapper
   - Rate limit before each call
5. **Handle partial failures**:
   - If N actions and M fail: (N-M) succeed and persist; M report errors in status; reconcile requeues with backoff; next cycle recomputes fresh (convergent)

### Retry Policy

| Error Type | Strategy |
|---|---|
| 429 Too Many Requests | Respect `Retry-After` header, requeue |
| 412 Precondition Failed (ETag) | Re-GET, recompute, retry (max 3) |
| 5xx Server Error | Exponential backoff: 1s, 2s, 4s (max 3 retries) |
| 401/403 Auth Error | Do not retry, report error in status |
| Network timeout | Retry once, then requeue with backoff |

### TDD Acceptance Tests

| Test ID | Test | Assertion |
|---|---|---|
| `T7.1` | ARM returns 429 | Call retried after `Retry-After` delay |
| `T7.2` | ARM returns 500 three times then succeeds | Retried with backoff, succeeds on 4th call |
| `T7.3` | ARM returns 500 four times | Gives up, error reported in status |
| `T7.4` | ARM returns 403 | No retry, error reported immediately |
| `T7.5` | 3 of 5 actions fail | 2 succeed and persist, 3 report errors, reconcile requeued |
| `T7.6` | Rate limiter throttles burst of 20 calls | Calls spread over time, none dropped |
| `T7.7` | ETag conflict during retry loop | Re-GET + recompute succeeds |

### Test Files

- `internal/azure/retry_test.go`
- `internal/azure/ratelimit_test.go`
- `internal/controller/errors_test.go`

---

## Phase 8: Integration & E2E Testing

### Goal

Validate the complete system with envtest-based integration tests and optional live Azure E2E tests.

### Step-by-Step Actions

1. **Create test infrastructure**:
   - `test/integration/` — envtest suite with CRDs and fake Azure
   - `test/e2e/` — optional live Azure E2E (CI-gated)
   - `test/fixtures/` — sample CRDs, pod manifests
2. **Implement envtest suite setup**:
   - Bootstrap envtest with PodASGMapping CRD
   - Inject fake `AddressPrefixSetAPI` client
   - Register `MappingReconciler`
3. **Write integration test scenarios** (see table below)
4. **Write E2E test scenarios** (gated behind `AZURE_E2E=true` env var)
5. **Add CI pipeline configuration** for integration tests

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

### Test Files

- `test/integration/suite_test.go`
- `test/integration/reconcile_test.go`
- `test/e2e/e2e_test.go`
- `test/fixtures/sample_mapping.yaml`
- `test/fixtures/sample_pods.yaml`

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

```go
// Grouped by phase:
func TestPhase2_ParseValidASGResourceID(t *testing.T) { ... }

// Or grouped in suites:
func TestDesiredStateEngine(t *testing.T) {
    t.Run("single mapping with matching pods", func(t *testing.T) { ... })
    t.Run("cross-subscription ASG references", func(t *testing.T) { ... })
}
```

---

## Appendix C: Configuration Reference

| Environment Variable | Required | Default | Description |
|---|---|---|---|
| `CLUSTER_NAME` | Yes | — | Unique identifier for this cluster (used in ownership keys) |
| `AZURE_SUBSCRIPTION_ID` | No | — | Default subscription (overridden by per-ASG resource IDs) |
| `AZURE_RESOURCE_GROUP` | No | — | Default resource group (overridden by per-ASG resource IDs) |
| `RESYNC_INTERVAL_SECONDS` | No | `60` | Periodic resync interval |
| `ARM_RATE_LIMIT_RPS` | No | `10` | Per-subscription ARM API rate limit (requests/second) |
| `MAX_CONCURRENT_ACTIONS` | No | `5` | Maximum parallel ARM calls per reconcile cycle |

> **Note:** `AZURE_NSG_NAME` from the current config is no longer needed — the new design manages ASG addressPrefixSets, not NSG rules directly.

---

## Phase Dependency Graph

```
Phase 1 (CRD Types)
  └──► Phase 2 (Domain Model)
         └──► Phase 3 (Desired-State Engine)
                └──► Phase 4 (Azure Executor)
                       └──► Phase 5 (Controller Wiring)  ◄── Phase 3
                              └──► Phase 6 (Status Reporting)
                                     └──► Phase 7 (Error Handling)
                                            └──► Phase 8 (Integration & E2E)
```

Each phase builds on the previous. Phases 1–3 are pure logic with no I/O. Phase 4 introduces Azure interaction (behind interfaces). Phase 5 wires everything together. Phases 6–7 add production hardening. Phase 8 validates the complete system.
