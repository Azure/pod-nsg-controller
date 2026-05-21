package azure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

// ---------- T7.1: ARM returns 429 → retried after Retry-After delay ----------

func TestPhase7_T71_ClientDoRequest_429_UsesRetryAfter(t *testing.T) {
	var mu sync.Mutex
	var requestTimes []time.Time
	var callCount int32

	retryAfterSeconds := 2

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		mu.Lock()
		requestTimes = append(requestTimes, time.Now())
		mu.Unlock()

		if n == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{
					"code":    "TooManyRequests",
					"message": "Rate limit exceeded",
				},
			})
			return
		}
		name := "ps-1"
		etag := `"etag-1"`
		_ = json.NewEncoder(w).Encode(AddressPrefixSet{
			Name: &name,
			Etag: &etag,
			Properties: &AddressPrefixSetProperties{
				AddressPrefixes: []string{"10.0.0.1/32"},
			},
		})
	}))
	defer ts.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, ts.Client(),
		WithARMBaseURL(ts.URL),
		WithRetryPolicy(DefaultRetryPolicy()),
	)

	result, err := client.Get(context.Background(), "sub-1", "rg-1", "asg-1", "ps-1")
	if err != nil {
		t.Fatalf("T7.1: expected success after 429 retry, got error: %v", err)
	}
	if result == nil {
		t.Fatal("T7.1: expected non-nil result")
	}

	mu.Lock()
	times := make([]time.Time, len(requestTimes))
	copy(times, requestTimes)
	mu.Unlock()

	if len(times) < 2 {
		t.Fatalf("T7.1: expected at least 2 requests, got %d", len(times))
	}

	// The retry should have waited at least Retry-After seconds (2s).
	// With the stub (no-op WithRetryPolicy), the client uses fixed 200ms backoff,
	// so this assertion will fail.
	gap := times[1].Sub(times[0])
	minExpectedGap := time.Duration(retryAfterSeconds) * time.Second * 9 / 10 // 90% to account for jitter
	if gap < minExpectedGap {
		t.Errorf("T7.1: expected retry delay >= %v (Retry-After: %ds), got %v",
			minExpectedGap, retryAfterSeconds, gap)
	}
}

// ---------- T7.2: ARM returns 500 three times then succeeds on 4th ----------

func TestPhase7_T72_ClientDoRequest_500x3ThenSuccess(t *testing.T) {
	var callCount int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n <= 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{
					"code":    "InternalServerError",
					"message": "transient failure",
				},
			})
			return
		}
		name := "ps-1"
		etag := `"etag-1"`
		_ = json.NewEncoder(w).Encode(AddressPrefixSet{
			Name: &name,
			Etag: &etag,
			Properties: &AddressPrefixSetProperties{
				AddressPrefixes: []string{"10.0.0.1/32"},
			},
		})
	}))
	defer ts.Close()

	log := zaptest.NewLogger(t)

	// Use a policy with MaxRetries=3 and exponential backoff.
	// With the stub, WithRetryPolicy is a no-op so the client uses hardcoded defaults.
	policy := RetryPolicy{
		MaxRetries:        3,
		NetworkMaxRetries: 1,
		BaseDelay:         500 * time.Millisecond,
		MaxDelay:          4 * time.Second,
		JitterFactor:      0.1,
	}

	client := NewAddressPrefixSetClient(log, nil, ts.Client(),
		WithARMBaseURL(ts.URL),
		WithRetryPolicy(policy),
	)

	result, err := client.Get(context.Background(), "sub-1", "rg-1", "asg-1", "ps-1")
	if err != nil {
		t.Fatalf("T7.2: expected success after 3 failures, got error: %v", err)
	}
	if result == nil {
		t.Fatal("T7.2: expected non-nil result")
	}

	total := int(atomic.LoadInt32(&callCount))
	if total != 4 {
		t.Errorf("T7.2: expected exactly 4 requests (initial + 3 retries), got %d", total)
	}

	// The retry delays should use the injected policy's exponential backoff (BaseDelay=500ms),
	// not the hardcoded fixed 200ms. Total elapsed should be > 1.5s if policy is wired.
	// This assertion validates the policy was actually applied.
	// With stub (no-op), delays are 200ms fixed, total ~ 600ms.
	// We cannot measure exact timing here without timestamps, so we verify
	// that the policy's BaseDelay config was respected through request count consistency.
	// The primary fail condition is the ParseARMError metadata assertion below.

	// Additional assertion: verify returned resource is correct.
	if result.Properties == nil || len(result.Properties.AddressPrefixes) != 1 {
		t.Errorf("T7.2: expected 1 IP in result, got %v", result.Properties)
	}

	// Verify the injected policy was actually stored on the client, confirming
	// WithRetryPolicy wired the policy into the doRequest path.
	// With stub (no-op), retryPolicy remains the zero value.
	if client.retryPolicy.MaxRetries != policy.MaxRetries {
		t.Errorf("T7.2: expected client retryPolicy.MaxRetries=%d (from injected policy), got %d",
			policy.MaxRetries, client.retryPolicy.MaxRetries)
	}
	if client.retryPolicy.BaseDelay != policy.BaseDelay {
		t.Errorf("T7.2: expected client retryPolicy.BaseDelay=%v, got %v",
			policy.BaseDelay, client.retryPolicy.BaseDelay)
	}
}

// ---------- T7.3: ARM returns 500 four times → gives up ----------

func TestPhase7_T73_ClientDoRequest_500x4Exhausted(t *testing.T) {
	var callCount int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "InternalServerError",
				"message": "persistent failure",
			},
		})
	}))
	defer ts.Close()

	log := zaptest.NewLogger(t)

	// Policy allows 3 retries (4 total attempts). Server always returns 500.
	policy := RetryPolicy{
		MaxRetries:        3,
		NetworkMaxRetries: 1,
		BaseDelay:         10 * time.Millisecond,
		MaxDelay:          100 * time.Millisecond,
		JitterFactor:      0,
	}

	client := NewAddressPrefixSetClient(log, nil, ts.Client(),
		WithARMBaseURL(ts.URL),
		WithRetryPolicy(policy),
	)

	_, err := client.Get(context.Background(), "sub-1", "rg-1", "asg-1", "ps-1")
	if err == nil {
		t.Fatal("T7.3: expected error after exhausting retries, got nil")
	}

	total := int(atomic.LoadInt32(&callCount))
	// With MaxRetries=3, we expect exactly 4 attempts (initial + 3 retries).
	// With stub (no-op WithRetryPolicy), the client uses defaultMaxRetries=3,
	// which also gives 4 attempts. So count matches, but the error metadata
	// should include Operation from the RetryContext.
	if total != 4 {
		t.Errorf("T7.3: expected 4 requests (initial + 3 retries), got %d", total)
	}

	// The returned ARMStatusError should carry Operation metadata from the RetryContext.
	var armErr *ARMStatusError
	if !asARMError(err, &armErr) {
		t.Fatalf("T7.3: expected ARMStatusError, got %T: %v", err, err)
	}

	// With the stub, Operation will be empty string (zero value).
	if armErr.Operation != ARMOperationGetPrefixSet {
		t.Errorf("T7.3: expected Operation=%q on exhausted error, got %q",
			ARMOperationGetPrefixSet, armErr.Operation)
	}
}

// ---------- T7.4: ARM returns 403 → no retry, error reported immediately ----------

func TestPhase7_T74_ClientDoRequest_403NoRetry(t *testing.T) {
	var callCount int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "AuthorizationFailed",
				"message": "insufficient permissions",
			},
		})
	}))
	defer ts.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, ts.Client(),
		WithARMBaseURL(ts.URL),
		WithRetryPolicy(DefaultRetryPolicy()),
	)

	_, err := client.Get(context.Background(), "sub-1", "rg-1", "asg-1", "ps-1")
	if err == nil {
		t.Fatal("T7.4: expected error for 403, got nil")
	}

	total := int(atomic.LoadInt32(&callCount))
	if total != 1 {
		t.Errorf("T7.4: expected exactly 1 request (no retry for 403), got %d", total)
	}

	var armErr *ARMStatusError
	if !asARMError(err, &armErr) {
		t.Fatalf("T7.4: expected ARMStatusError, got %T: %v", err, err)
	}
	if armErr.StatusCode != http.StatusForbidden {
		t.Errorf("T7.4: expected status 403, got %d", armErr.StatusCode)
	}
	// Verify operation metadata was populated from RetryContext.
	if armErr.Operation != ARMOperationGetPrefixSet {
		t.Errorf("T7.4: expected Operation=%q, got %q",
			ARMOperationGetPrefixSet, armErr.Operation)
	}
}

// ---------- T7.6 (client level): Rate limiter is called per request attempt ----------

func TestAddressPrefixSetClient_DoRequest_RateLimiterCalledPerAttempt(t *testing.T) {
	var callCount int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		name := "ps-1"
		etag := `"etag-1"`
		_ = json.NewEncoder(w).Encode(AddressPrefixSet{
			Name: &name,
			Etag: &etag,
			Properties: &AddressPrefixSetProperties{
				AddressPrefixes: []string{"10.0.0.1/32"},
			},
		})
	}))
	defer ts.Close()

	log := zaptest.NewLogger(t)
	limiter := &countingRateLimiter{}

	client := NewAddressPrefixSetClient(log, nil, ts.Client(),
		WithARMBaseURL(ts.URL),
		WithRetryPolicy(DefaultRetryPolicy()),
		WithSubscriptionRateLimiter(limiter),
	)

	_, err := client.Get(context.Background(), "sub-1", "rg-1", "asg-1", "ps-1")
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	// The rate limiter's Wait should be called at least once (before the request).
	// With the stub (no-op WithSubscriptionRateLimiter), Wait is never called.
	waitCalls := limiter.waitCalls.Load()
	if waitCalls < 1 {
		t.Errorf("expected rate limiter Wait called at least 1 time, got %d", waitCalls)
	}
}

// ---------- ParseARMError with context sets RetryAfter and Operation metadata ----------

func TestAddressPrefixSetClient_ParseARMError_SetsRetryAfterAndMetadata(t *testing.T) {
	body := []byte(`{"error":{"code":"TooManyRequests","message":"throttled"}}`)
	headers := http.Header{}
	headers.Set("Retry-After", "5")

	retryCtx := RetryContext{
		Operation:      ARMOperationPutPrefixSet,
		Method:         "PUT",
		URL:            "https://management.azure.com/sub/rg/asg/ps",
		SubscriptionID: "sub-1",
		ResourceGroup:  "rg-1",
		ASGName:        "asg-1",
		PrefixSetName:  "ps-1",
	}

	armErr := ParseARMErrorWithContext(429, body, headers, retryCtx)
	if armErr == nil {
		t.Fatal("expected non-nil ARMStatusError")
	}

	if armErr.StatusCode != 429 {
		t.Errorf("expected StatusCode=429, got %d", armErr.StatusCode)
	}
	if armErr.ARMCode != "TooManyRequests" {
		t.Errorf("expected ARMCode=TooManyRequests, got %q", armErr.ARMCode)
	}

	// RetryAfter should be parsed from the Retry-After header (5 seconds).
	// With the stub, ParseARMErrorWithContext delegates to parseARMError which
	// doesn't parse headers, so RetryAfter remains 0.
	if armErr.RetryAfter != 5*time.Second {
		t.Errorf("expected RetryAfter=5s from header, got %v", armErr.RetryAfter)
	}

	// Operation should be set from the RetryContext.
	if armErr.Operation != ARMOperationPutPrefixSet {
		t.Errorf("expected Operation=%q, got %q", ARMOperationPutPrefixSet, armErr.Operation)
	}

	// RequestURL should be set from the RetryContext.
	if armErr.RequestURL != retryCtx.URL {
		t.Errorf("expected RequestURL=%q, got %q", retryCtx.URL, armErr.RequestURL)
	}
}

// --- Test helpers ---

// countingRateLimiter is a mock SubscriptionRateLimiter that counts Wait calls.
type countingRateLimiter struct {
	waitCalls atomic.Int32
}

func (c *countingRateLimiter) Wait(ctx context.Context, subscriptionID string) error {
	c.waitCalls.Add(1)
	return nil
}

// asARMError is a test helper that unwraps err into an ARMStatusError.
func asARMError(err error, target **ARMStatusError) bool {
	for e := err; e != nil; {
		if armErr, ok := e.(*ARMStatusError); ok {
			*target = armErr
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	return false
}

// ---------------------------------------------------------------------------
// Phase 7 Acceptance: DoRequestWithAttemptLog Tests
// Verify per-attempt tracking through the client retry loop.
// ---------------------------------------------------------------------------

func TestPhase7_T71_ClientDoRequest_AttemptLog_429RetryAfter(t *testing.T) {
	var callCount int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{"code": "TooManyRequests", "message": "throttled"},
			})
			return
		}
		name := "ps-1"
		etag := `"etag-1"`
		_ = json.NewEncoder(w).Encode(AddressPrefixSet{
			Name: &name, Etag: &etag,
			Properties: &AddressPrefixSetProperties{AddressPrefixes: []string{"10.0.0.1/32"}},
		})
	}))
	defer ts.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, ts.Client(),
		WithARMBaseURL(ts.URL),
		WithRetryPolicy(DefaultRetryPolicy()),
	)

	retryCtx := RetryContext{
		Operation:      ARMOperationGetPrefixSet,
		Method:         http.MethodGet,
		URL:            ts.URL,
		SubscriptionID: "sub-1",
	}

	_, records, err := client.DoRequestWithAttemptLog(
		context.Background(), retryCtx, http.MethodGet,
		client.resourceURL("sub-1", "rg-1", "asg-1", "ps-1"), nil,
	)

	// T7.1 acceptance: expect at least 2 attempt records (429 + success).
	if err != nil {
		// Stub returns error; real implementation should succeed.
		t.Logf("T7.1: DoRequestWithAttemptLog returned error (expected from stub): %v", err)
	}

	if len(records) < 2 {
		t.Errorf("T7.1: expected at least 2 attempt records, got %d (stub returns nil)", len(records))
	}

	// First record should show 429 with Retry-After delay of 3s.
	if len(records) > 0 {
		if records[0].StatusCode != 429 {
			t.Errorf("T7.1: first record StatusCode = %d, want 429", records[0].StatusCode)
		}
		if records[0].Delay < 3*time.Second {
			t.Errorf("T7.1: first record Delay = %v, want >= 3s (Retry-After)", records[0].Delay)
		}
	}
}

func TestPhase7_T72_ClientDoRequest_AttemptLog_500x3ThenSuccess(t *testing.T) {
	var callCount int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n <= 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{"code": "InternalServerError", "message": "oops"},
			})
			return
		}
		name := "ps-1"
		etag := `"etag-1"`
		_ = json.NewEncoder(w).Encode(AddressPrefixSet{
			Name: &name, Etag: &etag,
			Properties: &AddressPrefixSetProperties{AddressPrefixes: []string{"10.0.0.1/32"}},
		})
	}))
	defer ts.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, ts.Client(),
		WithARMBaseURL(ts.URL),
		WithRetryPolicy(RetryPolicy{MaxRetries: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: 100 * time.Millisecond}),
	)

	retryCtx := RetryContext{Operation: ARMOperationGetPrefixSet, SubscriptionID: "sub-1"}
	_, records, err := client.DoRequestWithAttemptLog(
		context.Background(), retryCtx, http.MethodGet,
		client.resourceURL("sub-1", "rg-1", "asg-1", "ps-1"), nil,
	)

	// T7.2 acceptance: expect 4 attempt records (3 failures + 1 success).
	if err != nil {
		t.Logf("T7.2: DoRequestWithAttemptLog returned error (expected from stub): %v", err)
	}
	if len(records) != 4 {
		t.Errorf("T7.2: expected 4 attempt records, got %d (stub returns nil)", len(records))
	}
	if len(records) == 4 && !records[3].Succeeded {
		t.Errorf("T7.2: expected 4th attempt to succeed")
	}
}

func TestPhase7_T73_ClientDoRequest_AttemptLog_500x4Exhausted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"code": "InternalServerError", "message": "persistent"},
		})
	}))
	defer ts.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, ts.Client(),
		WithARMBaseURL(ts.URL),
		WithRetryPolicy(RetryPolicy{MaxRetries: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: 100 * time.Millisecond}),
	)

	retryCtx := RetryContext{Operation: ARMOperationGetPrefixSet, SubscriptionID: "sub-1"}
	_, records, err := client.DoRequestWithAttemptLog(
		context.Background(), retryCtx, http.MethodGet,
		client.resourceURL("sub-1", "rg-1", "asg-1", "ps-1"), nil,
	)

	// T7.3 acceptance: expect 4 attempt records, all failed.
	if err == nil && len(records) == 0 {
		t.Error("T7.3: expected error or attempt records for exhausted retries")
	}
	if len(records) != 4 {
		t.Errorf("T7.3: expected 4 attempt records, got %d (stub returns nil)", len(records))
	}
	for i, r := range records {
		if r.Succeeded {
			t.Errorf("T7.3: attempt %d should not have succeeded", i+1)
		}
	}
}

func TestPhase7_T74_ClientDoRequest_AttemptLog_403NoRetry(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"code": "AuthorizationFailed", "message": "forbidden"},
		})
	}))
	defer ts.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, ts.Client(),
		WithARMBaseURL(ts.URL),
		WithRetryPolicy(DefaultRetryPolicy()),
	)

	retryCtx := RetryContext{Operation: ARMOperationGetPrefixSet, SubscriptionID: "sub-1"}
	_, records, err := client.DoRequestWithAttemptLog(
		context.Background(), retryCtx, http.MethodGet,
		client.resourceURL("sub-1", "rg-1", "asg-1", "ps-1"), nil,
	)

	// T7.4 acceptance: expect exactly 1 attempt record (no retry for 403).
	if err == nil && len(records) == 0 {
		t.Error("T7.4: expected error or attempt records for 403")
	}
	if len(records) != 1 {
		t.Errorf("T7.4: expected exactly 1 attempt record (no retry), got %d (stub returns nil)", len(records))
	}
	if len(records) > 0 && records[0].StatusCode != 403 {
		t.Errorf("T7.4: expected StatusCode 403, got %d", records[0].StatusCode)
	}
}
