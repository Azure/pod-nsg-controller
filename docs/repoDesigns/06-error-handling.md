# 06 — Error Handling Design

> Package: `internal/controller` (errors.go) + `internal/azure` (retry.go) · Error classification, requeue strategy, resilience

## Overview

The error handling system classifies failures into retriable and non-retriable categories, translates Azure ARM errors into requeue decisions, and ensures partial failures are surfaced in CR status while successful actions persist.

## Error Classification

### Error Classes

```go
type ErrorClass string

const (
    ErrorClassRetriable    ErrorClass = "Retriable"
    ErrorClassNonRetriable ErrorClass = "NonRetriable"
)
```

### Classification Rules

| Azure Response | Error Class | Retry? | Requeue? |
|---------------|-------------|--------|----------|
| 429 (Too Many Requests) | Retriable | Yes (with Retry-After) | Yes |
| 500, 502, 503, 504 | Retriable | Yes (backoff) | Yes |
| 408 (Timeout) | Retriable | Yes (backoff) | Yes |
| Network errors | Retriable | Yes (limited) | Yes |
| 401, 403 (Auth) | Non-retriable | No | Yes (with error) |
| 404 (Not Found) | Non-retriable | No | Depends on context |
| 412 (Precondition Failed) | — | ETag retry only | — |
| Validation failures | Non-retriable | No | No (deterministic) |

### `ClassifiedActionFailure`

```go
type ClassifiedActionFailure struct {
    Result         azure.ActionResult
    Class          ErrorClass
    Reason         string
    RetryAfterHint time.Duration
}
```

### `ActionFailureSummary`

```go
type ActionFailureSummary struct {
    Failures          []ClassifiedActionFailure
    RetriableCount    int
    NonRetriableCount int
    MaxRetryAfterHint time.Duration
}
```

## Requeue Strategy

### `RequeuePolicy`

```go
type RequeuePolicy struct {
    ResyncInterval            time.Duration // normal resync (default: 60s)
    ExhaustedRetryBackoff     time.Duration // after retries exhausted (default: 8s)
    MaxRetryAfterRequeueDelay time.Duration // cap on Retry-After (default: 5m)
}
```

### Requeue Decision Functions

```go
func DecideRequeueFromActionSummary(summary ActionFailureSummary, policy RequeuePolicy) ctrl.Result
func DecideRequeueFromSystemError(err error, policy RequeuePolicy) ctrl.Result
```

#### Action Failure Requeue Logic

```
If has retriable failures with RetryAfterHint:
  → RequeueAfter = min(MaxRetryAfterHint, MaxRetryAfterRequeueDelay)

If has retriable failures without hint:
  → RequeueAfter = ExhaustedRetryBackoff

If only non-retriable failures:
  → RequeueAfter = ResyncInterval (normal resync)
```

#### System Error Requeue Logic

```
If error has RetryAfterHint (from 429):
  → RequeueAfter = min(hint, MaxRetryAfterRequeueDelay)

Otherwise:
  → RequeueAfter = ExhaustedRetryBackoff
```

## Reconciler Error Handling Patterns

### Validation Failures

Deterministic spec errors (malformed ASG resource IDs):
- Log the error
- Write status with `Accepted=False`
- Return `nil` (no requeue — watch events trigger on spec change)

### System/Pre-Action Failures

Infrastructure errors before Azure operations (e.g., listing pods, parsing annotations):
- Write final status with `Reconciled=False` (if `StatusUpdater != nil`)
- Return requeue decision based on error classification
- Status write failure takes precedence over requeue

### Action Failures (Partial)

When some actions succeed and others fail:
1. Successes persist (Azure state is updated)
2. Failures are reported per-row in `MappingStatus.Error`
3. `Reconciled=False` condition is set
4. Requeue is based on `DecideRequeueFromActionSummary`
5. Next reconcile recomputes full diff — successes are no-ops, failures retry

### Status Write Failures

Error precedence rule: if `UpdateAfterReconcile` fails (non-stale, non-notfound), that error is returned directly and never masked by requeue decisions.

## ARM Retry Flow

```
doRequest(ctx, retryCtx, method, url, body, headers)
│
├─ Attempt 1:
│   ├─ rateLimiter.Wait()
│   ├─ HTTP request
│   └─ parseARMError()
│
├─ DecideRetry(err, 1, policy)
│   └─ Retry? → sleep(backoff) → Attempt 2
│
├─ Attempt 2:
│   └─ ...
│
└─ Max retries exhausted → return last error
```

### Retry-After Handling

For 429 responses:
1. Parse `Retry-After` header (seconds or HTTP-date)
2. If valid → use as retry delay
3. If missing/invalid → fall back to policy backoff

### ETag Conflict (412)

Handled exclusively by `Executor.executeWithETagRetry`:
1. Re-GET the current resource from Azure
2. Recompute diff for the single target
3. If no-op → treat as success (already converged)
4. Otherwise → retry with updated action
5. Max 3 attempts

## Status Reporting for Errors

### Per-Mapping Row Status

Each `MappingStatus` row reports errors independently:

```
mappingStatuses:
- selectorHash: "abc123"
  matchedPods: 5
  asgSyncState: "Error"
  error: "UpdatePrefixSet asg-prod: azure ARM error: status=403 code=AuthorizationFailed"
  lastSyncTime: "2025-01-01T10:00:00Z"   # preserved from last success
- selectorHash: "def456"
  matchedPods: 3
  asgSyncState: "Synced"
  lastSyncTime: "2025-01-02T15:30:00Z"   # updated this reconcile
```

### Condition Summary

```yaml
conditions:
- type: Accepted
  status: "True"
  reason: SpecValid
- type: Reconciled
  status: "False"
  reason: ReconcileFailed
  message: "action failures: UpdatePrefixSet asg-prod: 403 AuthorizationFailed"
```
