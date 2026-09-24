#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CONTROLLER_NAMESPACE="${CONTROLLER_NAMESPACE:-pod-nsg-controller-system}"
CONTROLLER_DEPLOYMENT="${CONTROLLER_DEPLOYMENT:-pod-nsg-controller}"
CANDIDATE_IMAGE="${CANDIDATE_IMAGE:?CANDIDATE_IMAGE is required}"

required=(
  CLUSTER_A_KUBECONFIG CLUSTER_A_NAME
  CLUSTER_B_KUBECONFIG CLUSTER_B_NAME
)
for name in "${required[@]}"; do
  if [[ -z "${!name:-}" ]]; then
    echo "${name} is required" >&2
    exit 1
  fi
done

deploy_cluster() {
  local kubeconfig="$1"
  local cluster_name="$2"
  local subscription_id="$3"
  local resource_group="$4"

  kubectl --kubeconfig "$kubeconfig" apply -f "${REPO_ROOT}/config/crd/"
  kubectl --kubeconfig "$kubeconfig" create namespace "$CONTROLLER_NAMESPACE" \
    --dry-run=client -o yaml | kubectl --kubeconfig "$kubeconfig" apply -f -
  kubectl --kubeconfig "$kubeconfig" apply -f "${REPO_ROOT}/config/rbac/"

  kubectl --kubeconfig "$kubeconfig" -n "$CONTROLLER_NAMESPACE" create secret generic pod-nsg-controller-azure \
    --from-literal=subscription-id="${subscription_id:-unused}" \
    --from-literal=resource-group="${resource_group:-unused}" \
    --from-literal=nsg-name=unused \
    --dry-run=client -o yaml | kubectl --kubeconfig "$kubeconfig" apply -f -

  kubectl --kubeconfig "$kubeconfig" apply -f "${REPO_ROOT}/config/manager/manager.yaml"
  kubectl --kubeconfig "$kubeconfig" -n "$CONTROLLER_NAMESPACE" \
    set image "deployment/${CONTROLLER_DEPLOYMENT}" "manager=${CANDIDATE_IMAGE}"
  kubectl --kubeconfig "$kubeconfig" -n "$CONTROLLER_NAMESPACE" \
    set env "deployment/${CONTROLLER_DEPLOYMENT}" "CLUSTER_NAME=${cluster_name}"
  kubectl --kubeconfig "$kubeconfig" -n "$CONTROLLER_NAMESPACE" patch \
    "deployment/${CONTROLLER_DEPLOYMENT}" --type=strategic \
    -p '{"spec":{"template":{"spec":{"containers":[{"name":"manager","imagePullPolicy":"Always"}]}}}}'

  if [[ -n "${REGISTRY_SERVER:-}" && -n "${REGISTRY_USERNAME:-}" && -n "${REGISTRY_PASSWORD:-}" ]]; then
    kubectl --kubeconfig "$kubeconfig" -n "$CONTROLLER_NAMESPACE" create secret docker-registry release-registry \
      --docker-server="$REGISTRY_SERVER" \
      --docker-username="$REGISTRY_USERNAME" \
      --docker-password="$REGISTRY_PASSWORD" \
      --dry-run=client -o yaml | kubectl --kubeconfig "$kubeconfig" apply -f -
    kubectl --kubeconfig "$kubeconfig" -n "$CONTROLLER_NAMESPACE" patch serviceaccount pod-nsg-controller \
      --type=merge -p '{"imagePullSecrets":[{"name":"release-registry"}]}'
  fi

  kubectl --kubeconfig "$kubeconfig" -n "$CONTROLLER_NAMESPACE" rollout status \
    "deployment/${CONTROLLER_DEPLOYMENT}" --timeout=5m

  local deployed_image
  deployed_image="$(kubectl --kubeconfig "$kubeconfig" -n "$CONTROLLER_NAMESPACE" get \
    "deployment/${CONTROLLER_DEPLOYMENT}" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}')"
  if [[ "$deployed_image" != "$CANDIDATE_IMAGE" ]]; then
    echo "deployed image mismatch: got ${deployed_image}, want ${CANDIDATE_IMAGE}" >&2
    exit 1
  fi
}

deploy_cluster \
  "$CLUSTER_A_KUBECONFIG" \
  "$CLUSTER_A_NAME" \
  "${CLUSTER_A_SUBSCRIPTION_ID:-}" \
  "${CLUSTER_A_RESOURCE_GROUP:-}"

deploy_cluster \
  "$CLUSTER_B_KUBECONFIG" \
  "$CLUSTER_B_NAME" \
  "${CLUSTER_B_SUBSCRIPTION_ID:-}" \
  "${CLUSTER_B_RESOURCE_GROUP:-}"
