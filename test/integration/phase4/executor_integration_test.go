package phase4_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/model"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func asgResourceID(sub, rg, name string) string {
	return "/subscriptions/" + sub + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/applicationSecurityGroups/" + name
}

func makePod(ns, name string, labels map[string]string, ip string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Status:     corev1.PodStatus{PodIP: ip},
	}
}

func makeMapping(ns, name string, rules []v1alpha1.Mapping) v1alpha1.PodASGMapping {
	return v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.PodASGMappingSpec{Mappings: rules},
	}
}

func sortedStrings(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Test: Full pipeline ComputeDesiredState → ComputeDiff → Executor → Fake
// ---------------------------------------------------------------------------

func TestIntegration_FullPipeline_DesiredState_Diff_Execute(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	clusterName := "test-cluster"
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)

	executor := azure.NewExecutor(log, factory, 4)

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})

	pods := []corev1.Pod{
		makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("default", "web-2", map[string]string{"app": "web"}, "10.0.0.2"),
		makePod("default", "db-1", map[string]string{"app": "db"}, "10.0.1.1"),
	}

	// Phase 3: Compute desired state
	desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)
	if len(desired) != 1 {
		t.Fatalf("expected 1 desired target, got %d", len(desired))
	}

	// Phase 3: Compute diff (actual is empty → creates)
	actual := map[engine.ASGTarget]engine.ActualPrefixSet{}
	actions := engine.ComputeDiff(desired, actual)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Kind != engine.CreatePrefixSet {
		t.Fatalf("expected CreatePrefixSet, got %s", actions[0].Kind)
	}

	// Phase 4: Execute actions
	results := executor.Execute(ctx, actions)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("action failed: %v", results[0].Err)
	}

	// Verify fake client state matches desired
	prefixSetName := model.OwnershipKey(clusterName, "default", "web-map")
	got, err := fakeClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("Get from fake failed: %v", err)
	}
	gotIPs := sortedStrings(got.Properties.AddressPrefixes)
	wantIPs := []string{"10.0.0.1", "10.0.0.2"}
	if fmt.Sprintf("%v", gotIPs) != fmt.Sprintf("%v", wantIPs) {
		t.Errorf("IPs mismatch: got %v, want %v", gotIPs, wantIPs)
	}
}

// ---------------------------------------------------------------------------
// Test: Full pipeline with update cycle (create → pod change → update)
// ---------------------------------------------------------------------------

func TestIntegration_FullPipeline_CreateThenUpdate(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	clusterName := "cluster-a"
	sub := "sub-100"
	rg := "rg-1"
	asgName := "asg-web"
	prefixSetName := model.OwnershipKey(clusterName, "ns1", "map1")

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)
	executor := azure.NewExecutor(log, factory, 2)

	// --- Round 1: Create ---
	mapping := makeMapping("ns1", "map1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"role": "api"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})
	pods1 := []corev1.Pod{
		makePod("ns1", "api-1", map[string]string{"role": "api"}, "10.1.0.1"),
	}

	desired1 := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods1)
	actions1 := engine.ComputeDiff(desired1, nil)
	results1 := executor.Execute(ctx, actions1)
	for _, r := range results1 {
		if !r.Success {
			t.Fatalf("round 1 failed: %v", r.Err)
		}
	}

	// Verify round 1
	got1, _ := fakeClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if len(got1.Properties.AddressPrefixes) != 1 || got1.Properties.AddressPrefixes[0] != "10.1.0.1" {
		t.Fatalf("round 1: unexpected IPs %v", got1.Properties.AddressPrefixes)
	}

	// --- Round 2: New pod joins → Update ---
	pods2 := []corev1.Pod{
		makePod("ns1", "api-1", map[string]string{"role": "api"}, "10.1.0.1"),
		makePod("ns1", "api-2", map[string]string{"role": "api"}, "10.1.0.2"),
	}

	desired2 := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods2)

	// Build actual from fake client for diff
	listed, err := fakeClient.List(ctx, sub, rg, asgName)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	actualMap := make(map[engine.ASGTarget]engine.ActualPrefixSet)
	for _, aps := range listed {
		if aps.Name == nil {
			continue
		}
		ips := make(map[string]struct{})
		if aps.Properties != nil {
			for _, ip := range aps.Properties.AddressPrefixes {
				ips[ip] = struct{}{}
			}
		}
		// Find the matching target from desired2
		for target := range desired2 {
			if target.PrefixSetName == *aps.Name {
				actualMap[target] = engine.ActualPrefixSet{IPs: ips}
			}
		}
	}

	actions2 := engine.ComputeDiff(desired2, actualMap)
	if len(actions2) != 1 {
		t.Fatalf("expected 1 update action, got %d", len(actions2))
	}
	if actions2[0].Kind != engine.UpdatePrefixSet {
		t.Fatalf("expected UpdatePrefixSet, got %s", actions2[0].Kind)
	}

	results2 := executor.Execute(ctx, actions2)
	for _, r := range results2 {
		if !r.Success {
			t.Fatalf("round 2 failed: %v", r.Err)
		}
	}

	// Verify round 2
	got2, _ := fakeClient.Get(ctx, sub, rg, asgName, prefixSetName)
	gotIPs := sortedStrings(got2.Properties.AddressPrefixes)
	wantIPs := []string{"10.1.0.1", "10.1.0.2"}
	if fmt.Sprintf("%v", gotIPs) != fmt.Sprintf("%v", wantIPs) {
		t.Errorf("round 2 IPs: got %v, want %v", gotIPs, wantIPs)
	}
}

// ---------------------------------------------------------------------------
// Test: Full pipeline with delete cycle (create → mapping removed → delete)
// ---------------------------------------------------------------------------

func TestIntegration_FullPipeline_CreateThenDelete(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	clusterName := "cluster-del"
	sub := "sub-del"
	rg := "rg-del"
	asgName := "asg-del"
	prefixSetName := model.OwnershipKey(clusterName, "ns1", "m1")

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)
	executor := azure.NewExecutor(log, factory, 2)

	// Round 1: Create
	mapping := makeMapping("ns1", "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"x": "y"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})
	pods := []corev1.Pod{makePod("ns1", "p1", map[string]string{"x": "y"}, "10.0.0.1")}

	desired1 := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)
	actions1 := engine.ComputeDiff(desired1, nil)
	results1 := executor.Execute(ctx, actions1)
	if !results1[0].Success {
		t.Fatalf("create failed: %v", results1[0].Err)
	}

	// Verify exists
	_, err := fakeClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("resource should exist: %v", err)
	}

	// Round 2: No mappings → delete
	desired2 := engine.ComputeDesiredState(clusterName, nil, nil)

	// Build actual from what's in the fake
	target := actions1[0].Target
	got, _ := fakeClient.Get(ctx, sub, rg, asgName, prefixSetName)
	ips := make(map[string]struct{})
	for _, ip := range got.Properties.AddressPrefixes {
		ips[ip] = struct{}{}
	}
	actualMap := map[engine.ASGTarget]engine.ActualPrefixSet{
		target: {IPs: ips},
	}

	actions2 := engine.ComputeDiff(desired2, actualMap)
	if len(actions2) != 1 || actions2[0].Kind != engine.DeletePrefixSet {
		t.Fatalf("expected 1 DeletePrefixSet action, got %v", actions2)
	}

	results2 := executor.Execute(ctx, actions2)
	if !results2[0].Success {
		t.Fatalf("delete failed: %v", results2[0].Err)
	}

	// Verify deleted
	_, err = fakeClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if !azure.IsNotFound(err) {
		t.Errorf("expected not found after delete, got err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: Cross-subscription routing through executor + fake factory
// ---------------------------------------------------------------------------

func TestIntegration_Executor_CrossSubscriptionRouting(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	sub1, sub2 := "sub-aaa", "sub-bbb"
	rg, asgName := "rg-shared", "asg-shared"
	clusterName := "cluster-xsub"

	client1 := fake.NewClient()
	client2 := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub1, client1)
	factory.RegisterClient(sub2, client2)

	executor := azure.NewExecutor(log, factory, 4)

	// Build mappings targeting two subscriptions
	mapping := makeMapping("default", "xsub-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "front"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub1, rg, asgName)},
				{ResourceID: asgResourceID(sub2, rg, asgName)},
			},
		},
	})
	pods := []corev1.Pod{
		makePod("default", "fe-1", map[string]string{"tier": "front"}, "10.0.0.1"),
	}

	desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)
	actions := engine.ComputeDiff(desired, nil)
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions for 2 subscriptions, got %d", len(actions))
	}

	results := executor.Execute(ctx, actions)
	for i, r := range results {
		if !r.Success {
			t.Errorf("action[%d] failed: %v", i, r.Err)
		}
	}

	// Verify each subscription's fake has the data
	prefixSetName := model.OwnershipKey(clusterName, "default", "xsub-map")
	for _, tc := range []struct {
		sub    string
		client *fake.Client
	}{
		{sub1, client1},
		{sub2, client2},
	} {
		got, err := tc.client.Get(ctx, tc.sub, rg, asgName, prefixSetName)
		if err != nil {
			t.Errorf("sub=%s: Get failed: %v", tc.sub, err)
			continue
		}
		if len(got.Properties.AddressPrefixes) != 1 || got.Properties.AddressPrefixes[0] != "10.0.0.1" {
			t.Errorf("sub=%s: IPs mismatch: got %v", tc.sub, got.Properties.AddressPrefixes)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: ETag retry via recompute using real fake client + engine.ComputeDiff
// ---------------------------------------------------------------------------

func TestIntegration_Executor_ETagRetryWithFakeClient(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	sub := "sub-etag"
	rg := "rg-etag"
	asgName := "asg-etag"
	prefixSetName := "cluster-etag-ns-map"

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)

	// Inject 1 x 412 on Put
	err412 := &azure.ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "etag mismatch"}
	fakeClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationPut,
		SubscriptionID: sub,
		ResourceGroup:  rg,
		ASGName:        asgName,
		PrefixSetName:  prefixSetName,
	}, err412, 1)

	executor := azure.NewExecutor(log, factory, 1)

	action := engine.Action{
		Kind: engine.CreatePrefixSet,
		Target: engine.ASGTarget{
			SubscriptionID: sub,
			ResourceGroup:  rg,
			ASGName:        asgName,
			PrefixSetName:  prefixSetName,
		},
		DesiredIPs: []string{"10.0.0.1"},
	}

	results := executor.Execute(ctx, []engine.Action{action})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("expected success after retry, got error: %v", results[0].Err)
	}

	// Verify the data landed
	got, err := fakeClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("Get after retry: %v", err)
	}
	if len(got.Properties.AddressPrefixes) != 1 || got.Properties.AddressPrefixes[0] != "10.0.0.1" {
		t.Errorf("IPs mismatch after retry: got %v", got.Properties.AddressPrefixes)
	}
}

// ---------------------------------------------------------------------------
// Test: ETag retry exhaustion with fake client
// ---------------------------------------------------------------------------

func TestIntegration_Executor_ETagRetryExhausted(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	sub := "sub-exhaust"
	rg := "rg-exhaust"
	asgName := "asg-exhaust"
	prefixSetName := "cluster-exhaust-ns-map"

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)

	// Inject 3 x 412 on Put (maxRetries is 3, so all attempts fail)
	err412 := &azure.ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "etag mismatch"}
	fakeClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationPut,
		SubscriptionID: sub,
		ResourceGroup:  rg,
		ASGName:        asgName,
		PrefixSetName:  prefixSetName,
	}, err412, 3)

	executor := azure.NewExecutor(log, factory, 1)

	action := engine.Action{
		Kind: engine.CreatePrefixSet,
		Target: engine.ASGTarget{
			SubscriptionID: sub,
			ResourceGroup:  rg,
			ASGName:        asgName,
			PrefixSetName:  prefixSetName,
		},
		DesiredIPs: []string{"10.0.0.1"},
	}

	results := executor.Execute(ctx, []engine.Action{action})
	if results[0].Success {
		t.Fatal("expected failure after exhausting retries")
	}
	if !azure.IsPreconditionFailed(results[0].Err) {
		t.Errorf("expected 412 error, got: %v", results[0].Err)
	}
}

// ---------------------------------------------------------------------------
// Test: One action fails, others succeed (error isolation)
// ---------------------------------------------------------------------------

func TestIntegration_Executor_ErrorIsolation(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	sub := "sub-iso"
	rg := "rg-iso"

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)

	// Inject a permanent error on the second prefix set
	fakeClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationPut,
		SubscriptionID: sub,
		ResourceGroup:  rg,
		ASGName:        "asg-iso",
		PrefixSetName:  "ps-fail",
	}, fmt.Errorf("transient ARM failure"), 1)

	executor := azure.NewExecutor(log, factory, 4)

	actions := []engine.Action{
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: sub, ResourceGroup: rg,
				ASGName: "asg-iso", PrefixSetName: "ps-ok",
			},
			DesiredIPs: []string{"10.0.0.1"},
		},
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: sub, ResourceGroup: rg,
				ASGName: "asg-iso", PrefixSetName: "ps-fail",
			},
			DesiredIPs: []string{"10.0.0.2"},
		},
	}

	results := executor.Execute(ctx, actions)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	var okCount, failCount int
	for _, r := range results {
		if r.Success {
			okCount++
		} else {
			failCount++
		}
	}
	if okCount != 1 || failCount != 1 {
		t.Errorf("expected 1 ok + 1 fail, got ok=%d fail=%d", okCount, failCount)
	}

	// Verify the successful one is in the store
	got, err := fakeClient.Get(ctx, sub, rg, "asg-iso", "ps-ok")
	if err != nil {
		t.Errorf("ps-ok should exist: %v", err)
	} else if got.Properties.AddressPrefixes[0] != "10.0.0.1" {
		t.Errorf("ps-ok IPs wrong: %v", got.Properties.AddressPrefixes)
	}
}

// ---------------------------------------------------------------------------
// Test: Empty IP list passes through the pipeline
// ---------------------------------------------------------------------------

func TestIntegration_FullPipeline_EmptyIPList(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	sub := "sub-empty"
	rg := "rg-empty"
	asgName := "asg-empty"
	prefixSetName := "cluster-empty-ns-map"

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)
	executor := azure.NewExecutor(log, factory, 1)

	// Directly create an action with empty IPs (simulating T4.10 / desired state
	// where pods matched but all lost IPs)
	action := engine.Action{
		Kind: engine.CreatePrefixSet,
		Target: engine.ASGTarget{
			SubscriptionID: sub, ResourceGroup: rg,
			ASGName: asgName, PrefixSetName: prefixSetName,
		},
		DesiredIPs: []string{},
	}

	results := executor.Execute(ctx, []engine.Action{action})
	if !results[0].Success {
		t.Fatalf("empty IP create failed: %v", results[0].Err)
	}

	got, err := fakeClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if len(got.Properties.AddressPrefixes) != 0 {
		t.Errorf("expected empty IPs, got %v", got.Properties.AddressPrefixes)
	}
}

// ---------------------------------------------------------------------------
// Test: Context cancellation stops executor
// ---------------------------------------------------------------------------

func TestIntegration_Executor_ContextCancellation(t *testing.T) {
	log := zaptest.NewLogger(t)

	sub := "sub-cancel"
	rg := "rg-cancel"

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)
	executor := azure.NewExecutor(log, factory, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	actions := []engine.Action{
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: sub, ResourceGroup: rg,
				ASGName: "asg", PrefixSetName: "ps",
			},
			DesiredIPs: []string{"10.0.0.1"},
		},
	}

	results := executor.Execute(ctx, actions)
	if results[0].Success {
		t.Fatal("expected failure due to cancelled context")
	}
	if results[0].Err != context.Canceled {
		t.Errorf("expected context.Canceled, got: %v", results[0].Err)
	}
}

// ---------------------------------------------------------------------------
// Test: Executor bounded concurrency with fake client
// ---------------------------------------------------------------------------

func TestIntegration_Executor_BoundedConcurrency(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	maxParallel := 2
	var concurrent int64
	var maxConcurrent int64
	var mu sync.Mutex

	// Use a slow fake client wrapper that tracks concurrency
	slowClient := &concurrencyTrackingClient{
		inner:          fake.NewClient(),
		delay:          50 * time.Millisecond,
		concurrent:     &concurrent,
		maxConcurrent:  &maxConcurrent,
		concurrentLock: &mu,
	}

	factory := &singleClientFactory{client: slowClient}
	executor := azure.NewExecutor(log, factory, maxParallel)

	// Create 6 actions
	var actions []engine.Action
	for i := 0; i < 6; i++ {
		actions = append(actions, engine.Action{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub-conc", ResourceGroup: "rg-conc",
				ASGName: "asg-conc", PrefixSetName: fmt.Sprintf("ps-%d", i),
			},
			DesiredIPs: []string{fmt.Sprintf("10.0.0.%d", i)},
		})
	}

	results := executor.Execute(ctx, actions)
	for i, r := range results {
		if !r.Success {
			t.Errorf("action[%d] failed: %v", i, r.Err)
		}
	}

	mu.Lock()
	observed := maxConcurrent
	mu.Unlock()

	if observed > int64(maxParallel) {
		t.Errorf("max concurrent %d exceeded maxParallel %d", observed, maxParallel)
	}
}

// concurrencyTrackingClient wraps a client and tracks max concurrency.
type concurrencyTrackingClient struct {
	inner          azure.AddressPrefixSetAPI
	delay          time.Duration
	concurrent     *int64
	maxConcurrent  *int64
	concurrentLock *sync.Mutex
}

func (c *concurrencyTrackingClient) trackEnter() {
	cur := atomic.AddInt64(c.concurrent, 1)
	c.concurrentLock.Lock()
	if cur > *c.maxConcurrent {
		*c.maxConcurrent = cur
	}
	c.concurrentLock.Unlock()
}

func (c *concurrencyTrackingClient) trackExit() {
	atomic.AddInt64(c.concurrent, -1)
}

func (c *concurrencyTrackingClient) Get(ctx context.Context, sub, rg, asg, ps string) (*azure.AddressPrefixSet, error) {
	c.trackEnter()
	defer c.trackExit()
	time.Sleep(c.delay)
	return c.inner.Get(ctx, sub, rg, asg, ps)
}

func (c *concurrencyTrackingClient) Put(ctx context.Context, sub, rg, asg, ps string, ips []string) error {
	c.trackEnter()
	defer c.trackExit()
	time.Sleep(c.delay)
	return c.inner.Put(ctx, sub, rg, asg, ps, ips)
}

func (c *concurrencyTrackingClient) Delete(ctx context.Context, sub, rg, asg, ps string) error {
	c.trackEnter()
	defer c.trackExit()
	time.Sleep(c.delay)
	return c.inner.Delete(ctx, sub, rg, asg, ps)
}

func (c *concurrencyTrackingClient) List(ctx context.Context, sub, rg, asg string) ([]azure.AddressPrefixSet, error) {
	return c.inner.List(ctx, sub, rg, asg)
}

// singleClientFactory returns the same client for any subscription.
type singleClientFactory struct {
	client azure.AddressPrefixSetAPI
}

func (f *singleClientFactory) ForSubscription(_ string) (azure.AddressPrefixSetAPI, error) {
	return f.client, nil
}

// ---------------------------------------------------------------------------
// Test: ClientFactory + httptest server integration
// ---------------------------------------------------------------------------

func TestIntegration_ClientFactory_HttpTestServer(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	sub := "sub-http"
	rg := "rg-http"
	asgName := "asg-http"
	psName := "ps-http"
	etag := `"test-etag-1"`

	// Create a mock ARM server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/addressPrefixSets/"+psName):
			w.Header().Set("ETag", etag)
			json.NewEncoder(w).Encode(azure.AddressPrefixSet{
				Name: &psName,
				Etag: &etag,
				Properties: &azure.AddressPrefixSetProperties{
					AddressPrefixes: []string{"10.0.0.1"},
				},
			})
		case r.Method == http.MethodPut && strings.Contains(path, "/addressPrefixSets/"+psName):
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodGet && strings.Contains(path, "/addressPrefixSets"):
			// List
			json.NewEncoder(w).Encode(azure.AddressPrefixSetListResult{
				Value: []azure.AddressPrefixSet{
					{
						Name: &psName,
						Etag: &etag,
						Properties: &azure.AddressPrefixSetProperties{
							AddressPrefixes: []string{"10.0.0.1"},
						},
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{"code": "NotFound", "message": "not found"},
			})
		}
	}))
	defer srv.Close()

	// Create factory with nil credential (test mode) and httptest client
	factory := azure.NewClientFactoryWithCredential(
		log,
		nil, // nil credential = test mode
		srv.Client(),
		azure.WithFactoryARMBaseURL(srv.URL),
	)

	// Get client for subscription
	client, err := factory.ForSubscription(sub)
	if err != nil {
		t.Fatalf("ForSubscription failed: %v", err)
	}

	// Verify the client works against httptest
	got, err := client.Get(ctx, sub, rg, asgName, psName)
	if err != nil {
		t.Fatalf("Get via factory client failed: %v", err)
	}
	if got.Etag == nil || *got.Etag != etag {
		t.Errorf("ETag mismatch: got %v, want %s", got.Etag, etag)
	}

	// Verify caching: second call returns same client
	client2, err := factory.ForSubscription(sub)
	if err != nil {
		t.Fatalf("second ForSubscription failed: %v", err)
	}
	// Both should work
	got2, err := client2.Get(ctx, sub, rg, asgName, psName)
	if err != nil {
		t.Fatalf("Get via cached client failed: %v", err)
	}
	if got2.Properties.AddressPrefixes[0] != "10.0.0.1" {
		t.Errorf("cached client returned wrong data: %v", got2.Properties.AddressPrefixes)
	}
}

// ---------------------------------------------------------------------------
// Test: Executor with real ClientFactory + httptest (full stack)
// ---------------------------------------------------------------------------

func TestIntegration_Executor_WithRealClientFactory_HttpTest(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	sub := "sub-full"
	rg := "rg-full"
	asgName := "asg-full"
	psName := "cluster-full-ns-map"
	etag := `"v-1"`

	var mu sync.Mutex
	storedIPs := []string{"old-ip"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/addressPrefixSets/"+psName):
			w.Header().Set("ETag", etag)
			json.NewEncoder(w).Encode(azure.AddressPrefixSet{
				Name: &psName,
				Etag: &etag,
				Properties: &azure.AddressPrefixSetProperties{
					AddressPrefixes: storedIPs,
				},
			})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/addressPrefixSets/"+psName):
			var body azure.AddressPrefixSet
			json.NewDecoder(r.Body).Decode(&body)
			if body.Properties != nil {
				storedIPs = body.Properties.AddressPrefixes
			}
			etag = `"v-2"`
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/addressPrefixSets"):
			w.Header().Set("ETag", etag)
			json.NewEncoder(w).Encode(azure.AddressPrefixSetListResult{
				Value: []azure.AddressPrefixSet{
					{
						Name: &psName,
						Etag: &etag,
						Properties: &azure.AddressPrefixSetProperties{
							AddressPrefixes: storedIPs,
						},
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{"code": "NotFound", "message": "not found"},
			})
		}
	}))
	defer srv.Close()

	factory := azure.NewClientFactoryWithCredential(
		log, nil, srv.Client(),
		azure.WithFactoryARMBaseURL(srv.URL),
	)
	executor := azure.NewExecutor(log, factory, 2)

	action := engine.Action{
		Kind: engine.UpdatePrefixSet,
		Target: engine.ASGTarget{
			SubscriptionID: sub, ResourceGroup: rg,
			ASGName: asgName, PrefixSetName: psName,
		},
		DesiredIPs: []string{"10.0.0.1", "10.0.0.2"},
	}

	results := executor.Execute(ctx, []engine.Action{action})
	if !results[0].Success {
		t.Fatalf("executor failed: %v", results[0].Err)
	}

	// Verify server-side state
	mu.Lock()
	gotIPs := sortedStrings(storedIPs)
	mu.Unlock()

	wantIPs := []string{"10.0.0.1", "10.0.0.2"}
	if fmt.Sprintf("%v", gotIPs) != fmt.Sprintf("%v", wantIPs) {
		t.Errorf("server IPs: got %v, want %v", gotIPs, wantIPs)
	}
}

// ---------------------------------------------------------------------------
// Test: Error propagation across factory → executor boundary
// ---------------------------------------------------------------------------

func TestIntegration_Executor_FactoryError_Propagation(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	// Factory with no registered subscriptions
	factory := fake.NewClientFactory()
	executor := azure.NewExecutor(log, factory, 2)

	action := engine.Action{
		Kind: engine.CreatePrefixSet,
		Target: engine.ASGTarget{
			SubscriptionID: "unknown-sub", ResourceGroup: "rg",
			ASGName: "asg", PrefixSetName: "ps",
		},
		DesiredIPs: []string{"10.0.0.1"},
	}

	results := executor.Execute(ctx, []engine.Action{action})
	if results[0].Success {
		t.Fatal("expected failure for unknown subscription")
	}
	if !strings.Contains(results[0].Err.Error(), "unknown-sub") {
		t.Errorf("error should mention subscription: %v", results[0].Err)
	}
}

// ---------------------------------------------------------------------------
// Test: Multiple mapping CRs targeting same ASG converge via pipeline
// ---------------------------------------------------------------------------

func TestIntegration_FullPipeline_MultipleMappingsSameASG(t *testing.T) {
	log := zaptest.NewLogger(t)
	ctx := context.Background()

	clusterName := "cluster-multi"
	sub := "sub-multi"
	rg := "rg-multi"
	asgName := "asg-shared"

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)
	executor := azure.NewExecutor(log, factory, 4)

	// Two different mappings in the same namespace pointing to the same ASG
	// Each mapping gets its own prefix set name via OwnershipKey
	mapping1 := makeMapping("prod", "web-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})
	mapping2 := makeMapping("prod", "api-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})

	pods := []corev1.Pod{
		makePod("prod", "web-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("prod", "api-1", map[string]string{"app": "api"}, "10.0.1.1"),
		makePod("prod", "api-2", map[string]string{"app": "api"}, "10.0.1.2"),
	}

	desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping1, mapping2}, pods)
	if len(desired) != 2 {
		t.Fatalf("expected 2 desired targets (one per mapping), got %d", len(desired))
	}

	actions := engine.ComputeDiff(desired, nil)
	if len(actions) != 2 {
		t.Fatalf("expected 2 create actions, got %d", len(actions))
	}

	results := executor.Execute(ctx, actions)
	for i, r := range results {
		if !r.Success {
			t.Errorf("action[%d] failed: %v", i, r.Err)
		}
	}

	// Verify both prefix sets exist with correct IPs
	ps1 := model.OwnershipKey(clusterName, "prod", "web-mapping")
	ps2 := model.OwnershipKey(clusterName, "prod", "api-mapping")

	got1, err := fakeClient.Get(ctx, sub, rg, asgName, ps1)
	if err != nil {
		t.Fatalf("Get ps1 failed: %v", err)
	}
	if len(got1.Properties.AddressPrefixes) != 1 {
		t.Errorf("ps1 should have 1 IP, got %v", got1.Properties.AddressPrefixes)
	}

	got2, err := fakeClient.Get(ctx, sub, rg, asgName, ps2)
	if err != nil {
		t.Fatalf("Get ps2 failed: %v", err)
	}
	gotIPs2 := sortedStrings(got2.Properties.AddressPrefixes)
	wantIPs2 := []string{"10.0.1.1", "10.0.1.2"}
	if fmt.Sprintf("%v", gotIPs2) != fmt.Sprintf("%v", wantIPs2) {
		t.Errorf("ps2 IPs: got %v, want %v", gotIPs2, wantIPs2)
	}
}
