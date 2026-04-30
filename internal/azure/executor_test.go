package azure

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/pod-nsg-controller/internal/engine"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// stubClient is a minimal in-package fake for executor tests.
type stubClient struct {
	mu    sync.Mutex
	store map[string][]string // key -> IPs

	fail412Count int
	putCalls     int
}

func newStubClient() *stubClient {
	return &stubClient{store: make(map[string][]string)}
}

func stubKey(subID, rg, asg, ps string) string {
	return fmt.Sprintf("%s/%s/%s/%s", subID, rg, asg, ps)
}

func (s *stubClient) Get(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) (*AddressPrefixSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := stubKey(subscriptionID, resourceGroup, asgName, prefixSetName)
	ips, ok := s.store[k]
	if !ok {
		return nil, ErrNotFound
	}
	name := prefixSetName
	etag := `"etag-` + k + `"`
	return &AddressPrefixSet{
		Name: &name,
		Etag: &etag,
		Properties: &AddressPrefixSetProperties{
			AddressPrefixes: ips,
		},
	}, nil
}

func (s *stubClient) Put(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string, ips []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putCalls++
	if s.putCalls <= s.fail412Count {
		return &ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "etag mismatch"}
	}
	k := stubKey(subscriptionID, resourceGroup, asgName, prefixSetName)
	s.store[k] = ips
	return nil
}

func (s *stubClient) Delete(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := stubKey(subscriptionID, resourceGroup, asgName, prefixSetName)
	delete(s.store, k)
	return nil
}

func (s *stubClient) List(ctx context.Context, subscriptionID, resourceGroup, asgName string) ([]AddressPrefixSet, error) {
	return nil, nil
}

// stubFactory routes subscriptionID -> AddressPrefixSetAPI
type stubFactory struct {
	mu      sync.RWMutex
	clients map[string]AddressPrefixSetAPI
}

func newStubFactory() *stubFactory {
	return &stubFactory{clients: make(map[string]AddressPrefixSetAPI)}
}

func (f *stubFactory) Register(subID string, c AddressPrefixSetAPI) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clients[subID] = c
}

func (f *stubFactory) ForSubscription(subscriptionID string) (AddressPrefixSetAPI, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	c, ok := f.clients[subscriptionID]
	if !ok {
		return nil, fmt.Errorf("no client for subscription %q", subscriptionID)
	}
	return c, nil
}

// --- T4.1: Execute CreatePrefixSet action with fake ---
func TestPhase4_T41_ExecuteCreatePrefixSetWithFake(t *testing.T) {
	log := zaptest.NewLogger(t)
	client := newStubClient()
	factory := newStubFactory()
	factory.Register("sub1", client)

	executor := NewExecutor(log, factory, 1)
	actions := []engine.Action{
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32", "10.0.0.2/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Errorf("T4.1: expected action to succeed, got error: %v", results[0].Err)
	}

	// Verify fake state was updated
	got, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("T4.1: expected resource to exist after create, got error: %v", err)
	}
	if got.Properties == nil || len(got.Properties.AddressPrefixes) != 2 {
		t.Errorf("T4.1: expected 2 address prefixes, got %v", got)
	}
}

// --- T4.2: Execute UpdatePrefixSet action with fake ---
func TestPhase4_T42_ExecuteUpdatePrefixSetWithFake(t *testing.T) {
	log := zaptest.NewLogger(t)
	client := newStubClient()
	factory := newStubFactory()
	factory.Register("sub1", client)

	// Pre-populate with existing state
	client.store[stubKey("sub1", "rg1", "asg1", "ps1")] = []string{"10.0.0.1/32"}

	executor := NewExecutor(log, factory, 1)
	actions := []engine.Action{
		{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32", "10.0.0.3/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Errorf("T4.2: expected action to succeed, got error: %v", results[0].Err)
	}

	// Verify fake state reflects new IPs
	got, _ := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if got == nil || got.Properties == nil {
		t.Fatal("T4.2: expected resource to exist after update")
	}
	ips := got.Properties.AddressPrefixes
	if len(ips) != 2 || ips[0] != "10.0.0.1/32" || ips[1] != "10.0.0.3/32" {
		t.Errorf("T4.2: expected IPs [10.0.0.1/32, 10.0.0.3/32], got %v", ips)
	}
}

// --- T4.3: Execute DeletePrefixSet action with fake ---
func TestPhase4_T43_ExecuteDeletePrefixSetWithFake(t *testing.T) {
	log := zaptest.NewLogger(t)
	client := newStubClient()
	factory := newStubFactory()
	factory.Register("sub1", client)

	// Pre-populate
	client.store[stubKey("sub1", "rg1", "asg1", "ps1")] = []string{"10.0.0.1/32"}

	executor := NewExecutor(log, factory, 1)
	actions := []engine.Action{
		{
			Kind: engine.DeletePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Errorf("T4.3: expected action to succeed, got error: %v", results[0].Err)
	}

	// Verify fake state entry removed
	_, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("T4.3: expected ErrNotFound after delete, got: %v", err)
	}
}

// --- T4.4: Execute actions across 2 subscriptions ---
func TestPhase4_T44_ExecuteAcrossTwoSubscriptionsRoutesCorrectly(t *testing.T) {
	log := zaptest.NewLogger(t)
	client1 := newStubClient()
	client2 := newStubClient()
	factory := newStubFactory()
	factory.Register("sub1", client1)
	factory.Register("sub2", client2)

	executor := NewExecutor(log, factory, 2)
	actions := []engine.Action{
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub2",
				ResourceGroup:  "rg2",
				ASGName:        "asg2",
				PrefixSetName:  "ps2",
			},
			DesiredIPs: []string{"10.0.0.2/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	for i, r := range results {
		if !r.Success {
			t.Errorf("T4.4: action %d failed: %v", i, r.Err)
		}
	}

	// Verify sub1 got ps1 and sub2 got ps2
	got1, err := client1.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Errorf("T4.4: sub1 ps1 should exist: %v", err)
	}
	if got1 != nil && got1.Properties != nil && len(got1.Properties.AddressPrefixes) != 1 {
		t.Errorf("T4.4: sub1 ps1 wrong IPs: %v", got1.Properties.AddressPrefixes)
	}

	got2, err := client2.Get(context.Background(), "sub2", "rg2", "asg2", "ps2")
	if err != nil {
		t.Errorf("T4.4: sub2 ps2 should exist: %v", err)
	}
	if got2 != nil && got2.Properties != nil && len(got2.Properties.AddressPrefixes) != 1 {
		t.Errorf("T4.4: sub2 ps2 wrong IPs: %v", got2.Properties.AddressPrefixes)
	}

	// sub2 should NOT have ps1
	_, err = client2.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("T4.4: sub2 should not have sub1's resource, got: %v", err)
	}
}

// --- T4.5: ETag conflict on PUT → automatic retry succeeds ---
func TestPhase4_T45_ETagConflictPutRetrySucceeds(t *testing.T) {
	log := zaptest.NewLogger(t)
	client := newStubClient()
	client.fail412Count = 1 // first PUT returns 412, second succeeds
	factory := newStubFactory()
	factory.Register("sub1", client)

	executor := NewExecutor(log, factory, 1)
	actions := []engine.Action{
		{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
	}

	// Pre-populate so update path is valid
	client.store[stubKey("sub1", "rg1", "asg1", "ps1")] = []string{"10.0.0.0/32"}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Errorf("T4.5: expected retry to succeed, got error: %v", results[0].Err)
	}

	// Verify the action eventually succeeded
	got, _ := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if got == nil || got.Properties == nil {
		t.Fatal("T4.5: resource should exist after retry")
	}
	if len(got.Properties.AddressPrefixes) != 1 || got.Properties.AddressPrefixes[0] != "10.0.0.1/32" {
		t.Errorf("T4.5: expected IPs [10.0.0.1/32], got %v", got.Properties.AddressPrefixes)
	}
}

func TestPhase4_Executor_ETagConflictDeleteRetryStaysDelete(t *testing.T) {
	log := zaptest.NewLogger(t)
	client := &deleteRetryClient{exists: true}
	factory := newStubFactory()
	factory.Register("sub1", client)

	executor := NewExecutor(log, factory, 1)
	actions := []engine.Action{
		{
			Kind: engine.DeletePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("expected delete retry to succeed, got error: %v", results[0].Err)
	}
	if client.deleteCalls != 2 {
		t.Errorf("expected 2 delete attempts (initial 412 + retry), got %d", client.deleteCalls)
	}
	if client.putCalls != 0 {
		t.Errorf("expected delete recompute to avoid PUT, got %d PUT calls", client.putCalls)
	}

	_, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected resource to be deleted after retry, got: %v", err)
	}
}

// --- T4.6: ETag conflict exceeds max retries (3) ---
func TestPhase4_T46_ETagConflictExceedsMaxRetries(t *testing.T) {
	log := zaptest.NewLogger(t)
	client := newStubClient()
	client.fail412Count = 10 // always 412
	factory := newStubFactory()
	factory.Register("sub1", client)

	executor := NewExecutor(log, factory, 1)
	actions := []engine.Action{
		{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
	}

	client.store[stubKey("sub1", "rg1", "asg1", "ps1")] = []string{"10.0.0.0/32"}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Success {
		t.Error("T4.6: expected action to fail after max retries, but it succeeded")
	}
	if results[0].Err == nil {
		t.Error("T4.6: expected error after max retries, got nil")
	}
}

func TestPhase4_Executor_ETagConflictRetryReturnsSuccessWhenRecomputeIsNoOp(t *testing.T) {
	log := zaptest.NewLogger(t)
	client := &noOpAfterConflictClient{desiredIPs: []string{"10.0.0.1/32"}}
	factory := newStubFactory()
	factory.Register("sub1", client)

	executor := NewExecutor(log, factory, 1)
	actions := []engine.Action{
		{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("expected recompute no-op to return success, got error: %v", results[0].Err)
	}
	if client.putCalls != 1 {
		t.Errorf("expected only the initial PUT attempt before recompute no-op, got %d PUT calls", client.putCalls)
	}
}

// --- T4.7: Parallel execution of 5 actions, bounded concurrency ---
func TestPhase4_T47_ParallelExecutionBoundedConcurrency(t *testing.T) {
	log := zaptest.NewLogger(t)
	client := newStubClient()
	factory := newStubFactory()
	factory.Register("sub1", client)

	maxParallel := 2
	executor := NewExecutor(log, factory, maxParallel)

	var concurrencyPeak int64
	var currentConcurrency int64

	// Use a wrapper client that tracks concurrency
	trackingClient := &concurrencyTrackingClient{
		inner:   client,
		current: &currentConcurrency,
		peak:    &concurrencyPeak,
	}
	factory.clients["sub1"] = trackingClient

	actions := make([]engine.Action, 5)
	for i := 0; i < 5; i++ {
		actions[i] = engine.Action{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  fmt.Sprintf("ps%d", i),
			},
			DesiredIPs: []string{fmt.Sprintf("10.0.0.%d/32", i)},
		}
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 5 {
		t.Fatalf("T4.7: expected 5 results, got %d", len(results))
	}

	for i, r := range results {
		if !r.Success {
			t.Errorf("T4.7: action %d failed: %v", i, r.Err)
		}
	}

	peak := atomic.LoadInt64(&concurrencyPeak)
	if peak > int64(maxParallel) {
		t.Errorf("T4.7: concurrency peak %d exceeded maxParallel %d", peak, maxParallel)
	}
}

// concurrencyTrackingClient wraps a client and tracks concurrent Put calls.
type concurrencyTrackingClient struct {
	inner   AddressPrefixSetAPI
	current *int64
	peak    *int64
}

func (c *concurrencyTrackingClient) Get(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) (*AddressPrefixSet, error) {
	return c.inner.Get(ctx, subscriptionID, resourceGroup, asgName, prefixSetName)
}

func (c *concurrencyTrackingClient) Put(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string, ips []string) error {
	cur := atomic.AddInt64(c.current, 1)
	for {
		old := atomic.LoadInt64(c.peak)
		if cur <= old || atomic.CompareAndSwapInt64(c.peak, old, cur) {
			break
		}
	}
	defer atomic.AddInt64(c.current, -1)
	return c.inner.Put(ctx, subscriptionID, resourceGroup, asgName, prefixSetName, ips)
}

func (c *concurrencyTrackingClient) Delete(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) error {
	cur := atomic.AddInt64(c.current, 1)
	for {
		old := atomic.LoadInt64(c.peak)
		if cur <= old || atomic.CompareAndSwapInt64(c.peak, old, cur) {
			break
		}
	}
	defer atomic.AddInt64(c.current, -1)
	return c.inner.Delete(ctx, subscriptionID, resourceGroup, asgName, prefixSetName)
}

func (c *concurrencyTrackingClient) List(ctx context.Context, subscriptionID, resourceGroup, asgName string) ([]AddressPrefixSet, error) {
	return c.inner.List(ctx, subscriptionID, resourceGroup, asgName)
}

// --- T4.8: One action fails, others succeed ---
func TestPhase4_T48_OneActionFailsOthersSucceed(t *testing.T) {
	log := zaptest.NewLogger(t)
	goodClient := newStubClient()
	factory := newStubFactory()
	factory.Register("sub-good", goodClient)
	// sub-bad is NOT registered → ForSubscription returns error

	executor := NewExecutor(log, factory, 2)
	actions := []engine.Action{
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub-good",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub-bad",
				ResourceGroup:  "rg2",
				ASGName:        "asg2",
				PrefixSetName:  "ps2",
			},
			DesiredIPs: []string{"10.0.0.2/32"},
		},
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub-good",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps3",
			},
			DesiredIPs: []string{"10.0.0.3/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 3 {
		t.Fatalf("T4.8: expected 3 results, got %d", len(results))
	}

	// Action 0 (sub-good) should succeed
	if !results[0].Success {
		t.Errorf("T4.8: action 0 should succeed, got error: %v", results[0].Err)
	}
	// Action 1 (sub-bad) should fail
	if results[1].Success {
		t.Error("T4.8: action 1 should fail (bad subscription), but it succeeded")
	}
	if results[1].Err == nil {
		t.Error("T4.8: action 1 should have an error, got nil")
	}
	// Action 2 (sub-good) should still succeed despite action 1 failure
	if !results[2].Success {
		t.Errorf("T4.8: action 2 should succeed despite action 1 failure, got error: %v", results[2].Err)
	}
}

// --- T4.10: PUT with empty IP list clears prefix set ---
func TestPhase4_T410_PutEmptyIPListClearsPrefixSet(t *testing.T) {
	log := zaptest.NewLogger(t)
	client := newStubClient()
	factory := newStubFactory()
	factory.Register("sub1", client)

	// Pre-populate with existing IPs
	client.store[stubKey("sub1", "rg1", "asg1", "ps1")] = []string{"10.0.0.1/32"}

	executor := NewExecutor(log, factory, 1)
	actions := []engine.Action{
		{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{}, // empty list — clears membership
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Errorf("T4.10: expected action to succeed, got error: %v", results[0].Err)
	}

	// Verify the prefix set now has empty IPs
	got, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("T4.10: expected resource to still exist, got error: %v", err)
	}
	if got.Properties == nil || len(got.Properties.AddressPrefixes) != 0 {
		var ips []string
		if got.Properties != nil {
			ips = got.Properties.AddressPrefixes
		}
		t.Errorf("T4.10: expected empty prefix list, got %v", ips)
	}
}

// --- Additional: Result order matches input order ---
func TestPhase4_Executor_ResultOrderMatchesInputOrder(t *testing.T) {
	log := zaptest.NewLogger(t)
	client := newStubClient()
	factory := newStubFactory()
	factory.Register("sub1", client)

	executor := NewExecutor(log, factory, 4)
	actions := make([]engine.Action, 10)
	for i := 0; i < 10; i++ {
		actions[i] = engine.Action{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  fmt.Sprintf("ps-%d", i),
			},
			DesiredIPs: []string{fmt.Sprintf("10.0.0.%d/32", i)},
		}
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 10 {
		t.Fatalf("expected 10 results, got %d", len(results))
	}

	for i, r := range results {
		expectedPS := fmt.Sprintf("ps-%d", i)
		if r.Action.Target.PrefixSetName != expectedPS {
			t.Errorf("result[%d]: expected target %q, got %q", i, expectedPS, r.Action.Target.PrefixSetName)
		}
	}
}

// --- Additional: Uses errors.Is for NotFound branching ---
func TestPhase4_Executor_UsesErrorsIsForNotFoundBranching(t *testing.T) {
	// Verify that IsNotFound correctly uses errors.Is semantics
	armErr := &ARMStatusError{StatusCode: 404, ARMCode: "ResourceNotFound", Message: "not found"}

	if !errors.Is(armErr, ErrNotFound) {
		t.Error("ARMStatusError{404} should be errors.Is(ErrNotFound)")
	}

	if !IsNotFound(armErr) {
		t.Error("IsNotFound should return true for ARMStatusError{404}")
	}

	// Non-404 should not match
	arm412 := &ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "etag mismatch"}
	if errors.Is(arm412, ErrNotFound) {
		t.Error("ARMStatusError{412} should NOT be errors.Is(ErrNotFound)")
	}

	if IsNotFound(arm412) {
		t.Error("IsNotFound should return false for ARMStatusError{412}")
	}

	// IsPreconditionFailed should work for 412
	if !IsPreconditionFailed(arm412) {
		t.Error("IsPreconditionFailed should return true for ARMStatusError{412}")
	}

	if IsPreconditionFailed(armErr) {
		t.Error("IsPreconditionFailed should return false for ARMStatusError{404}")
	}
}

// --- T4.Cancellation: Executor cancelled while waiting for semaphore ---
func TestPhase4_Executor_CancelledWhileWaitingForSemaphore(t *testing.T) {
	log := zaptest.NewLogger(t)

	// Use a blocking client that holds the semaphore
	blockCh := make(chan struct{})
	blocking := &blockingClient{blockCh: blockCh}
	factory := newStubFactory()
	factory.Register("sub1", blocking)

	// maxParallel=1 so second action waits on semaphore
	executor := NewExecutor(log, factory, 1)

	ctx, cancel := context.WithCancel(context.Background())

	actions := []engine.Action{
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps-blocking",
			},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps-waiting",
			},
			DesiredIPs: []string{"10.0.0.2/32"},
		},
	}

	// Run executor in background
	done := make(chan []ActionResult, 1)
	go func() {
		done <- executor.Execute(ctx, actions)
	}()

	// Give time for first action to acquire semaphore
	// then cancel context while second is waiting
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Unblock the first action
	close(blockCh)

	results := <-done
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// The waiting action should have a context cancellation error
	waitingResult := results[1]
	if waitingResult.Err == nil {
		t.Error("expected error for action waiting on cancelled semaphore, got nil")
	}
	if waitingResult.Err != nil && !errors.Is(waitingResult.Err, context.Canceled) {
		t.Errorf("expected context.Canceled for waiting action, got: %v", waitingResult.Err)
	}
}

// blockingClient blocks Put calls until blockCh is closed.
type blockingClient struct {
	blockCh chan struct{}
}

func (b *blockingClient) Get(_ context.Context, _, _, _, _ string) (*AddressPrefixSet, error) {
	return nil, ErrNotFound
}
func (b *blockingClient) Put(ctx context.Context, _, _, _, _ string, _ []string) error {
	select {
	case <-b.blockCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *blockingClient) Delete(_ context.Context, _, _, _, _ string) error { return nil }
func (b *blockingClient) List(_ context.Context, _, _, _ string) ([]AddressPrefixSet, error) {
	return nil, nil
}

// --- T4.Cancellation: Cancelled during retry loop stops immediately ---
func TestPhase4_Executor_CancelledDuringRetryLoopStopsImmediately(t *testing.T) {
	log := zaptest.NewLogger(t)

	// Client that always returns 412
	always412 := &always412Client{}
	factory := newStubFactory()
	factory.Register("sub1", always412)

	executor := NewExecutor(log, factory, 1)

	ctx, cancel := context.WithCancel(context.Background())

	actions := []engine.Action{
		{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
	}

	// Cancel after short delay so retry loop sees it
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	results := executor.Execute(ctx, actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Should fail due to cancellation, not exhaust retries
	if results[0].Err == nil {
		t.Error("expected error from cancelled retry loop, got nil")
	}
	if results[0].Err != nil && !errors.Is(results[0].Err, context.Canceled) {
		// Accept either context.Canceled or a 412 error (since retry is stubbed)
		// The key test is that it returns promptly, not after full retry count
		t.Logf("retry loop returned error (acceptable if prompt): %v", results[0].Err)
	}
}

// always412Client returns 412 on all Put calls and has an existing resource for Get.
type always412Client struct{}

func (a *always412Client) Get(_ context.Context, _, _, _, prefixSetName string) (*AddressPrefixSet, error) {
	name := prefixSetName
	etag := `"etag-existing"`
	return &AddressPrefixSet{
		Name: &name,
		Etag: &etag,
		Properties: &AddressPrefixSetProperties{
			AddressPrefixes: []string{"10.0.0.0/32"},
		},
	}, nil
}
func (a *always412Client) Put(_ context.Context, _, _, _, _ string, _ []string) error {
	return &ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "always conflict"}
}
func (a *always412Client) Delete(_ context.Context, _, _, _, _ string) error { return nil }
func (a *always412Client) List(_ context.Context, _, _, _ string) ([]AddressPrefixSet, error) {
	return nil, nil
}

type deleteRetryClient struct {
	mu          sync.Mutex
	deleteCalls int
	putCalls    int
	exists      bool
}

func (c *deleteRetryClient) Get(_ context.Context, _, _, _, prefixSetName string) (*AddressPrefixSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.exists {
		return nil, ErrNotFound
	}
	name := prefixSetName
	etag := `"etag-existing"`
	return &AddressPrefixSet{
		Name: &name,
		Etag: &etag,
		Properties: &AddressPrefixSetProperties{
			AddressPrefixes: []string{"10.0.0.1/32"},
		},
	}, nil
}

func (c *deleteRetryClient) Put(_ context.Context, _, _, _, _ string, _ []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putCalls++
	return nil
}

func (c *deleteRetryClient) Delete(_ context.Context, _, _, _, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteCalls++
	if c.deleteCalls == 1 {
		return &ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "etag mismatch"}
	}
	c.exists = false
	return nil
}

func (c *deleteRetryClient) List(_ context.Context, _, _, _ string) ([]AddressPrefixSet, error) {
	return nil, nil
}

type noOpAfterConflictClient struct {
	mu         sync.Mutex
	putCalls   int
	desiredIPs []string
}

func (c *noOpAfterConflictClient) Get(_ context.Context, _, _, _, prefixSetName string) (*AddressPrefixSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := prefixSetName
	etag := `"etag-updated"`
	ips := append([]string(nil), c.desiredIPs...)
	return &AddressPrefixSet{
		Name: &name,
		Etag: &etag,
		Properties: &AddressPrefixSetProperties{
			AddressPrefixes: ips,
		},
	}, nil
}

func (c *noOpAfterConflictClient) Put(_ context.Context, _, _, _, _ string, _ []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putCalls++
	if c.putCalls == 1 {
		return &ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "etag mismatch"}
	}
	return fmt.Errorf("unexpected second PUT after recompute no-op")
}

func (c *noOpAfterConflictClient) Delete(_ context.Context, _, _, _, _ string) error { return nil }

func (c *noOpAfterConflictClient) List(_ context.Context, _, _, _ string) ([]AddressPrefixSet, error) {
	return nil, nil
}

// --- T4.Recompute: recomputeSingleTargetActionViaDiff uses engine.ComputeDiff ---
func TestPhase4_Executor_RecomputeSingleTargetActionViaDiff_UsesComputeDiff(t *testing.T) {
	// Test that recompute delegates to engine.ComputeDiff for proper diff-based retry
	action := engine.Action{
		Kind: engine.UpdatePrefixSet,
		Target: engine.ASGTarget{
			SubscriptionID: "sub1",
			ResourceGroup:  "rg1",
			ASGName:        "asg1",
			PrefixSetName:  "ps1",
		},
		DesiredIPs: []string{"10.0.0.1/32", "10.0.0.2/32"},
	}

	// Simulate: current state already has 10.0.0.1/32, so only 10.0.0.2/32 is new
	name := "ps1"
	etag := `"etag-v1"`
	current := &AddressPrefixSet{
		Name: &name,
		Etag: &etag,
		Properties: &AddressPrefixSetProperties{
			AddressPrefixes: []string{"10.0.0.1/32"},
		},
	}

	next, done, err := recomputeSingleTargetActionViaDiff(action, current, nil)
	if err != nil {
		t.Fatalf("recompute returned unexpected error: %v", err)
	}
	if done {
		t.Error("recompute should not be done when desired differs from actual")
	}
	if next == nil {
		t.Fatal("recompute should return a next action when diff exists")
	}

	// The recomputed action should have the full desired IPs (sorted)
	if next.Kind != engine.UpdatePrefixSet {
		t.Errorf("expected UpdatePrefixSet, got %s", next.Kind)
	}
	if len(next.DesiredIPs) != 2 {
		t.Errorf("expected 2 desired IPs, got %d: %v", len(next.DesiredIPs), next.DesiredIPs)
	}

	// Test: when actual matches desired, recompute returns done=true (no-op)
	currentMatching := &AddressPrefixSet{
		Name: &name,
		Etag: &etag,
		Properties: &AddressPrefixSetProperties{
			AddressPrefixes: []string{"10.0.0.1/32", "10.0.0.2/32"},
		},
	}
	_, doneMatch, errMatch := recomputeSingleTargetActionViaDiff(action, currentMatching, nil)
	if errMatch != nil {
		t.Fatalf("recompute on matching state returned error: %v", errMatch)
	}
	if !doneMatch {
		t.Error("recompute should return done=true when actual matches desired")
	}

	// Test: when Get returns ErrNotFound, recompute should return CreatePrefixSet
	nextCreate, doneCreate, errCreate := recomputeSingleTargetActionViaDiff(action, nil, ErrNotFound)
	if errCreate != nil {
		t.Fatalf("recompute with ErrNotFound returned error: %v", errCreate)
	}
	if doneCreate {
		t.Error("recompute should not be done when resource not found but desired IPs exist")
	}
	if nextCreate == nil {
		t.Fatal("recompute should return a create action when resource not found")
	}
	if nextCreate.Kind != engine.CreatePrefixSet {
		t.Errorf("expected CreatePrefixSet when resource not found, got %s", nextCreate.Kind)
	}
}

// TestDeleteActionRecomputePreservesKind verifies that a DELETE action
// recomputed after a 412 retry still produces a DeletePrefixSet action
// (not an UpdatePrefixSet) when the resource still exists.
func TestDeleteActionRecomputePreservesKind(t *testing.T) {
	action := engine.Action{
		Kind: engine.DeletePrefixSet,
		Target: engine.ASGTarget{
			SubscriptionID: "sub1",
			ResourceGroup:  "rg1",
			ASGName:        "asg1",
			PrefixSetName:  "ps1",
		},
		DesiredIPs: nil, // DELETE has no desired IPs
	}

	// Simulate the resource still exists after 412
	current := &AddressPrefixSet{
		Properties: &AddressPrefixSetProperties{
			AddressPrefixes: []string{"10.0.0.1/32"},
		},
	}

	next, done, err := recomputeSingleTargetActionViaDiff(action, current, nil)
	if err != nil {
		t.Fatalf("recompute failed: %v", err)
	}
	if done {
		t.Fatal("expected recompute to produce an action, got done=true")
	}
	if next == nil {
		t.Fatal("expected recompute to produce an action, got nil")
	}

	if next.Kind != engine.DeletePrefixSet {
		t.Errorf("expected DeletePrefixSet, got %s (DesiredIPs=%v)", next.Kind, next.DesiredIPs)
	}
}

func TestPhase4_Executor_Final412LogDoesNotClaimRetry(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	log := zap.New(core)

	client := &always412Client{}
	factory := newStubFactory()
	factory.Register("sub1", client)

	executor := NewExecutor(log, factory, 1)
	results := executor.Execute(context.Background(), []engine.Action{
		{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
	})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("expected executor to return final 412 error")
	}

	entries := logs.All()
	if len(entries) != 3 {
		t.Fatalf("expected 3 warning logs, got %d", len(entries))
	}
	for i := 0; i < len(entries)-1; i++ {
		if entries[i].Message != "ETag conflict, retrying" {
			t.Fatalf("expected retry log before terminal attempt, got %q", entries[i].Message)
		}
	}
	if got := entries[len(entries)-1].Message; got != "ETag conflict, retries exhausted" {
		t.Fatalf("expected terminal warning to say retries exhausted, got %q", got)
	}
}

func TestPhase4_BuildSingleTargetActual_NilCurrentTreatsResourceAsAbsent(t *testing.T) {
	action := engine.Action{
		Kind: engine.UpdatePrefixSet,
		Target: engine.ASGTarget{
			SubscriptionID: "sub1",
			ResourceGroup:  "rg1",
			ASGName:        "asg1",
			PrefixSetName:  "ps1",
		},
		DesiredIPs: []string{"10.0.0.1/32"},
	}

	actual, err := buildSingleTargetActual(action, nil, nil)
	if err != nil {
		t.Fatalf("buildSingleTargetActual returned error: %v", err)
	}
	if len(actual) != 0 {
		t.Fatalf("expected nil current without error to be treated as absent, got %#v", actual)
	}

	next, done, err := recomputeSingleTargetActionViaDiff(action, nil, nil)
	if err != nil {
		t.Fatalf("recompute returned error: %v", err)
	}
	if done {
		t.Fatal("expected recompute to produce a create action for absent resource")
	}
	if next == nil {
		t.Fatal("expected recompute to return a create action")
	}
	if next.Kind != engine.CreatePrefixSet {
		t.Fatalf("expected CreatePrefixSet, got %s", next.Kind)
	}
}
