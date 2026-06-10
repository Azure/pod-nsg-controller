package phase9_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/model"
	"go.uber.org/zap/zaptest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---------------------------------------------------------------------------
// T9.1: Create PodASGMapping + deploy 3 pods → all 3 IPs in prefix set
// ---------------------------------------------------------------------------

func TestPhase9_T91_CreateMappingAndThreePods_AllIPsPresent(t *testing.T) {
	zapLog := zaptest.NewLogger(t)
	_ = zapLog

	pe := setupPhase9Env(t, phase9EnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer pe.teardown(t)

	executor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	pe.startManager(t, executor)

	ns := "t91-ns"
	createNamespace(t, pe.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping1", spec)

	labels := map[string]string{"app": "web"}
	createPodWithIP(t, pe.k8sClient, ns, "pod-1", "10.0.0.1", labels)
	createPodWithIP(t, pe.k8sClient, ns, "pod-2", "10.0.0.2", labels)
	createPodWithIP(t, pe.k8sClient, ns, "pod-3", "10.0.0.3", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "mapping1")
	fakeClient := pe.fakeClientsBySub["sub1"]

	eventually(t, 30*time.Second, 500*time.Millisecond, "all 3 IPs in prefix set", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg1", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		sort.Strings(ips)
		want := []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32"}
		if len(ips) != len(want) {
			return false, fmt.Sprintf("got %d IPs, want %d: %v", len(ips), len(want), ips)
		}
		for i := range want {
			if ips[i] != want[i] {
				return false, fmt.Sprintf("mismatch at %d: got %s want %s", i, ips[i], want[i])
			}
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.2: Scale pods from 3 to 5 → 2 new IPs added
// ---------------------------------------------------------------------------

func TestPhase9_T92_ScaleUp_ThreeToFive_AddsTwoIPs(t *testing.T) {
	pe := setupPhase9Env(t, phase9EnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer pe.teardown(t)

	executor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	pe.startManager(t, executor)

	ns := "t92-ns"
	createNamespace(t, pe.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping1", spec)

	labels := map[string]string{"app": "web"}
	createPodWithIP(t, pe.k8sClient, ns, "pod-1", "10.0.0.1", labels)
	createPodWithIP(t, pe.k8sClient, ns, "pod-2", "10.0.0.2", labels)
	createPodWithIP(t, pe.k8sClient, ns, "pod-3", "10.0.0.3", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "mapping1")
	fakeClient := pe.fakeClientsBySub["sub1"]

	// Wait for initial 3 IPs
	eventually(t, 30*time.Second, 500*time.Millisecond, "initial 3 IPs", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg1", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) == 3, fmt.Sprintf("got %d IPs", len(ips))
	})

	// Scale up: add 2 more pods
	createPodWithIP(t, pe.k8sClient, ns, "pod-4", "10.0.0.4", labels)
	createPodWithIP(t, pe.k8sClient, ns, "pod-5", "10.0.0.5", labels)

	eventually(t, 30*time.Second, 500*time.Millisecond, "5 IPs after scale-up", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg1", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		if len(ips) != 5 {
			return false, fmt.Sprintf("got %d IPs, want 5: %v", len(ips), ips)
		}
		sort.Strings(ips)
		want := []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32", "10.0.0.4/32", "10.0.0.5/32"}
		for i := range want {
			if ips[i] != want[i] {
				return false, fmt.Sprintf("mismatch at %d: got %s want %s", i, ips[i], want[i])
			}
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.3: Scale pods from 5 to 2 → 3 IPs removed
// ---------------------------------------------------------------------------

func TestPhase9_T93_ScaleDown_FiveToTwo_RemovesThreeIPs(t *testing.T) {
	pe := setupPhase9Env(t, phase9EnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer pe.teardown(t)

	executor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	pe.startManager(t, executor)

	ns := "t93-ns"
	createNamespace(t, pe.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping1", spec)

	labels := map[string]string{"app": "web"}
	for i := 1; i <= 5; i++ {
		createPodWithIP(t, pe.k8sClient, ns, fmt.Sprintf("pod-%d", i), fmt.Sprintf("10.0.0.%d", i), labels)
	}

	prefixSetName := model.OwnershipKey("test-cluster", ns, "mapping1")
	fakeClient := pe.fakeClientsBySub["sub1"]

	// Wait for 5 IPs
	eventually(t, 30*time.Second, 500*time.Millisecond, "5 IPs present", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg1", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) == 5, fmt.Sprintf("got %d IPs", len(ips))
	})

	// Scale down: delete 3 pods
	deletePod(t, pe.k8sClient, ns, "pod-3")
	deletePod(t, pe.k8sClient, ns, "pod-4")
	deletePod(t, pe.k8sClient, ns, "pod-5")

	eventually(t, 30*time.Second, 500*time.Millisecond, "2 IPs after scale-down", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg1", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		if len(ips) != 2 {
			return false, fmt.Sprintf("got %d IPs, want 2: %v", len(ips), ips)
		}
		sorted := make([]string, len(ips))
		copy(sorted, ips)
		sort.Strings(sorted)
		expected := []string{"10.0.0.1/32", "10.0.0.2/32"}
		for i, ip := range expected {
			if sorted[i] != ip {
				return false, fmt.Sprintf("expected IPs %v, got %v", expected, sorted)
			}
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.4: Delete PodASGMapping → all owned prefix sets cleaned up
// ---------------------------------------------------------------------------

func TestPhase9_T94_DeleteMapping_CleansOwnedPrefixSets(t *testing.T) {
	pe := setupPhase9Env(t, phase9EnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer pe.teardown(t)

	executor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	pe.startManager(t, executor)

	ns := "t94-ns"
	createNamespace(t, pe.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping1", spec)

	labels := map[string]string{"app": "web"}
	createPodWithIP(t, pe.k8sClient, ns, "pod-1", "10.0.0.1", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "mapping1")
	fakeClient := pe.fakeClientsBySub["sub1"]

	// Wait for IP to appear
	eventually(t, 30*time.Second, 500*time.Millisecond, "IP in prefix set", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg1", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) >= 1, fmt.Sprintf("got %d IPs", len(ips))
	})

	// Delete the mapping
	deleteMapping(t, pe.k8sClient, ns, "mapping1")

	// Verify prefix set is deleted
	eventually(t, 30*time.Second, 500*time.Millisecond, "prefix set cleaned up", func() (bool, string) {
		_, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg1", prefixSetName)
		if ok {
			return false, "prefix set still exists"
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.5: Pod IP changes (recreate) → old IP removed, new IP added
// ---------------------------------------------------------------------------

func TestPhase9_T95_PodIPRecreate_ReplacesOldIPWithNewIP(t *testing.T) {
	pe := setupPhase9Env(t, phase9EnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer pe.teardown(t)

	executor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	pe.startManager(t, executor)

	ns := "t95-ns"
	createNamespace(t, pe.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping1", spec)

	labels := map[string]string{"app": "web"}
	createPodWithIP(t, pe.k8sClient, ns, "pod-1", "10.0.0.1", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "mapping1")
	fakeClient := pe.fakeClientsBySub["sub1"]

	// Wait for old IP
	eventually(t, 30*time.Second, 500*time.Millisecond, "old IP present", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg1", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		for _, ip := range ips {
			if ip == "10.0.0.1/32" {
				return true, ""
			}
		}
		return false, fmt.Sprintf("10.0.0.1 not in %v", ips)
	})

	// Simulate pod IP change: delete pod and recreate with new IP
	deletePod(t, pe.k8sClient, ns, "pod-1")
	createPodWithIP(t, pe.k8sClient, ns, "pod-1-new", "10.0.0.99", labels)

	// Verify old IP removed and new IP added
	eventually(t, 30*time.Second, 500*time.Millisecond, "new IP replaces old", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg1", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		hasOld := false
		hasNew := false
		for _, ip := range ips {
			if ip == "10.0.0.1/32" {
				hasOld = true
			}
			if ip == "10.0.0.99/32" {
				hasNew = true
			}
		}
		if hasOld {
			return false, "old IP still present"
		}
		if !hasNew {
			return false, "new IP not present"
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.6: Selector change excludes pods → excluded pod IPs removed
// ---------------------------------------------------------------------------

func TestPhase9_T96_SelectorChange_RemovesExcludedPodIPs(t *testing.T) {
	pe := setupPhase9Env(t, phase9EnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer pe.teardown(t)

	executor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	pe.startManager(t, executor)

	ns := "t96-ns"
	createNamespace(t, pe.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping1", spec)

	// Create 2 pods: one with app=web only, one with app=web AND tier=frontend
	createPodWithIP(t, pe.k8sClient, ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})
	createPodWithIP(t, pe.k8sClient, ns, "pod-2", "10.0.0.2", map[string]string{"app": "web", "tier": "frontend"})

	prefixSetName := model.OwnershipKey("test-cluster", ns, "mapping1")
	fakeClient := pe.fakeClientsBySub["sub1"]

	// Wait for both IPs
	eventually(t, 30*time.Second, 500*time.Millisecond, "both IPs present", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg1", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) == 2, fmt.Sprintf("got %d IPs: %v", len(ips), ips)
	})

	// Update selector to require tier=frontend (excludes pod-1)
	ctx := context.Background()
	var mapping v1alpha1.PodASGMapping
	key := client.ObjectKey{Namespace: ns, Name: "mapping1"}
	if err := pe.k8sClient.Get(ctx, key, &mapping); err != nil {
		t.Fatalf("get mapping: %v", err)
	}
	patch := client.MergeFrom(mapping.DeepCopy())
	mapping.Spec.Mappings[0].PodSelector.MatchLabels = map[string]string{"app": "web", "tier": "frontend"}
	if err := pe.k8sClient.Patch(ctx, &mapping, patch); err != nil {
		t.Fatalf("patch mapping selector: %v", err)
	}

	// Verify only pod-2's IP remains
	eventually(t, 30*time.Second, 500*time.Millisecond, "only pod-2 IP remains", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg1", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		if len(ips) != 1 {
			return false, fmt.Sprintf("got %d IPs: %v, want 1", len(ips), ips)
		}
		if ips[0] != "10.0.0.2/32" {
			return false, fmt.Sprintf("got IP %s, want 10.0.0.2", ips[0])
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.7: Cross-subscription: 2 ASGs in different subs → both receive correct IPs
// ---------------------------------------------------------------------------

func TestPhase9_T97_CrossSubscription_TwoASGsRoutedCorrectly(t *testing.T) {
	pe := setupPhase9Env(t, phase9EnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub-a", "sub-b"},
	})
	defer pe.teardown(t)

	executor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	pe.startManager(t, executor)

	ns := "t97-ns"
	createNamespace(t, pe.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "cross-sub"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub-a", "rg-a", "asg-a")},
					{ResourceID: asgResourceID("sub-b", "rg-b", "asg-b")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping1", spec)

	labels := map[string]string{"app": "cross-sub"}
	createPodWithIP(t, pe.k8sClient, ns, "pod-1", "10.0.0.1", labels)
	createPodWithIP(t, pe.k8sClient, ns, "pod-2", "10.0.0.2", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "mapping1")
	fakeClientA := pe.fakeClientsBySub["sub-a"]
	fakeClientB := pe.fakeClientsBySub["sub-b"]

	want := []string{"10.0.0.1/32", "10.0.0.2/32"}

	// Both subscriptions should receive both IPs
	eventually(t, 30*time.Second, 500*time.Millisecond, "sub-a has both IPs", func() (bool, string) {
		ips, ok := fakeClientA.PeekPrefixes("sub-a", "rg-a", "asg-a", prefixSetName)
		if !ok {
			return false, "prefix set not found in sub-a"
		}
		sort.Strings(ips)
		if len(ips) != 2 {
			return false, fmt.Sprintf("sub-a got %d IPs: %v", len(ips), ips)
		}
		for i := range want {
			if ips[i] != want[i] {
				return false, fmt.Sprintf("sub-a mismatch at %d", i)
			}
		}
		return true, ""
	})

	eventually(t, 30*time.Second, 500*time.Millisecond, "sub-b has both IPs", func() (bool, string) {
		ips, ok := fakeClientB.PeekPrefixes("sub-b", "rg-b", "asg-b", prefixSetName)
		if !ok {
			return false, "prefix set not found in sub-b"
		}
		sort.Strings(ips)
		if len(ips) != 2 {
			return false, fmt.Sprintf("sub-b got %d IPs: %v", len(ips), ips)
		}
		for i := range want {
			if ips[i] != want[i] {
				return false, fmt.Sprintf("sub-b mismatch at %d", i)
			}
		}
		return true, ""
	})

	// Negative routing isolation checks: ensure writes were exclusive to the correct client.
	// fakeClientA must NOT contain sub-b's resource (proves no broadcast to wrong client).
	if _, found := fakeClientA.PeekPrefixes("sub-b", "rg-b", "asg-b", prefixSetName); found {
		t.Fatal("routing isolation violated: fakeClientA contains sub-b prefix set")
	}
	// fakeClientB must NOT contain sub-a's resource (proves no broadcast to wrong client).
	if _, found := fakeClientB.PeekPrefixes("sub-a", "rg-a", "asg-a", prefixSetName); found {
		t.Fatal("routing isolation violated: fakeClientB contains sub-a prefix set")
	}
}

// ---------------------------------------------------------------------------
// T9.8: Controller restart mid-reconcile → deterministic convergence
// ---------------------------------------------------------------------------

func TestPhase9_T98_ControllerRestartMidReconcile_DeterministicConvergence(t *testing.T) {
	pe := setupPhase9Env(t, phase9EnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer pe.teardown(t)

	// Use a real executor as the delegate
	realExecutor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	blocker := newBlockingExecutor(realExecutor)
	pe.startManager(t, blocker)

	ns := "t98-ns"
	createNamespace(t, pe.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "restart"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-restart")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping1", spec)

	labels := map[string]string{"app": "restart"}
	createPodWithIP(t, pe.k8sClient, ns, "pod-1", "10.0.0.1", labels)
	createPodWithIP(t, pe.k8sClient, ns, "pod-2", "10.0.0.2", labels)

	// Wait for executor to be called (blocked)
	select {
	case <-blocker.started:
		// Executor was invoked and is now blocked
	case <-time.After(30 * time.Second):
		t.Fatal("executor was never called")
	}

	// Restart the manager while executor is blocked (simulates crash)
	newExecutor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	pe.restartManager(t, newExecutor)

	// Trigger reconcile on the new manager
	pe.triggerReconcile(t, client.ObjectKey{Namespace: ns, Name: "mapping1"}, "post-restart")

	prefixSetName := model.OwnershipKey("test-cluster", ns, "mapping1")
	fakeClient := pe.fakeClientsBySub["sub1"]

	// After restart, reconcile should converge to correct state
	eventually(t, 30*time.Second, 500*time.Millisecond, "converged after restart", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-restart", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		sort.Strings(ips)
		want := []string{"10.0.0.1/32", "10.0.0.2/32"}
		if len(ips) != 2 {
			return false, fmt.Sprintf("got %d IPs: %v", len(ips), ips)
		}
		for i := range want {
			if ips[i] != want[i] {
				return false, fmt.Sprintf("mismatch at %d", i)
			}
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.9: Drift injection — stale IP manually added → reconcile removes it
// ---------------------------------------------------------------------------

func TestPhase9_T99_DriftInjectedStaleIP_RemovedOnReconcile(t *testing.T) {
	pe := setupPhase9Env(t, phase9EnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer pe.teardown(t)

	executor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	pe.startManager(t, executor)

	ns := "t99-ns"
	createNamespace(t, pe.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "drift"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-drift")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping1", spec)

	labels := map[string]string{"app": "drift"}
	createPodWithIP(t, pe.k8sClient, ns, "pod-1", "10.0.0.1", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "mapping1")
	fakeClient := pe.fakeClientsBySub["sub1"]

	// Wait for correct state
	eventually(t, 30*time.Second, 500*time.Millisecond, "initial IP present", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-drift", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) == 1 && ips[0] == "10.0.0.1/32", fmt.Sprintf("got %v", ips)
	})

	// Inject drift: add a stale IP directly to fake Azure
	ctx := context.Background()
	if err := fakeClient.Put(ctx, "sub1", "rg1", "asg-drift", prefixSetName, []string{"10.0.0.1/32", "10.99.99.99/32"}); err != nil {
		t.Fatalf("inject drift: %v", err)
	}

	// Trigger reconcile to detect and fix drift
	pe.triggerReconcile(t, client.ObjectKey{Namespace: ns, Name: "mapping1"}, "drift-detect")

	// Verify stale IP is removed
	eventually(t, 30*time.Second, 500*time.Millisecond, "stale IP removed", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-drift", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		for _, ip := range ips {
			if ip == "10.99.99.99/32" {
				return false, "stale IP still present"
			}
		}
		if len(ips) != 1 || ips[0] != "10.0.0.1/32" {
			return false, fmt.Sprintf("unexpected IPs: %v", ips)
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.10: Drift injection — valid IP manually removed → reconcile re-adds it
// ---------------------------------------------------------------------------

func TestPhase9_T910_DriftInjectedMissingIP_ReaddedOnReconcile(t *testing.T) {
	pe := setupPhase9Env(t, phase9EnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer pe.teardown(t)

	executor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	pe.startManager(t, executor)

	ns := "t910-ns"
	createNamespace(t, pe.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "drift-missing"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-dm")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping1", spec)

	labels := map[string]string{"app": "drift-missing"}
	createPodWithIP(t, pe.k8sClient, ns, "pod-1", "10.0.0.1", labels)
	createPodWithIP(t, pe.k8sClient, ns, "pod-2", "10.0.0.2", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "mapping1")
	fakeClient := pe.fakeClientsBySub["sub1"]

	// Wait for both IPs
	eventually(t, 30*time.Second, 500*time.Millisecond, "both IPs present", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-dm", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) == 2, fmt.Sprintf("got %d IPs", len(ips))
	})

	// Inject drift: remove one IP
	ctx := context.Background()
	if err := fakeClient.Put(ctx, "sub1", "rg1", "asg-dm", prefixSetName, []string{"10.0.0.1/32"}); err != nil {
		t.Fatalf("inject drift: %v", err)
	}

	// Trigger reconcile
	pe.triggerReconcile(t, client.ObjectKey{Namespace: ns, Name: "mapping1"}, "drift-readd")

	// Verify missing IP re-added
	eventually(t, 30*time.Second, 500*time.Millisecond, "missing IP re-added", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-dm", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		sort.Strings(ips)
		if len(ips) != 2 {
			return false, fmt.Sprintf("got %d IPs: %v", len(ips), ips)
		}
		want := []string{"10.0.0.1/32", "10.0.0.2/32"}
		for i := range want {
			if ips[i] != want[i] {
				return false, fmt.Sprintf("mismatch: %v", ips)
			}
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.11: Two PodASGMappings in same namespace → isolated ownership
// ---------------------------------------------------------------------------

func TestPhase9_T911_TwoMappings_IsolatedOwnership(t *testing.T) {
	pe := setupPhase9Env(t, phase9EnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer pe.teardown(t)

	executor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	pe.startManager(t, executor)

	ns := "t911-ns"
	createNamespace(t, pe.k8sClient, ns)

	// Mapping A selects app=alpha
	specA := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "alpha"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-shared")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping-a", specA)

	// Mapping B selects app=beta
	specB := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "beta"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-shared")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping-b", specB)

	createPodWithIP(t, pe.k8sClient, ns, "pod-alpha", "10.0.0.1", map[string]string{"app": "alpha"})
	createPodWithIP(t, pe.k8sClient, ns, "pod-beta", "10.0.0.2", map[string]string{"app": "beta"})

	prefixSetA := model.OwnershipKey("test-cluster", ns, "mapping-a")
	prefixSetB := model.OwnershipKey("test-cluster", ns, "mapping-b")
	fakeClient := pe.fakeClientsBySub["sub1"]

	// Each mapping manages its own prefix set
	eventually(t, 30*time.Second, 500*time.Millisecond, "mapping-a owns alpha IP", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-shared", prefixSetA)
		if !ok {
			return false, "prefix set A not found"
		}
		if len(ips) != 1 || ips[0] != "10.0.0.1/32" {
			return false, fmt.Sprintf("mapping-a IPs: %v", ips)
		}
		return true, ""
	})

	eventually(t, 30*time.Second, 500*time.Millisecond, "mapping-b owns beta IP", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-shared", prefixSetB)
		if !ok {
			return false, "prefix set B not found"
		}
		if len(ips) != 1 || ips[0] != "10.0.0.2/32" {
			return false, fmt.Sprintf("mapping-b IPs: %v", ips)
		}
		return true, ""
	})

	// Delete mapping-a → only prefix set A is removed, B stays
	deleteMapping(t, pe.k8sClient, ns, "mapping-a")

	eventually(t, 30*time.Second, 500*time.Millisecond, "prefix set A cleaned, B intact", func() (bool, string) {
		_, okA := fakeClient.PeekPrefixes("sub1", "rg1", "asg-shared", prefixSetA)
		ipsB, okB := fakeClient.PeekPrefixes("sub1", "rg1", "asg-shared", prefixSetB)
		if okA {
			return false, "prefix set A still exists"
		}
		if !okB || len(ipsB) != 1 {
			return false, fmt.Sprintf("prefix set B missing or wrong: ok=%v ips=%v", okB, ipsB)
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.12: PodASGMapping with overlapping selectors → union across ASGs
// ---------------------------------------------------------------------------

func TestPhase9_T912_OverlappingSelectors_UnionAcrossASGs(t *testing.T) {
	pe := setupPhase9Env(t, phase9EnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer pe.teardown(t)

	executor := azure.NewExecutor(pe.zapLog, pe.fakeFactory, 5)
	pe.startManager(t, executor)

	ns := "t912-ns"
	createNamespace(t, pe.k8sClient, ns)

	// Two rules: one selects app=web, another selects tier=frontend
	// A pod with both labels should have IPs in both ASGs
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-web")},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"tier": "frontend"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-frontend")},
				},
			},
		},
	}
	createMapping(t, pe.k8sClient, ns, "mapping1", spec)

	// pod-overlap matches BOTH selectors
	createPodWithIP(t, pe.k8sClient, ns, "pod-overlap", "10.0.0.1", map[string]string{"app": "web", "tier": "frontend"})
	// pod-web-only matches only app=web
	createPodWithIP(t, pe.k8sClient, ns, "pod-web-only", "10.0.0.2", map[string]string{"app": "web"})

	prefixSetName := model.OwnershipKey("test-cluster", ns, "mapping1")
	fakeClient := pe.fakeClientsBySub["sub1"]

	// asg-web should have both pod IPs (pod-overlap + pod-web-only)
	eventually(t, 30*time.Second, 500*time.Millisecond, "asg-web has union of web-selector pods", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-web", prefixSetName)
		if !ok {
			return false, "prefix set for asg-web not found"
		}
		sort.Strings(ips)
		want := []string{"10.0.0.1/32", "10.0.0.2/32"}
		if len(ips) != 2 {
			return false, fmt.Sprintf("asg-web got %d IPs: %v", len(ips), ips)
		}
		for i := range want {
			if ips[i] != want[i] {
				return false, fmt.Sprintf("asg-web mismatch at %d: %v", i, ips)
			}
		}
		return true, ""
	})

	// asg-frontend should have only pod-overlap IP
	eventually(t, 30*time.Second, 500*time.Millisecond, "asg-frontend has only frontend-selector pod", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-frontend", prefixSetName)
		if !ok {
			return false, "prefix set for asg-frontend not found"
		}
		if len(ips) != 1 || ips[0] != "10.0.0.1/32" {
			return false, fmt.Sprintf("asg-frontend got: %v", ips)
		}
		return true, ""
	})
}

// Ensure fake import is used (prevents unused import error in compilation).
var _ *fake.Client
