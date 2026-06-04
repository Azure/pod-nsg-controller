# Scope Report: Phase 6 - Tunable ARM Concurrency

## Spec alignment
- Source: `docs/PERFORMANCE-IMPROVEMENTS.md`
- Relevant section: **Phase 6: Tunable ARM Concurrency** (`docs/PERFORMANCE-IMPROVEMENTS.md:351-390`)
- Deliverables called out by spec:
  - Raise defaults to `MAX_CONCURRENT_ACTIONS=10`, `ARM_RATE_LIMIT_RPS=20`
  - Make ARM concurrency tunable at runtime (no restart)
  - Add/verify ARM metrics for execution latency/concurrency/conflicts
  - Update config reference in `docs/SPECIFICATION.md` Appendix C

## Repository/package layout (relevant)
- `cmd/`: process bootstrap and dependency wiring (`cmd/main.go`)
- `internal/config/`: env parsing and defaults (`config.Load`)
- `internal/azure/`: executor, ARM client, per-subscription rate limiter, metrics option wiring
- `internal/controller/`: reconcile callsite that invokes executor (`MappingReconciler`)
- `internal/metrics/`: Prometheus collector definitions and registration
- `api/`: CRD types
- `test/integration/`: phase integration tests (no dedicated tunable-concurrency phase coverage found)

## Root cause analysis
Current ARM concurrency/rate settings are **startup-fixed**, not dynamically tunable:

1. `internal/config/config.go:Load()` sets defaults to `MaxConcurrentActions=5` and `ARMRateLimitRPS=10`.
2. `cmd/main.go:86-105` reads config once, then constructs:
   - `azure.NewARMRateLimiter(..., cfg.ARMRateLimitRPS, ...)`
   - `azure.NewExecutor(..., cfg.MaxConcurrentActions, ...)`
3. `internal/azure/executor.go` stores `maxParallel int` on `Executor`; `Execute()` creates `sem := make(chan struct{}, e.maxParallel)` each run with no runtime update path.
4. `internal/azure/ratelimit.go` stores fixed `rps/burst` and per-subscription `rate.Limiter`s; interface only exposes `Wait(...)`, with no update/reconfigure contract.

So the spec’s runtime-tuning requirement is blocked by constructor-only configuration plus interfaces that do not support mutable runtime limits.

## Affected code path trace
1. Startup config load: `internal/config/config.go:Load()`
2. Wiring at process start: `cmd/main.go:61-105`
3. Executor callsite in reconcile: `internal/controller/mapping_reconciler.go:480-483`
4. Bounded parallel ARM execution: `internal/azure/executor.go:46-115`
5. ARM request throttling path:  
   - `internal/azure/client_factory.go:253-255` wires limiter into clients  
   - `internal/azure/address_prefix_set_client.go:149-153` calls `rateLimiter.Wait(...)` per request  
   - `internal/azure/ratelimit.go:159-210` enforces per-subscription token-bucket waits

## Interfaces/contracts involved
- `internal/controller/mapping_reconciler.go`
  - `type Executor interface { Execute(ctx, actions) []azure.ActionResult }`
  - No contract for updating concurrency limits at runtime.
- `internal/azure/ratelimit.go`
  - `type SubscriptionRateLimiter interface { Wait(ctx, subscriptionID) error }`
  - No contract for runtime RPS reconfiguration.
- `internal/azure/interfaces.go`
  - `AddressPrefixSetClientFactory` / `AddressPrefixSetAPI` consumed by executor.

## Existing metrics and gap vs Phase 6 spec
- Already present:
  - `pod_nsg_controller_arm_request_duration_seconds` in `internal/metrics/arm.go`, emitted by `internal/azure/address_prefix_set_client.go`
  - Retry/rate-limit metrics via `arm_retries_total`, `arm_rate_limit_delays_total`, `arm_rate_limit_delay_seconds`
- Missing from Phase 6 deliverables:
  - `arm_call_duration_seconds` (executor-level action metric)
  - `arm_concurrent_actions` (executor in-flight gauge)
  - `arm_etag_conflicts_total` (dedicated conflict counter; currently only represented indirectly as retry reason `etag-conflict`)

## Logging and error-handling patterns near the change
- Logging:
  - controller-runtime path uses `logr` (`cmd/main.go` sets `zapr` backend)
  - Azure modules use `*zap.Logger` with structured fields (`executor.go`, `address_prefix_set_client.go`, `ratelimit.go`)
- Error handling:
  - wrapping via `github.com/pkg/errors` (`errors.Wrap`, `errors.Wrapf`) is the dominant pattern in config/controller/Azure modules
  - sentinel/error classification in Azure path (`IsNotFound`, `IsPreconditionFailed`) should remain intact

## Existing modules to modify (prefer existing files)
1. `internal/config/config.go`  
   - Change defaults: `ARMRateLimitRPS` to `20`, `MaxConcurrentActions` to `10`
2. `cmd/main.go`  
   - Keep wiring centralized; add runtime-tuning hook orchestration here (or invoke tuner from here) so reconciler wiring stays unchanged
3. `internal/azure/executor.go`  
   - Add executor-level metrics (`arm_call_duration_seconds`, `arm_concurrent_actions`, `arm_etag_conflicts_total`)
   - Add runtime-safe tunable concurrency mechanism (mutable limit path)
4. `internal/azure/ratelimit.go`  
   - Add runtime-safe tunable RPS path for existing limiter instance
5. `internal/azure/metrics_options.go`  
   - Extend option/wiring surface for new executor-level metric observer(s)
6. `internal/metrics/arm.go` and `internal/metrics/metrics.go`  
   - Define/register/rebind new ARM metric collectors
7. `internal/azure/address_prefix_set_client.go`  
   - Validate/adjust operation labeling for `arm_request_duration_seconds` to match Phase 6 expectation (GET/PUT/DELETE labeling contract)
8. `docs/SPECIFICATION.md`  
   - Update Appendix C defaults (`ARM_RATE_LIMIT_RPS=20`, `MAX_CONCURRENT_ACTIONS=10`)

## Existing tests to extend
- `internal/config/config_resilience_test.go`
  - `TestLoad_ARMRateLimitRPS_Default10` -> update expected default
  - `TestLoad_MaxConcurrentActions_Default5` -> update expected default
- `internal/config/config_test.go`
  - add a focused defaults test matching Phase 6 acceptance (`TestLoad_Defaults`)
- `internal/azure/executor_test.go`
  - add Phase 6 metric assertions (`TestExecutor_Metrics`) for action duration + ETag conflict counter + concurrent gauge behavior
- `internal/azure/address_prefix_set_client_test.go`
  - add `TestClient_RequestMetrics` coverage for GET/PUT/DELETE operation labels on request-duration metric
- `internal/metrics/arm_test.go`
  - extend collector-level tests for new metric names and label sets
- `internal/azure/metrics_wiring_test.go` and/or `internal/azure/arm_prometheus_integration_test.go`
  - extend end-to-end wiring assertions for new executor metrics

## New files assessment
- **No new files are required for production logic.**  
  Existing config, Azure runtime, metrics, and wiring modules are the correct extension points.
- New files should only be considered if introducing an explicit runtime tuning component cannot be cleanly embedded in existing `cmd/main.go` + `internal/azure/*` modules.

## Related spec sections
- `docs/PERFORMANCE-IMPROVEMENTS.md:351-390` (Phase 6 problem/design/changes/acceptance/performance target)
- `docs/PERFORMANCE-IMPROVEMENTS.md:439-444` (appendix defaults already reflecting Phase 6 target values)
- `docs/SPECIFICATION.md:844-854` (Appendix C currently still documents old defaults)

## Risk assessment
1. **Behavioral risk:** increasing concurrency/RPS can increase ARM throttling or transient 429/5xx rates if tuning is too aggressive.
2. **Correctness risk:** runtime mutability of concurrency/rate limits requires thread-safe update semantics under active reconciles.
3. **Metrics cardinality risk:** avoid introducing high-cardinality labels when adding executor-level metrics.
4. **Backward-compat risk:** metric name changes can break dashboards/alerts; additions should be additive and documented.
5. **Controller stability risk:** bad runtime tuning inputs must validate and fail loudly without silent fallback.
