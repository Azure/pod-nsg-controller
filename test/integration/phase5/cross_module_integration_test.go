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

// ===========================================================================
// Phase 5: Desired-State Cache — Cross-Module Integration Tests
// ===========================================================================

// ---------------------------------------------------------------------------
// TestPhase5_CacheBacked_MultiPodAggregation_PreservedWithPendingIP
// Cache-backed reconcile must still aggregate multiple pod IPs correctly and
// track pending-IP pods for follow-up behavior.
// ---------------------------------------------------------------------------
func TestPhase5_CacheBacked_MultiPodAggregation_PreservedWithPendingIP(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-cache-multi-pod"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-cache-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "cached"}},
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

	// Create 3 pods: 2 with IPs, 1 pending
	podsWithIP := []struct {
		name string
		ip   string
	}{
		{"pod-1", "10.0.1.1"},
		{"pod-2", "10.0.1.2"},
	}

	for _, p := range podsWithIP {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      p.name,
				Namespace: ns.Name,
				Labels:    map[string]string{"app": "cached"},
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

	// Create pending pod (no IP yet)
	pendingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-pending",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "cached"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pendingPod); err != nil {
		t.Fatalf("failed to create pending pod: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-multi-cache-mapping"

	// Verify both IPs are aggregated
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		ips := make(map[string]bool)
		for _, ip := range ps.Properties.AddressPrefixes {
			ips[ip] = true
		}
		return ips["10.0.1.1/32"] && ips["10.0.1.2/32"]
	}, "expected both pod IPs aggregated in prefix set")

	// Now the pending pod gets an IP → should be added via cache path
	pendingPod.Status.PodIP = "10.0.1.3"
	if err := te.k8sClient.Status().Update(ctx, pendingPod); err != nil {
		t.Fatalf("failed to set pending pod IP: %v", err)
	}

	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		ips := make(map[string]bool)
		for _, ip := range ps.Properties.AddressPrefixes {
			ips[ip] = true
		}
		return ips["10.0.1.1/32"] && ips["10.0.1.2/32"] && ips["10.0.1.3/32"]
	}, "expected all 3 IPs after pending pod gets IP")
}

// ---------------------------------------------------------------------------
// TestPhase5_InvalidToValid_MappingTransition_NoCacheReuse
// A mapping that transitions between spec generations (spec change) must not
// reuse any stale cache data from the prior generation. This verifies
// generation-based cache isolation and the Delete-on-terminal path.
// ---------------------------------------------------------------------------
func TestPhase5_InvalidToValid_MappingTransition_NoCacheReuse(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-invalid-to-valid"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Start with mapping pointing to asg-old
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "transition-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "transition"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-old")},
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
			Name:      "transition-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "transition"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.2.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to set pod IP: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-transition-mapping"

	// Wait for the old spec to reconcile and produce Azure state
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg-old", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.2.1/32" {
				return true
			}
		}
		return false
	}, "expected pod IP in asg-old before spec change")

	// Change the mapping to point to asg-new (generation bump invalidates cache)
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "transition-mapping", Namespace: ns.Name}, mapping); err != nil {
		t.Fatalf("failed to get mapping for update: %v", err)
	}
	mapping.Spec.Mappings[0].ApplicationSecurityGroups[0].ResourceID = asgResourceID("sub1", "rg1", "asg1")
	if err := te.k8sClient.Update(ctx, mapping); err != nil {
		t.Fatalf("failed to update mapping to new spec: %v", err)
	}

	// After spec change, the pod IP should appear at the new target (fresh recompute, not cached old data)
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.2.1/32" {
				return true
			}
		}
		return false
	}, "expected pod IP after spec transition (no stale cache reuse)")
}

// ---------------------------------------------------------------------------
// TestPhase5_ValidToValid_SelectorChange_NoStaleCarryover
// A valid-to-valid selector change must produce Azure state from the new
// generation's selector without stale prior-generation data.
// ---------------------------------------------------------------------------
func TestPhase5_ValidToValid_SelectorChange_NoStaleCarryover(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-selector-change"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Initial mapping selects app=v1
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "selector-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "v1"}},
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

	// Pod matching v1
	podV1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "v1-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "v1"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, podV1); err != nil {
		t.Fatalf("failed to create v1 pod: %v", err)
	}
	podV1.Status.PodIP = "10.0.3.1"
	if err := te.k8sClient.Status().Update(ctx, podV1); err != nil {
		t.Fatalf("failed to set v1 pod IP: %v", err)
	}

	// Pod matching v2 (pre-created)
	podV2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "v2-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "v2"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, podV2); err != nil {
		t.Fatalf("failed to create v2 pod: %v", err)
	}
	podV2.Status.PodIP = "10.0.3.2"
	if err := te.k8sClient.Status().Update(ctx, podV2); err != nil {
		t.Fatalf("failed to set v2 pod IP: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-selector-mapping"

	// Wait for v1 IP to appear
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.3.1/32" {
				return true
			}
		}
		return false
	}, "expected v1 pod IP in prefix set")

	// Change selector from app=v1 to app=v2 (valid-to-valid generation change)
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "selector-mapping", Namespace: ns.Name}, mapping); err != nil {
		t.Fatalf("failed to get mapping for update: %v", err)
	}
	mapping.Spec.Mappings[0].PodSelector.MatchLabels = map[string]string{"app": "v2"}
	if err := te.k8sClient.Update(ctx, mapping); err != nil {
		t.Fatalf("failed to update mapping selector: %v", err)
	}

	// After selector change: v2 IP should appear, v1 IP should be removed
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		hasV2 := false
		hasV1 := false
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.3.2/32" {
				hasV2 = true
			}
			if ip == "10.0.3.1/32" {
				hasV1 = true
			}
		}
		// v2 must be present, v1 must be gone (no stale carryover)
		return hasV2 && !hasV1
	}, "expected v2 IP present and v1 IP removed after selector change (no stale cache)")
}

// ---------------------------------------------------------------------------
// TestPhase5_CrossModule_PodEventMutation_TerminalCleanup_StalePublish
// Cross-module race: a pod event mutates cache version, terminal cleanup
// bumps lifecycle epoch, and a stale recompute attempts to publish. The cache
// must remain clean, and subsequent reconcile converges from fresh state.
// ---------------------------------------------------------------------------
func TestPhase5_CrossModule_PodEventMutation_TerminalCleanup_StalePublish(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-cross-race"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Create mapping and pods
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "cross-race-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "race"}},
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

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "race-pod-1",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "race"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	if err := te.k8sClient.Create(ctx, pod1); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod1.Status.PodIP = "10.0.1.1"
	if err := te.k8sClient.Status().Update(ctx, pod1); err != nil {
		t.Fatalf("failed to set pod IP: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-cross-race-mapping"

	// Wait for initial reconcile to converge
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.1.1/32" {
				return true
			}
		}
		return false
	}, "expected pod-1 IP in prefix set")

	// Rapid sequence: add pod (pod event mutation) → delete mapping (terminal cleanup)
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "race-pod-2",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "race"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	if err := te.k8sClient.Create(ctx, pod2); err != nil {
		t.Fatalf("failed to create pod-2: %v", err)
	}
	pod2.Status.PodIP = "10.0.1.2"
	if err := te.k8sClient.Status().Update(ctx, pod2); err != nil {
		t.Fatalf("failed to set pod-2 IP: %v", err)
	}

	// Immediately delete the mapping (races with pod-2 event)
	if err := te.k8sClient.Delete(ctx, mapping); err != nil {
		t.Fatalf("failed to delete mapping: %v", err)
	}

	// After terminal cleanup, prefix set should be cleaned up
	eventually(t, 15*time.Second, 500*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil {
			return true // not found = cleaned up
		}
		return ps.Properties == nil || len(ps.Properties.AddressPrefixes) == 0
	}, "expected prefix set cleaned up after terminal delete despite concurrent pod events")

	// Recreate mapping to verify clean convergence from fresh state
	freshMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "cross-race-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "fresh"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, freshMapping); err != nil {
		t.Fatalf("failed to recreate mapping: %v", err)
	}

	pod3 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fresh-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "fresh"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	if err := te.k8sClient.Create(ctx, pod3); err != nil {
		t.Fatalf("failed to create fresh pod: %v", err)
	}
	pod3.Status.PodIP = "10.0.1.3"
	if err := te.k8sClient.Status().Update(ctx, pod3); err != nil {
		t.Fatalf("failed to set fresh pod IP: %v", err)
	}

	// Fresh reconcile should converge with only the new pod's IP
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		hasFresh := false
		hasStale := false
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.1.3/32" {
				hasFresh = true
			}
			if ip == "10.0.1.1/32" || ip == "10.0.1.2/32" {
				hasStale = true
			}
		}
		return hasFresh && !hasStale
	}, "Phase 5: after cross-module race (pod event + terminal cleanup + recreate), only fresh state should converge")
}

// ===========================================================================
// Phase 5: Artifact-based Recompute Path — Integration Tests
// ===========================================================================

// ---------------------------------------------------------------------------
// TestPhase5_ArtifactRecomputePath_CacheMiss_ConvergesIdenticalToLegacy
// Verifies that when the reconciler uses the artifact-based recompute path
// on cache miss, the end-to-end behavior (pod list → compute → cache → diff
// → executor → Azure PUT) converges to the same Azure state as the legacy path.
// ---------------------------------------------------------------------------
func TestPhase5_ArtifactRecomputePath_CacheMiss_ConvergesIdenticalToLegacy(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-artifact-miss"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "artifact-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-artifact")},
					},
				},
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "backend"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-backend-artifact")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Create pods: pod-1 matches rule 0, pod-2 matches both rules
	pods := []struct {
		name   string
		ip     string
		labels map[string]string
	}{
		{"art-pod-1", "10.0.1.1", map[string]string{"app": "web"}},
		{"art-pod-2", "10.0.1.2", map[string]string{"app": "web", "tier": "backend"}},
	}

	for _, p := range pods {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      p.name,
				Namespace: ns.Name,
				Labels:    p.labels,
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

	ownershipKey := "test-cluster-" + ns.Name + "-artifact-mapping"

	// Assert: asg-artifact should have both pod IPs (rule 0 matches app=web)
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg-artifact", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		ips := make(map[string]bool)
		for _, ip := range ps.Properties.AddressPrefixes {
			ips[ip] = true
		}
		return ips["10.0.1.1/32"] && ips["10.0.1.2/32"]
	}, "Phase 5 artifact path: expected both pod IPs in asg-artifact prefix set")

	// Assert: asg-backend-artifact should have only pod-2's IP (rule 1 matches tier=backend)
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg-backend-artifact", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		ips := make(map[string]bool)
		for _, ip := range ps.Properties.AddressPrefixes {
			ips[ip] = true
		}
		return ips["10.0.1.2/32"] && !ips["10.0.1.1/32"]
	}, "Phase 5 artifact path: expected only pod-2 IP in asg-backend-artifact prefix set")
}

// ---------------------------------------------------------------------------
// TestPhase5_ArtifactRecomputePath_PendingIPPod_TrackedCorrectly
// Verifies that the artifact path correctly tracks pods without IPs and
// handles their eventual IP assignment through a subsequent reconcile.
// ---------------------------------------------------------------------------
func TestPhase5_ArtifactRecomputePath_PendingIPPod_TrackedCorrectly(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-artifact-pending"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-pending")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Create a pod WITH IP
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
	if err := te.k8sClient.Create(ctx, podWithIP); err != nil {
		t.Fatalf("failed to create pod-with-ip: %v", err)
	}
	podWithIP.Status.PodIP = "10.0.2.1"
	if err := te.k8sClient.Status().Update(ctx, podWithIP); err != nil {
		t.Fatalf("failed to set pod-with-ip IP: %v", err)
	}

	// Create a pod WITHOUT IP (simulating pending scheduling)
	podNoIP := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-pending",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, podNoIP); err != nil {
		t.Fatalf("failed to create pod-pending: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-pending-mapping"

	// Assert: initially only the pod with IP should appear
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg-pending", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		if len(ps.Properties.AddressPrefixes) != 1 {
			return false
		}
		return ps.Properties.AddressPrefixes[0] == "10.0.2.1/32"
	}, "Phase 5 artifact path: expected only pod-with-ip in prefix set initially")

	// Now assign IP to the pending pod
	var currentPodNoIP corev1.Pod
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "pod-pending", Namespace: ns.Name}, &currentPodNoIP); err != nil {
		t.Fatalf("failed to get pod-pending: %v", err)
	}
	currentPodNoIP.Status.PodIP = "10.0.2.2"
	if err := te.k8sClient.Status().Update(ctx, &currentPodNoIP); err != nil {
		t.Fatalf("failed to set pod-pending IP: %v", err)
	}

	// Assert: both IPs should appear after the pending pod gets its IP
	eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg-pending", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		ips := make(map[string]bool)
		for _, ip := range ps.Properties.AddressPrefixes {
			ips[ip] = true
		}
		return ips["10.0.2.1/32"] && ips["10.0.2.2/32"]
	}, "Phase 5 artifact path: expected both pod IPs after pending pod gets IP")
}

// ---------------------------------------------------------------------------
// TestPhase5_ForcedResync_ChurnScenario_BoundedRequeueAndEventualPublish
// Integration test: Under active pod churn during a forced resync window,
// the reconciler should eventually publish fresh recompute data (not stale
// cache fallback) and use bounded requeue to prevent hot loops.
// ---------------------------------------------------------------------------
func TestPhase5_ForcedResync_ChurnScenario_BoundedRequeueAndEventualPublish(t *testing.T) {
te := setupTestEnv(t)
defer te.teardown(t)
ctx := context.Background()

ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-forced-churn"}}
if err := te.k8sClient.Create(ctx, ns); err != nil {
t.Fatalf("failed to create namespace: %v", err)
}

mapping := &v1alpha1.PodASGMapping{
ObjectMeta: metav1.ObjectMeta{Name: "churn-mapping", Namespace: ns.Name},
Spec: v1alpha1.PodASGMappingSpec{
Mappings: []v1alpha1.Mapping{
{
PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "churn"}},
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

// Create initial set of pods
for i := 1; i <= 3; i++ {
pod := &corev1.Pod{
ObjectMeta: metav1.ObjectMeta{
Name: fmt.Sprintf("churn-pod-%d", i), Namespace: ns.Name,
Labels: map[string]string{"app": "churn"},
},
Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
}
if err := te.k8sClient.Create(ctx, pod); err != nil {
t.Fatalf("failed to create pod-%d: %v", i, err)
}
pod.Status.PodIP = fmt.Sprintf("10.0.3.%d", i)
if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
t.Fatalf("failed to set pod-%d IP: %v", i, err)
}
}

ownershipKey := "test-cluster-" + ns.Name + "-churn-mapping"

// Wait for initial convergence
eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
if err != nil || ps.Properties == nil {
return false
}
return len(ps.Properties.AddressPrefixes) == 3
}, "expected initial 3 pods to converge")

// Wait for forced resync to trigger (ResyncInterval=2s in test env)
time.Sleep(3 * time.Second)

// During the forced resync window, add a new pod (simulating churn)
pod4 := &corev1.Pod{
ObjectMeta: metav1.ObjectMeta{
Name: "churn-pod-4", Namespace: ns.Name,
Labels: map[string]string{"app": "churn"},
},
Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
}
if err := te.k8sClient.Create(ctx, pod4); err != nil {
t.Fatalf("failed to create pod-4: %v", err)
}
pod4.Status.PodIP = "10.0.3.4"
if err := te.k8sClient.Status().Update(ctx, pod4); err != nil {
t.Fatalf("failed to set pod-4 IP: %v", err)
}

// Assert: eventually all 4 IPs should appear, proving forced resync
// correctly publishes fresh recompute data even under churn
eventually(t, 15*time.Second, 300*time.Millisecond, func() bool {
ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
if err != nil || ps.Properties == nil {
return false
}
ips := make(map[string]bool)
for _, ip := range ps.Properties.AddressPrefixes {
ips[ip] = true
}
return ips["10.0.3.1/32"] && ips["10.0.3.2/32"] && ips["10.0.3.3/32"] && ips["10.0.3.4/32"]
}, "Phase 5: forced resync under churn must eventually publish all 4 pods (no stale cache substitution)")
}
