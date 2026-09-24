//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/controller"
	"github.com/Azure/pod-nsg-controller/internal/model"
	"go.uber.org/zap/zaptest"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	multiClusterBackendMapping   = "backend-asg-mapping"
	multiClusterFrontendMapping  = "frontend-asg-mapping"
	multiClusterBackendWorkload  = "backend"
	multiClusterFrontendWorkload = "frontend"
	releaseControllerNamespace   = "pod-nsg-controller-system"
	releaseControllerDeployment  = "pod-nsg-controller"
	releaseControllerContainer   = "manager"
)

type multiClusterConfig struct {
	Namespace    string
	FixtureImage string
	Timeout      time.Duration
	PollInterval time.Duration
	Targets      [2]e2eASGTarget
	Clusters     [2]multiClusterEndpoint
}

type multiClusterEndpoint struct {
	Name                 string
	KubeconfigPath       string
	KubeContext          string
	ControllerNamespace  string
	ControllerDeployment string
	ControllerContainer  string
}

type multiClusterRuntime struct {
	config    multiClusterEndpoint
	client    client.Client
	clientset kubernetes.Interface
}

type multiClusterSuite struct {
	t          *testing.T
	config     multiClusterConfig
	clusters   [2]multiClusterRuntime
	apis       map[string]azure.AddressPrefixSetAPI
	logStarted metav1.Time
}

func TestMultiClusterE2E(t *testing.T) {
	cfg := requireMultiClusterConfig(t)
	suite := newMultiClusterSuite(t, cfg)
	t.Cleanup(suite.cleanup)
	suite.setup()

	t.Run("single-cluster-scale-up", suite.testSingleClusterScaleUp)
	t.Run("concurrent-two-cluster-writes", suite.testConcurrentWrites)
	t.Run("scale-down-and-cleanup", suite.testScaleDownAndCleanup)
	t.Run("parallel-scale-up-35-pods", suite.testParallelScaleUp)
}

func requireMultiClusterConfig(t *testing.T) multiClusterConfig {
	t.Helper()
	requiredEnv := []string{
		"CLUSTER_A_KUBECONFIG",
		"CLUSTER_A_NAME",
		"CLUSTER_B_KUBECONFIG",
		"CLUSTER_B_NAME",
		"RELEASE_CLUSTER_A_ASG_RESOURCE_IDS",
		"RELEASE_CLUSTER_B_ASG_RESOURCE_IDS",
	}
	configured := false
	for _, key := range requiredEnv {
		if strings.TrimSpace(envOrDefault(key, "")) != "" {
			configured = true
			break
		}
	}
	if !configured {
		t.Skip("release multi-cluster environment is not configured")
	}

	clusterTargetIDs := [2][]string{
		splitRequiredEnv(t, "RELEASE_CLUSTER_A_ASG_RESOURCE_IDS", 2),
		splitRequiredEnv(t, "RELEASE_CLUSTER_B_ASG_RESOURCE_IDS", 2),
	}
	for i, workload := range []string{"backend", "frontend"} {
		if !strings.EqualFold(clusterTargetIDs[0][i], clusterTargetIDs[1][i]) {
			t.Fatalf(
				"cluster A and cluster B %s ASG resource IDs must match to test concurrent writes to shared ASGs",
				workload,
			)
		}
	}

	var targets [2]e2eASGTarget
	for i, resourceID := range clusterTargetIDs[0] {
		parsed, err := model.ParseASGResourceID(resourceID)
		if err != nil {
			t.Fatalf("invalid RELEASE_CLUSTER_A_ASG_RESOURCE_IDS entry %q: %v", resourceID, err)
		}
		targets[i] = e2eASGTarget{ResourceID: resourceID, Parsed: parsed}
	}

	if strings.EqualFold(targets[0].ResourceID, targets[1].ResourceID) {
		t.Fatal("release ASG resource ID lists must contain distinct backend and frontend ASGs")
	}

	cfg := multiClusterConfig{
		Namespace:    fmt.Sprintf("pod-nsg-mc-%d", time.Now().UnixNano()),
		FixtureImage: "registry.k8s.io/pause:3.9",
		Timeout:      5 * time.Minute,
		PollInterval: time.Second,
		Targets:      targets,
		Clusters: [2]multiClusterEndpoint{
			requireMultiClusterEndpoint(t, "A"),
			requireMultiClusterEndpoint(t, "B"),
		},
	}
	if strings.EqualFold(cfg.Clusters[0].Name, cfg.Clusters[1].Name) {
		t.Fatal("CLUSTER_A_NAME and CLUSTER_B_NAME must be distinct")
	}
	return cfg
}

func requireMultiClusterEndpoint(t *testing.T, cluster string) multiClusterEndpoint {
	t.Helper()
	base := "CLUSTER_" + cluster + "_"
	return multiClusterEndpoint{
		Name:                 envRequired(t, base+"NAME"),
		KubeconfigPath:       envRequired(t, base+"KUBECONFIG"),
		ControllerNamespace:  releaseControllerNamespace,
		ControllerDeployment: releaseControllerDeployment,
		ControllerContainer:  releaseControllerContainer,
	}
}

func envRequired(t *testing.T, key string) string {
	t.Helper()
	value := strings.TrimSpace(envOrDefault(key, ""))
	if value == "" {
		t.Fatalf("%s is required", key)
	}
	return value
}

func splitRequiredEnv(t *testing.T, key string, count int) []string {
	t.Helper()
	parts := strings.Split(envRequired(t, key), ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			values = append(values, value)
		}
	}
	if len(values) != count {
		t.Fatalf("%s must contain exactly %d comma-separated values, got %d", key, count, len(values))
	}
	return values
}

func newMultiClusterSuite(t *testing.T, cfg multiClusterConfig) *multiClusterSuite {
	t.Helper()
	scheme := e2eScheme(t)
	suite := &multiClusterSuite{
		t:          t,
		config:     cfg,
		apis:       make(map[string]azure.AddressPrefixSetAPI),
		logStarted: metav1.Now(),
	}

	for i, clusterCfg := range cfg.Clusters {
		restCfg := buildKubeRESTConfig(t, liveE2EConfig{
			KubeconfigPath: clusterCfg.KubeconfigPath,
			KubeContext:    clusterCfg.KubeContext,
		})
		clientset, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			t.Fatalf("build Kubernetes clientset for %s: %v", clusterCfg.Name, err)
		}
		suite.clusters[i] = multiClusterRuntime{
			config:    clusterCfg,
			client:    buildKubeClient(t, restCfg, scheme),
			clientset: clientset,
		}
	}

	zapLog := zaptest.NewLogger(t)
	factory := newLivePrefixSetFactory(t, zapLog)
	for _, target := range cfg.Targets {
		subscriptionID := strings.ToLower(target.Parsed.SubscriptionID)
		if _, exists := suite.apis[subscriptionID]; exists {
			continue
		}
		api, err := factory.ForSubscription(target.Parsed.SubscriptionID)
		if err != nil {
			t.Fatalf("create Azure client for subscription %s: %v", target.Parsed.SubscriptionID, err)
		}
		suite.apis[subscriptionID] = api
	}
	return suite
}

func (s *multiClusterSuite) setup() {
	s.t.Helper()
	for i := range s.clusters {
		cluster := &s.clusters[i]
		requireCRDInstalled(s.t, cluster.client)
		requireExternalControllerReady(s.t, cluster.client, liveE2EConfig{
			ControllerNamespace:  cluster.config.ControllerNamespace,
			ControllerDeployment: cluster.config.ControllerDeployment,
		})

		ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.config.Namespace}}
		err := cluster.client.Create(ctx, namespace)
		cancel()
		if err != nil {
			s.t.Fatalf("create namespace %s in %s: %v", s.config.Namespace, cluster.config.Name, err)
		}

		s.createMapping(cluster, multiClusterBackendMapping, multiClusterBackendWorkload, s.config.Targets[0])
		s.createMapping(cluster, multiClusterFrontendMapping, multiClusterFrontendWorkload, s.config.Targets[1])
		s.createDeployment(cluster, multiClusterBackendWorkload)
		s.createDeployment(cluster, multiClusterFrontendWorkload)
	}
}

func (s *multiClusterSuite) createMapping(cluster *multiClusterRuntime, name, workload string, target e2eASGTarget) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
	defer cancel()
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.config.Namespace},
		Spec: v1alpha1.PodASGMappingSpec{Mappings: []v1alpha1.Mapping{{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": workload}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{
				ResourceID: target.ResourceID,
			}},
		}}},
	}
	if err := cluster.client.Create(ctx, mapping); err != nil {
		s.t.Fatalf("create mapping %s in %s: %v", name, cluster.config.Name, err)
	}
}

func (s *multiClusterSuite) createDeployment(cluster *multiClusterRuntime, workload string) {
	s.t.Helper()
	replicas := int32(0)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: workload, Namespace: s.config.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": workload}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": workload}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "fixture",
					Image: s.config.FixtureImage,
				}}},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
	defer cancel()
	if err := cluster.client.Create(ctx, deployment); err != nil {
		s.t.Fatalf("create deployment %s in %s: %v", workload, cluster.config.Name, err)
	}
}

func (s *multiClusterSuite) testSingleClusterScaleUp(t *testing.T) {
	s.scale(t, &s.clusters[0], multiClusterBackendWorkload, 5)
	s.scale(t, &s.clusters[0], multiClusterFrontendWorkload, 5)
	s.verifyCluster(t, &s.clusters[0], 5, 5)
	s.assertNo412Errors(t)
}

func (s *multiClusterSuite) testConcurrentWrites(t *testing.T) {
	s.scale(t, &s.clusters[0], multiClusterBackendWorkload, 2)
	s.scale(t, &s.clusters[0], multiClusterFrontendWorkload, 2)
	s.verifyCluster(t, &s.clusters[0], 2, 2)

	s.scaleParallel(t, []scaleRequest{
		{cluster: &s.clusters[0], workload: multiClusterBackendWorkload, replicas: 5},
		{cluster: &s.clusters[0], workload: multiClusterFrontendWorkload, replicas: 5},
		{cluster: &s.clusters[1], workload: multiClusterBackendWorkload, replicas: 2},
		{cluster: &s.clusters[1], workload: multiClusterFrontendWorkload, replicas: 2},
	})
	s.verifyCluster(t, &s.clusters[0], 5, 5)
	s.verifyCluster(t, &s.clusters[1], 2, 2)
	s.verifyDistinctOwnedPrefixSets(t)
	s.assertNo412Errors(t)
}

func (s *multiClusterSuite) testScaleDownAndCleanup(t *testing.T) {
	s.scaleParallel(t, []scaleRequest{
		{cluster: &s.clusters[0], workload: multiClusterBackendWorkload, replicas: 2},
		{cluster: &s.clusters[0], workload: multiClusterFrontendWorkload, replicas: 2},
		{cluster: &s.clusters[1], workload: multiClusterBackendWorkload, replicas: 0},
		{cluster: &s.clusters[1], workload: multiClusterFrontendWorkload, replicas: 0},
	})
	s.verifyCluster(t, &s.clusters[0], 2, 2)
	s.verifyCluster(t, &s.clusters[1], 0, 0)
	s.deleteMappings(t, &s.clusters[1])
	s.waitForClusterPrefixSetsDeleted(t, s.clusters[1].config.Name)
	s.assertNo412Errors(t)
}

func (s *multiClusterSuite) testParallelScaleUp(t *testing.T) {
	s.createMapping(&s.clusters[1], multiClusterBackendMapping, multiClusterBackendWorkload, s.config.Targets[0])
	s.createMapping(&s.clusters[1], multiClusterFrontendMapping, multiClusterFrontendWorkload, s.config.Targets[1])

	s.scaleParallel(t, []scaleRequest{
		{cluster: &s.clusters[0], workload: multiClusterBackendWorkload, replicas: 13},
		{cluster: &s.clusters[0], workload: multiClusterFrontendWorkload, replicas: 12},
		{cluster: &s.clusters[1], workload: multiClusterBackendWorkload, replicas: 5},
		{cluster: &s.clusters[1], workload: multiClusterFrontendWorkload, replicas: 5},
	})
	s.verifyCluster(t, &s.clusters[0], 13, 12)
	s.verifyCluster(t, &s.clusters[1], 5, 5)
	s.verifyDistinctOwnedPrefixSets(t)
	s.assertNo412Errors(t)
}

type scaleRequest struct {
	cluster  *multiClusterRuntime
	workload string
	replicas int
}

func (s *multiClusterSuite) scale(t *testing.T, cluster *multiClusterRuntime, workload string, replicas int) {
	t.Helper()
	if err := s.updateScale(cluster, workload, replicas); err != nil {
		t.Fatalf("scale %s/%s to %d: %v", cluster.config.Name, workload, replicas, err)
	}
}

func (s *multiClusterSuite) scaleParallel(t *testing.T, requests []scaleRequest) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, len(requests))
	for _, request := range requests {
		request := request
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.updateScale(request.cluster, request.workload, request.replicas); err != nil {
				errs <- fmt.Errorf("scale %s/%s to %d: %w",
					request.cluster.config.Name, request.workload, request.replicas, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
}

func (s *multiClusterSuite) updateScale(cluster *multiClusterRuntime, workload string, replicas int) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
	defer cancel()
	key := types.NamespacedName{Namespace: s.config.Namespace, Name: workload}
	var deployment appsv1.Deployment
	if err := cluster.client.Get(ctx, key, &deployment); err != nil {
		return err
	}
	value := int32(replicas)
	deployment.Spec.Replicas = &value
	return cluster.client.Update(ctx, &deployment)
}

func (s *multiClusterSuite) verifyCluster(t *testing.T, cluster *multiClusterRuntime, backendReplicas, frontendReplicas int) {
	t.Helper()
	s.verifyWorkload(t, cluster, multiClusterBackendMapping, multiClusterBackendWorkload, s.config.Targets[0], backendReplicas)
	s.verifyWorkload(t, cluster, multiClusterFrontendMapping, multiClusterFrontendWorkload, s.config.Targets[1], frontendReplicas)
}

func (s *multiClusterSuite) verifyWorkload(
	t *testing.T,
	cluster *multiClusterRuntime,
	mappingName string,
	workload string,
	target e2eASGTarget,
	replicas int,
) {
	t.Helper()
	podIPs := s.waitForWorkloadIPsExact(t, cluster, workload, replicas)
	s.waitForMappingSynced(t, cluster, mappingName, replicas)

	prefixSetName := model.OwnershipKey(cluster.config.Name, s.config.Namespace, mappingName)
	api := s.apiForTarget(t, target)
	waitForPrefixSetIPs(t, api, target, prefixSetName, podIPs, s.config.Timeout, s.config.PollInterval)
	s.waitForProvisioningSucceeded(t, api, target, prefixSetName)
}

func (s *multiClusterSuite) waitForWorkloadIPsExact(
	t *testing.T,
	cluster *multiClusterRuntime,
	workload string,
	replicas int,
) []string {
	t.Helper()
	deadline := time.Now().Add(s.config.Timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		var pods corev1.PodList
		err := cluster.client.List(ctx, &pods,
			client.InNamespace(s.config.Namespace),
			client.MatchingLabels{"app": workload})
		cancel()
		if err == nil {
			ips := make([]string, 0, replicas)
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.DeletionTimestamp == nil && pod.Status.PodIP != "" {
					ips = append(ips, pod.Status.PodIP)
				}
			}
			if len(ips) == replicas {
				sortStrings(ips)
				return ips
			}
		}
		time.Sleep(s.config.PollInterval)
	}
	t.Fatalf("workload %s/%s in %s did not reach exactly %d active pod IPs",
		s.config.Namespace, workload, cluster.config.Name, replicas)
	return nil
}

func (s *multiClusterSuite) waitForMappingSynced(t *testing.T, cluster *multiClusterRuntime, mappingName string, matchedPods int) {
	t.Helper()
	deadline := time.Now().Add(s.config.Timeout)
	key := types.NamespacedName{Namespace: s.config.Namespace, Name: mappingName}
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		var mapping v1alpha1.PodASGMapping
		err := cluster.client.Get(ctx, key, &mapping)
		cancel()
		if err == nil && len(mapping.Status.MappingStatuses) == 1 {
			status := mapping.Status.MappingStatuses[0]
			if status.ASGSyncState == controller.SyncStateSynced && status.MatchedPods == matchedPods {
				return
			}
		}
		time.Sleep(s.config.PollInterval)
	}
	t.Fatalf("mapping %s/%s in %s did not reach Synced with matchedPods=%d",
		s.config.Namespace, mappingName, cluster.config.Name, matchedPods)
}

func (s *multiClusterSuite) waitForProvisioningSucceeded(
	t *testing.T,
	api azure.AddressPrefixSetAPI,
	target e2eASGTarget,
	prefixSetName string,
) {
	t.Helper()
	deadline := time.Now().Add(s.config.Timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		prefixSet, err := api.Get(ctx, target.Parsed.SubscriptionID, target.Parsed.ResourceGroup, target.Parsed.ASGName, prefixSetName)
		cancel()
		if err == nil && prefixSet != nil && prefixSet.Properties != nil &&
			prefixSet.Properties.ProvisioningState != nil &&
			strings.EqualFold(*prefixSet.Properties.ProvisioningState, "Succeeded") {
			return
		}
		time.Sleep(s.config.PollInterval)
	}
	t.Fatalf("prefix set %s on %s did not reach provisioningState=Succeeded", prefixSetName, target.Parsed.ASGName)
}

func (s *multiClusterSuite) verifyDistinctOwnedPrefixSets(t *testing.T) {
	t.Helper()
	for _, mappingTarget := range []struct {
		mapping string
		target  e2eASGTarget
	}{
		{mapping: multiClusterBackendMapping, target: s.config.Targets[0]},
		{mapping: multiClusterFrontendMapping, target: s.config.Targets[1]},
	} {
		want := map[string]struct{}{
			model.OwnershipKey(s.clusters[0].config.Name, s.config.Namespace, mappingTarget.mapping): {},
			model.OwnershipKey(s.clusters[1].config.Name, s.config.Namespace, mappingTarget.mapping): {},
		}
		api := s.apiForTarget(t, mappingTarget.target)
		ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
		prefixSets, err := api.List(ctx,
			mappingTarget.target.Parsed.SubscriptionID,
			mappingTarget.target.Parsed.ResourceGroup,
			mappingTarget.target.Parsed.ASGName)
		cancel()
		if err != nil {
			t.Fatalf("list prefix sets for %s: %v", mappingTarget.target.Parsed.ASGName, err)
		}
		for _, prefixSet := range prefixSets {
			if prefixSet.Name != nil {
				delete(want, *prefixSet.Name)
			}
		}
		if len(want) != 0 {
			t.Fatalf("ASG %s is missing owned prefix sets: %v", mappingTarget.target.Parsed.ASGName, mapKeys(want))
		}
	}
}

func (s *multiClusterSuite) deleteMappings(t *testing.T, cluster *multiClusterRuntime) {
	t.Helper()
	for _, name := range []string{multiClusterBackendMapping, multiClusterFrontendMapping} {
		ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
		mapping := &v1alpha1.PodASGMapping{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.config.Namespace}}
		err := cluster.client.Delete(ctx, mapping)
		cancel()
		if err != nil && client.IgnoreNotFound(err) != nil {
			t.Fatalf("delete mapping %s in %s: %v", name, cluster.config.Name, err)
		}
	}
}

func (s *multiClusterSuite) waitForClusterPrefixSetsDeleted(t *testing.T, clusterName string) {
	t.Helper()
	if err := s.waitForClusterPrefixSetsDeletedError(clusterName); err != nil {
		t.Fatal(err)
	}
}

func (s *multiClusterSuite) waitForClusterPrefixSetsDeletedError(clusterName string) error {
	for i, mapping := range []string{multiClusterBackendMapping, multiClusterFrontendMapping} {
		target := s.config.Targets[i]
		prefixSetName := model.OwnershipKey(clusterName, s.config.Namespace, mapping)
		api := s.apis[strings.ToLower(target.Parsed.SubscriptionID)]
		if api == nil {
			return fmt.Errorf("no Azure client for subscription %s", target.Parsed.SubscriptionID)
		}
		deadline := time.Now().Add(s.config.Timeout)
		deleted := false
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
			_, err := api.Get(ctx, target.Parsed.SubscriptionID, target.Parsed.ResourceGroup, target.Parsed.ASGName, prefixSetName)
			cancel()
			if azure.IsNotFound(err) {
				deleted = true
				break
			}
			time.Sleep(s.config.PollInterval)
		}
		if !deleted {
			return fmt.Errorf("prefix set %s was not deleted", prefixSetName)
		}
	}
	return nil
}

func (s *multiClusterSuite) assertNo412Errors(t *testing.T) {
	t.Helper()
	for i := range s.clusters {
		cluster := &s.clusters[i]
		ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
		var deployment appsv1.Deployment
		key := types.NamespacedName{
			Namespace: cluster.config.ControllerNamespace,
			Name:      cluster.config.ControllerDeployment,
		}
		if err := cluster.client.Get(ctx, key, &deployment); err != nil {
			cancel()
			t.Fatalf("get controller deployment in %s: %v", cluster.config.Name, err)
		}
		selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
		if err != nil {
			cancel()
			t.Fatalf("controller selector in %s: %v", cluster.config.Name, err)
		}
		pods, err := cluster.clientset.CoreV1().Pods(cluster.config.ControllerNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
		if err != nil {
			cancel()
			t.Fatalf("list controller pods in %s: %v", cluster.config.Name, err)
		}
		for _, pod := range pods.Items {
			logs, err := cluster.clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				Container: cluster.config.ControllerContainer,
				SinceTime: &s.logStarted,
			}).DoRaw(ctx)
			if err != nil {
				cancel()
				t.Fatalf("read controller logs for %s/%s: %v", cluster.config.Name, pod.Name, err)
			}
			lower := strings.ToLower(string(logs))
			if strings.Contains(lower, "preconditionfailed") ||
				strings.Contains(lower, `"statuscode":412`) ||
				strings.Contains(lower, `"statuscode": 412`) ||
				strings.Contains(lower, `"status":412`) ||
				strings.Contains(lower, `"status": 412`) ||
				strings.Contains(lower, "status=412") {
				cancel()
				t.Fatalf("controller %s/%s logged an ARM 412 error since test start", cluster.config.Name, pod.Name)
			}
		}
		cancel()
	}
}

func (s *multiClusterSuite) apiForTarget(t *testing.T, target e2eASGTarget) azure.AddressPrefixSetAPI {
	t.Helper()
	api := s.apis[strings.ToLower(target.Parsed.SubscriptionID)]
	if api == nil {
		t.Fatalf("no Azure client for subscription %s", target.Parsed.SubscriptionID)
	}
	return api
}

func (s *multiClusterSuite) cleanup() {
	for i := range s.clusters {
		cluster := &s.clusters[i]
		for _, workload := range []string{multiClusterBackendWorkload, multiClusterFrontendWorkload} {
			_ = s.updateScale(cluster, workload, 0)
		}
		for _, name := range []string{multiClusterBackendMapping, multiClusterFrontendMapping} {
			ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
			mapping := &v1alpha1.PodASGMapping{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.config.Namespace},
			}
			err := cluster.client.Delete(ctx, mapping)
			cancel()
			if err != nil && client.IgnoreNotFound(err) != nil {
				s.t.Errorf("cleanup mapping %s in %s: %v", name, cluster.config.Name, err)
			}
		}
	}
	for i := range s.clusters {
		if err := s.waitForClusterPrefixSetsDeletedError(s.clusters[i].config.Name); err != nil {
			s.t.Errorf("cleanup prefix sets for %s: %v", s.clusters[i].config.Name, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.config.Namespace}}
		if err := s.clusters[i].client.Delete(ctx, namespace); err != nil && client.IgnoreNotFound(err) != nil {
			s.t.Errorf("delete namespace %s from %s: %v", s.config.Namespace, s.clusters[i].config.Name, err)
		}
		cancel()
	}
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}
