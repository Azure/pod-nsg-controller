# 03 — Azure Client Design

> Package: `internal/azure` · ARM REST client, retry, rate limiting, executor

## Overview

The Azure layer provides a direct REST client for ARM Address Prefix Set resources (a child resource of Application Security Groups), along with retry policies, per-subscription rate limiting, and a concurrency-bounded executor for parallel action dispatch.

## Component Map

```
internal/azure/
├── interfaces.go                  # AddressPrefixSetAPI, ClientFactory, errors
├── address_prefix_set_client.go   # HTTP client implementation
├── client_factory.go              # Per-subscription client caching
├── executor.go                    # Bounded-concurrency action runner
├── retry.go                       # Retry policy, backoff, ARM classification
├── ratelimit.go                   # Per-subscription token bucket limiter
└── fake/
    └── fake_client.go             # In-memory client for tests
```

## Interfaces

```go
// AddressPrefixSetAPI — core operations on an ASG's address prefix sets
type AddressPrefixSetAPI interface {
    Get(ctx, subscriptionID, resourceGroup, asgName, prefixSetName) (*AddressPrefixSet, error)
    Put(ctx, subscriptionID, resourceGroup, asgName, prefixSetName, ips []string) error
    Delete(ctx, subscriptionID, resourceGroup, asgName, prefixSetName) error
    List(ctx, subscriptionID, resourceGroup, asgName) ([]AddressPrefixSet, error)
}

// AddressPrefixSetClientFactory — creates clients scoped to a subscription
type AddressPrefixSetClientFactory interface {
    ForSubscription(subscriptionID string) (AddressPrefixSetAPI, error)
}
```

## AddressPrefixSetClient

Direct ARM REST client (no Azure SDK dependency for this resource type).

### Configuration Options

```go
WithRetryPolicy(policy RetryPolicy)
WithSubscriptionRateLimiter(limiter SubscriptionRateLimiter)
WithHTTPClient(client *http.Client)
WithBaseURL(url string)
```

### Request Flow

```
API method (Get/Put/Delete/List)
    │
    └─ doRequest(ctx, retryCtx, method, url, body, headers)
         │
         ├─ rateLimiter.Wait(ctx, subscriptionID)   ← throttle
         ├─ http.NewRequestWithContext(...)
         ├─ acquireToken(ctx)                        ← Azure credential
         ├─ httpClient.Do(req)
         ├─ parseARMError(resp, body, op)            ← build ARMStatusError
         ├─ DecideRetry(err, attempt, policy)        ← retry decision
         └─ sleep(decision.Delay) or return
```

### ETag Handling

`Put` performs a read-before-write:
1. `GET` the current resource
2. If exists → use `If-Match: <etag>` (update)
3. If not found → use `If-None-Match: *` (create)
4. On 412 → handled by executor's ETag retry loop (not here)

### ARM Error Model

```go
type ARMStatusError struct {
    StatusCode int
    ARMCode    string
    Message    string
    Operation  ARMOperation
    RequestURL string
    RetryAfter time.Duration
}
```

`Unwrap()` returns `ErrNotFound` for 404 status, enabling `errors.Is(err, ErrNotFound)`.

## Client Factory

```go
type ClientFactory struct {
    log         *zap.Logger
    credential  azcore.TokenCredential
    httpClient  *http.Client
    baseURL     string
    retryPolicy RetryPolicy
    rateLimiter SubscriptionRateLimiter
    mu          sync.Mutex
    clients     map[string]AddressPrefixSetAPI
}
```

- Caches one client per subscription ID
- Forwards retry policy and rate limiter to each client
- Supports lazy credential resolution (`NewClientFactoryWithDefaultCredential`)

### Factory Options

```go
WithFactoryRetryPolicy(policy RetryPolicy)
WithFactorySubscriptionRateLimiter(limiter SubscriptionRateLimiter)
WithFactoryARMBaseURL(url string)
```

## Retry Policy

```go
type RetryPolicy struct {
    MaxRetries        int           // default 3
    NetworkMaxRetries int           // default 1
    BaseDelay         time.Duration // default 1s
    MaxDelay          time.Duration // default 8s
    JitterFactor      float64       // default 0.2
}
```

### Retry Classification

```
DecideRetry(err, attempt, policy) → RetryDecision
    │
    ├─ 429 (Too Many Requests) → retry with Retry-After or backoff
    ├─ 500, 502, 503, 504      → retry with exponential backoff
    ├─ 408 (Request Timeout)   → retry with backoff
    ├─ 412 (Precondition Failed) → NO retry (executor handles)
    ├─ 401, 403 (Auth failures)  → NO retry
    ├─ Network errors (DNS, TCP) → retry up to NetworkMaxRetries
    └─ All others              → NO retry
```

### Backoff

```
delay = min(baseDelay * 2^attempt, maxDelay) ± jitter
jitter = delay * jitterFactor * random(-1, 1)
```

### ARM Helpers

```go
IsRetriableARM(err)           // 429, 5xx, 408
IsAuthFailure(err)            // 401, 403
IsParentASGNotFound(err)      // 404 for parent ASG
IsChildPrefixSetNotFound(err) // 404 for child prefix set
ExtractRetryAfterHint(err)    // Retry-After from ARMStatusError
ParseRetryAfterValue(header)  // Parse Retry-After header
```

## Rate Limiter

```go
type SubscriptionRateLimiter interface {
    Wait(ctx context.Context, subscriptionID string) error
}
```

### ARMRateLimiter

Per-subscription token bucket rate limiter using `golang.org/x/time/rate`:

- One `rate.Limiter` per subscription (lazy-created)
- Bounded map size (default 4096) — evicts all on overflow
- Configurable RPS and clock (for testing)

```go
NewARMRateLimiter(log *zap.Logger, rps float64, opts ...ARMRateLimiterOption)
```

A `NoopSubscriptionRateLimiter()` is provided for testing.

## Executor

```go
type Executor struct {
    log         *zap.Logger
    factory     AddressPrefixSetClientFactory
    maxParallel int    // concurrency bound (default from config)
    maxRetries  int    // ETag retry limit (default 3)
}
```

### Execution Flow

```
Execute(ctx, actions) → []ActionResult
    │
    ├─ Acquire semaphore slot (bounded concurrency)
    ├─ For each action (goroutine):
    │   └─ executeWithETagRetry(ctx, action)
    │       ├─ executeAction(ctx, action)  ← Put or Delete
    │       ├─ On 412 (ETag conflict):
    │       │   ├─ Re-GET actual state
    │       │   ├─ Recompute diff for single target
    │       │   ├─ If no-op → return success
    │       │   └─ Retry with recomputed action
    │       └─ Return ActionResult{Success, Err}
    └─ Collect all results
```

### ETag Retry

The executor owns the 412 retry loop. It re-fetches the actual state and recomputes the diff for the single target:

```go
recomputeSingleTargetActionViaDiff(desired, actual, target) → *Action
```

If the recomputed diff produces no action (state already converged), the result is treated as success.

## Fake Client

`fake.Client` provides an in-memory `AddressPrefixSetAPI` for tests:

- Stores prefix sets in a `map[string]*AddressPrefixSet`
- Supports ETag tracking for 412 injection
- Per-operation error injection via `InjectKey` and FIFO queues
- `fake.ClientFactory` returns per-subscription fakes
- Thread-safe via `sync.Mutex`
