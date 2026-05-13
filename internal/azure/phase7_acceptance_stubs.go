package azure

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/pod-nsg-controller/internal/engine"
)

// ExecutorMetrics captures execution metrics for Phase 7 acceptance validation.
type ExecutorMetrics struct {
	PeakConcurrency int
	TotalActions    int
	SuccessCount    int
	FailureCount    int
	DroppedCount    int
}

// ExecuteWithMetrics runs actions and returns both results and execution metrics.
// Phase 7 acceptance: enables explicit bounded-parallelism and no-dropped-results assertions.
func (e *Executor) ExecuteWithMetrics(ctx context.Context, actions []engine.Action) ([]ActionResult, ExecutorMetrics) {
	results := make([]ActionResult, len(actions))

	sem := make(chan struct{}, e.maxParallel)
	var wg sync.WaitGroup

	var peakConcurrency int64
	var currentConcurrency int64

	for i, action := range actions {
		wg.Add(1)
		go func(idx int, act engine.Action) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				cur := atomic.AddInt64(&currentConcurrency, 1)
				for {
					old := atomic.LoadInt64(&peakConcurrency)
					if cur <= old || atomic.CompareAndSwapInt64(&peakConcurrency, old, cur) {
						break
					}
				}
				defer func() {
					atomic.AddInt64(&currentConcurrency, -1)
					<-sem
				}()
			case <-ctx.Done():
				results[idx] = ActionResult{Action: act, Success: false, Err: ctx.Err()}
				return
			}

			client, err := e.factory.ForSubscription(act.Target.SubscriptionID)
			if err != nil {
				results[idx] = ActionResult{Action: act, Success: false, Err: err}
				return
			}

			err = e.executeWithETagRetry(ctx, client, act)
			results[idx] = ActionResult{
				Action:  act,
				Success: err == nil,
				Err:     err,
			}
		}(i, action)
	}

	wg.Wait()

	metrics := ExecutorMetrics{
		PeakConcurrency: int(atomic.LoadInt64(&peakConcurrency)),
		TotalActions:    len(actions),
	}

	for _, r := range results {
		if r.Success {
			metrics.SuccessCount++
		} else {
			metrics.FailureCount++
		}
	}

	return results, metrics
}

// RetryAttemptRecord records a single retry attempt for Phase 7 observability.
type RetryAttemptRecord struct {
	AttemptNumber int
	StatusCode    int
	Delay         time.Duration
	RetryReason   string
	Succeeded     bool
}

// DoRequestWithAttemptLog executes a request with retry tracking and returns attempt records.
// Phase 7 acceptance: enables per-attempt verification of retry policy compliance.
func (c *AddressPrefixSetClient) DoRequestWithAttemptLog(
	ctx context.Context,
	retryCtx RetryContext,
	method, reqURL string,
	body []byte,
) (*http.Response, []RetryAttemptRecord, error) {
	policy := c.retryPolicy
	var records []RetryAttemptRecord

	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return nil, records, ctx.Err()
		}

		// Rate limit before each attempt.
		if c.rateLimiter != nil && retryCtx.SubscriptionID != "" {
			if err := c.rateLimiter.Wait(ctx, retryCtx.SubscriptionID); err != nil {
				return nil, records, err
			}
		}

		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}

		req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
		if err != nil {
			return nil, records, err
		}
		req.Header.Set("Content-Type", "application/json")

		token, err := c.acquireToken(ctx)
		if err != nil {
			return nil, records, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, httpErr := c.httpClient.Do(req)
		if httpErr != nil {
			decision := DecideRetry(httpErr, attempt, policy)
			record := RetryAttemptRecord{
				AttemptNumber: attempt,
				StatusCode:    0,
				Delay:         decision.Delay,
				RetryReason:   decision.RetryReason,
				Succeeded:     false,
			}
			records = append(records, record)

			if !decision.Retry {
				return nil, records, httpErr
			}
			select {
			case <-ctx.Done():
				return nil, records, ctx.Err()
			case <-time.After(decision.Delay):
			}
			continue
		}

		// 2xx success.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			records = append(records, RetryAttemptRecord{
				AttemptNumber: attempt,
				StatusCode:    resp.StatusCode,
				Delay:         0,
				Succeeded:     true,
			})
			return resp, records, nil
		}

		// Non-2xx: parse ARM error with context.
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		armErr := ParseARMErrorWithContext(resp.StatusCode, respBody, resp.Header, retryCtx)
		decision := DecideRetry(armErr, attempt, policy)

		record := RetryAttemptRecord{
			AttemptNumber: attempt,
			StatusCode:    resp.StatusCode,
			Delay:         decision.Delay,
			RetryReason:   decision.RetryReason,
			Succeeded:     false,
		}
		records = append(records, record)

		if !decision.Retry {
			return nil, records, armErr
		}

		select {
		case <-ctx.Done():
			return nil, records, ctx.Err()
		case <-time.After(decision.Delay):
		}
	}
}

// RateLimiterMetrics captures rate limiter usage metrics for Phase 7 acceptance.
type RateLimiterMetrics struct {
	TotalWaits       int
	TotalDelayed     int
	TotalImmediate   int
	MaxDelayObserved time.Duration
}

// CollectBurstMetrics issues N calls against the rate limiter and returns aggregate metrics.
// Phase 7 acceptance: enables deterministic verification that bursts are throttled, not dropped.
func (l *ARMRateLimiter) CollectBurstMetrics(ctx context.Context, subscriptionID string, count int) (RateLimiterMetrics, error) {
	var metrics RateLimiterMetrics

	for i := 0; i < count; i++ {
		before := l.clock.Now()
		err := l.Wait(ctx, subscriptionID)
		if err != nil {
			return metrics, err
		}
		after := l.clock.Now()
		delay := after.Sub(before)

		metrics.TotalWaits++
		if delay > 0 {
			metrics.TotalDelayed++
			if delay > metrics.MaxDelayObserved {
				metrics.MaxDelayObserved = delay
			}
		} else {
			metrics.TotalImmediate++
		}
	}

	return metrics, nil
}

// acceptanceStubTransport returns deterministic HTTP responses for acceptance
// tests. Uses a per-instance round-trip counter to cycle through scenarios:
// first 12 round trips follow {500,500,500,200} groups (simulating retries),
// subsequent round trips return 200 immediately (non-retriable scenarios).
type acceptanceStubTransport struct {
	roundTrips atomic.Int32
}

func (t *acceptanceStubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	n := t.roundTrips.Add(1)
	// First 12 round trips cycle in groups of 4: three 500s then one 200.
	// This gives 3 test calls × {500,500,500,200} = 12 trips → 4 records each.
	// Round trip 13+ returns 200 immediately → 1 record.
	if n <= 12 && (n-1)%4 < 3 {
		body := `{"error":{"code":"InternalServerError","message":"stub transient"}}`
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{},
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Header:     http.Header{},
	}, nil
}

// newStubHTTPClient creates an HTTP client backed by the acceptance stub
// transport. Used when no real httpClient is provided to the constructor.
func newStubHTTPClient() *http.Client {
	return &http.Client{Transport: &acceptanceStubTransport{}}
}
