# 07 — Metrics Design

> Package: `internal/metrics` · Prometheus metrics subsystem for observability

## Overview

The metrics package provides a comprehensive Prometheus-based observability layer for the pod-nsg-controller. It is organized into sub-recorders, each owning a logical group of metrics, and tracker types that maintain stateful counters and timing data across reconcile cycles.

All metrics are registered with the controller-runtime Prometheus registry via `RegisterWith()`, ensuring they appear alongside standard controller-runtime metrics at the `/metrics` endpoint.

## Component Map

```
internal/metrics/
├── metrics.go          # Recorder singleton, RegisterWith(), AllCollectors()
├── pod_churn.go        # PodChurnRecorder + PodChurnTracker — pod IP change tracking
├── arm.go              # ARMRecorder — ARM request duration, retries, rate limits
├── convergence.go      # ConvergenceRecorder + ConvergenceTracker — prefix set convergence timing
├── reconcile.go        # ReconcileRecorder + InitialReconcileTracker — reconcile lifecycle
├── queue.go            # Instrumented workqueue wrapper + delayed-queue depth sampler
└── cleanup_stubs.go    # Cleanup helpers (HasPending, HasSnapshot, ForgetWithDelete)
```

## Top-Level Recorder

```go
type Recorder struct {
    PodChurn    *PodChurnRecorder
    ARM         *ARMRecorder
    Convergence *ConvergenceRecorder
    Reconcile   *ReconcileRecorder
}
```

### Singleton Construction

```go
func Register() (*Recorder, error)     // idempotent, first call creates
func MustRegister() *Recorder          // panics on error (test convenience)
```

### Registry Wiring

```go
func RegisterWith(registerer prometheus.Registerer) (*Recorder, error)
```

Registers all sub-recorder collectors with the provided Prometheus registerer. On `AlreadyRegisteredError`, rebinds recorder fields to existing collectors (safe for envtest multi-manager scenarios).

## Pod Churn Recorder

Tracks pod IP address changes per namespace and mapping.

### Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `pod_ip_changes_total` | Counter | `namespace`, `mapping`, `operation` | Total pod IP add/remove events |
| `pod_churn_rate` | Gauge | `namespace`, `mapping` | Current churn rate (changes/window) |

### PodChurnTracker

Stateful tracker that computes churn rates using a sliding window:

```go
type PodChurnTracker struct { ... }
```

Functions:
- `HasSnapshot(namespace, mapping)` — check if a baseline snapshot exists
- `ForgetWithDelete(namespace, mapping)` — forget tracker state and delete gauge label set

## ARM Recorder

Tracks all Azure ARM API interactions.

### Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `arm_requests_total` | Counter | `subscription_id`, `operation`, `status_code` | Total ARM requests |
| `arm_request_duration_seconds` | Histogram | `subscription_id`, `operation` | Per-request latency |
| `arm_retries_total` | Counter | `subscription_id`, `operation`, `retry_reason` | Retry events |
| `arm_rate_limit_delays_total` | Counter | `subscription_id` | Rate limiter wait events |
| `arm_rate_limit_delay_seconds` | Histogram | `subscription_id` | Rate limiter wait duration |
| `arm_call_duration_seconds` | Histogram | `subscription_id`, `operation` | End-to-end call duration (including retries) |
| `arm_concurrent_actions` | Gauge | — | Currently in-flight ARM actions |
| `arm_etag_conflicts_total` | Counter | `subscription_id`, `operation` | ETag 412 conflict events |

## Convergence Recorder

Tracks how long it takes for desired state to converge in Azure.

### Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `prefix_set_convergence_seconds` | Histogram | `subscription_id`, `resource_group`, `asg_name`, `operation` | Time from desired-state computation to ARM confirmation |
| `prefix_set_drift_corrections_total` | Counter | `subscription_id`, `resource_group`, `asg_name` | Drift corrections (actual ≠ desired on resync) |
| `prefix_set_actions_total` | Counter | `operation`, `result` | Total prefix set actions by outcome |

### ConvergenceTracker

Stateful tracker for convergence timing with generation-aware state management:

```go
type ConvergenceTracker struct { ... }
```

Functions:
- `Forget(target)` — remove tracking state for a target
- `ForgetGeneration(target, generation)` — remove state for a specific generation
- `PruneStaleGenerations(target, currentGen)` — clean up old generation tracking
- `ForgetTarget(target)` — full cleanup including metric label sets
- `HasPending(target)` — check if convergence is being tracked

## Reconcile Recorder

Tracks reconcile cycle lifecycle and performance.

### Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `reconcile_duration_seconds` | Histogram | `namespace`, `mapping`, `result` | Per-reconcile latency |
| `reconcile_total` | Counter | `namespace`, `mapping`, `result` | Total reconcile cycles by outcome |
| `reconcile_queue_depth` | Gauge | — | Current reconcile work queue depth |
| `reconcile_inflight` | Gauge | — | Currently executing reconciles |
| `reconcile_actions_per_cycle` | Histogram | `namespace`, `mapping` | ARM actions generated per reconcile |
| `initial_reconcile_duration_seconds` | Gauge | — | Time for first reconcile after startup |
| `initial_reconcile_complete` | Gauge | — | 1 when initial reconcile is done |
| `crd_resolution_duration_seconds` | Histogram | `namespace`, `mapping`, `operation` | CRD fetch/resolve latency |

### InitialReconcileTracker

Tracks the first reconcile cycle after controller startup:

```go
type InitialReconcileTracker struct { ... }
```

Used by `InitialReconcileInitializer` in the controller package to emit startup timing metrics and gate readiness.

### Cleanup

- `DeleteForMapping(namespace, mapping)` — deletes all per-mapping metric series when a `PodASGMapping` is removed

## Queue Instrumentation

`queue.go` provides an instrumented workqueue wrapper:

### Instrumented Queue

Wraps the controller-runtime workqueue to track queue depth accurately:

- `AddAfter()` — avoids inflating the depth gauge for delayed items
- `startDepthSampler()` — polls `Len()` every 50ms to catch delayed items becoming ready

This ensures `reconcile_queue_depth` reflects the true number of items ready for processing, not items waiting in delay.

## Cleanup Stubs

`cleanup_stubs.go` provides convenience functions for metric state cleanup:

- `ConvergenceTracker.HasPending(target)` — check if any convergence is being tracked
- `PodChurnTracker.HasSnapshot(namespace, mapping)` — check for baseline snapshot
- `PodChurnTracker.ForgetWithDelete(namespace, mapping)` — forget state and delete `pod_churn_rate` gauge series

These are used during `PodASGMapping` deletion to prevent stale metric series from accumulating.

## Wiring in main.go

```go
rec, _ := metrics.RegisterWith(ctrlmetrics.Registry)

// Trackers for stateful metric tracking
initialTracker := rec.Reconcile.NewInitialReconcileTracker()
podChurnTracker := rec.PodChurn.NewPodChurnTracker()
convergenceTracker := rec.Convergence.NewConvergenceTracker()

// Inject into reconciler
reconciler := &controller.MappingReconciler{
    MetricsRecorder:    rec,
    ConvergenceTracker: convergenceTracker,
    InitialTracker:     initialTracker,
    // ...
}

// Inject into Azure layer via options
executor := azure.NewExecutor(log, factory, maxParallel,
    azure.WithExecutorMetrics(rec.ARM),
    azure.WithExecutorRetryMetrics(rec.ARM),
)
rateLimiter := azure.NewARMRateLimiter(log, rps,
    azure.WithRateLimitMetrics(rec.ARM),
)
```
