package phase5_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ---------------------------------------------------------------------------
// Cross-module integration: Multiple pods aggregate IPs in a single prefix set
// Tests: Pod list → engine.ComputeDesiredState → diff → executor → Azure PUT
// ---------------------------------------------------------------------------
func TestPhase5_MultiplePods_AggregateIPsInPrefixSet(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-multi-pods"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-pod-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Create multiple pods with different IPs
	pods := []struct {
		name string
		ip   string
	}{
		{"web-pod-1", "10.0.0.1"},
		{"web-pod-2", "10.0.0.2"},
		{"web-pod-3", "10.0.0.3"},
	}

	for _, p := range pods {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      p.name,
				Namespace: ns.Name,
				Labels:    map[string]string{"app": "web"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
			},
		}
		if err := te.k8sClient.Create(ctx, pod); err != nil {
			t.Fatalf("failed to create pod %s: %v", p.name, err)
		}
		pod.Status.PodIP = p.ip
		if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
			t.Fatalf("failed to set pod %s IP: %v", p.name, err)
		}
	}

	ownershipKey := "test-cluster-" + ns.Name + "-multi-pod-mapping"

	// Assert: all 3 IPs should appear in the prefix set
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		ips := make(map[string]bool)
		for _, ip := range ps.Properties.AddressPrefixes {
			ips[ip] = true
		}
		return ips["10.0.0.1/32"] && ips["10.0.0.2/32"] && ips["10.0.0.3/32"]
	}, "expected all 3 pod IPs to be aggregated in the prefix set")
}

// ---------------------------------------------------------------------------
// Cross-module integration: Cross-subscription client routing
// Tests: ClientFactory → per-subscription Executor → reconciler pipeline
// ---------------------------------------------------------------------------
func TestPhase5_CrossSubscription_RoutesToCorrectClient(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	// Register a second subscription client
	fakeAzClient2 := fake.NewClient()
	te.fakeFactory.RegisterClient("sub2", fakeAzClient2)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-cross-sub"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Mapping with two rules targeting different subscriptions
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "cross-sub-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-sub1")},
						{ResourceID: asgResourceID("sub2", "rg2", "asg-sub2")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-sub-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to set pod IP: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-cross-sub-mapping"

	// Assert: prefix set created in sub1's client
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg-sub1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.0.1/32" {
				return true
			}
		}
		return false
	}, "expected pod IP in sub1 prefix set")

	// Assert: prefix set created in sub2's client
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := fakeAzClient2.Get(ctx, "sub2", "rg2", "asg-sub2", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.0.1/32" {
				return true
			}
		}
		return false
	}, "expected pod IP in sub2 prefix set")
}

// ---------------------------------------------------------------------------
// Cross-module integration: Executor ETag retry with reconciler
// Tests: Executor retry logic → fake client 412 → eventual success → reconciler
// ---------------------------------------------------------------------------
func TestPhase5_ExecutorETagRetry_EventualSuccess(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-etag-retry"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "etag-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Inject a transient 412 error (1 failure, then success on retry)
	ownershipKey := "test-cluster-" + ns.Name + "-etag-mapping"
	_ = te.fakeClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationPut,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		PrefixSetName:  ownershipKey,
	}, &azure.ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "etag mismatch"}, 1)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "etag-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to set pod IP: %v", err)
	}

	// Assert: despite the transient 412, the prefix set eventually gets created
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.0.1/32" {
				return true
			}
		}
		return false
	}, "expected prefix set to be created despite transient 412 (ETag retry)")
}

// ---------------------------------------------------------------------------
// Cross-module integration: Ownership annotation lifecycle
// Tests: ownership_store ↔ reconciler ↔ engine across add/remove cycles
// ---------------------------------------------------------------------------
func TestPhase5_OwnershipAnnotation_TracksASGLifecycle(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ownership-lifecycle"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "ownership-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ownership-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to set pod IP: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-ownership-mapping"

	// Wait for initial prefix set and ownership annotation
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		return err == nil && ps.Properties != nil && len(ps.Properties.AddressPrefixes) > 0
	}, "expected initial prefix set for ownership test")

	// Verify ownership annotation contains asg1
	eventually(t, 5*time.Second, 200*time.Millisecond, func() bool {
		var m v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "ownership-mapping", Namespace: ns.Name}, &m); err != nil {
			return false
		}
		ann, ok := m.Annotations[controller.OwnedASGsAnnotationKey]
		if !ok || ann == "" {
			return false
		}
		refs, err := controller.LoadOwnedASGs(&m)
		if err != nil || len(refs) == 0 {
			return false
		}
		for _, ref := range refs {
			if ref.ASGName == "asg1" && ref.SubscriptionID == "sub1" {
				return true
			}
		}
		return false
	}, "expected ownership annotation to track asg1")

	// Add a second ASG
	var current v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "ownership-mapping", Namespace: ns.Name}, &current); err != nil {
		t.Fatalf("failed to get mapping: %v", err)
	}
	current.Spec.Mappings[0].ApplicationSecurityGroups = append(
		current.Spec.Mappings[0].ApplicationSecurityGroups,
		v1alpha1.ASGReference{ResourceID: asgResourceID("sub1", "rg1", "asg2")},
	)
	if err := te.k8sClient.Update(ctx, &current); err != nil {
		t.Fatalf("failed to add asg2: %v", err)
	}

	// Verify ownership annotation contains both asg1 and asg2
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		var m v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "ownership-mapping", Namespace: ns.Name}, &m); err != nil {
			return false
		}
		refs, err := controller.LoadOwnedASGs(&m)
		if err != nil || len(refs) < 2 {
			return false
		}
		hasASG1, hasASG2 := false, false
		for _, ref := range refs {
			if ref.ASGName == "asg1" {
				hasASG1 = true
			}
			if ref.ASGName == "asg2" {
				hasASG2 = true
			}
		}
		return hasASG1 && hasASG2
	}, "expected ownership annotation to track both asg1 and asg2")

	// Remove asg1 from spec (keeping only asg2)
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "ownership-mapping", Namespace: ns.Name}, &current); err != nil {
		t.Fatalf("failed to re-get mapping: %v", err)
	}
	current.Spec.Mappings[0].ApplicationSecurityGroups = []v1alpha1.ASGReference{
		{ResourceID: asgResourceID("sub1", "rg1", "asg2")},
	}
	if err := te.k8sClient.Update(ctx, &current); err != nil {
		t.Fatalf("failed to remove asg1: %v", err)
	}

	// Verify ownership annotation no longer contains asg1 but still has asg2
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		var m v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "ownership-mapping", Namespace: ns.Name}, &m); err != nil {
			return false
		}
		refs, err := controller.LoadOwnedASGs(&m)
		if err != nil {
			return false
		}
		hasASG1, hasASG2 := false, false
		for _, ref := range refs {
			if ref.ASGName == "asg1" {
				hasASG1 = true
			}
			if ref.ASGName == "asg2" {
				hasASG2 = true
			}
		}
		return !hasASG1 && hasASG2
	}, "expected asg1 removed from ownership annotation after spec removal and successful delete")

	// Verify Azure state: asg1 prefix set deleted, asg2 prefix set present
	_, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
	if !azure.IsNotFound(err) {
		t.Error("expected asg1 prefix set to be deleted from Azure")
	}

	ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg2", ownershipKey)
	if err != nil {
		t.Fatalf("expected asg2 prefix set to exist: %v", err)
	}
	if ps.Properties == nil || len(ps.Properties.AddressPrefixes) == 0 {
		t.Error("expected asg2 prefix set to have IPs")
	}
}

// ---------------------------------------------------------------------------
// Cross-module integration: Pod without IP is excluded from desired state
// Tests: engine.ComputeDesiredState correctly skips pods with empty IP
// ---------------------------------------------------------------------------
func TestPhase5_PodWithoutIP_ExcludedFromPrefixSet(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-no-ip"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "no-ip-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Create a pod with IP and one without
	podWithIP := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-with-ip",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	podNoIP := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-no-ip",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}

	if err := te.k8sClient.Create(ctx, podWithIP); err != nil {
		t.Fatalf("failed to create pod-with-ip: %v", err)
	}
	if err := te.k8sClient.Create(ctx, podNoIP); err != nil {
		t.Fatalf("failed to create pod-no-ip: %v", err)
	}

	podWithIP.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, podWithIP); err != nil {
		t.Fatalf("failed to set pod-with-ip IP: %v", err)
	}
	// pod-no-ip intentionally has no IP set

	ownershipKey := "test-cluster-" + ns.Name + "-no-ip-mapping"

	// Wait for prefix set containing only the pod with IP
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		if len(ps.Properties.AddressPrefixes) != 1 {
			return false
		}
		return ps.Properties.AddressPrefixes[0] == "10.0.0.1/32"
	}, "expected only pod with IP to appear in prefix set (pod without IP excluded)")

	// Now assign IP to the previously IP-less pod → should be picked up
	var currentPodNoIP corev1.Pod
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "pod-no-ip", Namespace: ns.Name}, &currentPodNoIP); err != nil {
		t.Fatalf("failed to get pod-no-ip: %v", err)
	}
	currentPodNoIP.Status.PodIP = "10.0.0.2"
	if err := te.k8sClient.Status().Update(ctx, &currentPodNoIP); err != nil {
		t.Fatalf("failed to set pod-no-ip IP: %v", err)
	}

	// Assert: both IPs now in the prefix set
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		ips := make(map[string]bool)
		for _, ip := range ps.Properties.AddressPrefixes {
			ips[ip] = true
		}
		return ips["10.0.0.1/32"] && ips["10.0.0.2/32"] && len(ps.Properties.AddressPrefixes) == 2
	}, "expected both IPs in prefix set after delayed IP assignment")
}

// ---------------------------------------------------------------------------
// Cross-module integration: Multiple selectors in single mapping
// Tests: model.CompileSelector → engine.ComputeDesiredState aggregation
// ---------------------------------------------------------------------------
func TestPhase5_MultipleSelectorRules_AggregateToSameASG(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-multi-selector"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Mapping with two rules, different selectors, same ASG target
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-sel-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "frontend"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "backend"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	frontendPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "frontend-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"tier": "frontend"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	backendPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backend-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"tier": "backend"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}

	if err := te.k8sClient.Create(ctx, frontendPod); err != nil {
		t.Fatalf("failed to create frontend-pod: %v", err)
	}
	if err := te.k8sClient.Create(ctx, backendPod); err != nil {
		t.Fatalf("failed to create backend-pod: %v", err)
	}

	frontendPod.Status.PodIP = "10.0.1.1"
	if err := te.k8sClient.Status().Update(ctx, frontendPod); err != nil {
		t.Fatalf("failed to set frontend-pod IP: %v", err)
	}
	backendPod.Status.PodIP = "10.0.2.1"
	if err := te.k8sClient.Status().Update(ctx, backendPod); err != nil {
		t.Fatalf("failed to set backend-pod IP: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-multi-sel-mapping"

	// Assert: both IPs from different selectors aggregated into single prefix set
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		ips := make(map[string]bool)
		for _, ip := range ps.Properties.AddressPrefixes {
			ips[ip] = true
		}
		return ips["10.0.1.1/32"] && ips["10.0.2.1/32"]
	}, "expected IPs from both selectors aggregated in single prefix set")
}

// ---------------------------------------------------------------------------
// Cross-module integration: Mapping with unresolvable subscription
// Tests: Error propagation from ClientFactory through executor to reconciler
// ---------------------------------------------------------------------------
func TestPhase5_UnknownSubscription_GracefulError(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-unknown-sub"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Mapping targeting an unregistered subscription
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "unknown-sub-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("unknown-sub", "rg1", "asg1")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unknown-sub-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to set pod IP: %v", err)
	}

	// Wait for finalizer (reconcile at least attempted)
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		var m v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "unknown-sub-mapping", Namespace: ns.Name}, &m); err != nil {
			return false
		}
		for _, f := range m.Finalizers {
			if f == controller.CleanupFinalizer {
				return true
			}
		}
		return false
	}, "expected finalizer to be added despite unknown subscription error")

	// The reconciler should not crash and should continue retrying.
	// Verify the mapping still exists (controller didn't panic).
	var m v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "unknown-sub-mapping", Namespace: ns.Name}, &m); err != nil {
		t.Fatalf("expected mapping to still exist after unknown subscription error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Cross-module integration: Deterministic IP ordering in prefix set
// Tests: engine.ComputeDiff → sorted DesiredIPs → executor → Azure PUT
// ---------------------------------------------------------------------------
func TestPhase5_PrefixSetIPs_DeterministicOrder(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ip-order"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "order-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Create pods with IPs in reverse lexicographic order
	ips := []string{"10.0.0.9", "10.0.0.5", "10.0.0.1", "10.0.0.7", "10.0.0.3"}
	for i, ip := range ips {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("order-pod-%d", i),
				Namespace: ns.Name,
				Labels:    map[string]string{"app": "web"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
			},
		}
		if err := te.k8sClient.Create(ctx, pod); err != nil {
			t.Fatalf("failed to create pod %d: %v", i, err)
		}
		pod.Status.PodIP = ip
		if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
			t.Fatalf("failed to set pod %d IP: %v", i, err)
		}
	}

	ownershipKey := "test-cluster-" + ns.Name + "-order-mapping"

	// Wait for all IPs to appear
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		return len(ps.Properties.AddressPrefixes) == 5
	}, "expected all 5 IPs in prefix set")

	// Verify IPs are stored in sorted order (deterministic)
	ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
	if err != nil {
		t.Fatalf("failed to get prefix set: %v", err)
	}
	storedIPs := ps.Properties.AddressPrefixes
	sortedIPs := make([]string, len(storedIPs))
	copy(sortedIPs, storedIPs)
	sort.Strings(sortedIPs)

	for i := range storedIPs {
		if storedIPs[i] != sortedIPs[i] {
			t.Errorf("expected sorted IPs, got %v (expected %v)", storedIPs, sortedIPs)
			break
		}
	}
}
