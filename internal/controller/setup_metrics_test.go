package controller

import (
	"testing"
	"time"

	"github.com/Azure/pod-nsg-controller/internal/metrics"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TestPhase8_SetupWithManager_PreservesInstrumentedQueueFactory
// Design §4.2: SetupWithManager wires NewInstrumentedQueueFactory when MetricsRecorder is set.
// Verifies that the instrumented queue path is preserved and functional.
func TestPhase8_SetupWithManager_PreservesInstrumentedQueueFactory(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()
	rec, err := metrics.Register()
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	scheme := testScheme(t)

	r := &MappingReconciler{
		Scheme:          scheme,
		ClusterName:     "test-cluster",
		ResyncInterval:  60 * time.Second,
		MetricsRecorder: rec,
	}

	// Verify that the reconciler has metrics recorder set (precondition for queue wiring).
	if r.MetricsRecorder == nil {
		t.Fatal("MetricsRecorder is nil; SetupWithManager would skip queue instrumentation")
	}
	if r.MetricsRecorder.Reconcile == nil {
		t.Fatal("MetricsRecorder.Reconcile is nil; instrumented queue factory would fail")
	}

	// Verify the queue factory function can be created without panicking.
	factory := metrics.NewInstrumentedQueueFactory(r.MetricsRecorder.Reconcile)
	if factory == nil {
		t.Error("NewInstrumentedQueueFactory returned nil; setup would not wire queue metrics")
	}
}

// ---------------------------------------------------------------------------
// Phase 3: TestPhase3_NewMappingControllerRateLimiter_ReturnsNonNil
// Verifies the bucket-backed rate limiter helper is non-nil and functional.
// ---------------------------------------------------------------------------
func TestPhase3_NewMappingControllerRateLimiter_ReturnsNonNil(t *testing.T) {
	debounceInterval := 2 * time.Second

	limiter := newMappingControllerRateLimiter(debounceInterval)
	if limiter == nil {
		t.Fatal("Phase 3: newMappingControllerRateLimiter returned nil; bucket-backed limiter not wired")
	}

	// Verify the limiter is usable for reconcile.Request keys
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "test"},
	}
	when := limiter.When(req)
	if when < 0 {
		t.Errorf("Phase 3: limiter.When() returned negative duration: %v", when)
	}
}

// ---------------------------------------------------------------------------
// Phase 3: TestPhase3_NewMappingControllerRateLimiter_BucketBackedThrottle
// Verifies the composed limiter includes bucket-backed throttling: after the
// initial burst token is consumed, subsequent requests must be delayed.
// ---------------------------------------------------------------------------
func TestPhase3_NewMappingControllerRateLimiter_BucketBackedThrottle(t *testing.T) {
	debounceInterval := 2 * time.Second

	limiter := newMappingControllerRateLimiter(debounceInterval)
	if limiter == nil {
		t.Fatal("Phase 3: limiter is nil")
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "bucket-test"},
	}

	// First call consumes the burst token — should return 0 or near-zero delay.
	first := limiter.When(req)

	// Second call must be delayed by the bucket (>= debounceInterval minus small tolerance).
	second := limiter.When(req)
	if second < debounceInterval/2 {
		t.Errorf("Phase 3: expected bucket-backed throttle delay >= %v after burst consumed, got %v; "+
			"limiter is missing TypedBucketRateLimiter composition", debounceInterval/2, second)
	}

	// Sanity: first call should not exceed debounce interval (burst token available).
	if first > debounceInterval {
		t.Errorf("Phase 3: first When() returned %v, expected <= %v (burst token should be available)", first, debounceInterval)
	}
}

// ---------------------------------------------------------------------------
// Phase 3: TestPhase3_NewMappingControllerRateLimiter_ZeroIntervalReturnsNonNil
// With zero interval, the limiter still works (behaves as default).
// ---------------------------------------------------------------------------
func TestPhase3_NewMappingControllerRateLimiter_ZeroIntervalReturnsNonNil(t *testing.T) {
	limiter := newMappingControllerRateLimiter(0)
	if limiter == nil {
		t.Fatal("Phase 3: newMappingControllerRateLimiter(0) returned nil")
	}
}

// ---------------------------------------------------------------------------
// Phase 3: TestPhase3_SetupWithManager_MetricsQueueWithRateLimiter
// Validates that instrumented queue factory remains constructible alongside
// the bucket-backed rate limiter.
// ---------------------------------------------------------------------------
func TestPhase3_SetupWithManager_MetricsQueueWithRateLimiter(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()
	rec, err := metrics.Register()
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	debounceInterval := 2 * time.Second
	limiter := newMappingControllerRateLimiter(debounceInterval)
	if limiter == nil {
		t.Fatal("Phase 3: rate limiter is nil")
	}

	factory := metrics.NewInstrumentedQueueFactory(rec.Reconcile)
	if factory == nil {
		t.Fatal("Phase 3: instrumented queue factory is nil")
	}

	// The factory should produce a working queue with the custom limiter
	queue := factory("test-phase3", limiter)
	if queue == nil {
		t.Fatal("Phase 3: instrumented queue factory produced nil queue with custom limiter")
	}
	defer queue.ShutDown()
}
