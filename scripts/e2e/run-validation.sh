#!/usr/bin/env bash
# =============================================================================
# run-validation.sh - execute Tests 1-4 from docs/multi-cluster-test-setup.md
# against the deployed candidate, exactly per the documented pass criteria
# (EPIC-004 / ITEM-013, ITEM-012 baseline / FILE-019). The four tests are
# discrete, independently-invocable functions so a single test can be run for
# local debugging (GUD-003).
#
# Access model (resolving the EPIC-003 runner->API-server gap): the runner has
# NO path to the cluster API server, so all kubectl (scale / status / pod-IP /
# controller logs) runs ON each cluster's control-plane node through
# `az vm run-command` (lib.sh azrun::*). ASG address-prefix-set membership is
# verified from the runner with `az rest` against the public ARM endpoint
# (api-version 2025-07-01, CON-001). Every Azure call carries an explicit
# --subscription; there is NO `az account set` (RD-020).
#
# Pass criteria (docs/multi-cluster-test-setup.md, authoritative REQ-001):
#   Test 1  single-cluster scale-up      : A->10; both A mappings Synced;
#           each prefix set == the running /32 IPs; provisioningState Succeeded.
#   Test 2  multi-cluster concurrent     : A->10, B->4; both Synced; each shared
#           ASG has 2 prefix sets (one/cluster); ZERO 412 in controller logs.
#   Test 3  scale-down + cleanup         : A->4, delete all B; prefix sets hold
#           only running IPs; Cluster B prefix sets removed from the ASGs.
#   Test 4  parallel scale-up (stress)   : A->25 & B->10 concurrently; all 35
#           IPs across 4 prefix sets; ZERO 412; both Synced within the timeout.
#
# Both clusters' controllers write to the SAME shared ASGs in the PRIMARY run
# RG, each owning its own named prefix set (no cross-cluster ETag conflicts).
#
# Usage: run-validation.sh <all|test1|test2|test3|test4|baseline|names>
# Required env: TOPOLOGY (ss|xs), PRIMARY_SUBSCRIPTION_ID (+ SECONDARY_* for xs).
# Naming context: REPO, RUN_ID, RUN_ATTEMPT, DATE_UTC, GIT_SHA, GIT_REF.
# Tool seam: AZ_BIN (az).
#
# Traceability: ITEM-012, ITEM-013, ITEM-014, FR-005, REQ-001, RD-012, RD-020,
# NFR-003, NFR-012, CON-001, CON-003, TEST-004..007, AC-003..006, RISK-005.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"

# ---- inputs / configuration -------------------------------------------------
TOPOLOGY="${TOPOLOGY:-}"                                   # ss|xs (REQUIRED)
PRIMARY_SUBSCRIPTION_ID="${PRIMARY_SUBSCRIPTION_ID:-}"     # ASG + Cluster A owner (REQUIRED)
SECONDARY_SUBSCRIPTION_ID="${SECONDARY_SUBSCRIPTION_ID:-}" # Cluster B owner in xs (default: primary)
CONTROLLER_NAMESPACE="${CONTROLLER_NAMESPACE:-pod-nsg-controller-system}"
CONTROLLER_LOG_SELECTOR="${CONTROLLER_LOG_SELECTOR:-app.kubernetes.io/name=pod-nsg-controller}"
FIXTURE_IMAGE="${FIXTURE_IMAGE:-registry.k8s.io/pause:3.9}"
APS_API_VERSION="${APS_API_VERSION:-2025-07-01}"          # CON-001 (EUAP only)
SHARED_ASGS="${SHARED_ASGS:-asg-backend asg-frontend}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
AZ_BIN="${AZ_BIN:-az}"
KUBECONFIG_ON_NODE="${KUBECONFIG_ON_NODE:-/etc/kubernetes/admin.conf}"
RECONCILE_ATTEMPTS="${RECONCILE_ATTEMPTS:-30}"            # bounded reconciliation (NFR-003)
RECONCILE_DELAY="${RECONCILE_DELAY:-10}"
LOG_TAIL="${LOG_TAIL:-2000}"

lib::require_cmds jq sort sed grep

declare -gA CA=() CB=()

# ---- input validation / derivation -----------------------------------------
val::_require_inputs() {
  [[ -n "$TOPOLOGY" ]]                || log::die "TOPOLOGY is required (ss|xs)"
  [[ -n "$PRIMARY_SUBSCRIPTION_ID" ]] || log::die "PRIMARY_SUBSCRIPTION_ID is required (explicit subscription; RD-020)"
}

# val::_load_cluster <arrayname> <region> : populate a cluster's name/sub map.
val::_load_cluster() {
  local -n _dest="$1"; local region="$2" pairs k v
  pairs="$(naming::_region_pairs "$TCODE" "$region")" || log::die "name generation failed for ${TCODE}/${region}"
  _dest=()
  while IFS='=' read -r k v; do [[ -n "$k" ]] && _dest["$k"]="$v"; done <<< "$pairs"
  _dest[region]="$region"
  case "${_dest[subscription_role]}" in
    primary)   _dest[sub]="$PRIMARY_SUBSCRIPTION_ID" ;;
    secondary) _dest[sub]="$SECONDARY_SUBSCRIPTION_ID" ;;
    *) log::die "unexpected subscription role '${_dest[subscription_role]}'" ;;
  esac
  [[ -n "${_dest[sub]}" ]] || log::die "no subscription resolved for role '${_dest[subscription_role]}' (set SECONDARY_SUBSCRIPTION_ID for xs)"
}

val::_derive() {
  [[ "${VAL_DERIVED:-0}" == "1" ]] && return 0
  val::_require_inputs
  naming::_load_context
  TCODE="$(naming::topology_code "$TOPOLOGY")" || log::die "invalid TOPOLOGY '${TOPOLOGY}' (want ss|xs)"
  : "${SECONDARY_SUBSCRIPTION_ID:=$PRIMARY_SUBSCRIPTION_ID}"

  val::_load_cluster CA "$NAMING_PRIMARY_REGION"     # Cluster A (test-apps/role)
  val::_load_cluster CB "$NAMING_SECONDARY_REGION"   # Cluster B (default/app)

  # Shared ASGs live in the PRIMARY run RG under the PRIMARY subscription.
  local base
  base="$(naming::base "$NAMING_PURPOSE" "$TCODE" "$DATE_UTC" "$RUN_SUFFIX")"
  ASG_SUB="$PRIMARY_SUBSCRIPTION_ID"
  ASG_RG="$(naming::rbase "$base" "$NAMING_PRIMARY_REGION")"
  ASG_BACKEND_ID="/subscriptions/${ASG_SUB}/resourceGroups/${ASG_RG}/providers/Microsoft.Network/applicationSecurityGroups/asg-backend"
  ASG_FRONTEND_ID="/subscriptions/${ASG_SUB}/resourceGroups/${ASG_RG}/providers/Microsoft.Network/applicationSecurityGroups/asg-frontend"
  VAL_DERIVED=1
}

# ---- node kubectl (control-plane) + ARM REST seams --------------------------
val::_kubectl_capture() { # <arrayname> <kubectl-args>
  local -n _c="$1"; local args="$2"
  azrun::capture "$AZ_BIN" "${_c[sub]}" "${_c[resource_group]}" "${_c[cp_vm]}" \
    "KUBECONFIG=${KUBECONFIG_ON_NODE} kubectl ${args}"
}

val::_kubectl_exec() {  # <arrayname> <node-script>
  local -n _c="$1"; local script="$2"
  azrun::exec "$AZ_BIN" "${_c[sub]}" "${_c[resource_group]}" "${_c[cp_vm]}" "$script"
}

val::_rest_list() {  # <asg_resource_id> -> addressPrefixSets list envelope JSON
  "$AZ_BIN" rest --method get --subscription "$ASG_SUB" \
    --url "https://management.azure.com${1}/addressPrefixSets?api-version=${APS_API_VERSION}" 2>/dev/null || true
}

# ---- scaling (Cluster A: Deployment; Cluster B: standalone pods, CON-003) ----
val::_standalone_script() {  # <arrayname> <role> <n>
  local -n _c="$1"; local role="$2" n="$3" i
  printf ': PNC_OP scale %s %s %s\n' "${_c[cluster_id]}" "$role" "$n"
  # Wait for termination (default --wait=true) so recreated fixed-name pods do
  # not collide with still-terminating pods from a prior scale.
  printf 'KUBECONFIG=%s kubectl -n %s delete pods -l %s=%s --ignore-not-found || true\n' \
    "$KUBECONFIG_ON_NODE" "${_c[namespace]}" "${_c[pod_label]}" "$role"
  for (( i = 0; i < n; i++ )); do
    printf 'KUBECONFIG=%s kubectl -n %s run %s-%s --image=%s --labels=%s=%s --restart=Never || true\n' \
      "$KUBECONFIG_ON_NODE" "${_c[namespace]}" "$role" "$i" "$FIXTURE_IMAGE" "${_c[pod_label]}" "$role"
  done
}

val::scale() {  # <arrayname> <role> <n>
  local -n _c="$1"; local role="$2" n="$3" script
  if [[ "${_c[cluster_id]}" == "A" ]]; then
    script="$(printf ': PNC_OP scale %s %s %s\nKUBECONFIG=%s kubectl -n %s scale deploy/%s --replicas=%s' \
      "${_c[cluster_id]}" "$role" "$n" "$KUBECONFIG_ON_NODE" "${_c[namespace]}" "$role" "$n")"
  else
    script="$(val::_standalone_script "$1" "$role" "$n")"
  fi
  val::_kubectl_exec "$1" "$script" \
    || log::die "scale ${_c[cluster_id]}/${role}->${n} failed on ${_c[cp_vm]} (subscription ${_c[sub]})"
  log::debug "scaled ${_c[cluster_id]}/${role} -> ${n}"
}

# ---- readbacks --------------------------------------------------------------
val::_pod_ips() {  # <arrayname> <role> -> sorted running /32 pod IPs (no /32)
  local -n _c="$1"; local role="$2"
  val::_kubectl_capture "$1" "-n ${_c[namespace]} get pods -l ${_c[pod_label]}=${role} --field-selector=status.phase=Running -o json" \
    | jq -r '.items[]? | select(.status.phase == "Running") | .status.podIP // empty' 2>/dev/null | sort -u
}

val::_mapping_state() {  # <arrayname> <role> -> "<asgSyncState> <matchedPods>"
  local -n _c="$1"; local role="$2" j
  j="$(val::_kubectl_capture "$1" "-n ${_c[namespace]} get podasgmappings ${role}-asg-mapping -o json")" || true
  printf '%s' "$j" | jq -r '"\(.status.mappingStatuses[0].asgSyncState // "Unknown") \(.status.mappingStatuses[0].matchedPods // 0)"' 2>/dev/null \
    || printf 'Unknown 0'
}

val::_prefix_addresses() {  # <asg_id> <prefix_set_name> -> sorted addresses (no /32)
  val::_rest_list "$1" | jq -r --arg n "$2" \
    '.value[]? | select(.name == $n) | .properties.addressPrefixSet[]?' 2>/dev/null | sed 's#/32$##' | sort -u
}

val::_prefix_provisioning() {  # <asg_id> <prefix_set_name> -> provisioningState
  val::_rest_list "$1" | jq -r --arg n "$2" \
    '.value[]? | select(.name == $n) | .properties.provisioningState // ""' 2>/dev/null
}

val::_rest_count() { val::_rest_list "$1" | jq -r '.value | length' 2>/dev/null; }

val::_rest_has() {  # <asg_id> <prefix_set_name> : 0 if present
  val::_rest_list "$1" | jq -e --arg n "$2" 'any(.value[]?; .name == $n)' >/dev/null 2>&1
}

# val::_wait_prefix_absent <asg_id> <prefix_set_name> : bounded poll until the
# named prefix set is gone from the ASG. Scale-down -> pod termination ->
# controller prefix-set removal lags, so this MUST be polled, not one-shot.
val::_wait_prefix_absent() {
  local attempt=1
  while true; do
    val::_rest_has "$1" "$2" || return 0
    if (( attempt >= RECONCILE_ATTEMPTS )); then
      log::error "prefix set '${2}' still present on ASG after ${attempt} attempt(s) (not cleaned up)"
      return 1
    fi
    sleep "$RECONCILE_DELAY"; attempt=$(( attempt + 1 ))
  done
}

val::_no_412() {  # <arrayname> : 0 when the controller log shows zero 412s
  local logs n
  logs="$(val::_kubectl_capture "$1" "-n ${CONTROLLER_NAMESPACE} logs -l ${CONTROLLER_LOG_SELECTOR} --tail=${LOG_TAIL} --all-containers --prefix 2>/dev/null")" || true
  n="$(printf '%s' "$logs" | grep -c -E '412|PreconditionFailed' || true)"
  n="${n//[^0-9]/}"; n="${n:-0}"
  (( n == 0 ))
}

# ---- per-(cluster,role) reconciliation verification -------------------------
# Bounded polling until: mapping Synced AND matchedPods==expected AND the ASG
# prefix set for THIS cluster equals the running pod IPs AND provisioningState
# Succeeded. Fails closed on timeout (NFR-003).
val::_verify_role() {  # <arrayname> <role> <expected_count>
  local -n _c="$1"; local role="$2" want="$3"
  local asg_id ps_name
  if [[ "$role" == "backend" ]]; then asg_id="$ASG_BACKEND_ID"; ps_name="${_c[prefix_set_backend]}"
  else asg_id="$ASG_FRONTEND_ID"; ps_name="${_c[prefix_set_frontend]}"; fi

  local attempt=1 st matched pod_ips rest_ips prov state_line
  while true; do
    state_line="$(val::_mapping_state "$1" "$role")"
    st="${state_line%% *}"; matched="${state_line##* }"
    pod_ips="$(val::_pod_ips "$1" "$role")"
    rest_ips="$(val::_prefix_addresses "$asg_id" "$ps_name")"
    prov="$(val::_prefix_provisioning "$asg_id" "$ps_name")"
    if [[ "$st" == "Synced" && "$matched" == "$want" && "$prov" == "Succeeded" \
          && -n "$pod_ips" && "$pod_ips" == "$rest_ips" ]]; then
      log::info "verify ${_c[cluster_id]}/${role}: Synced matchedPods=${matched} ($(printf '%s' "$pod_ips" | wc -w) IPs)"
      return 0
    fi
    if (( attempt >= RECONCILE_ATTEMPTS )); then
      log::error "verify ${_c[cluster_id]}/${role} FAILED: state=${st} matched=${matched}/${want} prov=${prov:-<none>} podIPs=[$(printf '%s' "$pod_ips" | tr '\n' ' ')] restIPs=[$(printf '%s' "$rest_ips" | tr '\n' ' ')]"
      return 1
    fi
    log::debug "awaiting reconcile ${_c[cluster_id]}/${role} (${attempt}/${RECONCILE_ATTEMPTS}): state=${st} matched=${matched}/${want}"
    sleep "$RECONCILE_DELAY"; attempt=$(( attempt + 1 ))
  done
}

# ---- result recording (machine-readable per-test; ITEM-014) -----------------
val::_record_test() {  # <testname> <status> [detail]
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  manifest::put_json "$MANIFEST_PATH" "validate.${TCODE}.tests.${1}" \
    "$(jq -n --arg n "$1" --arg s "$2" --arg d "${3:-}" '{name:$n, status:$s, detail:$d}')"
}

val::_finish() {  # <testname> <rc>
  if (( $2 == 0 )); then val::_record_test "$1" pass; log::info "${1} (${TCODE}) PASS"
  else val::_record_test "$1" fail; log::error "${1} (${TCODE}) FAIL"; fi
  return "$2"
}

val::_baseline_reset() {
  val::scale CA backend 2;  val::scale CA frontend 2
  val::scale CB backend 0;  val::scale CB frontend 0
}

# ---- the four documented tests ----------------------------------------------
val::test1() {
  val::_derive
  log::info "Test 1 (${TCODE}): single-cluster scale-up (Cluster A -> 10 pods)"
  local rc=0
  val::_baseline_reset
  val::scale CA backend 5; val::scale CA frontend 5
  val::_verify_role CA backend 5  || rc=1
  val::_verify_role CA frontend 5 || rc=1
  val::_finish test1 "$rc"
}

val::test2() {
  val::_derive
  log::info "Test 2 (${TCODE}): multi-cluster concurrent writes (A->10, B->4)"
  local rc=0
  val::_baseline_reset
  val::scale CA backend 5; val::scale CA frontend 5
  val::scale CB backend 2; val::scale CB frontend 2
  val::_verify_role CA backend 5  || rc=1
  val::_verify_role CA frontend 5 || rc=1
  val::_verify_role CB backend 2  || rc=1
  val::_verify_role CB frontend 2 || rc=1
  # Each shared ASG now carries 2 prefix sets (one per cluster).
  [[ "$(val::_rest_count "$ASG_BACKEND_ID")" == "2" ]]  || { log::error "asg-backend must have 2 prefix sets (one/cluster)"; rc=1; }
  [[ "$(val::_rest_count "$ASG_FRONTEND_ID")" == "2" ]] || { log::error "asg-frontend must have 2 prefix sets (one/cluster)"; rc=1; }
  # Zero 412 PreconditionFailed in either controller's logs (RISK-005).
  val::_no_412 CA || { log::error "412 PreconditionFailed found in Cluster A controller logs"; rc=1; }
  val::_no_412 CB || { log::error "412 PreconditionFailed found in Cluster B controller logs"; rc=1; }
  val::_finish test2 "$rc"
}

val::test3() {
  val::_derive
  log::info "Test 3 (${TCODE}): scale-down and cleanup (A->4, delete all B)"
  local rc=0
  val::_baseline_reset
  # Establish the scaled state (10 A + 4 B) first, then scale down.
  val::scale CA backend 5; val::scale CA frontend 5
  val::scale CB backend 2; val::scale CB frontend 2
  val::scale CA backend 2; val::scale CA frontend 2
  val::scale CB backend 0; val::scale CB frontend 0
  val::_verify_role CA backend 2  || rc=1
  val::_verify_role CA frontend 2 || rc=1
  # Cluster B prefix sets must be removed from BOTH shared ASGs.
  val::_wait_prefix_absent "$ASG_BACKEND_ID"  "${CB[prefix_set_backend]}"  || rc=1
  val::_wait_prefix_absent "$ASG_FRONTEND_ID" "${CB[prefix_set_frontend]}" || rc=1
  val::_finish test3 "$rc"
}

val::test4() {
  val::_derive
  log::info "Test 4 (${TCODE}): parallel scale-up stress (A->25, B->10)"
  local rc=0
  val::_baseline_reset
  # Issue all scale operations across BOTH clusters before verifying, so the two
  # controllers write the shared ASGs concurrently (the stress condition).
  val::scale CA backend 13; val::scale CA frontend 12
  val::scale CB backend 5;  val::scale CB frontend 5
  val::_verify_role CA backend 13 || rc=1
  val::_verify_role CA frontend 12 || rc=1
  val::_verify_role CB backend 5  || rc=1
  val::_verify_role CB frontend 5 || rc=1
  [[ "$(val::_rest_count "$ASG_BACKEND_ID")" == "2" ]]  || { log::error "asg-backend must have 2 prefix sets"; rc=1; }
  [[ "$(val::_rest_count "$ASG_FRONTEND_ID")" == "2" ]] || { log::error "asg-frontend must have 2 prefix sets"; rc=1; }
  val::_no_412 CA || { log::error "412 PreconditionFailed in Cluster A logs under stress"; rc=1; }
  val::_no_412 CB || { log::error "412 PreconditionFailed in Cluster B logs under stress"; rc=1; }
  val::_finish test4 "$rc"
}

val::baseline() {
  val::_derive
  log::info "establishing baseline (Cluster A=4, Cluster B=0)"
  val::_baseline_reset
  val::_verify_role CA backend 2  || return 1
  val::_verify_role CA frontend 2 || return 1
  val::_record_test baseline pass
}

val::all() {
  val::_derive
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  local rc=0
  val::test1 || rc=1
  val::test2 || rc=1
  val::test3 || rc=1
  val::test4 || rc=1
  manifest::put "$MANIFEST_PATH" "validate.${TCODE}.status" "$([[ $rc -eq 0 ]] && echo pass || echo fail)"
  gha::output validate_status "$([[ $rc -eq 0 ]] && echo pass || echo fail)"
  if (( rc == 0 )); then log::info "validate ${TCODE}: Tests 1-4 all PASS"; else log::error "validate ${TCODE}: one or more tests FAILED"; fi
  return "$rc"
}

val::names() {
  val::_derive
  printf 'topology=%s\nasg_subscription=%s\nasg_resource_group=%s\n' "$TCODE" "$ASG_SUB" "$ASG_RG"
  printf 'clusterA rg=%s cp=%s ns=%s label=%s sub=%s\n' "${CA[resource_group]}" "${CA[cp_vm]}" "${CA[namespace]}" "${CA[pod_label]}" "${CA[sub]}"
  printf 'clusterB rg=%s cp=%s ns=%s label=%s sub=%s\n' "${CB[resource_group]}" "${CB[cp_vm]}" "${CB[namespace]}" "${CB[pod_label]}" "${CB[sub]}"
  printf 'prefix_sets A: %s | %s\n' "${CA[prefix_set_backend]}" "${CA[prefix_set_frontend]}"
  printf 'prefix_sets B: %s | %s\n' "${CB[prefix_set_backend]}" "${CB[prefix_set_frontend]}"
}

val::usage() {
  cat <<'USAGE'
Usage: run-validation.sh <command>

Execute the four documented multi-cluster tests against the deployed candidate.
kubectl runs ON each control-plane node via `az vm run-command`; ASG membership
is verified from the runner with `az rest` (addressPrefixSets, api 2025-07-01).

Commands:
  all       Run Tests 1-4 and record per-test + aggregate results
  test1     Single-cluster scale-up (Cluster A -> 10)
  test2     Multi-cluster concurrent writes (A->10, B->4; zero 412)
  test3     Scale-down and cleanup (A->4, delete B; B prefix sets removed)
  test4     Parallel scale-up stress (A->25 & B->10; 35 IPs, zero 412)
  baseline  Establish/verify the baseline (A=4, B=0)
  names     Print derived cluster/ASG names and exit

Required env: TOPOLOGY (ss|xs), PRIMARY_SUBSCRIPTION_ID (+ SECONDARY_* for xs).
Naming context: REPO, RUN_ID, RUN_ATTEMPT, DATE_UTC, GIT_SHA, GIT_REF.
Tool seam: AZ_BIN (az).
USAGE
}

val::main() {
  local cmd="${1:-all}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    all)      val::all ;;
    test1)    val::test1 ;;
    test2)    val::test2 ;;
    test3)    val::test3 ;;
    test4)    val::test4 ;;
    baseline) val::baseline ;;
    names)    val::names ;;
    help|-h|--help) val::usage ;;
    *) log::error "unknown command: ${cmd}"; val::usage >&2; return 1 ;;
  esac
}

# Auto-run ONLY when executed directly. When sourced (EPIC-010's
# run-cross-sub-validation.sh reuses val::_derive + val::test1..4 for the
# topology-keyed Tests 1-4 and the in-cluster IMDS/ARM probes), the caller drives
# val::* explicitly and this guard prevents val::main from running the full
# suite on source. Behaviour when run as a script is unchanged.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  val::main "$@"
fi
