package integration_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/model"
	"go.uber.org/zap/zaptest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---------------------------------------------------------------------------
// T9.1: Create PodASGMapping + deploy 3 pods → all 3 IPs in prefix set
// ---------------------------------------------------------------------------

func TestReconcile_CreateMappingAndThreePods_AllIPsPresent(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	ie := setupIntegrationEnv(t, integrationEnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer ie.teardown(t)

	executor := azure.NewExecutor(zapLog, ie.fakeFactory, 5)
	ie.startManager(t, executor)

	ns := "reconcile-t1-ns"
	createNamespace(t, ie.k8sClient, ns)

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
	createMapping(t, ie.k8sClient, ns, "mapping1", spec)

	labels := map[string]string{"app": "web"}
	createPodWithIP(t, ie.k8sClient, ns, "pod-1", "10.0.0.1", labels)
	createPodWithIP(t, ie.k8sClient, ns, "pod-2", "10.0.0.2", labels)
	createPodWithIP(t, ie.k8sClient, ns, "pod-3", "10.0.0.3", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "mapping1")
	fakeClient := ie.fakeClientsBySub["sub1"]

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

func TestReconcile_ScaleUp_ThreeToFive_AddsTwoIPs(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	ie := setupIntegrationEnv(t, integrationEnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer ie.teardown(t)

	executor := azure.NewExecutor(zapLog, ie.fakeFactory, 5)
	ie.startManager(t, executor)

	ns := "reconcile-t2-ns"
	createNamespace(t, ie.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "api"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-api")},
				},
			},
		},
	}
	createMapping(t, ie.k8sClient, ns, "api-mapping", spec)

	labels := map[string]string{"app": "api"}
	createPodWithIP(t, ie.k8sClient, ns, "pod-1", "10.1.0.1", labels)
	createPodWithIP(t, ie.k8sClient, ns, "pod-2", "10.1.0.2", labels)
	createPodWithIP(t, ie.k8sClient, ns, "pod-3", "10.1.0.3", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "api-mapping")
	fakeClient := ie.fakeClientsBySub["sub1"]

	eventually(t, 30*time.Second, 500*time.Millisecond, "initial 3 IPs", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-api", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) == 3, fmt.Sprintf("got %d IPs, want 3", len(ips))
	})

	// Scale up: add 2 more pods
	createPodWithIP(t, ie.k8sClient, ns, "pod-4", "10.1.0.4", labels)
	createPodWithIP(t, ie.k8sClient, ns, "pod-5", "10.1.0.5", labels)

	eventually(t, 30*time.Second, 500*time.Millisecond, "scaled to 5 IPs", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-api", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) == 5, fmt.Sprintf("got %d IPs, want 5: %v", len(ips), ips)
	})
}

// ---------------------------------------------------------------------------
// T9.4: Delete PodASGMapping → owned prefix sets cleaned up
// ---------------------------------------------------------------------------

func TestReconcile_DeleteMapping_CleansOwnedPrefixSets(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	ie := setupIntegrationEnv(t, integrationEnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer ie.teardown(t)

	executor := azure.NewExecutor(zapLog, ie.fakeFactory, 5)
	ie.startManager(t, executor)

	ns := "reconcile-t4-ns"
	createNamespace(t, ie.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "cleanup"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-cleanup")},
				},
			},
		},
	}
	createMapping(t, ie.k8sClient, ns, "cleanup-mapping", spec)

	labels := map[string]string{"app": "cleanup"}
	createPodWithIP(t, ie.k8sClient, ns, "pod-1", "10.4.0.1", labels)
	createPodWithIP(t, ie.k8sClient, ns, "pod-2", "10.4.0.2", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "cleanup-mapping")
	fakeClient := ie.fakeClientsBySub["sub1"]

	eventually(t, 30*time.Second, 500*time.Millisecond, "IPs present before delete", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-cleanup", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) == 2, fmt.Sprintf("got %d IPs, want 2", len(ips))
	})

	// Delete the mapping
	deleteMapping(t, ie.k8sClient, ns, "cleanup-mapping")

	eventually(t, 30*time.Second, 500*time.Millisecond, "prefix set cleaned after delete", func() (bool, string) {
		_, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-cleanup", prefixSetName)
		if ok {
			return false, "prefix set still exists (expected NotFound)"
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.7: Cross-subscription mapping routes to correct ASG
// ---------------------------------------------------------------------------

func TestReconcile_CrossSubscription_TwoASGsRoutedCorrectly(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	ie := setupIntegrationEnv(t, integrationEnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub-a", "sub-b"},
	})
	defer ie.teardown(t)

	executor := azure.NewExecutor(zapLog, ie.fakeFactory, 5)
	ie.startManager(t, executor)

	ns := "reconcile-t7-ns"
	createNamespace(t, ie.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"tier": "frontend"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub-a", "rg-a", "asg-frontend")},
					{ResourceID: asgResourceID("sub-b", "rg-b", "asg-frontend-mirror")},
				},
			},
		},
	}
	createMapping(t, ie.k8sClient, ns, "cross-sub-mapping", spec)

	labels := map[string]string{"tier": "frontend"}
	createPodWithIP(t, ie.k8sClient, ns, "fe-pod-1", "10.7.0.1", labels)
	createPodWithIP(t, ie.k8sClient, ns, "fe-pod-2", "10.7.0.2", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "cross-sub-mapping")
	fakeClientA := ie.fakeClientsBySub["sub-a"]
	fakeClientB := ie.fakeClientsBySub["sub-b"]

	eventually(t, 30*time.Second, 500*time.Millisecond, "IPs in sub-a ASG", func() (bool, string) {
		ips, ok := fakeClientA.PeekPrefixes("sub-a", "rg-a", "asg-frontend", prefixSetName)
		if !ok {
			return false, "prefix set not found in sub-a"
		}
		return len(ips) == 2, fmt.Sprintf("sub-a: got %d IPs, want 2", len(ips))
	})

	eventually(t, 30*time.Second, 500*time.Millisecond, "IPs in sub-b ASG", func() (bool, string) {
		ips, ok := fakeClientB.PeekPrefixes("sub-b", "rg-b", "asg-frontend-mirror", prefixSetName)
		if !ok {
			return false, "prefix set not found in sub-b"
		}
		return len(ips) == 2, fmt.Sprintf("sub-b: got %d IPs, want 2", len(ips))
	})
}

// ---------------------------------------------------------------------------
// T9.9: Drift — stale IP injected externally → removed on reconcile
// ---------------------------------------------------------------------------

func TestReconcile_DriftInjectedStaleIP_RemovedOnReconcile(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	ie := setupIntegrationEnv(t, integrationEnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer ie.teardown(t)

	executor := azure.NewExecutor(zapLog, ie.fakeFactory, 5)
	ie.startManager(t, executor)

	ns := "reconcile-t9-ns"
	createNamespace(t, ie.k8sClient, ns)

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
	createMapping(t, ie.k8sClient, ns, "drift-mapping", spec)

	labels := map[string]string{"app": "drift"}
	createPodWithIP(t, ie.k8sClient, ns, "drift-pod-1", "10.9.0.1", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "drift-mapping")
	fakeClient := ie.fakeClientsBySub["sub1"]

	eventually(t, 30*time.Second, 500*time.Millisecond, "initial IP present", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-drift", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) == 1, fmt.Sprintf("got %d IPs, want 1", len(ips))
	})

	// Inject drift: add a stale IP directly to fake Azure
	ctx := context.Background()
	if err := fakeClient.Put(ctx, "sub1", "rg1", "asg-drift", prefixSetName, []string{"10.9.0.1/32", "10.9.99.99/32"}); err != nil {
		t.Fatalf("inject drift: %v", err)
	}

	// Trigger reconcile to detect drift
	ie.triggerReconcile(t, client.ObjectKey{Namespace: ns, Name: "drift-mapping"})

	eventually(t, 30*time.Second, 500*time.Millisecond, "stale IP removed after reconcile", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-drift", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		sort.Strings(ips)
		if len(ips) == 1 && ips[0] == "10.9.0.1/32" {
			return true, ""
		}
		return false, fmt.Sprintf("got %v, want only [10.9.0.1/32]", ips)
	})
}

// ---------------------------------------------------------------------------
// T9.3: Scale pods from 5 to 2 → 3 IPs removed
// ---------------------------------------------------------------------------

func TestReconcile_ScaleDown_FiveToTwo_RemovesThreeIPs(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	ie := setupIntegrationEnv(t, integrationEnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer ie.teardown(t)

	executor := azure.NewExecutor(zapLog, ie.fakeFactory, 5)
	ie.startManager(t, executor)

	ns := "reconcile-t3-ns"
	createNamespace(t, ie.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "scale"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-scale")},
				},
			},
		},
	}
	createMapping(t, ie.k8sClient, ns, "scale-mapping", spec)

	labels := map[string]string{"app": "scale"}
	for i := 1; i <= 5; i++ {
		createPodWithIP(t, ie.k8sClient, ns, fmt.Sprintf("pod-%d", i), fmt.Sprintf("10.3.0.%d", i), labels)
	}

	prefixSetName := model.OwnershipKey("test-cluster", ns, "scale-mapping")
	fakeClient := ie.fakeClientsBySub["sub1"]

	eventually(t, 30*time.Second, 500*time.Millisecond, "5 IPs present", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-scale", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) == 5, fmt.Sprintf("got %d IPs", len(ips))
	})

	// Scale down: delete 3 pods
	deletePod(t, ie.k8sClient, ns, "pod-3")
	deletePod(t, ie.k8sClient, ns, "pod-4")
	deletePod(t, ie.k8sClient, ns, "pod-5")

	eventually(t, 30*time.Second, 500*time.Millisecond, "2 IPs after scale-down", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-scale", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		if len(ips) != 2 {
			return false, fmt.Sprintf("got %d IPs, want 2: %v", len(ips), ips)
		}
		sorted := make([]string, len(ips))
		copy(sorted, ips)
		sort.Strings(sorted)
		want := []string{"10.3.0.1/32", "10.3.0.2/32"}
		for i, ip := range want {
			if sorted[i] != ip {
				return false, fmt.Sprintf("expected %v, got %v", want, sorted)
			}
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.5: Pod IP recreate → old IP removed, new IP added
// ---------------------------------------------------------------------------

func TestReconcile_PodIPRecreate_ReplacesOldIPWithNewIP(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	ie := setupIntegrationEnv(t, integrationEnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer ie.teardown(t)

	executor := azure.NewExecutor(zapLog, ie.fakeFactory, 5)
	ie.startManager(t, executor)

	ns := "reconcile-t5-ns"
	createNamespace(t, ie.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "recreate"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-recreate")},
				},
			},
		},
	}
	createMapping(t, ie.k8sClient, ns, "recreate-mapping", spec)

	labels := map[string]string{"app": "recreate"}
	createPodWithIP(t, ie.k8sClient, ns, "pod-1", "10.5.0.1", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "recreate-mapping")
	fakeClient := ie.fakeClientsBySub["sub1"]

	eventually(t, 30*time.Second, 500*time.Millisecond, "old IP present", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-recreate", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		for _, ip := range ips {
			if ip == "10.5.0.1/32" {
				return true, ""
			}
		}
		return false, fmt.Sprintf("10.5.0.1/32 not in %v", ips)
	})

	// Simulate pod IP change: delete and recreate with new IP
	deletePod(t, ie.k8sClient, ns, "pod-1")
	createPodWithIP(t, ie.k8sClient, ns, "pod-1-new", "10.5.0.99", labels)

	eventually(t, 30*time.Second, 500*time.Millisecond, "new IP replaces old", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-recreate", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		hasOld := false
		hasNew := false
		for _, ip := range ips {
			if ip == "10.5.0.1/32" {
				hasOld = true
			}
			if ip == "10.5.0.99/32" {
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

func TestReconcile_SelectorChange_RemovesExcludedPodIPs(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	ie := setupIntegrationEnv(t, integrationEnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer ie.teardown(t)

	executor := azure.NewExecutor(zapLog, ie.fakeFactory, 5)
	ie.startManager(t, executor)

	ns := "reconcile-t6-ns"
	createNamespace(t, ie.k8sClient, ns)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-selector")},
				},
			},
		},
	}
	createMapping(t, ie.k8sClient, ns, "selector-mapping", spec)

	createPodWithIP(t, ie.k8sClient, ns, "pod-1", "10.6.0.1", map[string]string{"app": "web"})
	createPodWithIP(t, ie.k8sClient, ns, "pod-2", "10.6.0.2", map[string]string{"app": "web", "tier": "frontend"})

	prefixSetName := model.OwnershipKey("test-cluster", ns, "selector-mapping")
	fakeClient := ie.fakeClientsBySub["sub1"]

	eventually(t, 30*time.Second, 500*time.Millisecond, "both IPs present", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-selector", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) == 2, fmt.Sprintf("got %d IPs: %v", len(ips), ips)
	})

	// Update selector to require tier=frontend (excludes pod-1)
	ctx := context.Background()
	var mapping v1alpha1.PodASGMapping
	key := client.ObjectKey{Namespace: ns, Name: "selector-mapping"}
	if err := ie.k8sClient.Get(ctx, key, &mapping); err != nil {
		t.Fatalf("get mapping: %v", err)
	}
	patch := client.MergeFrom(mapping.DeepCopy())
	mapping.Spec.Mappings[0].PodSelector.MatchLabels = map[string]string{"app": "web", "tier": "frontend"}
	if err := ie.k8sClient.Patch(ctx, &mapping, patch); err != nil {
		t.Fatalf("patch mapping selector: %v", err)
	}

	eventually(t, 30*time.Second, 500*time.Millisecond, "only pod-2 IP remains", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-selector", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		if len(ips) != 1 {
			return false, fmt.Sprintf("got %d IPs: %v, want 1", len(ips), ips)
		}
		if ips[0] != "10.6.0.2/32" {
			return false, fmt.Sprintf("got IP %s, want 10.6.0.2/32", ips[0])
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// T9.8: Controller restart mid-reconcile → deterministic convergence
// ---------------------------------------------------------------------------

func TestReconcile_ControllerRestart_DeterministicConvergence(t *testing.T) {
	ie := setupIntegrationEnv(t, integrationEnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer ie.teardown(t)

	realExecutor := azure.NewExecutor(ie.zapLog, ie.fakeFactory, 5)
	blocker := newBlockingExecutor(realExecutor)
	ie.startManager(t, blocker)

	ns := "reconcile-t8-ns"
	createNamespace(t, ie.k8sClient, ns)

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
	createMapping(t, ie.k8sClient, ns, "restart-mapping", spec)

	labels := map[string]string{"app": "restart"}
	createPodWithIP(t, ie.k8sClient, ns, "pod-1", "10.8.0.1", labels)
	createPodWithIP(t, ie.k8sClient, ns, "pod-2", "10.8.0.2", labels)

	// Wait for executor to be called (blocked)
	select {
	case <-blocker.started:
	case <-time.After(30 * time.Second):
		t.Fatal("executor was never called")
	}

	// Restart manager while executor is blocked (simulates crash)
	newExecutor := azure.NewExecutor(ie.zapLog, ie.fakeFactory, 5)
	ie.restartManager(t, newExecutor)

	// Trigger reconcile on the new manager
	ie.triggerReconcile(t, client.ObjectKey{Namespace: ns, Name: "restart-mapping"})

	prefixSetName := model.OwnershipKey("test-cluster", ns, "restart-mapping")
	fakeClient := ie.fakeClientsBySub["sub1"]

	eventually(t, 30*time.Second, 500*time.Millisecond, "converged after restart", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-restart", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		sort.Strings(ips)
		want := []string{"10.8.0.1/32", "10.8.0.2/32"}
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
// T9.10: Drift — valid IP manually removed → reconcile re-adds it
// ---------------------------------------------------------------------------

func TestReconcile_DriftMissingIP_ReaddedOnReconcile(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	ie := setupIntegrationEnv(t, integrationEnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer ie.teardown(t)

	executor := azure.NewExecutor(zapLog, ie.fakeFactory, 5)
	ie.startManager(t, executor)

	ns := "reconcile-t10-ns"
	createNamespace(t, ie.k8sClient, ns)

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
	createMapping(t, ie.k8sClient, ns, "dm-mapping", spec)

	labels := map[string]string{"app": "drift-missing"}
	createPodWithIP(t, ie.k8sClient, ns, "pod-1", "10.10.0.1", labels)
	createPodWithIP(t, ie.k8sClient, ns, "pod-2", "10.10.0.2", labels)

	prefixSetName := model.OwnershipKey("test-cluster", ns, "dm-mapping")
	fakeClient := ie.fakeClientsBySub["sub1"]

	eventually(t, 30*time.Second, 500*time.Millisecond, "both IPs present", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-dm", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		return len(ips) == 2, fmt.Sprintf("got %d IPs", len(ips))
	})

	// Inject drift: remove one IP
	ctx := context.Background()
	if err := fakeClient.Put(ctx, "sub1", "rg1", "asg-dm", prefixSetName, []string{"10.10.0.1/32"}); err != nil {
		t.Fatalf("inject drift: %v", err)
	}

	// Trigger reconcile
	ie.triggerReconcile(t, client.ObjectKey{Namespace: ns, Name: "dm-mapping"})

	eventually(t, 30*time.Second, 500*time.Millisecond, "missing IP re-added", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-dm", prefixSetName)
		if !ok {
			return false, "prefix set not found"
		}
		sort.Strings(ips)
		if len(ips) != 2 {
			return false, fmt.Sprintf("got %d IPs: %v", len(ips), ips)
		}
		want := []string{"10.10.0.1/32", "10.10.0.2/32"}
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

func TestReconcile_TwoMappings_IsolatedOwnership(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	ie := setupIntegrationEnv(t, integrationEnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer ie.teardown(t)

	executor := azure.NewExecutor(zapLog, ie.fakeFactory, 5)
	ie.startManager(t, executor)

	ns := "reconcile-t11-ns"
	createNamespace(t, ie.k8sClient, ns)

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
	createMapping(t, ie.k8sClient, ns, "mapping-a", specA)

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
	createMapping(t, ie.k8sClient, ns, "mapping-b", specB)

	createPodWithIP(t, ie.k8sClient, ns, "pod-alpha", "10.11.0.1", map[string]string{"app": "alpha"})
	createPodWithIP(t, ie.k8sClient, ns, "pod-beta", "10.11.0.2", map[string]string{"app": "beta"})

	prefixSetA := model.OwnershipKey("test-cluster", ns, "mapping-a")
	prefixSetB := model.OwnershipKey("test-cluster", ns, "mapping-b")
	fakeClient := ie.fakeClientsBySub["sub1"]

	eventually(t, 30*time.Second, 500*time.Millisecond, "mapping-a owns alpha IP", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-shared", prefixSetA)
		if !ok {
			return false, "prefix set A not found"
		}
		if len(ips) != 1 || ips[0] != "10.11.0.1/32" {
			return false, fmt.Sprintf("mapping-a IPs: %v", ips)
		}
		return true, ""
	})

	eventually(t, 30*time.Second, 500*time.Millisecond, "mapping-b owns beta IP", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-shared", prefixSetB)
		if !ok {
			return false, "prefix set B not found"
		}
		if len(ips) != 1 || ips[0] != "10.11.0.2/32" {
			return false, fmt.Sprintf("mapping-b IPs: %v", ips)
		}
		return true, ""
	})

	// Delete mapping-a → only prefix set A is removed, B stays
	deleteMapping(t, ie.k8sClient, ns, "mapping-a")

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
// T9.12: Overlapping selectors → union across ASGs
// ---------------------------------------------------------------------------

func TestReconcile_OverlappingSelectors_UnionAcrossASGs(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	ie := setupIntegrationEnv(t, integrationEnvOptions{
		ClusterName:   "test-cluster",
		Subscriptions: []string{"sub1"},
	})
	defer ie.teardown(t)

	executor := azure.NewExecutor(zapLog, ie.fakeFactory, 5)
	ie.startManager(t, executor)

	ns := "reconcile-t12-ns"
	createNamespace(t, ie.k8sClient, ns)

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
	createMapping(t, ie.k8sClient, ns, "overlap-mapping", spec)

	// pod-overlap matches BOTH selectors
	createPodWithIP(t, ie.k8sClient, ns, "pod-overlap", "10.12.0.1", map[string]string{"app": "web", "tier": "frontend"})
	// pod-web-only matches only app=web
	createPodWithIP(t, ie.k8sClient, ns, "pod-web-only", "10.12.0.2", map[string]string{"app": "web"})

	prefixSetName := model.OwnershipKey("test-cluster", ns, "overlap-mapping")
	fakeClient := ie.fakeClientsBySub["sub1"]

	// asg-web should have both pod IPs
	eventually(t, 30*time.Second, 500*time.Millisecond, "asg-web has union", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-web", prefixSetName)
		if !ok {
			return false, "prefix set for asg-web not found"
		}
		sort.Strings(ips)
		want := []string{"10.12.0.1/32", "10.12.0.2/32"}
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
	eventually(t, 30*time.Second, 500*time.Millisecond, "asg-frontend has frontend only", func() (bool, string) {
		ips, ok := fakeClient.PeekPrefixes("sub1", "rg1", "asg-frontend", prefixSetName)
		if !ok {
			return false, "prefix set for asg-frontend not found"
		}
		if len(ips) != 1 || ips[0] != "10.12.0.1/32" {
			return false, fmt.Sprintf("asg-frontend got: %v", ips)
		}
		return true, ""
	})
}
