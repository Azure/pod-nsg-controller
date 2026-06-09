# 08 — Testing & Infrastructure Design

> Packages: `internal/testing`, `cmd/testops`, `test/integration`, `scripts/poc` · Test utilities, smoke tests, integration phases, PoC provisioning

## Overview

The pod-nsg-controller uses a layered validation strategy:

- **Unit tests** live next to production packages and rely heavily on fake Azure clients.
- **Integration tests** use controller-runtime's `envtest` framework and are organized into progressive phases.
- **`cmd/testops`** is a standalone smoke-test binary for validating Azure operations outside Kubernetes.
- **`scripts/poc`** provisions real multi-cluster Azure environments for manual end-to-end validation.

This keeps fast logic tests close to the code, pushes controller wiring through a real API server in-process, and still leaves a path for validating real Azure networking and identity behaviour.

## Test Utilities

### Package: `internal/testing/controllertest`

Test-only helpers for controller acceptance and status validation. The package is intentionally split out of production code so Phase 7 assertions do not leak testing scaffolding into the shipped controller paths.

#### Types

```go
type PartialFailureReport struct {
    Summary          controller.ActionFailureSummary
    SucceededTargets []string
    FailedTargets    []string
    RequeueResult    ctrl.Result
    StatusSnapshot   v1alpha1.PodASGMappingStatus
}

type StatusContractViolation struct {
    Field   string
    Got     string
    Want    string
    Message string
}
```

#### Functions

| Function | Description |
|----------|-------------|
| `BuildPartialFailureReport(...)` | Classifies action results, derives succeeded/failed targets, computes the requeue result, and snapshots the resulting CR status |
| `ValidatePhase7StatusContract(...)` | Verifies the Phase 7 partial-failure status contract: `Reconciled` condition behaviour, mapping row sync states, and error propagation |

These helpers are consumed by `test/integration/phase7/` to keep failure-contract assertions consistent across the main and extended resilience suites.

## Testops Binary

### Package: `cmd/testops`

A standalone CLI binary for smoke-testing Azure SDK and REST integration without starting the controller:

```
cmd/testops/
└── main.go    # Smoke-test binary for ASG + AddressPrefixSet operations
```

### What It Tests

| Client | Operations |
|--------|------------|
| ASG Client | `Get`, `UpdateTags`, `List`, `ListAll`, `CreateOrUpdate`, `Delete` |
| AddressPrefixSet Client | `Put`, `Get`, `List`, `Delete` |

The binary executes the operations sequentially, records each outcome in a `testResult` struct, logs a final summary, and exits non-zero if any check fails.

### Configuration

| Variable | Description |
|----------|-------------|
| `AZURE_SUBSCRIPTION_ID` | Target Azure subscription |
| `AZURE_RESOURCE_GROUP` | Target Azure resource group |
| `TEST_ASG_NAME` | Existing ASG used for GET/PATCH/LIST validation |

### Usage

```bash
# Run directly
go run ./cmd/testops

# Or build first
go build -o testops ./cmd/testops
./testops
```

This is useful when validating Azure credentials, API reachability, or recent client changes independently of Kubernetes reconciliation.

## Integration Test Phases

### Framework

Integration coverage is built on controller-runtime's `envtest` package, which starts a real kube-apiserver and etcd process in-process. Each phase exercises a broader slice of the system, from scheme registration through metrics emission and partial-failure contracts.

Most phase suites also bootstrap `KUBEBUILDER_ASSETS` in `TestMain` so they can discover local envtest binaries consistently.

### Package: `test/integration/testutil`

```
// manager.go — shared envtest manager helper
func NewEnvtestManager(t *testing.T, cfg *rest.Config, scheme *runtime.Scheme) ctrl.Manager
```

`NewEnvtestManager` creates a controller-runtime manager with loopback-only metrics and health probe listeners (`BindAddress: "0"`). This prevents integration suites from colliding on the default `:8080` / `:8081` listeners when multiple envtest managers are created in the same test process.

### Phase Structure

| Phase | Directory | Focus | Description |
|-------|-----------|-------|-------------|
| 1 | `phase1/` | Scheme & API Server | CRD registration, scheme composition, envtest bootstrap |
| 2 | `phase2/` | Model | Resource ID parsing, ownership keys, selector/index behaviour |
| 3 | `phase3/` | Engine | Desired-state computation, diffing, patch-threshold decisions |
| 4 | `phase4/` | Executor | Azure client factory paths, ETag retry, retry/rate-limit behaviour, bounded execution |
| 5 | `phase5/` | Controller Wiring | Full reconciler wiring, pod-triggered reconcile flow, cross-module integration |
| 6 | `phase6/` | Status Reporting | Status updater semantics, condition management, stale/conflict handling, manager helper behaviour |
| 7 | `phase7/` | Error Resilience | Partial failures, failure classification, requeue decisions, status contract validation |
| 8 | `phase8/` | Observability | Metrics recorder wiring, counters/histograms, queue depth, convergence tracking |

### Test Files

```
test/integration/
├── phase1/
│   └── scheme_integration_test.go
├── phase2/
│   └── model_integration_test.go
├── phase3/
│   └── engine_integration_test.go
├── phase4/
│   └── executor_integration_test.go
├── phase5/
│   ├── controller_wiring_integration_test.go
│   └── cross_module_integration_test.go
├── phase6/
│   └── status_reporting_integration_test.go
├── phase7/
│   ├── error_resilience_integration_test.go
│   └── error_resilience_extended_integration_test.go
├── phase8/
│   └── observability_metrics_integration_test.go
└── testutil/
    └── manager.go
```

### Running Integration Tests

```bash
# Canonical target
make test

# Run a single phase directly
go test ./test/integration/phase5/... -v

# Explicit envtest assets if needed
KUBEBUILDER_ASSETS=$(setup-envtest use -p path) go test ./test/integration/... -v
```

The `Makefile` test target installs envtest assets, runs unit coverage and envtest coverage separately, and merges them into `cover.out`.

## PoC Provisioning Scripts

### Package: `scripts/poc`

Shell scripts for provisioning and validating a real multi-cluster proof-of-concept environment in Azure:

```
scripts/poc/
├── setup-cluster-westus2.sh        # Provision West US 2 cluster
├── setup-cluster-eastus2.sh        # Provision East US 2 cluster
├── setup-cluster-eastus2euap.sh    # Provision East US 2 EUAP cluster
├── setup-cross-sub-rbac.sh         # Grant mesh cross-subscription RG access
├── validate-poc.sh                 # Validate cluster health, IMDS, ARM access
└── teardown-poc.sh                 # Delete RGs and kubeconfigs
```

### Setup Flow

```
1. setup-cluster-*.sh (three regions)
   ├── Create RG, VNet, subnet, NAT gateway, and VMs
   ├── Enable managed identity on cluster VMs
   ├── Install kubeadm / kubelet / kubectl
   ├── Install Azure CNI
   ├── Configure azure0 bridge and routing
   ├── Configure IMDS masquerade for pod identity access
   └── Export kubeconfig

2. setup-cross-sub-rbac.sh
   ├── Collect VM managed identity principal IDs
   └── Grant Network Contributor on each cluster RG to all cluster identities

3. validate-poc.sh
   ├── Check node readiness and core system pods
   ├── Launch a pod to verify IMDS reachability
   └── Execute cross-subscription ARM calls from inside each cluster

4. teardown-poc.sh
   └── Delete resource groups and local kubeconfig files
```

### When to Use

These scripts are for **manual validation**, not CI:

- validating cross-subscription ASG management
- exercising real Azure CNI networking paths
- verifying IMDS-based token acquisition from pods
- testing multi-region or mixed-subscription scenarios

## Fake Client for Unit and Integration Tests

### Package: `internal/azure/fake`

The fake Azure package provides an in-memory `AddressPrefixSetAPI` implementation and a subscription-scoped fake factory:

```go
type Client struct {
    // Thread-safe in-memory prefix set store + error injection state
}

type ClientFactory struct {
    // Returns registered fake clients by subscription
}
```

### Capabilities

- thread-safe storage guarded by `sync.RWMutex`
- deterministic ETag/version tracking for optimistic-concurrency tests
- per-operation, per-resource error injection via `InjectKey`
- GET call counters and peek helpers for test observability
- subscription-specific fake factory wiring for executor/controller tests

### Usage Pattern

```go
fakeClient := fake.NewClient()
fakeFactory := fake.NewClientFactory()
fakeFactory.RegisterClient("sub-id", fakeClient)

_ = fakeClient.InjectError(fake.InjectKey{
    Operation:      fake.OperationPut,
    SubscriptionID: "sub-id",
    ResourceGroup:  "rg",
    ASGName:        "asg",
    PrefixSetName:  "prefix-set",
}, someError, 1)

executor := azure.NewExecutor(log, fakeFactory, 5)
```

This fake layer underpins the fast test path for executor, controller, and integration suites that need deterministic Azure behaviour without calling ARM.
