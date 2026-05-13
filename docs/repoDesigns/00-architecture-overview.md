# Pod NSG Controller — Architecture Overview

## Purpose

The **pod-nsg-controller** is a Kubernetes controller that manages Azure Application Security Group (ASG) membership for pods. It watches `PodASGMapping` custom resources and pods, computes the desired set of IP addresses per ASG, and synchronises that state with Azure via ARM Address Prefix Sets.

## High-Level Flow

```
┌──────────────────────────────────────────────────────────────────┐
│                     Kubernetes Cluster                            │
│                                                                  │
│  PodASGMapping CR ──────────┐                                    │
│  (spec: selectors → ASGs)   │   watches                         │
│                             ▼                                    │
│               ┌─────────────────────────┐                        │
│  Pod events ──│   MappingReconciler     │                        │
│               │   (controller-runtime)  │                        │
│               └──────────┬──────────────┘                        │
│                          │                                       │
└──────────────────────────┼───────────────────────────────────────┘
                           │
              ┌────────────▼────────────┐
              │   Desired-State Engine  │
              │  ComputeDesiredState()  │
              │  ComputeDiff()          │
              └────────────┬────────────┘
                           │  Actions[]
              ┌────────────▼────────────┐
              │      ARM Executor       │
              │  (bounded concurrency)  │
              │  ETag retry on 412      │
              └────────────┬────────────┘
                           │
              ┌────────────▼────────────┐
              │  Azure ARM REST API     │
              │  Address Prefix Sets    │
              │  (per-ASG child resources)│
              └─────────────────────────┘
```

## Package Layout

```
pod-nsg-controller/
├── api/v1alpha1/            # CRD types: PodASGMapping, status, conditions
├── cmd/main.go              # Entry point — wires all components
├── internal/
│   ├── azure/               # ARM REST client, retry, rate limiting, executor
│   │   └── fake/            # In-memory fake client for tests
│   ├── config/              # Environment-based configuration
│   ├── controller/          # Reconciler, status updater, predicates, pod handler
│   ├── engine/              # Desired-state computation and diffing
│   └── model/               # Resource ID parsing, ownership keys, selectors
├── test/integration/        # Per-phase integration tests (envtest)
├── config/                  # Kubernetes manifests (CRD, RBAC, manager)
└── docs/                    # Specification and design documents
```

## Component Dependency Graph

```
cmd/main.go
  ├── config.Load()
  ├── azure.NewClientFactory()
  ├── azure.NewARMRateLimiter()
  ├── azure.NewExecutor()
  ├── controller.NewMappingStatusUpdater()
  └── controller.MappingReconciler
        ├── uses engine.ComputeDesiredState()
        ├── uses engine.ComputeDiff()
        ├── uses azure.Executor.Execute()
        └── uses controller.MappingStatusUpdater
              └── uses controller.ComputeStatus()
```

## Key Design Principles

1. **Desired-state convergence** — Every reconcile computes the full desired state and diffs it against Azure. The controller converges towards the desired state, never applying incremental patches.

2. **Ownership-based cleanup** — Each `PodASGMapping` tracks the ASGs it owns via a JSON annotation (`networking.azure.com/owned-asgs.v1`). On deletion, only owned resources are cleaned up.

3. **ETag-based optimistic concurrency** — ARM PUT operations use `If-Match` / `If-None-Match` headers. On 412 conflicts, the executor re-fetches, recomputes, and retries.

4. **Cross-subscription support** — ASG resource IDs can reference any Azure subscription. The `ClientFactory` caches per-subscription ARM clients with lazy credential resolution.

5. **Testability via interfaces** — All Azure operations go through `AddressPrefixSetAPI`. Tests use `fake.Client` for deterministic, in-memory behaviour.

6. **Structured logging** — All code uses `go.uber.org/zap`. Controller-runtime code uses `logr.Logger` backed by `zapr`. Azure clients use `*zap.Logger` directly.

## Reconciliation Lifecycle

```
   Reconcile(request)
        │
        ├── 1. Fetch PodASGMapping
        ├── 2. Handle deletion (cleanup owned prefix sets, remove finalizer)
        ├── 3. Ensure cleanup finalizer
        ├── 4. List pods in namespace
        ├── 5. Compute matched pods per mapping rule
        ├── 6. Validate ASG resource IDs → if invalid, write status, return nil
        ├── 7. Write Pending status
        ├── 8. Compute desired state (engine)
        ├── 9. Load ownership annotation
        ├──10. Fetch actual state from Azure
        ├──11. Compute diff → Actions[]
        ├──12. Pre-update ownership annotation
        ├──13. Execute actions (Azure ARM)
        ├──14. Post-update ownership annotation
        ├──15. Write final status (Synced / Error per row)
        └──16. Return RequeueAfter(resyncInterval) or error-based requeue
```

## Status Model

Each `PodASGMapping` has two conditions and per-mapping-rule status rows:

| Condition    | Meaning |
|-------------|---------|
| `Accepted`  | Spec validation passed (`True`) or failed (`False`) |
| `Reconciled`| Azure sync succeeded (`True`), in progress (`Unknown`), or failed (`False`) |

Each `MappingStatus` row tracks:
- `SelectorHash` — deterministic hash of the pod selector labels
- `MatchedPods` — number of pods matching the selector
- `ASGSyncState` — `Pending`, `Synced`, or `Error`
- `LastSyncTime` — last successful sync timestamp
- `Error` — error message (when `ASGSyncState = Error`)

## Configuration

All configuration is read from environment variables:

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `CLUSTER_NAME` | Yes | — | Unique cluster identity (lowercase) |
| `AZURE_SUBSCRIPTION_ID` | No | — | Default Azure subscription |
| `AZURE_RESOURCE_GROUP` | No | — | Default Azure resource group |
| `RESYNC_INTERVAL_SECONDS` | No | `60` | Periodic resync interval |
| `ARM_RATE_LIMIT_RPS` | No | `10` | Per-subscription ARM rate limit |
| `MAX_CONCURRENT_ACTIONS` | No | `5` | Max parallel ARM mutations |

## Related Design Documents

| Document | Description |
|----------|-------------|
| [01 — API Types](01-api-types.md) | CRD types, spec, status, and conditions |
| [02 — Controller](02-controller.md) | Reconciler, status updater, predicates, pod handler |
| [03 — Azure Client](03-azure-client.md) | ARM REST client, retry, rate limiting, executor |
| [04 — Engine](04-engine.md) | Desired-state computation and diff algorithm |
| [05 — Model & Config](05-model-and-config.md) | Resource ID parsing, ownership keys, config loading |
| [06 — Error Handling](06-error-handling.md) | Error classification, requeue strategy, resilience |
