package azure

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

var (
	// ErrRateLimitReservationDenied is returned when ReserveN returns !OK.
	ErrRateLimitReservationDenied = errors.New("rate limit reservation denied")
	// ErrRateLimitInfiniteDelay is returned when reservation delay is infinite.
	ErrRateLimitInfiniteDelay = errors.New("rate limit reservation returned infinite delay")
)

// maxSubscriptionLimiters caps the per-subscription limiter map to prevent
// unbounded growth in long-lived processes that touch many subscriptions.
const maxSubscriptionLimiters = 4096

// SubscriptionRateLimiter rate-limits ARM calls per subscription.
type SubscriptionRateLimiter interface {
	Wait(ctx context.Context, subscriptionID string) error
}

// RateLimitClock abstracts time for deterministic testing.
type RateLimitClock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// realClock uses real time.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ARMRateLimiterOption configures an ARMRateLimiter.
type ARMRateLimiterOption func(*ARMRateLimiter)

// WithClock sets a custom clock for testing.
func WithClock(clock RateLimitClock) ARMRateLimiterOption {
	return func(l *ARMRateLimiter) {
		l.clock = clock
	}
}

// WithMaxSubscriptions overrides the maximum number of cached per-subscription
// limiters. When the cap is reached the map is cleared to reclaim memory.
func WithMaxSubscriptions(n int) ARMRateLimiterOption {
	return func(l *ARMRateLimiter) {
		if n > 0 {
			l.maxSubs = n
		}
	}
}

// ARMRateLimiter provides per-subscription token-bucket rate limiting.
type ARMRateLimiter struct {
	log               *zap.Logger
	rps               float64
	burst             int
	clock             RateLimitClock
	maxSubs           int
	reserve           func(*rate.Limiter, time.Time, int) rateReservation
	rateLimitObserver armRateLimitObserver
	mu                sync.Mutex
	limiters          map[string]*rate.Limiter
}

type rateReservation interface {
	OK() bool
	DelayFrom(time.Time) time.Duration
	CancelAt(time.Time)
}

type standardRateReservation struct {
	*rate.Reservation
}

func reserveToken(limiter *rate.Limiter, now time.Time, tokens int) rateReservation {
	return standardRateReservation{Reservation: limiter.ReserveN(now, tokens)}
}

// NewARMRateLimiter creates a new per-subscription rate limiter.
// burst = max(1, ceil(rps)), except when rps <= 0 burst = 0.
func NewARMRateLimiter(log *zap.Logger, rps float64, opts ...ARMRateLimiterOption) *ARMRateLimiter {
	if log == nil {
		log = zap.NewNop()
	}
	burst := int(max(1, int(ceil(rps))))
	if rps <= 0 {
		burst = 0
	}
	l := &ARMRateLimiter{
		log:      log,
		rps:      rps,
		burst:    burst,
		clock:    realClock{},
		maxSubs:  maxSubscriptionLimiters,
		reserve:  reserveToken,
		limiters: make(map[string]*rate.Limiter),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// ceil returns math.Ceil as a float64 helper to keep the import tidy.
func ceil(f float64) float64 {
	if f <= 0 {
		return 0
	}
	i := float64(int(f))
	if f > i {
		return i + 1
	}
	return i
}

func (l *ARMRateLimiter) getLimiter(subscriptionID string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.limiters[subscriptionID]
	if !ok {
		// Evict all entries when the map grows beyond the cap.
		// This is a simple, predictable strategy that bounds memory
		// without adding LRU complexity.
		if len(l.limiters) >= l.maxSubs {
			l.log.Warn("rate limiter map reached capacity, evicting all entries",
				zap.Int("capacity", l.maxSubs),
			)
			l.limiters = make(map[string]*rate.Limiter)
		}
		lim = rate.NewLimiter(rate.Limit(l.rps), l.burst)
		l.limiters[subscriptionID] = lim
	}
	return lim
}

// isInfiniteDelay returns true when the delay from a rate.Reservation is
// effectively infinite and the caller should not wait.
func isInfiniteDelay(d time.Duration) bool {
	return d >= rate.InfDuration
}

// Wait blocks until the rate limiter allows a call for the given subscription.
func (l *ARMRateLimiter) Wait(ctx context.Context, subscriptionID string) error {
	lim := l.getLimiter(subscriptionID)
	now := l.clock.Now()
	reserve := l.reserve
	if reserve == nil {
		reserve = reserveToken
	}

	reservation := reserve(lim, now, 1)
	if !reservation.OK() {
		l.log.Warn("rate limit reservation denied",
			zap.String("subscriptionID", subscriptionID),
			zap.Float64("rps", l.rps),
		)
		return ErrRateLimitReservationDenied
	}

	delay := reservation.DelayFrom(now)

	if isInfiniteDelay(delay) {
		reservation.CancelAt(now)
		l.log.Warn("rate limit reservation returned infinite delay",
			zap.String("subscriptionID", subscriptionID),
		)
		return ErrRateLimitInfiniteDelay
	}

	if delay <= 0 {
		return nil
	}

	// Emit rate limit delay metric for positive delays only
	if l.rateLimitObserver != nil {
		l.rateLimitObserver.ObserveRateLimitDelay(subscriptionID, delay)
	}

	l.log.Debug("rate limiter throttling request",
		zap.String("subscriptionID", subscriptionID),
		zap.Duration("delay", delay),
	)

	if err := l.clock.Sleep(ctx, delay); err != nil {
		reservation.CancelAt(l.clock.Now())
		l.log.Debug("rate limiter wait cancelled",
			zap.String("subscriptionID", subscriptionID),
			zap.Error(err),
		)
		return err
	}

	return nil
}
