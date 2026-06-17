//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/model"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// liveControllerMode indicates the E2E controller execution model.
type liveControllerMode string

const (
	liveControllerModeExternal liveControllerMode = "external"
)

// e2eASGTarget holds a parsed ASG resource ID for E2E assertions.
type e2eASGTarget struct {
	ResourceID string
	Parsed     model.ParsedASGReference
}

// liveE2EConfig holds all configuration for E2E tests.
type liveE2EConfig struct {
	Enabled              bool
	ControllerMode       liveControllerMode
	KubeconfigPath       string
	KubeContext          string
	ClusterName          string
	ControllerNamespace  string
	ControllerDeployment string
	Namespace            string
	MappingName          string
	WorkloadName         string
	FixtureImage         string
	LifecycleTargets     []e2eASGTarget
	CrossSubTargets      []e2eASGTarget
	ScaleReplicaGoal     int
	ScaleSLO             time.Duration
	Timeout              time.Duration
	PollInterval         time.Duration
}

func requireLiveE2EConfig(t *testing.T) liveE2EConfig {
	t.Helper()

	if os.Getenv("AZURE_E2E") != "true" {
		t.Skip("AZURE_E2E != true; skipping E2E tests")
	}

	clusterName := os.Getenv("E2E_CLUSTER_NAME")
	if clusterName == "" {
		t.Fatal("E2E_CLUSTER_NAME is required when AZURE_E2E=true")
	}

	primaryASG := os.Getenv("E2E_PRIMARY_ASG_RESOURCE_ID")
	if primaryASG == "" {
		t.Fatal("E2E_PRIMARY_ASG_RESOURCE_ID is required when AZURE_E2E=true")
	}

	parsed, err := model.ParseASGResourceID(primaryASG)
	if err != nil {
		t.Fatalf("invalid E2E_PRIMARY_ASG_RESOURCE_ID: %v", err)
	}

	cfg := liveE2EConfig{
		Enabled:              true,
		ControllerMode:       liveControllerModeExternal,
		ClusterName:          clusterName,
		KubeconfigPath:       envOrDefault("E2E_KUBECONFIG", os.Getenv("KUBECONFIG")),
		KubeContext:          os.Getenv("E2E_KUBE_CONTEXT"),
		ControllerNamespace:  envOrDefault("E2E_CONTROLLER_NAMESPACE", "pod-nsg-controller-system"),
		ControllerDeployment: envOrDefault("E2E_CONTROLLER_DEPLOYMENT", "pod-nsg-controller"),
		Namespace:            envOrDefault("E2E_NAMESPACE", fmt.Sprintf("phase9-e2e-%d", time.Now().UnixNano()%10000)),
		MappingName:          "e2e-mapping",
		WorkloadName:         "e2e-workload",
		FixtureImage:         envOrDefault("E2E_FIXTURE_IMAGE", "registry.k8s.io/pause:3.9"),
		LifecycleTargets:     []e2eASGTarget{{ResourceID: primaryASG, Parsed: parsed}},
		ScaleReplicaGoal:     envIntOrDefault("E2E_SCALE_REPLICAS", 100),
		ScaleSLO:             time.Duration(envIntOrDefault("E2E_SLO_SECONDS", 10)) * time.Second,
		Timeout:              time.Duration(envIntOrDefault("E2E_TIMEOUT_SECONDS", 180)) * time.Second,
		PollInterval:         time.Duration(envIntOrDefault("E2E_POLL_INTERVAL_MS", 1000)) * time.Millisecond,
	}

	return cfg
}

func requireCrossSubTargets(t *testing.T) []e2eASGTarget {
	t.Helper()

	crossSubRaw := os.Getenv("E2E_CROSS_SUB_ASG_RESOURCE_IDS")
	if crossSubRaw == "" {
		t.Fatal("E2E_CROSS_SUB_ASG_RESOURCE_IDS is required for T9.E2")
	}

	ids := strings.Split(crossSubRaw, ",")
	seenSubs := make(map[string]struct{})
	targets := make([]e2eASGTarget, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		p, err := model.ParseASGResourceID(id)
		if err != nil {
			t.Fatalf("invalid E2E_CROSS_SUB_ASG_RESOURCE_IDS entry %q: %v", id, err)
		}
		seenSubs[strings.ToLower(p.SubscriptionID)] = struct{}{}
		targets = append(targets, e2eASGTarget{ResourceID: id, Parsed: p})
	}
	if len(seenSubs) < 2 {
		t.Fatal("E2E_CROSS_SUB_ASG_RESOURCE_IDS must contain at least 2 distinct subscriptions")
	}

	return targets
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func envIntOrDefault(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return defaultVal
}

func buildKubeRESTConfig(t *testing.T, cfg liveE2EConfig) *rest.Config {
	t.Helper()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.KubeconfigPath != "" {
		rules.ExplicitPath = cfg.KubeconfigPath
	}

	overrides := &clientcmd.ConfigOverrides{}
	if cfg.KubeContext != "" {
		overrides.CurrentContext = cfg.KubeContext
	}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	restCfg, err := kubeConfig.ClientConfig()
	if err != nil {
		t.Fatalf("failed to build kube REST config: %v", err)
	}
	return restCfg
}

func buildKubeClient(t *testing.T, restCfg *rest.Config, scheme *runtime.Scheme) client.Client {
	t.Helper()
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("failed to build kube client: %v", err)
	}
	return c
}

func e2eScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func requireExternalControllerReady(t *testing.T, c client.Client, cfg liveE2EConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var deploy appsv1.Deployment
	key := types.NamespacedName{Namespace: cfg.ControllerNamespace, Name: cfg.ControllerDeployment}
	if err := c.Get(ctx, key, &deploy); err != nil {
		t.Fatalf("controller deployment not found: %v", err)
	}

	if deploy.Status.AvailableReplicas < 1 {
		t.Fatalf("controller deployment has %d available replicas, need >= 1", deploy.Status.AvailableReplicas)
	}
}

func requireCRDInstalled(t *testing.T, c client.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var list v1alpha1.PodASGMappingList
	if err := c.List(ctx, &list, client.InNamespace("default")); err != nil {
		t.Fatalf("PodASGMapping CRD not installed or inaccessible: %v", err)
	}
}

func newLivePrefixSetFactory(t *testing.T, zapLog *zap.Logger) azure.AddressPrefixSetClientFactory {
	t.Helper()
	factory, err := azure.NewClientFactoryWithDefaultCredential(zapLog, nil)
	if err != nil {
		t.Fatalf("failed to create live Azure client factory: %v", err)
	}
	return factory
}

func waitForPrefixSetIPs(t *testing.T, api azure.AddressPrefixSetAPI, target e2eASGTarget, prefixSetName string, want []string, timeout, interval time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		// Bound the individual Azure call to the remaining budget so a stalled
		// GET cannot run past the outer deadline.
		callCtx, callCancel := context.WithTimeout(context.Background(), remaining)
		ps, err := api.Get(callCtx, target.Parsed.SubscriptionID, target.Parsed.ResourceGroup, target.Parsed.ASGName, prefixSetName)
		callCancel()
		if err == nil && ps != nil && ps.Properties != nil {
			got := ps.Properties.AddressPrefixes
			if ipsMatch(got, want) {
				return
			}
		}
		// Recompute remaining after the call to avoid sleeping past deadline.
		if time.Until(deadline) <= interval {
			break
		}
		time.Sleep(interval)
	}
	t.Fatalf("timed out waiting for prefix set IPs on %s/%s", target.Parsed.ASGName, prefixSetName)
}

func waitForPodsWithIPs(t *testing.T, c client.Client, ns, workload string, replicas int, timeout time.Duration) []string {
	t.Helper()
	ips, _ := waitForPodsWithIPsTimestamped(t, c, ns, workload, replicas, timeout)
	return ips
}

// waitForPodsWithIPsTimestamped returns the pod IPs and a conservative SLO
// start timestamp: the last poll time at which fewer than `replicas` pods had
// IPs. This is guaranteed to be ≤ the true instant all pods obtained IPs,
// because the condition was still unsatisfied at that recorded time. Using this
// as the SLO clock start means the test can never under-report convergence
// time (false pass); it may over-report by up to one poll interval (false
// fail), which is the conservative direction for an acceptance test.
//
// This avoids anchoring the SLO to PodReady LastTransitionTime, which can lag
// IP assignment and cause under-reporting.
func waitForPodsWithIPsTimestamped(t *testing.T, c client.Client, ns, workload string, replicas int, timeout time.Duration) ([]string, time.Time) {
	t.Helper()
	// lastUnderCountTime tracks the most recent poll instant where the
	// replica target was NOT yet met. The true "all pods have IPs" event
	// must have occurred after this instant, so using it as the SLO start
	// is always conservative (≤ true start).
	lastUnderCountTime := time.Now()
	deadline := lastUnderCountTime.Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		// Capture timestamp BEFORE the LIST call. If the LIST shows the target
		// is still unmet, this conservative timestamp guarantees the recorded
		// lower bound precedes any state transition that occurred during the call.
		preCallTime := time.Now()
		// Bound the individual Kubernetes call to remaining budget so a
		// stalled LIST cannot run past the outer deadline.
		callCtx, callCancel := context.WithTimeout(context.Background(), remaining)
		var pods corev1.PodList
		err := c.List(callCtx, &pods,
			client.InNamespace(ns),
			client.MatchingLabels{"app": workload})
		callCancel()
		if err == nil {
			ips := collectReadyIPs(pods.Items)
			if len(ips) >= replicas {
				return ips[:replicas], lastUnderCountTime
			}
			// Successful list confirmed target not yet met — advance lower bound
			// using the pre-call timestamp to stay conservative.
			lastUnderCountTime = preCallTime
		}
		// On list error, do NOT advance lastUnderCountTime — we cannot confirm
		// whether the target was met, so the bound must remain conservative.
		// Recompute remaining after the call to avoid sleeping past deadline.
		if time.Until(deadline) <= 2*time.Second {
			break
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for %d pods with IPs in %s", replicas, ns)
	return nil, time.Time{}
}

// collectReadyIPs returns IPs of pods that the controller considers eligible
// for propagation: any pod with a non-empty PodIP, regardless of phase.
// This matches the controller's desired-state logic (desired_state.go:52).
func collectReadyIPs(pods []corev1.Pod) []string {
	var ips []string
	for _, p := range pods {
		if p.Status.PodIP != "" {
			ips = append(ips, p.Status.PodIP)
		}
	}
	return ips
}

func measureScaleConvergence(t *testing.T, start time.Time, pollFn func() bool, timeout, interval time.Duration) time.Duration {
	t.Helper()
	deadline := start.Add(timeout)
	for {
		if time.Until(deadline) <= 0 {
			break
		}
		if pollFn() {
			return time.Since(start)
		}
		// Recompute remaining after pollFn (which may include API calls)
		// to avoid sleeping past the deadline.
		if time.Until(deadline) <= interval {
			break
		}
		time.Sleep(interval)
	}
	t.Fatalf("scale convergence not achieved within %v", timeout)
	return 0
}

func ipsMatch(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	gotSorted := make([]string, len(got))
	for i, s := range got {
		gotSorted[i] = normalizeToCIDR(s)
	}
	wantSorted := make([]string, len(want))
	for i, s := range want {
		wantSorted[i] = normalizeToCIDR(s)
	}
	sortStrings(gotSorted)
	sortStrings(wantSorted)
	for i := range gotSorted {
		if gotSorted[i] != wantSorted[i] {
			return false
		}
	}
	return true
}

func diffIPs(got, want []string) (missing []string, extra []string) {
	gotCounts := make(map[string]int, len(got))
	for _, s := range got {
		gotCounts[normalizeToCIDR(s)]++
	}
	wantCounts := make(map[string]int, len(want))
	for _, s := range want {
		wantCounts[normalizeToCIDR(s)]++
	}

	for normalizedWant, wantCount := range wantCounts {
		if delta := wantCount - gotCounts[normalizedWant]; delta > 0 {
			for i := 0; i < delta; i++ {
				missing = append(missing, normalizedWant)
			}
		}
	}
	for normalizedGot, gotCount := range gotCounts {
		if delta := gotCount - wantCounts[normalizedGot]; delta > 0 {
			for i := 0; i < delta; i++ {
				extra = append(extra, normalizedGot)
			}
		}
	}

	sortStrings(missing)
	sortStrings(extra)
	return missing, extra
}

// normalizeToCIDR converts a bare IP to CIDR notation (/32 for IPv4, /128 for IPv6).
// If the input already contains a slash it is returned unchanged.
func normalizeToCIDR(ip string) string {
	if strings.Contains(ip, "/") {
		return ip
	}
	if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() == nil {
		return ip + "/128"
	}
	return ip + "/32"
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---------------------------------------------------------------------------
// T9.E1: Full lifecycle — create mapping → deploy pods → verify ASG → delete
// ---------------------------------------------------------------------------

func TestPhase9E2E_T9E1_FullLifecycle_CreateVerifyDelete(t *testing.T) {
	cfg := requireLiveE2EConfig(t)

	zapLog := zaptest.NewLogger(t)
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	scheme := e2eScheme(t)
	restCfg := buildKubeRESTConfig(t, cfg)
	c := buildKubeClient(t, restCfg, scheme)

	requireCRDInstalled(t, c)
	requireExternalControllerReady(t, c, cfg)

	ctx := context.Background()

	// Create test namespace
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	defer func() {
		_ = c.Delete(ctx, ns)
	}()

	target := cfg.LifecycleTargets[0]

	// Create mapping
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.MappingName,
			Namespace: cfg.Namespace,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": cfg.WorkloadName},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: target.ResourceID},
					},
				},
			},
		},
	}
	if err := c.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	// Deploy 3 pods
	replicas := int32(3)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.WorkloadName,
			Namespace: cfg.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": cfg.WorkloadName},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": cfg.WorkloadName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "pause", Image: cfg.FixtureImage},
					},
				},
			},
		},
	}
	if err := c.Create(ctx, deploy); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	// Wait for pods with IPs
	podIPs := waitForPodsWithIPs(t, c, cfg.Namespace, cfg.WorkloadName, 3, cfg.Timeout)

	// Verify IPs in real ASG prefix set
	factory := newLivePrefixSetFactory(t, zapLog)
	api, err := factory.ForSubscription(target.Parsed.SubscriptionID)
	if err != nil {
		t.Fatalf("get API for subscription: %v", err)
	}

	prefixSetName := model.OwnershipKey(cfg.ClusterName, cfg.Namespace, cfg.MappingName)
	waitForPrefixSetIPs(t, api, target, prefixSetName, podIPs, cfg.Timeout, cfg.PollInterval)

	// Delete mapping
	if err := c.Delete(ctx, mapping); err != nil {
		t.Fatalf("delete mapping: %v", err)
	}

	// Verify prefix set is cleaned up
	cleanupDeadline := time.Now().Add(cfg.Timeout)
	for {
		remaining := time.Until(cleanupDeadline)
		if remaining <= 0 {
			break
		}
		callCtx, callCancel := context.WithTimeout(context.Background(), remaining)
		_, getErr := api.Get(callCtx, target.Parsed.SubscriptionID, target.Parsed.ResourceGroup, target.Parsed.ASGName, prefixSetName)
		callCancel()
		if azure.IsNotFound(getErr) {
			return // Success: cleaned up
		}
		// Recompute remaining after the call to avoid sleeping past deadline.
		if time.Until(cleanupDeadline) <= cfg.PollInterval {
			break
		}
		time.Sleep(cfg.PollInterval)
	}
	t.Fatal("prefix set not cleaned up after mapping deletion")
}

// ---------------------------------------------------------------------------
// T9.E2: Cross-subscription ASG update
// ---------------------------------------------------------------------------

func TestPhase9E2E_T9E2_CrossSubscriptionPropagation(t *testing.T) {
	cfg := requireLiveE2EConfig(t)
	cfg.CrossSubTargets = requireCrossSubTargets(t)

	zapLog := zaptest.NewLogger(t)
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	scheme := e2eScheme(t)
	restCfg := buildKubeRESTConfig(t, cfg)
	c := buildKubeClient(t, restCfg, scheme)

	requireCRDInstalled(t, c)
	requireExternalControllerReady(t, c, cfg)

	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace + "-crosssub"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	defer func() {
		_ = c.Delete(ctx, ns)
	}()

	// Build ASG references from cross-sub targets
	asgRefs := make([]v1alpha1.ASGReference, len(cfg.CrossSubTargets))
	for i, tgt := range cfg.CrossSubTargets {
		asgRefs[i] = v1alpha1.ASGReference{ResourceID: tgt.ResourceID}
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-sub-mapping",
			Namespace: ns.Name,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "cross-sub-e2e"},
					},
					ApplicationSecurityGroups: asgRefs,
				},
			},
		},
	}
	if err := c.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	defer func() {
		_ = c.Delete(ctx, mapping)
	}()

	// Deploy pods
	replicas := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-sub-workload",
			Namespace: ns.Name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "cross-sub-e2e"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "cross-sub-e2e"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "pause", Image: cfg.FixtureImage},
					},
				},
			},
		},
	}
	if err := c.Create(ctx, deploy); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	podIPs := waitForPodsWithIPs(t, c, ns.Name, "cross-sub-e2e", 2, cfg.Timeout)

	factory := newLivePrefixSetFactory(t, zapLog)
	prefixSetName := model.OwnershipKey(cfg.ClusterName, ns.Name, "cross-sub-mapping")

	// Verify each cross-subscription target has the IPs
	for _, tgt := range cfg.CrossSubTargets {
		api, err := factory.ForSubscription(tgt.Parsed.SubscriptionID)
		if err != nil {
			t.Fatalf("get API for subscription %s: %v", tgt.Parsed.SubscriptionID, err)
		}
		waitForPrefixSetIPs(t, api, tgt, prefixSetName, podIPs, cfg.Timeout, cfg.PollInterval)
	}
}

// ---------------------------------------------------------------------------
// T9.E3: Scale to 100 pods → converges within SLO (~10s)
// ---------------------------------------------------------------------------

func TestPhase9E2E_T9E3_ScaleTo100Pods_ConvergesWithin10Seconds(t *testing.T) {
	cfg := requireLiveE2EConfig(t)

	zapLog := zaptest.NewLogger(t)
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	scheme := e2eScheme(t)
	restCfg := buildKubeRESTConfig(t, cfg)
	c := buildKubeClient(t, restCfg, scheme)

	requireCRDInstalled(t, c)
	requireExternalControllerReady(t, c, cfg)

	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace + "-scale"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	defer func() {
		_ = c.Delete(ctx, ns)
	}()

	target := cfg.LifecycleTargets[0]

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scale-mapping",
			Namespace: ns.Name,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "scale-e2e"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: target.ResourceID},
					},
				},
			},
		},
	}
	if err := c.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	defer func() {
		_ = c.Delete(ctx, mapping)
	}()

	replicas := int32(cfg.ScaleReplicaGoal)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scale-workload",
			Namespace: ns.Name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "scale-e2e"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "scale-e2e"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "pause", Image: cfg.FixtureImage},
					},
				},
			},
		},
	}
	if err := c.Create(ctx, deploy); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	// Wait for all pods to have IPs, capturing the readiness timestamp for
	// accurate SLO measurement (avoids under-reporting due to poll cadence).
	podIPs, readyAt := waitForPodsWithIPsTimestamped(t, c, ns.Name, "scale-e2e", cfg.ScaleReplicaGoal, cfg.Timeout)

	// Measure convergence time from when pods actually became ready
	start := readyAt

	factory := newLivePrefixSetFactory(t, zapLog)
	api, err := factory.ForSubscription(target.Parsed.SubscriptionID)
	if err != nil {
		t.Fatalf("get API: %v", err)
	}
	prefixSetName := model.OwnershipKey(cfg.ClusterName, ns.Name, "scale-mapping")

	pollDeadline := start.Add(cfg.Timeout)
	elapsed := measureScaleConvergence(t, start, func() bool {
		remaining := time.Until(pollDeadline)
		if remaining <= 0 {
			return false
		}
		callCtx, callCancel := context.WithTimeout(context.Background(), remaining)
		defer callCancel()
		ps, err := api.Get(callCtx, target.Parsed.SubscriptionID, target.Parsed.ResourceGroup, target.Parsed.ASGName, prefixSetName)
		if err != nil || ps == nil || ps.Properties == nil {
			return false
		}
		return ipsMatch(ps.Properties.AddressPrefixes, podIPs)
	}, cfg.Timeout, cfg.PollInterval)

	zapLog.Info("scale convergence measured",
		zap.Duration("elapsed", elapsed),
		zap.Duration("slo", cfg.ScaleSLO),
		zap.Int("expectedIPCount", cfg.ScaleReplicaGoal),
	)

	if elapsed > cfg.ScaleSLO {
		// Gather diagnostics
		diagCtx, diagCancel := context.WithTimeout(context.Background(), 10*time.Second)
		ps, _ := api.Get(diagCtx, target.Parsed.SubscriptionID, target.Parsed.ResourceGroup, target.Parsed.ASGName, prefixSetName)
		diagCancel()
		var observedIPs []string
		observedCount := 0
		if ps != nil && ps.Properties != nil {
			observedIPs = append(observedIPs, ps.Properties.AddressPrefixes...)
			observedCount = len(observedIPs)
		}
		missingIPs, extraIPs := diffIPs(observedIPs, podIPs)
		t.Fatalf("SLO violated: convergence took %v (SLO=%v), expected=%d observed=%d missingIPs=%v extraIPs=%v",
			elapsed, cfg.ScaleSLO, cfg.ScaleReplicaGoal, observedCount, missingIPs, extraIPs)
	}
}
