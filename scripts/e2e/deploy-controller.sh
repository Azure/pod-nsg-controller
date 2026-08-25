#!/usr/bin/env bash
# =============================================================================
# deploy-controller.sh - deploy the candidate controller to ONE self-managed
# cluster and apply the documented PodASGMappings + workloads, then establish
# the baseline (EPIC-004 / ITEM-011, ITEM-012 / FILE-018).
#
# Runner->API-server access (resolving the issue surfaced by EPIC-003): the
# provisioning NSG admits 6443 from `VirtualNetwork` only and the retrieved
# kubeconfig points at the control-plane PUBLIC IP, so the GitHub runner has NO
# path to the cluster API server. The simplest secure pattern - reusing the ONLY
# node-access path already proven in provisioning, adding no ingress - is to run
# `kubectl` ON the control-plane node through `az vm run-command invoke`
# (lib.sh azrun::*). ARM/ACR calls run on the runner. Every Azure call carries
# an explicit --subscription; there is NO `az account set` (RD-020).
#
# ITEM-011: mint a short-lived, repository-scoped ACR token, write it as a
#   Kubernetes imagePullSecret, and apply CRD + RBAC + manager with the image
#   overridden to the candidate DIGEST (SEC-004 / RD-004 / Section 3.5). The
#   controller runs on the control-plane node (nodeSelector/toleration, REQ-003).
# ITEM-012: apply the `backend-asg-mapping`/`frontend-asg-mapping` PodASGMappings
#   pointing at the shared ASGs in the PRIMARY run RG, plus the documented
#   Cluster A (Deployments, ns `test-apps`, label `role`) vs Cluster B
#   (standalone pods, ns `default`, label `app`) asymmetry (CON-003), and
#   establish the baseline (A=4 running pods, B=0).
#
# CLUSTER_NAME is set to the cluster's own resource group so the controller's
# ownership key OwnershipKey(clusterName, ns, mapping) == the documented
# address-prefix-set name normalize(clusterRG)-<ns>-<mapping> (Section 3.4.4).
#
# All resource names come from naming.sh; subscription IDs are supplied
# explicitly and are NOT name inputs (Section 3.4.1). Both clusters' controllers
# target the SAME shared ASGs in the primary run RG (FR-028).
#
# Usage: deploy-controller.sh <all|render|deploy|baseline|record|names>
# Required env: TOPOLOGY (ss|xs), REGION (canary), PRIMARY_SUBSCRIPTION_ID,
#   IMAGE_REFERENCE (candidate @sha256 digest ref); deploy also needs STAGING_ACR.
# Tool seam (hermetic tests): AZ_BIN (az).
#
# Traceability: ITEM-011, ITEM-012, FR-004, FR-011, FR-028, REQ-001, REQ-003,
# SEC-004, RD-004, RD-012, RD-020, CON-003, NFR-003, NFR-012, PRD 3.3/3.4/3.5.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"

# ---- inputs / configuration (all overridable; explicit context required) ----
TOPOLOGY="${TOPOLOGY:-}"                                   # ss|xs      (REQUIRED)
REGION="${REGION:-}"                                       # canary     (REQUIRED)
PRIMARY_SUBSCRIPTION_ID="${PRIMARY_SUBSCRIPTION_ID:-}"     # ASG + Cluster A owner (REQUIRED)
SECONDARY_SUBSCRIPTION_ID="${SECONDARY_SUBSCRIPTION_ID:-}" # Cluster B owner in xs (default: primary)
IMAGE_REFERENCE="${IMAGE_REFERENCE:-}"                     # candidate @sha256 ref (REQUIRED for render/deploy)
STAGING_ACR="${STAGING_ACR:-}"                            # login server (REQUIRED for deploy)
STAGING_ACR_SUBSCRIPTION_ID="${STAGING_ACR_SUBSCRIPTION_ID:-}"  # ACR sub (default: primary)
CONTROLLER_STAGING_REPO="${CONTROLLER_STAGING_REPO:-candidate/pod-nsg-controller}"
CONTROLLER_NAMESPACE="${CONTROLLER_NAMESPACE:-pod-nsg-controller-system}"
SHARED_ASGS="${SHARED_ASGS:-asg-backend asg-frontend}"
FIXTURE_IMAGE="${FIXTURE_IMAGE:-registry.k8s.io/pause:3.9}"
BASELINE_BACKEND="${BASELINE_BACKEND:-2}"                 # Cluster A backend Deployment replicas
BASELINE_FRONTEND="${BASELINE_FRONTEND:-2}"              # Cluster A frontend Deployment replicas
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
RENDER_DIR="${RENDER_DIR:-${HOME}/.config/pnc-e2e}"
AZ_BIN="${AZ_BIN:-az}"
KUBECONFIG_ON_NODE="${KUBECONFIG_ON_NODE:-/etc/kubernetes/admin.conf}"
PULL_TOKEN_TTL_HOURS="${PULL_TOKEN_TTL_HOURS:-6}"        # short-lived (SEC-004)
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-180s}"
CRD_ESTABLISH_TIMEOUT="${CRD_ESTABLISH_TIMEOUT:-90s}"
BASELINE_ATTEMPTS="${BASELINE_ATTEMPTS:-30}"
BASELINE_DELAY="${BASELINE_DELAY:-10}"

lib::require_cmds jq base64 sed

# ---- input validation / derivation -----------------------------------------
deploy::_require_common() {
  [[ -n "$TOPOLOGY" ]]                || log::die "TOPOLOGY is required (ss|xs)"
  [[ -n "$REGION" ]]                  || log::die "REGION is required (canary region)"
  [[ -n "$PRIMARY_SUBSCRIPTION_ID" ]] || log::die "PRIMARY_SUBSCRIPTION_ID is required (explicit subscription; RD-020)"
}

deploy::_derive() {
  [[ "${DEPLOY_DERIVED:-0}" == "1" ]] && return 0
  deploy::_require_common
  naming::_load_context
  TCODE="$(naming::topology_code "$TOPOLOGY")" || log::die "invalid TOPOLOGY '${TOPOLOGY}' (want ss|xs)"
  local region_norm
  region_norm="$(lib::validate_canary_region "$REGION")" || log::die "REGION '${REGION}' is not a supported canary region"
  REGION="$region_norm"

  : "${SECONDARY_SUBSCRIPTION_ID:=$PRIMARY_SUBSCRIPTION_ID}"   # ss: both primary
  : "${STAGING_ACR_SUBSCRIPTION_ID:=$PRIMARY_SUBSCRIPTION_ID}"

  local pairs k v
  pairs="$(naming::_region_pairs "$TCODE" "$REGION")" || log::die "name generation failed for ${TCODE}/${REGION}"
  declare -gA N=()
  while IFS='=' read -r k v; do [[ -n "$k" ]] && N["$k"]="$v"; done <<< "$pairs"

  ROLE="${N[subscription_role]}"
  CLUSTER_ID="${N[cluster_id]}"
  RG="${N[resource_group]}"
  CP_VM="${N[cp_vm]}"
  NAMESPACE="${N[namespace]}"          # test-apps (A) | default (B)  (CON-003)
  POD_LABEL="${N[pod_label]}"          # role (A)      | app (B)      (CON-003)
  PREFIX_SET_BACKEND="${N[prefix_set_backend]}"
  PREFIX_SET_FRONTEND="${N[prefix_set_frontend]}"

  # Cluster-owning subscription: primary for Cluster A / ss; secondary for xs B.
  case "$ROLE" in
    primary)   CLUSTER_SUB="$PRIMARY_SUBSCRIPTION_ID" ;;
    secondary) CLUSTER_SUB="$SECONDARY_SUBSCRIPTION_ID" ;;
    *) log::die "unexpected subscription role '${ROLE}'" ;;
  esac
  [[ -n "$CLUSTER_SUB" ]] || log::die "no subscription resolved for role '${ROLE}' (set SECONDARY_SUBSCRIPTION_ID for xs)"

  # Cluster A uses Deployments; Cluster B uses standalone pods (baseline 0).
  if [[ "$CLUSTER_ID" == "A" ]]; then IS_CLUSTER_A=1; else IS_CLUSTER_A=0; fi

  # The shared ASGs live in the topology's PRIMARY region RG under the PRIMARY
  # subscription; BOTH clusters' controllers target them (FR-028).
  local base primary_rg
  base="$(naming::base "$NAMING_PURPOSE" "$TCODE" "$DATE_UTC" "$RUN_SUFFIX")"
  primary_rg="$(naming::rbase "$base" "$NAMING_PRIMARY_REGION")"
  ASG_SUB="$PRIMARY_SUBSCRIPTION_ID"
  ASG_RG="$primary_rg"
  ASG_BACKEND_ID="/subscriptions/${ASG_SUB}/resourceGroups/${ASG_RG}/providers/Microsoft.Network/applicationSecurityGroups/asg-backend"
  ASG_FRONTEND_ID="/subscriptions/${ASG_SUB}/resourceGroups/${ASG_RG}/providers/Microsoft.Network/applicationSecurityGroups/asg-frontend"

  # Short-lived repo-scoped pull token / secret names (deterministic, <=49/<=63).
  PULL_TOKEN_NAME="${RG}-pull"
  PULL_SECRET_NAME="pnc-candidate-pull"
  DEPLOY_DERIVED=1
}

# ---- YAML rendering (pure; no cloud) ----------------------------------------
# Common Section-3.4.6 correlation labels for K8s objects.
deploy::_labels_block() {
  local indent="$1"
  cat <<LBL
${indent}validation.networking.azure.com/purpose: "${NAMING_PURPOSE}"
${indent}validation.networking.azure.com/run-id: "${RUN_ID}"
${indent}validation.networking.azure.com/run-suffix: "${RUN_SUFFIX}"
${indent}validation.networking.azure.com/date-utc: "${DATE_UTC}"
${indent}validation.networking.azure.com/topology: "${TCODE}"
${indent}app.kubernetes.io/managed-by: github-actions
LBL
}

deploy::_mapping_yaml() {
  local name="$1" selector_value="$2" asg_id="$3"
  cat <<MAP
---
apiVersion: networking.azure.com/v1alpha1
kind: PodASGMapping
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
$(deploy::_labels_block "    ")
spec:
  mappings:
    - podSelector:
        matchLabels:
          ${POD_LABEL}: ${selector_value}
      applicationSecurityGroups:
        - resourceId: ${asg_id}
MAP
}

deploy::_deployment_yaml() {
  local name="$1" replicas="$2"
  cat <<DEP
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
$(deploy::_labels_block "    ")
spec:
  replicas: ${replicas}
  selector:
    matchLabels:
      ${POD_LABEL}: ${name}
  template:
    metadata:
      labels:
        ${POD_LABEL}: ${name}
$(deploy::_labels_block "        ")
    spec:
      containers:
        - name: app
          image: ${FIXTURE_IMAGE}
DEP
}

deploy::_namespace_yaml() {
  cat <<NS
---
apiVersion: v1
kind: Namespace
metadata:
  name: ${NAMESPACE}
  labels:
$(deploy::_labels_block "    ")
NS
}

# Render the CORE bundle (CRD + RBAC + manager@digest) and the WORKLOAD bundle
# (namespace + PodASGMappings + Cluster-A Deployments) to files under RENDER_DIR.
deploy::render() {
  deploy::_derive
  [[ -n "$IMAGE_REFERENCE" ]] || log::die "IMAGE_REFERENCE is required (candidate @sha256 digest ref)"
  mkdir -p "$RENDER_DIR"
  CORE_FILE="${RENDER_DIR}/deploy-core-${TCODE}-${REGION}.yaml"
  WORKLOAD_FILE="${RENDER_DIR}/deploy-workload-${TCODE}-${REGION}.yaml"

  local crd="${REPO_ROOT}/config/crd/podasgmapping.yaml"
  local rbac="${REPO_ROOT}/config/rbac/rbac.yaml"
  local manager="${REPO_ROOT}/config/manager/manager.yaml"
  local f
  for f in "$crd" "$rbac" "$manager"; do
    [[ -f "$f" ]] || log::die "required manifest not found: ${f}"
  done

  # CORE: CRD + RBAC + manager with image->digest and imagePullPolicy Never
  # disabled (a private-registry pull is required; Never would ErrImageNeverPull).
  {
    cat "$crd"
    printf '\n---\n'
    cat "$rbac"
    printf '\n---\n'
    sed -e "s|image: pod-nsg-controller:latest|image: ${IMAGE_REFERENCE}|" \
        -e "s|imagePullPolicy: Never|imagePullPolicy: IfNotPresent|" "$manager"
  } > "$CORE_FILE"

  # WORKLOAD: namespace + the two documented PodASGMappings (+ Cluster-A pods).
  {
    deploy::_namespace_yaml
    deploy::_mapping_yaml backend-asg-mapping  backend  "$ASG_BACKEND_ID"
    deploy::_mapping_yaml frontend-asg-mapping frontend "$ASG_FRONTEND_ID"
    if (( IS_CLUSTER_A == 1 )); then
      deploy::_deployment_yaml backend  "$BASELINE_BACKEND"
      deploy::_deployment_yaml frontend "$BASELINE_FRONTEND"
    fi
  } > "$WORKLOAD_FILE"

  log::info "rendered deploy bundle: ${CORE_FILE} + ${WORKLOAD_FILE} (cluster ${CLUSTER_ID}, ns ${NAMESPACE})"
}

# ---- ACR token (short-lived, repository-scoped) -----------------------------
deploy::mint_token() {
  deploy::_derive
  [[ -n "$STAGING_ACR" ]] || log::die "STAGING_ACR is required to mint the pull token"
  local acr_name="${STAGING_ACR%%.*}" exp
  exp="$(date -u -d "+${PULL_TOKEN_TTL_HOURS} hours" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
        || date -u -v+"${PULL_TOKEN_TTL_HOURS}"H +%Y-%m-%dT%H:%M:%SZ)"
  log::info "minting repository-scoped ACR token '${PULL_TOKEN_NAME}' for ${CONTROLLER_STAGING_REPO} (expires ${exp})"
  # Create the repo-scoped token (idempotent: a re-run may report it exists).
  lib::retry 3 5 -- "$AZ_BIN" acr token create --name "$PULL_TOKEN_NAME" \
    --registry "$acr_name" --repository "$CONTROLLER_STAGING_REPO" content/read \
    --subscription "$STAGING_ACR_SUBSCRIPTION_ID" --output none 2>/dev/null || \
    log::warn "acr token create returned non-zero for '${PULL_TOKEN_NAME}' (may already exist); regenerating credential"
  PULL_TOKEN_PASSWORD="$("$AZ_BIN" acr token credential generate --name "$PULL_TOKEN_NAME" \
    --registry "$acr_name" --password1 --expiration "$exp" \
    --subscription "$STAGING_ACR_SUBSCRIPTION_ID" \
    --query 'passwords[0].value' -o tsv)" \
    || log::die "failed to generate a credential for ACR token '${PULL_TOKEN_NAME}'"
  [[ -n "$PULL_TOKEN_PASSWORD" ]] || log::die "empty ACR token password for '${PULL_TOKEN_NAME}'"
}

# ---- node deploy script assembly --------------------------------------------
# The node script base64-decodes both bundles (avoids all quoting/expansion
# hazards), applies core, creates the two secrets, sets CLUSTER_NAME + the
# imagePullSecret, waits for the CRD to establish, applies the workload, and
# waits for the manager rollout. Secret data is piped to `kubectl apply` (never
# echoed to the node log), so the short-lived token is not surfaced.
deploy::_node_script() {
  local core_b64 wl_b64
  core_b64="$(base64 -w0 "$CORE_FILE")"
  wl_b64="$(base64 -w0 "$WORKLOAD_FILE")"
  cat <<NODE
set -o pipefail
export KUBECONFIG=${KUBECONFIG_ON_NODE}
base64 -d > /tmp/pnc-core.yaml <<'PNCCORE'
${core_b64}
PNCCORE
base64 -d > /tmp/pnc-workload.yaml <<'PNCWL'
${wl_b64}
PNCWL
kubectl apply -f /tmp/pnc-core.yaml
kubectl -n ${CONTROLLER_NAMESPACE} create secret docker-registry ${PULL_SECRET_NAME} --docker-server=${STAGING_ACR} --docker-username=${PULL_TOKEN_NAME} --docker-password='${PULL_TOKEN_PASSWORD}' --dry-run=client -o yaml | kubectl apply -f -
kubectl -n ${CONTROLLER_NAMESPACE} create secret generic pod-nsg-controller-azure --from-literal=subscription-id=${ASG_SUB} --from-literal=resource-group=${ASG_RG} --from-literal=nsg-name=${N[nsg]} --dry-run=client -o yaml | kubectl apply -f -
kubectl -n ${CONTROLLER_NAMESPACE} set env deploy/pod-nsg-controller CLUSTER_NAME=${RG}
kubectl -n ${CONTROLLER_NAMESPACE} patch deploy/pod-nsg-controller --type=strategic -p '{"spec":{"template":{"spec":{"imagePullSecrets":[{"name":"${PULL_SECRET_NAME}"}]}}}}'
kubectl wait --for=condition=established --timeout=${CRD_ESTABLISH_TIMEOUT} crd/podasgmappings.networking.azure.com
kubectl apply -f /tmp/pnc-workload.yaml
kubectl -n ${CONTROLLER_NAMESPACE} rollout status deploy/pod-nsg-controller --timeout=${ROLLOUT_TIMEOUT}
NODE
}

deploy::deploy() {
  deploy::render
  deploy::mint_token
  log::info "deploying candidate to cluster ${CLUSTER_ID} (${TCODE}/${REGION}) via control-plane ${CP_VM} in subscription ${CLUSTER_SUB}"
  azrun::exec "$AZ_BIN" "$CLUSTER_SUB" "$RG" "$CP_VM" "$(deploy::_node_script)" \
    || log::die "controller deploy failed on ${CP_VM} (subscription ${CLUSTER_SUB})"
  log::info "controller deploy applied on cluster ${CLUSTER_ID}"
}

# ---- baseline (ITEM-012): A=4 running pods, B=0; both mappings present -------
deploy::_count_running() {
  local selector="$1" out
  out="$(azrun::capture "$AZ_BIN" "$CLUSTER_SUB" "$RG" "$CP_VM" \
    "KUBECONFIG=${KUBECONFIG_ON_NODE} kubectl -n ${NAMESPACE} get pods -l ${selector} --no-headers 2>/dev/null")" || return 1
  printf '%s\n' "$out" | grep -cw Running || true
}

deploy::_count_mappings() {
  local out
  out="$(azrun::capture "$AZ_BIN" "$CLUSTER_SUB" "$RG" "$CP_VM" \
    "KUBECONFIG=${KUBECONFIG_ON_NODE} kubectl -n ${NAMESPACE} get podasgmappings -o name 2>/dev/null")" || return 1
  printf '%s\n' "$out" | grep -c 'podasgmapping' || true
}

deploy::baseline() {
  deploy::_derive
  local want_be want_fe maps attempt=1 be fe
  if (( IS_CLUSTER_A == 1 )); then want_be="$BASELINE_BACKEND"; want_fe="$BASELINE_FRONTEND"; else want_be=0; want_fe=0; fi
  BASELINE_EXPECTED=$(( want_be + want_fe ))

  # Mappings must exist on both clusters (workload applied by deploy).
  maps="$(deploy::_count_mappings)"; maps="${maps//[^0-9]/}"; maps="${maps:-0}"
  (( maps >= 2 )) || log::die "expected 2 PodASGMappings in ${NAMESPACE}, found ${maps}"

  while true; do
    be="$(deploy::_count_running "${POD_LABEL}=backend")";  be="${be//[^0-9]/}";  be="${be:-0}"
    fe="$(deploy::_count_running "${POD_LABEL}=frontend")"; fe="${fe//[^0-9]/}"; fe="${fe:-0}"
    if (( be == want_be && fe == want_fe )); then
      log::info "baseline OK for cluster ${CLUSTER_ID}: backend=${be} frontend=${fe} (expected ${BASELINE_EXPECTED} pods)"
      return 0
    fi
    if (( attempt >= BASELINE_ATTEMPTS )); then
      log::error "baseline not reached for cluster ${CLUSTER_ID}: backend=${be}/${want_be} frontend=${fe}/${want_fe}"
      return 1
    fi
    log::warn "awaiting baseline (cluster ${CLUSTER_ID}): backend=${be}/${want_be} frontend=${fe}/${want_fe} (${attempt}/${BASELINE_ATTEMPTS})"
    sleep "$BASELINE_DELAY"; attempt=$(( attempt + 1 ))
  done
}

# ---- manifest record --------------------------------------------------------
deploy::record() {
  deploy::_derive
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  # Baseline is a function of cluster identity (A=2/2, B=0/0), independent of
  # whether deploy::baseline has run yet, so record is always well-formed.
  local want_be want_fe expected rec
  if (( IS_CLUSTER_A == 1 )); then want_be="$BASELINE_BACKEND"; want_fe="$BASELINE_FRONTEND"; else want_be=0; want_fe=0; fi
  expected=$(( want_be + want_fe ))
  rec="$(jq -n \
    --arg sub "$CLUSTER_SUB" --arg role "$ROLE" --arg region "$REGION" --arg cid "$CLUSTER_ID" \
    --arg ns "$NAMESPACE" --arg label "$POD_LABEL" --arg rg "$RG" --arg cp "$CP_VM" \
    --arg cn "$RG" --arg cns "$CONTROLLER_NAMESPACE" \
    --arg img "$IMAGE_REFERENCE" --arg ps "$PULL_SECRET_NAME" --arg tok "$PULL_TOKEN_NAME" \
    --arg asub "$ASG_SUB" --arg arg "$ASG_RG" \
    --arg abe "$ASG_BACKEND_ID" --arg afe "$ASG_FRONTEND_ID" \
    --arg psb "$PREFIX_SET_BACKEND" --arg psf "$PREFIX_SET_FRONTEND" \
    --argjson want_be "$want_be" --argjson want_fe "$want_fe" \
    --argjson expected "$expected" \
    '{subscription_id:$sub, subscription_role:$role, region:$region, cluster_id:$cid,
      namespace:$ns, pod_label:$label, resource_group:$rg, control_plane_vm:$cp,
      cluster_name:$cn, controller_namespace:$cns, image_reference:$img,
      pull_secret:$ps, pull_token:$tok,
      asg_subscription_id:$asub, asg_resource_group:$arg,
      asgs:{backend:$abe, frontend:$afe},
      mappings:["backend-asg-mapping","frontend-asg-mapping"],
      prefix_sets:{backend:$psb, frontend:$psf},
      baseline:{backend:$want_be, frontend:$want_fe, expected_pods:$expected}}')"
  manifest::put_json "$MANIFEST_PATH" "deploy.${TCODE}.${REGION}" "$rec"
  gha::output resource_group "$RG"
  gha::output namespace "$NAMESPACE"
  gha::output cluster_name "$RG"
  log::info "recorded deploy.${TCODE}.${REGION} -> ${MANIFEST_PATH}"
}

deploy::names() {
  deploy::_derive
  local k
  for k in $(printf '%s\n' "${!N[@]}" | sort); do printf '%s=%s\n' "$k" "${N[$k]}"; done
  printf 'cluster_subscription=%s\nasg_subscription=%s\nasg_resource_group=%s\nasg_backend_id=%s\nasg_frontend_id=%s\npull_token=%s\n' \
    "$CLUSTER_SUB" "$ASG_SUB" "$ASG_RG" "$ASG_BACKEND_ID" "$ASG_FRONTEND_ID" "$PULL_TOKEN_NAME"
}

deploy::all() {
  deploy::_derive
  # Inventory the deploy coordinates even if baseline fails, so teardown and
  # diagnostics always have them (mirrors provision-cluster.sh, RISK-011).
  trap 'deploy::record || log::warn "manifest record failed"' EXIT
  deploy::deploy
  deploy::baseline
}

deploy::usage() {
  cat <<'USAGE'
Usage: deploy-controller.sh <command>

Deploy the candidate controller to ONE self-managed cluster and apply the
documented PodASGMappings + workloads, then establish the baseline. kubectl runs
ON the control-plane node via `az vm run-command` (the runner has no API path).

Commands:
  all       render -> deploy -> baseline -> record
  render    Render the CORE (CRD/RBAC/manager@digest) + WORKLOAD bundles (no cloud)
  deploy    Mint the repo-scoped ACR token, apply bundles + secrets + patches on the node
  baseline  Verify baseline pods (A=4, B=0) and that both mappings are present
  record    Record deploy coordinates + baseline into the run manifest
  names     Print derived names / subscriptions and exit

Required env: TOPOLOGY (ss|xs), REGION (canary), PRIMARY_SUBSCRIPTION_ID,
  IMAGE_REFERENCE (candidate @sha256 ref); deploy also needs STAGING_ACR.
Optional: SECONDARY_SUBSCRIPTION_ID (xs Cluster B), STAGING_ACR_SUBSCRIPTION_ID.
Naming context: REPO, RUN_ID, RUN_ATTEMPT, DATE_UTC, GIT_SHA, GIT_REF, TTL_HOURS.
Tool seam: AZ_BIN (az).
USAGE
}

deploy::main() {
  local cmd="${1:-all}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    all)      deploy::all ;;
    render)   deploy::render ;;
    deploy)   deploy::deploy ;;
    baseline) deploy::baseline; deploy::record ;;
    record)   deploy::record ;;
    names)    deploy::names ;;
    help|-h|--help) deploy::usage ;;
    *) log::error "unknown command: ${cmd}"; deploy::usage >&2; return 1 ;;
  esac
}

deploy::main "$@"
