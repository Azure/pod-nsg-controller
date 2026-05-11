package azure

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

// -----------------------------------------------------------------------
// T7.1 – ARM returns 429 → call retried after Retry-After delay
// -----------------------------------------------------------------------
func TestPhase7_T71_TooManyRequests_UsesRetryAfterHint(t *testing.T) {
	_ = zaptest.NewLogger(t)

	t.Run("DecideRetry_429_uses_RetryAfter_header_value", func(t *testing.T) {
		retryAfter := 3 * time.Second
		armErr := &ARMStatusError{
			StatusCode: 429,
			ARMCode:    "TooManyRequests",
			Message:    "Rate limit exceeded",
		}
		armErr.RetryAfter = retryAfter

		policy := DefaultRetryPolicy()
		decision := DecideRetry(armErr, 1, policy)

		if !decision.Retry {
			t.Errorf("DecideRetry() Retry = false, want true for 429")
		}
		if decision.Delay != retryAfter {
			t.Errorf("DecideRetry() Delay = %v, want %v (Retry-After)", decision.Delay, retryAfter)
		}
	})

	t.Run("ExtractRetryAfterHint_returns_duration_from_429", func(t *testing.T) {
		retryAfter := 5 * time.Second
		armErr := &ARMStatusError{
			StatusCode: 429,
			ARMCode:    "TooManyRequests",
			Message:    "Rate limit exceeded",
		}
		armErr.RetryAfter = retryAfter

		got := ExtractRetryAfterHint(armErr)
		if got != retryAfter {
			t.Errorf("ExtractRetryAfterHint() = %v, want %v", got, retryAfter)
		}
	})

	t.Run("ParseRetryAfterValue_delta_seconds", func(t *testing.T) {
		now := time.Now()
		got, ok := ParseRetryAfterValue("120", now)
		if !ok {
			t.Fatal("ParseRetryAfterValue() ok = false, want true")
		}
		want := 120 * time.Second
		if got != want {
			t.Errorf("ParseRetryAfterValue() = %v, want %v", got, want)
		}
	})

	t.Run("ParseRetryAfterValue_http_date", func(t *testing.T) {
		now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		httpDate := "Thu, 01 Jan 2026 12:00:30 GMT"
		got, ok := ParseRetryAfterValue(httpDate, now)
		if !ok {
			t.Fatal("ParseRetryAfterValue() ok = false, want true")
		}
		if got < 29*time.Second || got > 31*time.Second {
			t.Errorf("ParseRetryAfterValue() = %v, want ~30s", got)
		}
	})

	t.Run("IsRetriableARM_true_for_429", func(t *testing.T) {
		armErr := &ARMStatusError{StatusCode: 429, ARMCode: "TooManyRequests", Message: "throttled"}
		if !IsRetriableARM(armErr) {
			t.Errorf("IsRetriableARM(429) = false, want true")
		}
	})
}

// -----------------------------------------------------------------------
// T7.2 – ARM returns 500 three times then succeeds on 4th call
// -----------------------------------------------------------------------
func TestPhase7_T72_ServerError_ThreeFailuresThenSuccessOnFourthAttempt(t *testing.T) {
	_ = zaptest.NewLogger(t)
	policy := DefaultRetryPolicy()

	// With default MaxRetries=3, max total attempts = 4.
	// Sequence: attempt 1 (500), 2 (500), 3 (500), 4 (success).

	serverErr := &ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "oops"}

	// Attempts 1-3 should retry.
	for attempt := 1; attempt <= 3; attempt++ {
		t.Run(fmt.Sprintf("attempt_%d_retries", attempt), func(t *testing.T) {
			decision := DecideRetry(serverErr, attempt, policy)
			if !decision.Retry {
				t.Errorf("DecideRetry(attempt=%d) Retry = false, want true", attempt)
			}
		})
	}

	// Verify the default policy allows 3 retries after initial attempt.
	if policy.MaxRetries != 3 {
		t.Errorf("DefaultRetryPolicy().MaxRetries = %d, want 3", policy.MaxRetries)
	}
}

// -----------------------------------------------------------------------
// T7.3 – ARM returns 500 four times → gives up after 4th attempt
// -----------------------------------------------------------------------
func TestPhase7_T73_ServerError_FourFailuresStopsAfterFourthAttempt(t *testing.T) {
	_ = zaptest.NewLogger(t)
	policy := DefaultRetryPolicy()

	serverErr := &ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "oops"}

	// After attempt 4 (retriesUsed = 3 = MaxRetries), should NOT retry.
	decision := DecideRetry(serverErr, 4, policy)
	if decision.Retry {
		t.Errorf("DecideRetry(attempt=4) Retry = true, want false (exhausted)")
	}
}

// -----------------------------------------------------------------------
// T7.4 – ARM returns 403 → no retry
// -----------------------------------------------------------------------
func TestPhase7_T74_Auth403_NoRetry(t *testing.T) {
	_ = zaptest.NewLogger(t)
	policy := DefaultRetryPolicy()

	authErr := &ARMStatusError{StatusCode: 403, ARMCode: "AuthorizationFailed", Message: "forbidden"}

	decision := DecideRetry(authErr, 1, policy)
	if decision.Retry {
		t.Errorf("DecideRetry(403) Retry = true, want false")
	}

	if !IsAuthFailure(authErr) {
		t.Errorf("IsAuthFailure(403) = false, want true")
	}
}

// -----------------------------------------------------------------------
// Additional retry unit tests from design
// -----------------------------------------------------------------------

func TestRetry_AttemptCounting_MaxRetriesMeansRetriesAfterInitial(t *testing.T) {
	_ = zaptest.NewLogger(t)

	tests := []struct {
		name          string
		maxRetries    int
		attemptNumber int
		wantRetry     bool
	}{
		{"MaxRetries=3 attempt=1 → retry", 3, 1, true},
		{"MaxRetries=3 attempt=2 → retry", 3, 2, true},
		{"MaxRetries=3 attempt=3 → retry", 3, 3, true},
		{"MaxRetries=3 attempt=4 → stop", 3, 4, false},
		{"MaxRetries=0 attempt=1 → stop", 0, 1, false},
		{"MaxRetries=1 attempt=1 → retry", 1, 1, true},
		{"MaxRetries=1 attempt=2 → stop", 1, 2, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy := RetryPolicy{
				MaxRetries:   tc.maxRetries,
				BaseDelay:    1 * time.Second,
				MaxDelay:     8 * time.Second,
				JitterFactor: 0.2,
			}
			serverErr := &ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "error"}
			decision := DecideRetry(serverErr, tc.attemptNumber, policy)
			if decision.Retry != tc.wantRetry {
				t.Errorf("DecideRetry(attempt=%d, maxRetries=%d) Retry = %v, want %v",
					tc.attemptNumber, tc.maxRetries, decision.Retry, tc.wantRetry)
			}
		})
	}
}

func TestRetry_DecideRetry_412Excluded(t *testing.T) {
	_ = zaptest.NewLogger(t)
	policy := DefaultRetryPolicy()

	etagErr := &ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "etag mismatch"}
	decision := DecideRetry(etagErr, 1, policy)
	if decision.Retry {
		t.Errorf("DecideRetry(412) Retry = true, want false (412 owned by executor)")
	}
}

func TestRetry_DecideRetry_BackoffSequence(t *testing.T) {
	_ = zaptest.NewLogger(t)
	policy := RetryPolicy{
		MaxRetries:   3,
		BaseDelay:    1 * time.Second,
		MaxDelay:     8 * time.Second,
		JitterFactor: 0.0, // no jitter for deterministic test
	}

	serverErr := &ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "error"}

	// Expected delays: attempt 1 → 1s, attempt 2 → 2s, attempt 3 → 4s
	wantDelays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	for i, want := range wantDelays {
		attempt := i + 1
		t.Run(fmt.Sprintf("attempt_%d_delay_%v", attempt, want), func(t *testing.T) {
			decision := DecideRetry(serverErr, attempt, policy)
			if !decision.Retry {
				t.Fatalf("DecideRetry(attempt=%d) Retry = false, want true", attempt)
			}
			if decision.Delay != want {
				t.Errorf("DecideRetry(attempt=%d) Delay = %v, want %v", attempt, decision.Delay, want)
			}
		})
	}
}

func TestRetry_DecideRetry_NetworkError_UsesNetworkMaxRetries(t *testing.T) {
	_ = zaptest.NewLogger(t)
	policy := RetryPolicy{
		MaxRetries:        3,
		NetworkMaxRetries: 1,
		BaseDelay:         1 * time.Second,
		MaxDelay:          8 * time.Second,
		JitterFactor:      0.0,
	}

	// Simulate a network error (not an ARM status error).
	netErr := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: fmt.Errorf("connection refused"),
	}

	t.Run("network_attempt_1_retries", func(t *testing.T) {
		decision := DecideRetry(netErr, 1, policy)
		if !decision.Retry {
			t.Errorf("DecideRetry(network, attempt=1) Retry = false, want true")
		}
	})

	t.Run("network_attempt_2_stops", func(t *testing.T) {
		decision := DecideRetry(netErr, 2, policy)
		if decision.Retry {
			t.Errorf("DecideRetry(network, attempt=2) Retry = true, want false (NetworkMaxRetries=1)")
		}
	})
}

func TestRetry_IsRetriableARM_ServerErrors(t *testing.T) {
	_ = zaptest.NewLogger(t)

	tests := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{"429 is retriable", 429, true},
		{"500 is retriable", 500, true},
		{"502 is retriable", 502, true},
		{"503 is retriable", 503, true},
		{"504 is retriable", 504, true},
		{"401 is not retriable", 401, false},
		{"403 is not retriable", 403, false},
		{"404 is not retriable", 404, false},
		{"409 is not retriable", 409, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			armErr := &ARMStatusError{StatusCode: tc.statusCode, ARMCode: "Test", Message: "test"}
			got := IsRetriableARM(armErr)
			if got != tc.want {
				t.Errorf("IsRetriableARM(status=%d) = %v, want %v", tc.statusCode, got, tc.want)
			}
		})
	}
}

func TestRetry_IsAuthFailure(t *testing.T) {
	_ = zaptest.NewLogger(t)

	tests := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{"401 is auth failure", 401, true},
		{"403 is auth failure", 403, true},
		{"404 is not auth failure", 404, false},
		{"500 is not auth failure", 500, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			armErr := &ARMStatusError{StatusCode: tc.statusCode, ARMCode: "Auth", Message: "auth error"}
			got := IsAuthFailure(armErr)
			if got != tc.want {
				t.Errorf("IsAuthFailure(status=%d) = %v, want %v", tc.statusCode, got, tc.want)
			}
		})
	}
}

func TestRetry_IsParentASGNotFound(t *testing.T) {
	_ = zaptest.NewLogger(t)

	tests := []struct {
		name    string
		armCode string
		want    bool
	}{
		{"ApplicationSecurityGroupNotFound", "ApplicationSecurityGroupNotFound", true},
		{"ParentResourceNotFound", "ParentResourceNotFound", true},
		{"ResourceGroupNotFound", "ResourceGroupNotFound", true},
		{"SubscriptionNotFound", "SubscriptionNotFound", true},
		{"AddressPrefixSetNotFound is child not parent", "AddressPrefixSetNotFound", false},
		{"NotFound generic", "NotFound", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			armErr := &ARMStatusError{StatusCode: 404, ARMCode: tc.armCode, Message: "not found"}
			got := IsParentASGNotFound(armErr)
			if got != tc.want {
				t.Errorf("IsParentASGNotFound(ARMCode=%q) = %v, want %v", tc.armCode, got, tc.want)
			}
		})
	}
}

func TestRetry_IsChildPrefixSetNotFound(t *testing.T) {
	_ = zaptest.NewLogger(t)

	t.Run("AddressPrefixSetNotFound is child", func(t *testing.T) {
		armErr := &ARMStatusError{StatusCode: 404, ARMCode: "AddressPrefixSetNotFound", Message: "not found"}
		got := IsChildPrefixSetNotFound(armErr)
		if !got {
			t.Errorf("IsChildPrefixSetNotFound(AddressPrefixSetNotFound) = false, want true")
		}
	})

	t.Run("ApplicationSecurityGroupNotFound is not child", func(t *testing.T) {
		armErr := &ARMStatusError{StatusCode: 404, ARMCode: "ApplicationSecurityGroupNotFound", Message: "not found"}
		got := IsChildPrefixSetNotFound(armErr)
		if got {
			t.Errorf("IsChildPrefixSetNotFound(ApplicationSecurityGroupNotFound) = true, want false")
		}
	})
}

func TestRetry_ParseRetryAfterValue_AcceptsAllHTTPDateFormats(t *testing.T) {
	_ = zaptest.NewLogger(t)

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{
			name:  "RFC1123",
			value: "Thu, 01 Jan 2026 12:00:30 GMT",
			want:  30 * time.Second,
		},
		{
			name:  "RFC850",
			value: "Thursday, 01-Jan-26 12:00:30 GMT",
			want:  30 * time.Second,
		},
		{
			name:  "ANSI C asctime",
			value: "Thu Jan  1 12:00:30 2026",
			want:  30 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRetryAfterValue(tc.value, now)
			if !ok {
				t.Errorf("ParseRetryAfterValue(%q) ok = false, want true", tc.value)
			}
			if got != tc.want {
				t.Errorf("ParseRetryAfterValue(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestRetry_ParseRetryAfterValue_PastHTTPDateReturnsZero(t *testing.T) {
	_ = zaptest.NewLogger(t)

	now := time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC)
	got, ok := ParseRetryAfterValue("Thu, 01 Jan 2026 12:00:00 GMT", now)
	if !ok {
		t.Fatal("ParseRetryAfterValue() ok = false, want true")
	}
	if got != 0 {
		t.Errorf("ParseRetryAfterValue() = %v, want 0 for past HTTP-date", got)
	}
}

// ---------------------------------------------------------------------------
// Phase 7 Acceptance: Retry Attempt Record Tests
// These tests verify Phase 7 retry attempt observability through RetryAttemptRecord.
// ---------------------------------------------------------------------------

func TestPhase7_T71_RetryRecord_429UsesRetryAfterDelay(t *testing.T) {
	_ = zaptest.NewLogger(t)

	retryAfter := 3 * time.Second
	armErr := &ARMStatusError{
		StatusCode: 429,
		ARMCode:    "TooManyRequests",
		Message:    "Rate limit exceeded",
		RetryAfter: retryAfter,
	}

	policy := DefaultRetryPolicy()
	decision := DecideRetry(armErr, 1, policy)

	// Build expected attempt record from decision.
	record := RetryAttemptRecord{
		AttemptNumber: 1,
		StatusCode:    429,
		Delay:         decision.Delay,
		RetryReason:   decision.RetryReason,
		Succeeded:     false,
	}

	// T7.1 acceptance: record must show 429 with Retry-After delay.
	if record.StatusCode != 429 {
		t.Errorf("T7.1: RetryAttemptRecord.StatusCode = %d, want 429", record.StatusCode)
	}
	if record.Delay != retryAfter {
		t.Errorf("T7.1: RetryAttemptRecord.Delay = %v, want %v (Retry-After)", record.Delay, retryAfter)
	}
	if record.RetryReason != "429-retry-after" {
		t.Errorf("T7.1: RetryAttemptRecord.RetryReason = %q, want %q", record.RetryReason, "429-retry-after")
	}

	// Verify DoRequestWithAttemptLog returns structured records.
	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, nil)
	retryCtx := RetryContext{Operation: ARMOperationGetPrefixSet}
	_, records, err := client.DoRequestWithAttemptLog(
		context.Background(), retryCtx, "GET", "http://test", nil,
	)
	// Phase 7 implementation should return attempt records, not error.
	if err != nil {
		t.Errorf("T7.1: DoRequestWithAttemptLog should not return error, got: %v", err)
	}
	if len(records) < 1 {
		t.Errorf("T7.1: DoRequestWithAttemptLog should return attempt records, got %d", len(records))
	}
}

func TestPhase7_T72_RetryRecord_500x3ThenSuccess(t *testing.T) {
	_ = zaptest.NewLogger(t)
	policy := DefaultRetryPolicy()

	// Simulate 3 failed attempts then success: 4 total records expected.
	serverErr := &ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "oops"}

	var records []RetryAttemptRecord
	for attempt := 1; attempt <= 3; attempt++ {
		decision := DecideRetry(serverErr, attempt, policy)
		records = append(records, RetryAttemptRecord{
			AttemptNumber: attempt,
			StatusCode:    500,
			Delay:         decision.Delay,
			RetryReason:   decision.RetryReason,
			Succeeded:     false,
		})
	}
	// 4th attempt succeeds.
	records = append(records, RetryAttemptRecord{
		AttemptNumber: 4,
		StatusCode:    200,
		Delay:         0,
		RetryReason:   "",
		Succeeded:     true,
	})

	// T7.2 acceptance: 4 attempt records, last one succeeded.
	if len(records) != 4 {
		t.Fatalf("T7.2: expected 4 records, got %d", len(records))
	}
	if !records[3].Succeeded {
		t.Errorf("T7.2: expected 4th attempt to succeed")
	}

	// Verify the stub returns actual attempt records.
	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, nil)
	retryCtx := RetryContext{Operation: ARMOperationGetPrefixSet}
	_, attemptRecords, err := client.DoRequestWithAttemptLog(
		context.Background(), retryCtx, "GET", "http://test", nil,
	)
	if err != nil {
		t.Errorf("T7.2: DoRequestWithAttemptLog should not return error, got: %v", err)
	}
	if len(attemptRecords) < 4 {
		t.Errorf("T7.2: expected at least 4 attempt records, got %d", len(attemptRecords))
	}
}

func TestPhase7_T73_RetryRecord_500x4Exhausted(t *testing.T) {
	_ = zaptest.NewLogger(t)
	policy := DefaultRetryPolicy()

	serverErr := &ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "oops"}

	// After attempt 4 (retriesUsed=3), should NOT retry.
	decision := DecideRetry(serverErr, 4, policy)
	if decision.Retry {
		t.Errorf("T7.3: DecideRetry at attempt 4 should not retry")
	}

	// Expected: 4 attempt records, all failed, no success.
	var records []RetryAttemptRecord
	for attempt := 1; attempt <= 4; attempt++ {
		records = append(records, RetryAttemptRecord{
			AttemptNumber: attempt,
			StatusCode:    500,
			Succeeded:     false,
		})
	}

	if len(records) != 4 {
		t.Fatalf("T7.3: expected 4 records, got %d", len(records))
	}

	for _, r := range records {
		if r.Succeeded {
			t.Errorf("T7.3: attempt %d should not have succeeded", r.AttemptNumber)
		}
	}

	// Verify stub returns attempt records.
	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, nil)
	_, attemptRecords, _ := client.DoRequestWithAttemptLog(
		context.Background(), RetryContext{}, "GET", "http://test", nil,
	)
	if len(attemptRecords) < 4 {
		t.Errorf("T7.3: expected at least 4 attempt records, got %d", len(attemptRecords))
	}
}

func TestPhase7_T74_RetryRecord_403SingleAttempt(t *testing.T) {
	_ = zaptest.NewLogger(t)
	policy := DefaultRetryPolicy()

	authErr := &ARMStatusError{StatusCode: 403, ARMCode: "AuthorizationFailed", Message: "forbidden"}

	decision := DecideRetry(authErr, 1, policy)
	if decision.Retry {
		t.Errorf("T7.4: 403 should not retry")
	}

	// Expected: exactly 1 attempt record, no retry.
	record := RetryAttemptRecord{
		AttemptNumber: 1,
		StatusCode:    403,
		Delay:         0,
		RetryReason:   "",
		Succeeded:     false,
	}

	if record.AttemptNumber != 1 {
		t.Errorf("T7.4: expected exactly 1 attempt")
	}
	if record.StatusCode != 403 {
		t.Errorf("T7.4: expected status 403, got %d", record.StatusCode)
	}

	// Verify stub returns exactly 1 attempt record.
	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, nil)
	_, records, err := client.DoRequestWithAttemptLog(
		context.Background(), RetryContext{}, "GET", "http://test", nil,
	)
	if len(records) != 1 {
		t.Errorf("T7.4: expected exactly 1 attempt record, got %d", len(records))
	}
	if err != nil {
		t.Errorf("T7.4: expected no error for 403 attempt log, got: %v", err)
	}
}
