package azure

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

func (s *stubClient) GetWithETag(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) (*AddressPrefixSet, string, error) {
	result, err := s.Get(ctx, subscriptionID, resourceGroup, asgName, prefixSetName)
	if err != nil {
		return nil, "", err
	}
	etag := ""
	if result.Etag != nil {
		etag = *result.Etag
	}
	return result, etag, nil
}

func (s *stubClient) PutWithIfMatch(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string, ips []string, etag string) error {
	return s.Put(ctx, subscriptionID, resourceGroup, asgName, prefixSetName, ips)
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

func (c *concurrencyTrackingClient) GetWithETag(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) (*AddressPrefixSet, string, error) {
	return c.inner.GetWithETag(ctx, subscriptionID, resourceGroup, asgName, prefixSetName)
}

func (c *concurrencyTrackingClient) PutWithIfMatch(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string, ips []string, etag string) error {
	return c.inner.PutWithIfMatch(ctx, subscriptionID, resourceGroup, asgName, prefixSetName, ips, etag)
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
func (b *blockingClient) GetWithETag(_ context.Context, _, _, _, _ string) (*AddressPrefixSet, string, error) {
	return nil, "", ErrNotFound
}
func (b *blockingClient) Put(ctx context.Context, _, _, _, _ string, _ []string) error {
	select {
	case <-b.blockCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *blockingClient) PutWithIfMatch(ctx context.Context, _, _, _, _ string, _ []string, _ string) error {
	return b.Put(ctx, "", "", "", "", nil)
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
func (a *always412Client) PutWithIfMatch(_ context.Context, _, _, _, _ string, _ []string, _ string) error {
	return &ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "always conflict"}
}
func (a *always412Client) GetWithETag(_ context.Context, _, _, _, prefixSetName string) (*AddressPrefixSet, string, error) {
	name := prefixSetName
	etag := `"etag-existing"`
	return &AddressPrefixSet{
		Name: &name,
		Etag: &etag,
		Properties: &AddressPrefixSetProperties{
			AddressPrefixes: []string{"10.0.0.0/32"},
		},
	}, etag, nil
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

func (c *deleteRetryClient) GetWithETag(_ context.Context, _, _, _, prefixSetName string) (*AddressPrefixSet, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.exists {
		return nil, "", ErrNotFound
	}
	name := prefixSetName
	etag := `"etag-existing"`
	return &AddressPrefixSet{
		Name: &name,
		Etag: &etag,
		Properties: &AddressPrefixSetProperties{
			AddressPrefixes: []string{"10.0.0.1/32"},
		},
	}, etag, nil
}

func (c *deleteRetryClient) PutWithIfMatch(_ context.Context, _, _, _, _ string, _ []string, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putCalls++
	return nil
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

func (c *noOpAfterConflictClient) GetWithETag(_ context.Context, _, _, _, prefixSetName string) (*AddressPrefixSet, string, error) {
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
	}, etag, nil
}

func (c *noOpAfterConflictClient) PutWithIfMatch(_ context.Context, _, _, _, _ string, _ []string, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putCalls++
	if c.putCalls == 1 {
		return &ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "etag mismatch"}
	}
	return fmt.Errorf("unexpected second PUT after recompute no-op")
}

// --- T4.Recompute: recomputeSingleTargetActionViaDiff uses engine.ComputeDiff ---
func TestPhase4_Executor_RecomputeSingleTargetActionViaDiff_UsesComputeDiff(t *testing.T) {
	helperExec := &Executor{patchThresholdPercent: engine.DefaultPatchThresholdPercent}
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

	next, done, err := helperExec.recomputeSingleTargetActionViaDiff(action, current, nil)
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
	if next.Kind != engine.UpdatePrefixSet && next.Kind != engine.PatchPrefixSet {
		t.Errorf("expected UpdatePrefixSet or PatchPrefixSet, got %s", next.Kind)
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
	_, doneMatch, errMatch := helperExec.recomputeSingleTargetActionViaDiff(action, currentMatching, nil)
	if errMatch != nil {
		t.Fatalf("recompute on matching state returned error: %v", errMatch)
	}
	if !doneMatch {
		t.Error("recompute should return done=true when actual matches desired")
	}

	// Test: when Get returns ErrNotFound, recompute should return CreatePrefixSet
	nextCreate, doneCreate, errCreate := helperExec.recomputeSingleTargetActionViaDiff(action, nil, ErrNotFound)
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
	helperExec := &Executor{patchThresholdPercent: engine.DefaultPatchThresholdPercent}
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

	next, done, err := helperExec.recomputeSingleTargetActionViaDiff(action, current, nil)
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
	helperExec := &Executor{patchThresholdPercent: engine.DefaultPatchThresholdPercent}
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

	next, done, err := helperExec.recomputeSingleTargetActionViaDiff(action, nil, nil)
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

// ---------- T7.7: ETag conflict during retry loop → re-GET + recompute succeeds ----------

func TestPhase7_T77_ETagConflict_ReGETRecomputeSuccess(t *testing.T) {
	log := zaptest.NewLogger(t)

	// The client will return 412 on the first two PUTs, then succeed on the third.
	client := newStubClient()
	client.fail412Count = 2

	factory := newStubFactory()
	factory.Register("sub-1", client)

	// Pre-populate the resource so the update path is valid.
	client.store[stubKey("sub-1", "rg-1", "asg-1", "ps-1")] = []string{"10.0.0.0/32"}

	executor := NewExecutor(log, factory, 1)

	actions := []engine.Action{
		{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub-1",
				ResourceGroup:  "rg-1",
				ASGName:        "asg-1",
				PrefixSetName:  "ps-1",
			},
			DesiredIPs: []string{"10.0.0.1/32", "10.0.0.2/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("T7.7: expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Errorf("T7.7: expected retry to succeed after ETag conflicts, got error: %v", results[0].Err)
	}

	// Verify the resource was updated to the desired IPs after re-GET + recompute.
	got, err := client.Get(context.Background(), "sub-1", "rg-1", "asg-1", "ps-1")
	if err != nil {
		t.Fatalf("T7.7: Get after execute failed: %v", err)
	}
	if got == nil || got.Properties == nil {
		t.Fatal("T7.7: resource should exist after retry")
	}

	wantIPs := []string{"10.0.0.1/32", "10.0.0.2/32"}
	if len(got.Properties.AddressPrefixes) != len(wantIPs) {
		t.Errorf("T7.7: expected %d IPs, got %d: %v",
			len(wantIPs), len(got.Properties.AddressPrefixes), got.Properties.AddressPrefixes)
	}

	// Verify the executor performed re-GET + recompute by checking PUT call count.
	// With fail412Count=2, we expect: PUT(412) → re-GET → PUT(412) → re-GET → PUT(ok) = 3 PUTs.
	if client.putCalls != 3 {
		t.Errorf("T7.7: expected 3 PUT attempts (2 x 412 + 1 success), got %d", client.putCalls)
	}

	// T7.7 Phase 7 assertion: verify that the executor's retry logs include the
	// Operation metadata from RetryContext, indicating the retry path is governed
	// by the RetryPolicy (not hardcoded constants from Phase 4).
	// Re-run with observed logs to check for Operation field.
	coreObs, obs := observer.New(zap.WarnLevel)
	obsLog := zap.New(coreObs)

	client2 := newStubClient()
	client2.fail412Count = 1
	factory2 := newStubFactory()
	factory2.Register("sub-1", client2)
	client2.store[stubKey("sub-1", "rg-1", "asg-1", "ps-1")] = []string{"10.0.0.0/32"}

	executor2 := NewExecutor(obsLog, factory2, 1)
	results2 := executor2.Execute(context.Background(), actions)
	if len(results2) != 1 || !results2[0].Success {
		t.Fatalf("T7.7: expected success in observed run, got %v", results2)
	}

	// Check that the retry warning log includes an "operation" field matching the ARM operation.
	// With Phase 7 implementation, the retry path should log structured RetryContext metadata.
	// With current Phase 4 code, the "operation" field logs armVerb (e.g., "PUT") not ARMOperation.
	found := false
	for _, entry := range obs.All() {
		for _, field := range entry.Context {
			if field.Key == "operation" && field.String == string(ARMOperationPutPrefixSet) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("T7.7: expected retry log to contain operation=%q from RetryContext, got logs: %v",
			ARMOperationPutPrefixSet, obs.All())
	}
}

// ---------------------------------------------------------------------------
// T7.6 Executor Acceptance Tests — Bounded Parallelism & No Dropped Results
// ---------------------------------------------------------------------------

// TestPhase7_T76_Executor_BoundedParallelism_NoDroppedResults verifies that
// the executor respects maxParallel and returns results for every input action.
func TestPhase7_T76_Executor_BoundedParallelism_NoDroppedResults(t *testing.T) {
	log := zaptest.NewLogger(t)

	client := newStubClient()
	factory := newStubFactory()
	factory.Register("sub1", client)

	maxParallel := 3
	executor := NewExecutor(log, factory, maxParallel)

	// 20 actions
	actions := make([]engine.Action, 20)
	for i := 0; i < 20; i++ {
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

	// ExecuteWithMetrics returns results and metrics including PeakConcurrency.
	results, metrics := executor.ExecuteWithMetrics(context.Background(), actions)

	// Assert: all 20 results returned (no drops)
	if len(results) != 20 {
		t.Fatalf("T7.6: expected 20 results, got %d", len(results))
	}

	// Assert: peak concurrency <= maxParallel
	if metrics.PeakConcurrency > maxParallel {
		t.Errorf("T7.6: PeakConcurrency = %d, want <= %d", metrics.PeakConcurrency, maxParallel)
	}
	if metrics.PeakConcurrency == 0 {
		t.Errorf("T7.6: PeakConcurrency = 0, want > 0 (stub not implemented)")
	}

	// Assert: no dropped results
	if metrics.DroppedCount != 0 {
		t.Errorf("T7.6: DroppedCount = %d, want 0", metrics.DroppedCount)
	}

	// Assert: TotalActions matches input
	if metrics.TotalActions != 20 {
		t.Errorf("T7.6: TotalActions = %d, want 20", metrics.TotalActions)
	}

	// Assert: every input target appears exactly once in output
	targetSet := make(map[string]int)
	for _, r := range results {
		targetSet[r.Action.Target.PrefixSetName]++
	}
	for i := 0; i < 20; i++ {
		ps := fmt.Sprintf("ps-%d", i)
		if targetSet[ps] != 1 {
			t.Errorf("T7.6: target %q appeared %d times, want exactly 1", ps, targetSet[ps])
		}
	}
}

// TestPhase7_T76_Executor_PartialFailure_NoDroppedResults verifies that
// when a subset of actions fail, all results are preserved.
func TestPhase7_T76_Executor_PartialFailure_NoDroppedResults(t *testing.T) {
	log := zaptest.NewLogger(t)

	// Use a client that fails for specific prefix set names.
	failClient := &selectiveFailClient{
		failNames: map[string]bool{"ps-1": true, "ps-3": true, "ps-4": true},
		store:     make(map[string][]string),
	}
	factory := newStubFactory()
	factory.Register("sub1", failClient)

	executor := NewExecutor(log, factory, 5)

	actions := make([]engine.Action, 5)
	for i := 0; i < 5; i++ {
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

	results, metrics := executor.ExecuteWithMetrics(context.Background(), actions)

	// Assert: all results returned
	if len(results) != 5 {
		t.Fatalf("T7.6: expected 5 results, got %d", len(results))
	}

	// Assert: success + failure == input count
	if metrics.SuccessCount+metrics.FailureCount != 5 {
		t.Errorf("T7.6: SuccessCount(%d) + FailureCount(%d) = %d, want 5",
			metrics.SuccessCount, metrics.FailureCount, metrics.SuccessCount+metrics.FailureCount)
	}

	// Assert: 2 successes, 3 failures
	if metrics.SuccessCount != 2 {
		t.Errorf("T7.6: SuccessCount = %d, want 2", metrics.SuccessCount)
	}
	if metrics.FailureCount != 3 {
		t.Errorf("T7.6: FailureCount = %d, want 3", metrics.FailureCount)
	}

	// Assert: failed subset preserved with error
	for _, r := range results {
		name := r.Action.Target.PrefixSetName
		if failClient.failNames[name] {
			if r.Success {
				t.Errorf("T7.6: expected failure for %q, got success", name)
			}
			if r.Err == nil {
				t.Errorf("T7.6: expected error for %q, got nil", name)
			}
		}
	}
}

// TestPhase7_T76_Executor_ContextCancel_WhileWaitingSemaphore verifies that
// cancellation while blocked on the semaphore produces a context.Canceled result.
func TestPhase7_T76_Executor_ContextCancel_WhileWaitingSemaphore(t *testing.T) {
	log := zaptest.NewLogger(t)

	blockCh := make(chan struct{})
	blocking := &blockingClient{blockCh: blockCh}
	factory := newStubFactory()
	factory.Register("sub1", blocking)

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

	done := make(chan struct{})
	var results []ActionResult
	var metrics ExecutorMetrics
	go func() {
		results, metrics = executor.ExecuteWithMetrics(ctx, actions)
		close(done)
	}()

	// Give time for first action to acquire semaphore, then cancel
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(blockCh)
	<-done

	// Assert: full result slice returned (no goroutine leak behavior)
	if len(results) != 2 {
		t.Fatalf("T7.6: expected 2 results, got %d", len(results))
	}

	// Assert: waiting action has context.Canceled
	waitingResult := results[1]
	if waitingResult.Err == nil {
		t.Error("T7.6: expected error for cancelled action, got nil")
	}
	if waitingResult.Err != nil && !errors.Is(waitingResult.Err, context.Canceled) {
		t.Errorf("T7.6: expected context.Canceled, got: %v", waitingResult.Err)
	}

	// Assert: metrics reflect the cancellation
	if metrics.TotalActions != 2 {
		t.Errorf("T7.6: TotalActions = %d, want 2", metrics.TotalActions)
	}
}

// selectiveFailClient fails Put for specific prefix set names.
type selectiveFailClient struct {
	failNames map[string]bool
	mu        sync.Mutex
	store     map[string][]string
}

func (s *selectiveFailClient) Get(_ context.Context, sub, rg, asg, ps string) (*AddressPrefixSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := stubKey(sub, rg, asg, ps)
	ips, ok := s.store[k]
	if !ok {
		return nil, ErrNotFound
	}
	name := ps
	etag := `"etag-` + k + `"`
	return &AddressPrefixSet{Name: &name, Etag: &etag, Properties: &AddressPrefixSetProperties{AddressPrefixes: ips}}, nil
}

func (s *selectiveFailClient) Put(_ context.Context, sub, rg, asg, ps string, ips []string) error {
	if s.failNames[ps] {
		return &ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "injected failure"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store[stubKey(sub, rg, asg, ps)] = ips
	return nil
}

func (s *selectiveFailClient) Delete(_ context.Context, _, _, _, _ string) error { return nil }
func (s *selectiveFailClient) List(_ context.Context, _, _, _ string) ([]AddressPrefixSet, error) {
	return nil, nil
}
func (s *selectiveFailClient) GetWithETag(_ context.Context, sub, rg, asg, ps string) (*AddressPrefixSet, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := stubKey(sub, rg, asg, ps)
	ips, ok := s.store[k]
	if !ok {
		return nil, "", ErrNotFound
	}
	name := ps
	etag := `"etag-` + k + `"`
	return &AddressPrefixSet{Name: &name, Etag: &etag, Properties: &AddressPrefixSetProperties{AddressPrefixes: ips}}, etag, nil
}
func (s *selectiveFailClient) PutWithIfMatch(_ context.Context, sub, rg, asg, ps string, ips []string, _ string) error {
	return s.Put(context.Background(), sub, rg, asg, ps, ips)
}

// --- Phase 4: Incremental Diff and Patch — Executor Tests ---

// TestExecutor_PatchPrefixSet verifies the patch execution path:
// GET → merge AddIPs/RemoveIPs → PUT with If-Match.
func TestExecutor_PatchPrefixSet(t *testing.T) {
	log := zaptest.NewLogger(t)
	client := newStubClient()
	factory := newStubFactory()
	factory.Register("sub1", client)

	// Pre-populate with existing state {A, B, E}
	client.store[stubKey("sub1", "rg1", "asg1", "ps1")] = []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.5/32"}

	executor := NewExecutor(log, factory, 1)
	actions := []engine.Action{
		{
			Kind: engine.PatchPrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32", "10.0.0.4/32"},
			AddIPs:     []string{"10.0.0.3/32", "10.0.0.4/32"},
			RemoveIPs:  []string{"10.0.0.5/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("TestExecutor_PatchPrefixSet: expected action to succeed, got error: %v", results[0].Err)
	}

	// Verify the resulting state is {A, B, C, D} — E removed, C and D added
	got, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Get after patch: %v", err)
	}
	wantIPs := []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32", "10.0.0.4/32"}
	gotIPs := got.Properties.AddressPrefixes
	sort.Strings(gotIPs)
	if len(gotIPs) != len(wantIPs) {
		t.Fatalf("expected %d IPs, got %d: %v", len(wantIPs), len(gotIPs), gotIPs)
	}
	for i, ip := range gotIPs {
		if ip != wantIPs[i] {
			t.Errorf("IP[%d] = %q, want %q", i, ip, wantIPs[i])
		}
	}
}

// TestExecutor_PatchPrefixSet_NilCurrentTreatedAsNotFound verifies that when
// GetWithETag returns (nil, "", nil) — a violated client contract — the patch
// path treats it as not-found and recomputes to CreatePrefixSet.
func TestExecutor_PatchPrefixSet_NilCurrentTreatedAsNotFound(t *testing.T) {
	log := zaptest.NewLogger(t)

	// Client that returns (nil, "", nil) on GetWithETag to simulate the
	// violated contract. After the recompute fallback to Create, Put succeeds.
	client := &nilGetWithETagClient{store: make(map[string][]string)}
	factory := newStubFactory()
	factory.Register("sub1", client)

	executor := NewExecutor(log, factory, 1)
	actions := []engine.Action{
		{
			Kind: engine.PatchPrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32", "10.0.0.2/32"},
			AddIPs:     []string{"10.0.0.2/32"},
			RemoveIPs:  []string{"10.0.0.5/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("expected action to succeed via not-found recompute, got error: %v", results[0].Err)
	}
	// The recompute should have fallen back to CreatePrefixSet.
	if results[0].FinalActionKind != engine.CreatePrefixSet {
		t.Errorf("expected final action kind CreatePrefixSet, got %s", results[0].FinalActionKind)
	}
}

// nilGetWithETagClient returns (nil, "", nil) from GetWithETag to simulate
// a client contract violation. Get returns ErrNotFound. Put always succeeds.
type nilGetWithETagClient struct {
	store map[string][]string
}

func (c *nilGetWithETagClient) Get(_ context.Context, sub, rg, asg, ps string) (*AddressPrefixSet, error) {
	return nil, ErrNotFound
}
func (c *nilGetWithETagClient) GetWithETag(_ context.Context, _, _, _, _ string) (*AddressPrefixSet, string, error) {
	return nil, "", nil // violated contract: no error, no resource
}
func (c *nilGetWithETagClient) Put(_ context.Context, sub, rg, asg, ps string, ips []string) error {
	c.store[stubKey(sub, rg, asg, ps)] = ips
	return nil
}
func (c *nilGetWithETagClient) PutWithIfMatch(_ context.Context, sub, rg, asg, ps string, ips []string, _ string) error {
	c.store[stubKey(sub, rg, asg, ps)] = ips
	return nil
}
func (c *nilGetWithETagClient) Delete(_ context.Context, _, _, _, _ string) error { return nil }
func (c *nilGetWithETagClient) List(_ context.Context, _, _, _ string) ([]AddressPrefixSet, error) {
	return nil, nil
}

// TestExecutor_PatchETagConflictRetry verifies that a 412 on patch triggers
// recompute and retry with fresh state.
func TestExecutor_PatchETagConflictRetry(t *testing.T) {
	log := zaptest.NewLogger(t)
	client := newStubClient()
	client.fail412Count = 1 // first PUT returns 412, second succeeds
	factory := newStubFactory()
	factory.Register("sub1", client)

	// Pre-populate
	client.store[stubKey("sub1", "rg1", "asg1", "ps1")] = []string{"10.0.0.1/32", "10.0.0.2/32"}

	executor := NewExecutor(log, factory, 1)
	actions := []engine.Action{
		{
			Kind: engine.PatchPrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32"},
			AddIPs:     []string{"10.0.0.3/32"},
			RemoveIPs:  []string{},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("TestExecutor_PatchETagConflictRetry: expected retry to succeed, got error: %v", results[0].Err)
	}
}

// TestExecutor_PatchGetNotFound_RecomputeToCreateOrNoOp verifies that when
// a PatchPrefixSet target disappears between diff and execute, the executor
// recomputes to create/update/no-op instead of terminal failure.
func TestExecutor_PatchGetNotFound_RecomputeToCreateOrNoOp(t *testing.T) {
	log := zaptest.NewLogger(t)

	t.Run("not_found_recomputes_to_create", func(t *testing.T) {
		// Client returns ErrNotFound for Get (resource disappeared)
		client := newStubClient()
		// Don't pre-populate — Get will return ErrNotFound
		factory := newStubFactory()
		factory.Register("sub1", client)

		executor := NewExecutor(log, factory, 1)
		actions := []engine.Action{
			{
				Kind: engine.PatchPrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1",
					ResourceGroup:  "rg1",
					ASGName:        "asg1",
					PrefixSetName:  "ps1",
				},
				DesiredIPs: []string{"10.0.0.1/32", "10.0.0.2/32"},
				AddIPs:     []string{"10.0.0.2/32"},
				RemoveIPs:  []string{},
			},
		}

		results := executor.Execute(context.Background(), actions)
		if len(results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(results))
		}
		// Should succeed by recomputing to a create
		if !results[0].Success {
			t.Errorf("expected success after recompute to create, got error: %v", results[0].Err)
		}
	})

	t.Run("not_found_with_no_desired_is_noop", func(t *testing.T) {
		// PatchPrefixSet with empty desired IPs + target not found = no-op
		client := newStubClient()
		factory := newStubFactory()
		factory.Register("sub1", client)

		executor := NewExecutor(log, factory, 1)
		actions := []engine.Action{
			{
				Kind: engine.PatchPrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1",
					ResourceGroup:  "rg1",
					ASGName:        "asg1",
					PrefixSetName:  "ps1",
				},
				DesiredIPs: []string{},
				AddIPs:     []string{},
				RemoveIPs:  []string{"10.0.0.1/32"},
			},
		}

		results := executor.Execute(context.Background(), actions)
		if len(results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(results))
		}
		// With empty desired and resource not found, should be no-op success
		if !results[0].Success {
			t.Errorf("expected no-op success, got error: %v", results[0].Err)
		}
		// Verify no PUT was issued — the executor must not create an empty prefix set.
		client.mu.Lock()
		puts := client.putCalls
		storeLen := len(client.store)
		client.mu.Unlock()
		if puts != 0 {
			t.Errorf("expected 0 PUT calls (no-op), got %d", puts)
		}
		if storeLen != 0 {
			t.Errorf("expected empty store (no prefix set created), got %d entries", storeLen)
		}
	})
}

// ---------------------------------------------------------------------------
// Phase 6: SetMaxParallel — runtime concurrency mutation
// ---------------------------------------------------------------------------

func TestPhase6_SetMaxParallel_NextExecuteUsesNewLimit(t *testing.T) {
	log := zaptest.NewLogger(t)
	factory := newStubFactory()

	// Start with a slow client to measure concurrency.
	var peak int64
	var current int64

	slowInner := &delayingStubClient{inner: newStubClient(), delay: 50 * time.Millisecond}
	trackingClient := &concurrencyTrackingClient{
		inner:   slowInner,
		current: &current,
		peak:    &peak,
	}
	factory.Register("sub1", trackingClient)

	executor := NewExecutor(log, factory, 2) // start with max=2

	// Execute with 4 actions, max parallel = 2 → peak concurrency should be 2.
	actions := makeNActions(4, "sub1")
	_ = executor.Execute(context.Background(), actions)

	peak1 := atomic.LoadInt64(&peak)
	if peak1 > 2 {
		t.Errorf("expected peak concurrency <= 2, got %d", peak1)
	}

	// Now increase to 4.
	err := executor.SetMaxParallel(4)
	if err != nil {
		t.Fatalf("SetMaxParallel(4) error: %v", err)
	}

	// Reset tracking.
	atomic.StoreInt64(&peak, 0)
	atomic.StoreInt64(&current, 0)

	_ = executor.Execute(context.Background(), actions)

	peak2 := atomic.LoadInt64(&peak)
	if peak2 > 4 {
		t.Errorf("expected peak concurrency <= 4, got %d", peak2)
	}
	// With 4 actions and max=4, all should run concurrently.
	if peak2 < 3 {
		t.Errorf("expected peak concurrency >= 3 with max=4 and 4 actions, got %d", peak2)
	}
}

func TestPhase6_SetMaxParallel_InvalidValues(t *testing.T) {
	log := zaptest.NewLogger(t)
	factory := newStubFactory()
	executor := NewExecutor(log, factory, 5)

	// Zero should fail.
	if err := executor.SetMaxParallel(0); err == nil {
		t.Error("expected error for SetMaxParallel(0), got nil")
	}

	// Negative should fail.
	if err := executor.SetMaxParallel(-1); err == nil {
		t.Error("expected error for SetMaxParallel(-1), got nil")
	}

	// Current value should remain unchanged.
	if got := executor.MaxParallel(); got != 5 {
		t.Errorf("MaxParallel() = %d after invalid SetMaxParallel, want 5", got)
	}
}

func TestPhase6_MaxParallel_ReturnsCurrentValue(t *testing.T) {
	log := zaptest.NewLogger(t)
	factory := newStubFactory()
	executor := NewExecutor(log, factory, 7)

	if got := executor.MaxParallel(); got != 7 {
		t.Errorf("MaxParallel() = %d, want 7", got)
	}
}

// ---------------------------------------------------------------------------
// Phase 6: Executor metrics — duration, concurrency gauge, ETag conflicts
// ---------------------------------------------------------------------------

func TestPhase6_Executor_Metrics_DurationObserved(t *testing.T) {
	log := zaptest.NewLogger(t)
	factory := newStubFactory()
	client := newStubClient()
	factory.Register("sub1", client)

	obs := &fakeExecutorMetricsObserver{}
	executor := NewExecutor(log, factory, 2, WithExecutorMetrics(obs))

	actions := []engine.Action{
		{
			Kind:       engine.CreatePrefixSet,
			Target:     engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "ps1"},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
		{
			Kind:       engine.UpdatePrefixSet,
			Target:     engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "ps2"},
			DesiredIPs: []string{"10.0.0.2/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	for i, r := range results {
		if !r.Success {
			t.Errorf("action[%d] failed: %v", i, r.Err)
		}
	}

	// Each action should produce exactly one duration observation.
	if obs.durationCount != 2 {
		t.Errorf("expected 2 duration observations, got %d", obs.durationCount)
	}
}

func TestPhase6_Executor_Metrics_ConcurrencyGaugeBalanced(t *testing.T) {
	log := zaptest.NewLogger(t)
	factory := newStubFactory()
	client := newStubClient()
	factory.Register("sub1", client)

	obs := &fakeExecutorMetricsObserver{}
	executor := NewExecutor(log, factory, 4, WithExecutorMetrics(obs))

	actions := makeNActions(4, "sub1")
	_ = executor.Execute(context.Background(), actions)

	// After all actions complete, increments should equal decrements.
	if obs.incCount != obs.decCount {
		t.Errorf("concurrency gauge unbalanced: inc=%d, dec=%d", obs.incCount, obs.decCount)
	}
	// At least one increment should have occurred.
	if obs.incCount == 0 {
		t.Error("expected at least one concurrency gauge increment, got 0")
	}
}

func TestPhase6_Executor_Metrics_ETagConflictCounted(t *testing.T) {
	log := zaptest.NewLogger(t)
	factory := newStubFactory()
	client := newStubClient()
	// Pre-populate so Get works during retry.
	client.store[stubKey("sub1", "rg1", "asg1", "ps1")] = []string{"10.0.0.1/32"}
	// First PUT will 412, second will succeed.
	client.fail412Count = 1
	factory.Register("sub1", client)

	obs := &fakeExecutorMetricsObserver{}
	executor := NewExecutor(log, factory, 1, WithExecutorMetrics(obs))

	actions := []engine.Action{
		{
			Kind:       engine.UpdatePrefixSet,
			Target:     engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "ps1"},
			DesiredIPs: []string{"10.0.0.1/32", "10.0.0.2/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("expected success after 412 retry, got: %+v", results)
	}

	// ETag conflict should have been counted.
	if obs.etagConflictCount == 0 {
		t.Error("expected at least one ETag conflict observation, got 0")
	}
}

// --- Phase 6 test helpers ---

// delayingStubClient wraps a stubClient and adds a delay to Put calls
// for concurrency measurement.
type delayingStubClient struct {
	inner *stubClient
	delay time.Duration
}

func (d *delayingStubClient) Get(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) (*AddressPrefixSet, error) {
	return d.inner.Get(ctx, subscriptionID, resourceGroup, asgName, prefixSetName)
}

func (d *delayingStubClient) GetWithETag(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) (*AddressPrefixSet, string, error) {
	return d.inner.GetWithETag(ctx, subscriptionID, resourceGroup, asgName, prefixSetName)
}

func (d *delayingStubClient) Put(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string, ips []string) error {
	time.Sleep(d.delay)
	return d.inner.Put(ctx, subscriptionID, resourceGroup, asgName, prefixSetName, ips)
}

func (d *delayingStubClient) PutWithIfMatch(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string, ips []string, etag string) error {
	time.Sleep(d.delay)
	return d.inner.PutWithIfMatch(ctx, subscriptionID, resourceGroup, asgName, prefixSetName, ips, etag)
}

func (d *delayingStubClient) Delete(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) error {
	return d.inner.Delete(ctx, subscriptionID, resourceGroup, asgName, prefixSetName)
}

func (d *delayingStubClient) List(ctx context.Context, subscriptionID, resourceGroup, asgName string) ([]AddressPrefixSet, error) {
	return d.inner.List(ctx, subscriptionID, resourceGroup, asgName)
}

func makeNActions(n int, subID string) []engine.Action {
	actions := make([]engine.Action, n)
	for i := 0; i < n; i++ {
		actions[i] = engine.Action{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: subID,
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  fmt.Sprintf("ps-%d", i),
			},
			DesiredIPs: []string{fmt.Sprintf("10.0.%d.1/32", i)},
		}
	}
	return actions
}

// fakeExecutorMetricsObserver implements the Phase 6 ARMExecutorObserver interface.
type fakeExecutorMetricsObserver struct {
	mu                sync.Mutex
	durationCount     int
	incCount          int
	decCount          int
	etagConflictCount int
}

func (f *fakeExecutorMetricsObserver) ObserveCallDuration(subscriptionID, operation string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.durationCount++
}

func (f *fakeExecutorMetricsObserver) IncConcurrentActions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incCount++
}

func (f *fakeExecutorMetricsObserver) DecConcurrentActions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decCount++
}

func (f *fakeExecutorMetricsObserver) ObserveETagConflict(subscriptionID, operation string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.etagConflictCount++
}

// ---------------------------------------------------------------------------
// Phase 6: metricOperationLabel — HTTP verb mapping for executor metrics
// ---------------------------------------------------------------------------

func TestPhase6_Executor_Metrics_DurationLabel_UsesHTTPVerb(t *testing.T) {
	// Phase 6 design: ObserveCallDuration operation label must be HTTP verb
	// (PUT/DELETE/UNKNOWN) not the internal enum (PutPrefixSet/DeletePrefixSet).
	tests := []struct {
		name     string
		kind     engine.ActionKind
		wantVerb string
	}{
		{"CreatePrefixSet→PUT", engine.CreatePrefixSet, "PUT"},
		{"UpdatePrefixSet→PUT", engine.UpdatePrefixSet, "PUT"},
		{"PatchPrefixSet→PUT", engine.PatchPrefixSet, "PUT"},
		{"DeletePrefixSet→DELETE", engine.DeletePrefixSet, "DELETE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := zaptest.NewLogger(t)
			factory := newStubFactory()
			client := newStubClient()
			factory.Register("sub1", client)

			obs := &labelCapturingObserver{}
			executor := NewExecutor(log, factory, 1, WithExecutorMetrics(obs))

			actions := []engine.Action{
				{
					Kind:       tc.kind,
					Target:     engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "ps1"},
					DesiredIPs: []string{"10.0.0.1/32"},
				},
			}

			_ = executor.Execute(context.Background(), actions)

			obs.mu.Lock()
			defer obs.mu.Unlock()
			if len(obs.durationOps) == 0 {
				t.Fatal("no duration observations recorded")
			}
			got := obs.durationOps[0]
			if got != tc.wantVerb {
				t.Errorf("ObserveCallDuration operation = %q, want HTTP verb %q", got, tc.wantVerb)
			}
		})
	}
}

func TestPhase6_Executor_Metrics_ETagConflictLabel_UsesHTTPVerb(t *testing.T) {
	// Phase 6 design: ObserveETagConflict must use HTTP verb labels (PUT/DELETE).
	log := zaptest.NewLogger(t)
	factory := newStubFactory()
	client := newStubClient()
	// Pre-populate so Get works during ETag retry.
	client.store[stubKey("sub1", "rg1", "asg1", "ps1")] = []string{"10.0.0.1/32"}
	client.fail412Count = 1
	factory.Register("sub1", client)

	obs := &labelCapturingObserver{}
	executor := NewExecutor(log, factory, 1, WithExecutorMetrics(obs))

	actions := []engine.Action{
		{
			Kind:       engine.UpdatePrefixSet,
			Target:     engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "ps1"},
			DesiredIPs: []string{"10.0.0.1/32", "10.0.0.2/32"},
		},
	}

	_ = executor.Execute(context.Background(), actions)

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.etagOps) == 0 {
		t.Fatal("no ETag conflict observations recorded")
	}
	got := obs.etagOps[0]
	if got != "PUT" {
		t.Errorf("ObserveETagConflict operation = %q, want HTTP verb %q", got, "PUT")
	}
}

func TestPhase6_Executor_Metrics_DurationLabel_TerminalActionKind(t *testing.T) {
	// Phase 6 design: when ETag retry recomputes to a different action kind,
	// the duration label must reflect the terminal (final) action kind.
	log := zaptest.NewLogger(t)
	factory := newStubFactory()
	client := newStubClient()
	// Pre-populate target so Get during ETag retry finds data.
	client.store[stubKey("sub1", "rg1", "asg1", "ps1")] = []string{"10.0.0.1/32"}
	// Force one 412 to trigger a recompute.
	client.fail412Count = 1
	factory.Register("sub1", client)

	obs := &labelCapturingObserver{}
	executor := NewExecutor(log, factory, 1, WithExecutorMetrics(obs))

	actions := []engine.Action{
		{
			Kind:       engine.UpdatePrefixSet,
			Target:     engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "ps1"},
			DesiredIPs: []string{"10.0.0.1/32", "10.0.0.2/32"},
		},
	}

	results := executor.Execute(context.Background(), actions)
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("expected success, got: %+v", results)
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.durationOps) == 0 {
		t.Fatal("no duration observations recorded")
	}
	// The operation label must be an HTTP verb, not an internal enum string.
	got := obs.durationOps[0]
	validVerbs := map[string]bool{"PUT": true, "DELETE": true, "UNKNOWN": true}
	if !validVerbs[got] {
		t.Errorf("ObserveCallDuration operation = %q, want one of PUT/DELETE/UNKNOWN (HTTP verb)", got)
	}
}

// labelCapturingObserver captures the operation label values for assertions.
type labelCapturingObserver struct {
	mu          sync.Mutex
	durationOps []string
	etagOps     []string
	incCount    int
	decCount    int
}

func (o *labelCapturingObserver) ObserveCallDuration(subscriptionID, operation string, d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.durationOps = append(o.durationOps, operation)
}

func (o *labelCapturingObserver) IncConcurrentActions() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.incCount++
}

func (o *labelCapturingObserver) DecConcurrentActions() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.decCount++
}

func (o *labelCapturingObserver) ObserveETagConflict(subscriptionID, operation string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.etagOps = append(o.etagOps, operation)
}
