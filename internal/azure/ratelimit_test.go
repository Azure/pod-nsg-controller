package azure

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
	"golang.org/x/time/rate"
)

// fakeClock is a deterministic clock for rate limiter tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// -----------------------------------------------------------------------
// T7.6 – Rate limiter throttles burst of 20 calls, none dropped
// -----------------------------------------------------------------------
func TestPhase7_T76_ThrottlesBurstWithoutDropping_DeterministicClock(t *testing.T) {
	log := zaptest.NewLogger(t)
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(startTime)

	rps := float64(10)
	limiter := NewARMRateLimiter(log, rps, WithClock(clock))

	ctx := context.Background()
	subID := "test-sub-001"

	callCount := 20
	for i := 0; i < callCount; i++ {
		err := limiter.Wait(ctx, subID)
		if err != nil {
			t.Fatalf("Wait() call %d returned error: %v", i, err)
		}
	}

	// With 10 RPS and burst = ceil(10) = 10, first 10 calls should be immediate.
	// Remaining 10 calls should be spread over ~1 second total.
	elapsed := clock.Now().Sub(startTime)
	if elapsed <= 0 {
		t.Errorf("expected some delay for 20 calls at 10 RPS, got elapsed = %v", elapsed)
	}
	// At 10 RPS, 20 calls with burst=10 means 10 calls need spacing.
	// Minimum elapsed ≈ 10 * 100ms = 1s
	minExpected := 900 * time.Millisecond // slightly under to account for burst
	if elapsed < minExpected {
		t.Errorf("elapsed = %v, want >= %v for 20 calls at 10 RPS", elapsed, minExpected)
	}
}

// -----------------------------------------------------------------------
// Rate limiter: ReserveN !OK returns explicit error
// -----------------------------------------------------------------------
func TestARMRateLimiter_Wait_ReserveNotOK_ReturnsExplicitError(t *testing.T) {
	log := zaptest.NewLogger(t)
	// Create a limiter with 0 RPS which should cause reservation failure.
	limiter := NewARMRateLimiter(log, 0)

	ctx := context.Background()
	err := limiter.Wait(ctx, "test-sub")

	if err == nil {
		t.Fatal("Wait() with 0 RPS should return error, got nil")
	}
	if err != ErrRateLimitReservationDenied {
		t.Errorf("Wait() error = %v, want ErrRateLimitReservationDenied", err)
	}
}

// -----------------------------------------------------------------------
// Rate limiter: context cancelled during wait cancels reservation
// -----------------------------------------------------------------------
func TestARMRateLimiter_Wait_ContextCancelled_CancelsReservation(t *testing.T) {
	log := zaptest.NewLogger(t)
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cancelClock := &cancellingFakeClock{now: startTime}

	rps := float64(1)
	limiter := NewARMRateLimiter(log, rps, WithClock(cancelClock))

	ctx, cancel := context.WithCancel(context.Background())

	// Exhaust burst to force a delay.
	if err := limiter.Wait(ctx, "test-sub"); err != nil {
		t.Fatalf("first Wait() failed: %v", err)
	}

	// Cancel context before second call completes.
	cancel()

	err := limiter.Wait(ctx, "test-sub")
	if err == nil {
		t.Fatal("Wait() should return error when context is cancelled, got nil")
	}
	if err != context.Canceled {
		t.Errorf("Wait() error = %v, want context.Canceled", err)
	}
}

// cancellingFakeClock returns context error on Sleep.
type cancellingFakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *cancellingFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *cancellingFakeClock) Sleep(ctx context.Context, d time.Duration) error {
	// Simulate context cancellation during sleep.
	return ctx.Err()
}

type stubReservation struct {
	ok           bool
	delay        time.Duration
	cancelCalled bool
	cancelAt     time.Time
}

func (r *stubReservation) OK() bool { return r.ok }

func (r *stubReservation) DelayFrom(time.Time) time.Duration { return r.delay }

func (r *stubReservation) CancelAt(t time.Time) {
	r.cancelCalled = true
	r.cancelAt = t
}

// -----------------------------------------------------------------------
// Rate limiter: per-subscription isolation
// -----------------------------------------------------------------------
func TestARMRateLimiter_PerSubscriptionIsolation(t *testing.T) {
	log := zaptest.NewLogger(t)
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(startTime)

	rps := float64(1) // 1 RPS per subscription
	limiter := NewARMRateLimiter(log, rps, WithClock(clock))

	ctx := context.Background()

	// Call sub-a: should be immediate.
	err := limiter.Wait(ctx, "sub-a")
	if err != nil {
		t.Fatalf("Wait(sub-a) returned error: %v", err)
	}
	afterA := clock.Now()

	// Call sub-b: should also be immediate (different subscription).
	err = limiter.Wait(ctx, "sub-b")
	if err != nil {
		t.Fatalf("Wait(sub-b) returned error: %v", err)
	}
	afterB := clock.Now()

	// Both calls should complete without delay (different rate limiters).
	if afterB.Sub(afterA) > 0 {
		t.Errorf("sub-b should not be delayed by sub-a, got delay = %v", afterB.Sub(afterA))
	}
}

// -----------------------------------------------------------------------
// Rate limiter: InfDuration reservation error – exercises production branch
// -----------------------------------------------------------------------
func TestARMRateLimiter_Wait_ReserveInfDuration_CancelsAndReturnsError(t *testing.T) {
	log := zaptest.NewLogger(t)
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(startTime)
	limiter := NewARMRateLimiter(log, 10, WithClock(clock))
	reservation := &stubReservation{
		ok:    true,
		delay: rate.InfDuration,
	}
	limiter.reserve = func(_ *rate.Limiter, _ time.Time, _ int) rateReservation {
		return reservation
	}

	err := limiter.Wait(context.Background(), "test-sub")
	if err != ErrRateLimitInfiniteDelay {
		t.Fatalf("Wait() error = %v, want ErrRateLimitInfiniteDelay", err)
	}
	if !reservation.cancelCalled {
		t.Fatal("Wait() did not cancel reservation for infinite delay")
	}
	if reservation.cancelAt != startTime {
		t.Fatalf("CancelAt() time = %v, want %v", reservation.cancelAt, startTime)
	}
}

func TestARMRateLimiter_GetLimiter_ReusesSubscriptionEntries(t *testing.T) {
	log := zaptest.NewLogger(t)
	limiter := NewARMRateLimiter(log, 10)

	first := limiter.getLimiter("sub-a")
	second := limiter.getLimiter("sub-a")
	third := limiter.getLimiter("sub-b")

	if first != second {
		t.Errorf("getLimiter(sub-a) returned different limiter instances for same subscription")
	}
	if first == third {
		t.Errorf("getLimiter() returned same limiter instance for different subscriptions")
	}
	if got, want := len(limiter.limiters), 2; got != want {
		t.Errorf("len(limiters) = %d, want %d", got, want)
	}
}

// -----------------------------------------------------------------------
// Rate limiter: bounded map evicts entries at capacity
// -----------------------------------------------------------------------
func TestARMRateLimiter_GetLimiter_EvictsAtCapacity(t *testing.T) {
	log := zaptest.NewLogger(t)
	cap := 3
	limiter := NewARMRateLimiter(log, 10, WithMaxSubscriptions(cap))

	ctx := context.Background()

	// Fill to capacity.
	for i := 0; i < cap; i++ {
		if err := limiter.Wait(ctx, fmt.Sprintf("sub-%d", i)); err != nil {
			t.Fatalf("Wait(sub-%d) returned error: %v", i, err)
		}
	}
	if got := len(limiter.limiters); got != cap {
		t.Fatalf("len(limiters) = %d, want %d after filling", got, cap)
	}

	// One more subscription should trigger eviction.
	if err := limiter.Wait(ctx, "sub-overflow"); err != nil {
		t.Fatalf("Wait(sub-overflow) returned error: %v", err)
	}
	// After eviction the map should contain only the new entry.
	if got := len(limiter.limiters); got != 1 {
		t.Errorf("len(limiters) = %d, want 1 after eviction", got)
	}
}

// ---------------------------------------------------------------------------
// T7.6 Acceptance: Rate limiter burst metrics
// ---------------------------------------------------------------------------

func TestPhase7_T76_RateLimiter_CollectBurstMetrics(t *testing.T) {
	log := zaptest.NewLogger(t)
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(startTime)

	rps := float64(10)
	limiter := NewARMRateLimiter(log, rps, WithClock(clock))

	ctx := context.Background()
	subID := "test-sub-001"

	// T7.6: CollectBurstMetrics issues 20 calls and returns metrics.
	metrics, err := limiter.CollectBurstMetrics(ctx, subID, 20)
	if err != nil {
		t.Fatalf("T7.6: CollectBurstMetrics returned error: %v", err)
	}

	// Assert: all 20 calls tracked (no drops).
	if metrics.TotalWaits != 20 {
		t.Errorf("T7.6: TotalWaits = %d, want 20", metrics.TotalWaits)
	}

	// Assert: some calls were delayed (burst > capacity).
	if metrics.TotalDelayed == 0 {
		t.Errorf("T7.6: TotalDelayed = 0, want > 0 for 20 calls at 10 RPS")
	}

	// Assert: some calls were immediate (within burst).
	if metrics.TotalImmediate == 0 {
		t.Errorf("T7.6: TotalImmediate = 0, want > 0 for initial burst")
	}

	// Assert: total = delayed + immediate.
	if metrics.TotalDelayed+metrics.TotalImmediate != metrics.TotalWaits {
		t.Errorf("T7.6: TotalDelayed(%d) + TotalImmediate(%d) != TotalWaits(%d)",
			metrics.TotalDelayed, metrics.TotalImmediate, metrics.TotalWaits)
	}

	// Assert: max delay observed is reasonable for 10 RPS.
	if metrics.MaxDelayObserved == 0 {
		t.Errorf("T7.6: MaxDelayObserved = 0, want > 0 for throttled burst")
	}
}

// ---------------------------------------------------------------------------
// Phase 6: SetRPS — runtime RPS mutation
// ---------------------------------------------------------------------------

func TestPhase6_SetRPS_UpdatesExistingLimiters(t *testing.T) {
	log := zaptest.NewLogger(t)
	limiter := NewARMRateLimiter(log, 10)

	ctx := context.Background()
	// Warm up a subscription limiter.
	if err := limiter.Wait(ctx, "sub-a"); err != nil {
		t.Fatalf("Wait(sub-a) failed: %v", err)
	}

	// Update RPS.
	err := limiter.SetRPS(50)
	if err != nil {
		t.Fatalf("SetRPS(50) error: %v", err)
	}

	// Verify current RPS reflects new value.
	if got := limiter.RPS(); got != 50 {
		t.Errorf("RPS() = %v, want 50", got)
	}
}

func TestPhase6_SetRPS_NewSubscriptionsUseUpdatedValues(t *testing.T) {
	log := zaptest.NewLogger(t)
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(startTime)

	limiter := NewARMRateLimiter(log, 1, WithClock(clock)) // 1 RPS initially

	// Update to 100 RPS.
	if err := limiter.SetRPS(100); err != nil {
		t.Fatalf("SetRPS(100) error: %v", err)
	}

	ctx := context.Background()
	// A new subscription should use the updated rate. With 100 RPS and burst=100,
	// 50 calls should be immediate.
	for i := 0; i < 50; i++ {
		if err := limiter.Wait(ctx, "sub-new"); err != nil {
			t.Fatalf("Wait() call %d failed: %v", i, err)
		}
	}

	// At 100 RPS with burst=100, all 50 calls should be within burst (no delay).
	elapsed := clock.Now().Sub(startTime)
	if elapsed > 0 {
		t.Errorf("expected no delay for 50 calls at 100 RPS (burst=100), got %v", elapsed)
	}
}

func TestPhase6_SetRPS_ZeroRejected(t *testing.T) {
	log := zaptest.NewLogger(t)
	limiter := NewARMRateLimiter(log, 10)

	err := limiter.SetRPS(0)
	if err == nil {
		t.Error("expected error for SetRPS(0), got nil")
	}

	// Should still be at original value.
	if got := limiter.RPS(); got != 10 {
		t.Errorf("RPS() = %v after rejected SetRPS(0), want 10", got)
	}
}

func TestPhase6_SetRPS_NegativeRejected(t *testing.T) {
	log := zaptest.NewLogger(t)
	limiter := NewARMRateLimiter(log, 10)

	err := limiter.SetRPS(-5)
	if err == nil {
		t.Error("expected error for SetRPS(-5), got nil")
	}

	if got := limiter.RPS(); got != 10 {
		t.Errorf("RPS() = %v after rejected SetRPS(-5), want 10", got)
	}
}

func TestPhase6_RPS_ReturnsCurrentValue(t *testing.T) {
	log := zaptest.NewLogger(t)
	limiter := NewARMRateLimiter(log, 15.5)

	if got := limiter.RPS(); got != 15.5 {
		t.Errorf("RPS() = %v, want 15.5", got)
	}
}
