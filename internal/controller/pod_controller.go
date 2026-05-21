package controller

import (
	"context"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/config"
)

// PodReconciler reconciles Pod objects and manages their ASG memberships.
type PodReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	ASGClient *azure.ASGClient
	NICClient *azure.NICClient
	Config    *config.Config
}

// Reconcile handles pod create/update/delete events and manages ASG memberships.
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("pod", req.NamespacedName)

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("pod deleted; ASG memberships are not cleaned up automatically yet")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, errors.Wrap(err, "fetching pod")
	}

	if pod.Status.PodIP == "" {
		logger.V(1).Info("pod has no IP yet, requeuing")
		return ctrl.Result{Requeue: true}, nil
	}

	desiredASGs := getDesiredASGs(&pod)
	if len(desiredASGs) == 0 {
		logger.V(1).Info("no ASG annotations or labels found, skipping")
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling ASG memberships", "desiredASGs", desiredASGs, "podIP", pod.Status.PodIP)

	// Resolve ASG resource IDs from names.
	asgRefs, err := r.resolveASGs(ctx, desiredASGs, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(asgRefs) == 0 {
		logger.Info("no valid ASGs resolved, skipping")
		return ctrl.Result{}, nil
	}

	// TODO: Resolve the pod's NIC from node/pod metadata and update ASG associations.
	// This requires mapping pod IP → node → Azure VM/VMSS → NIC, which is
	// implemented as part of the NIC resolution feature.
	_ = asgRefs

	return ctrl.Result{}, nil
}

// getDesiredASGs reads the desired ASG names from pod labels and annotations.
func getDesiredASGs(pod *corev1.Pod) []string {
	var asgs []string

	// Check the single-ASG label.
	if asg, ok := pod.Labels[config.LabelASG]; ok && asg != "" {
		asgs = append(asgs, asg)
	}

	// Check the multi-ASG annotation (comma-separated).
	if annotation, ok := pod.Annotations[config.AnnotationASGs]; ok && annotation != "" {
		for _, asg := range strings.Split(annotation, ",") {
			asg = strings.TrimSpace(asg)
			if asg != "" {
				asgs = append(asgs, asg)
			}
		}
	}

	// Deduplicate.
	return deduplicate(asgs)
}

// resolveASGs looks up ASG resource IDs from their names.
func (r *PodReconciler) resolveASGs(ctx context.Context, asgNames []string, logger logr.Logger) ([]*armnetwork.ApplicationSecurityGroup, error) {
	var resolved []*armnetwork.ApplicationSecurityGroup

	for _, name := range asgNames {
		asg, err := r.ASGClient.Get(ctx, name)
		if err != nil {
			logger.Error(err, "ASG not found, skipping", "asg", name)
			continue
		}
		resolved = append(resolved, asg)
	}

	return resolved, nil
}

// SetupWithManager registers the controller with the manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}

// deduplicate removes duplicate strings from a slice.
func deduplicate(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		if _, ok := seen[item]; !ok {
			seen[item] = struct{}{}
			result = append(result, item)
		}
	}
	return result
}
