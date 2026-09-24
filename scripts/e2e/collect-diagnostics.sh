#!/usr/bin/env bash
# =============================================================================
# collect-diagnostics.sh - BEST-EFFORT topology-aware diagnostics collector for
# the always() diagnostics_ss / diagnostics_xs jobs (ITEM-015 / ITEM-040).
#
# Captures, on every run (pass or fail), the evidence needed to debug a
# cross-subscription reconciliation failure into an uploadable directory, from
# BOTH clusters (Cluster A in primary, Cluster B in secondary):
#   * node status, controller logs, PodASGMapping status, pod IPs (kubectl ON
#     each control-plane node via `az vm run-command`)
#   * shared-ASG addressPrefixSets membership (REST GET from the runner, primary
#     subscription)
#
# Collection is BEST-EFFORT: an individual capture may fail (a node may be gone,
# cleanup may have run) and that MUST NOT abort collection or fail the always()
# diagnostics step (AC-007). This script therefore does NOT use errexit and
# always exits 0. Captured text is SANITIZED - bearer/access tokens are redacted
# so no credential is ever written to an artifact (NFR-012). Every Azure call
# carries an explicit --subscription; there is NO `az account set` (RD-020).
#
# Usage: collect-diagnostics.sh <all|clusters|membership|names|help>
# Inputs (env): TOPOLOGY (default xs), PRIMARY_SUBSCRIPTION_ID [req],
#   SECONDARY_SUBSCRIPTION_ID, DIAG_DIR (default ./diagnostics), MANIFEST_PATH,
#   AZ_BIN, KUBECONFIG_ON_NODE, CONTROLLER_NAMESPACE, CONTROLLER_LOG_SELECTOR.
#
# Traceability: ITEM-015, ITEM-040, FR-006, AC-007, NFR-012, RD-020, CON-001, CON-003.
# =============================================================================
set -uo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"

lib::require_cmds jq sed

TOPOLOGY="${TOPOLOGY:-xs}"
PRIMARY_SUBSCRIPTION_ID="${PRIMARY_SUBSCRIPTION_ID:-}"
SECONDARY_SUBSCRIPTION_ID="${SECONDARY_SUBSCRIPTION_ID:-}"
DIAG_DIR="${DIAG_DIR:-diagnostics}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
AZ_BIN="${AZ_BIN:-az}"
KUBECONFIG_ON_NODE="${KUBECONFIG_ON_NODE:-/etc/kubernetes/admin.conf}"
CONTROLLER_NAMESPACE="${CONTROLLER_NAMESPACE:-pod-nsg-controller-system}"
CONTROLLER_LOG_SELECTOR="${CONTROLLER_LOG_SELECTOR:-app.kubernetes.io/name=pod-nsg-controller}"
APS_API_VERSION="${APS_API_VERSION:-2025-07-01}"
SHARED_ASGS="${SHARED_ASGS:-asg-backend asg-frontend}"
LOG_TAIL="${LOG_TAIL:-2000}"

declare -gA CA=() CB=()

# ---- sanitization (NFR-012): never write a credential to an artifact --------
diag::_sanitize() {
  sed -E \
    -e 's/([Bb]earer )[A-Za-z0-9._~+/=-]+/\1***REDACTED***/g' \
    -e 's/("access_token"[[:space:]]*:[[:space:]]*")[^"]*(")/\1***REDACTED***\2/g'
}

diag::_load_cluster() {  # <arrayname> <region> <subscription>
  local -n _dest="$1"; local region="$2" sub="$3" pairs k v
  pairs="$(naming::_region_pairs "$TCODE" "$region")" || { log::warn "name generation failed for ${TCODE}/${region}"; return 1; }
  _dest=()
  while IFS='=' read -r k v; do [[ -n "$k" ]] && _dest["$k"]="$v"; done <<< "$pairs"
  _dest[region]="$region"; _dest[sub]="$sub"
}

diag::_derive() {
  [[ "${DIAG_DERIVED:-0}" == "1" ]] && return 0
  [[ -n "$PRIMARY_SUBSCRIPTION_ID" ]] || { log::warn "PRIMARY_SUBSCRIPTION_ID is empty; diagnostics best-effort only"; }
  : "${SECONDARY_SUBSCRIPTION_ID:=$PRIMARY_SUBSCRIPTION_ID}"
  naming::_load_context
  TCODE="$(naming::topology_code "$TOPOLOGY")" || { log::warn "invalid TOPOLOGY '${TOPOLOGY}'"; TCODE=xs; }
  diag::_load_cluster CA "$NAMING_PRIMARY_REGION"   "$PRIMARY_SUBSCRIPTION_ID"   || true
  diag::_load_cluster CB "$NAMING_SECONDARY_REGION" "$SECONDARY_SUBSCRIPTION_ID" || true
  local base
  base="$(naming::base "$NAMING_PURPOSE" "$TCODE" "$DATE_UTC" "$RUN_SUFFIX")"
  ASG_SUB="$PRIMARY_SUBSCRIPTION_ID"
  ASG_RG="$(naming::rbase "$base" "$NAMING_PRIMARY_REGION")"
  DIAG_DERIVED=1
}

# ---- per-cluster capture (kubectl ON the control-plane node) ----------------
diag::_capture() {  # <arrayname> <kubectl-args> <outfile>
  local -n _c="$1"; local args="$2" out="$3" data
  [[ -n "${_c[cp_vm]:-}" && -n "${_c[sub]:-}" ]] || { log::warn "cluster map incomplete; skipping ${out}"; return 0; }
  data="$(azrun::capture "$AZ_BIN" "${_c[sub]}" "${_c[resource_group]}" "${_c[cp_vm]}" \
        "KUBECONFIG=${KUBECONFIG_ON_NODE} kubectl ${args}")" \
    || { log::warn "capture failed on ${_c[cp_vm]} (subscription ${_c[sub]}): kubectl ${args}"; return 0; }
  printf '%s\n' "$data" | diag::_sanitize > "$out"
}

diag::cluster() {  # <arrayname> <label>
  local -n _c="$1"; local label="$2"
  log::info "collecting diagnostics for cluster ${label} (subscription ${_c[sub]:-<none>})"
  diag::_capture "$1" "get nodes -o wide" "${DIAG_DIR}/cluster${label}-nodes.txt"
  diag::_capture "$1" "-n ${CONTROLLER_NAMESPACE} logs -l ${CONTROLLER_LOG_SELECTOR} --tail=${LOG_TAIL} --all-containers --prefix" \
    "${DIAG_DIR}/cluster${label}-controller-logs.txt"
  diag::_capture "$1" "-n ${_c[namespace]} get podasgmappings -o json" "${DIAG_DIR}/cluster${label}-podasgmappings.json"
  diag::_capture "$1" "-n ${_c[namespace]} get pods -o wide" "${DIAG_DIR}/cluster${label}-pods.txt"
}

# ---- shared-ASG addressPrefixSets membership (REST from the runner) ---------
diag::membership() {
  diag::_derive
  mkdir -p "$DIAG_DIR"
  local combined='{}' asg id data
  for asg in $SHARED_ASGS; do
    id="/subscriptions/${ASG_SUB}/resourceGroups/${ASG_RG}/providers/Microsoft.Network/applicationSecurityGroups/${asg}"
    data="$("$AZ_BIN" rest --method get --subscription "$ASG_SUB" \
          --url "https://management.azure.com${id}/addressPrefixSets?api-version=${APS_API_VERSION}" 2>/dev/null)" || data='{}'
    [[ -n "$data" ]] || data='{}'
    combined="$(jq -c -n --argjson c "$combined" --arg n "$asg" --argjson v "${data:-{\}}" '$c + {($n): $v}' 2>/dev/null || printf '%s' "$combined")"
  done
  printf '%s\n' "$combined" | diag::_sanitize > "${DIAG_DIR}/asg-membership.json"
  log::info "captured shared-ASG membership -> ${DIAG_DIR}/asg-membership.json"
}

diag::all() {
  diag::_derive
  mkdir -p "$DIAG_DIR"
  diag::cluster CA A
  diag::cluster CB B
  diag::membership
  # ITEM-015: the general diagnostics artifact always includes the
  # transparent-tunnel CNI/network evidence collected by EPIC-009. Keep this
  # best-effort so an unavailable node never suppresses the core captures.
  TOPOLOGY="$TCODE" PRIMARY_SUBSCRIPTION_ID="$PRIMARY_SUBSCRIPTION_ID" \
    SECONDARY_SUBSCRIPTION_ID="$SECONDARY_SUBSCRIPTION_ID" AZ_BIN="$AZ_BIN" \
    MANIFEST_PATH="$MANIFEST_PATH" CNI_DIAG_DIR="${DIAG_DIR}/cni" \
    bash "${HERE}/collect-cni-diagnostics.sh" all \
    || log::warn "CNI/network diagnostics collection failed (best-effort)"
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  manifest::put_json "$MANIFEST_PATH" "diagnostics.${TCODE}" \
    "$(jq -n --arg d "$DIAG_DIR" '{dir:$d, clusters:["A","B"]}')" || true
  gha::output diag_dir "$DIAG_DIR"
  log::info "${TCODE} diagnostics collected under ${DIAG_DIR} (best-effort)"
}

diag::names() {
  diag::_derive
  printf 'topology=%s\nasg_subscription=%s\nasg_resource_group=%s\n' "$TCODE" "$ASG_SUB" "$ASG_RG"
  printf 'clusterA rg=%s cp=%s sub=%s\n' "${CA[resource_group]:-}" "${CA[cp_vm]:-}" "${CA[sub]:-}"
  printf 'clusterB rg=%s cp=%s sub=%s\n' "${CB[resource_group]:-}" "${CB[cp_vm]:-}" "${CB[sub]:-}"
}

diag::usage() {
  cat <<'USAGE'
Usage: collect-diagnostics.sh <command>

BEST-EFFORT topology-aware diagnostics collector for diagnostics_ss/diagnostics_xs.
kubectl runs ON each control-plane node via `az vm run-command`; ASG membership
is read from the runner with `az rest`. Captured evidence is sanitized (tokens
redacted). Always exits 0.

Commands:
  all         Collect from both clusters + shared-ASG membership (default)
  clusters    Per-cluster node/controller/mapping/pod captures only
  membership  Shared-ASG addressPrefixSets membership only
  names       Print derived cluster/ASG names and exit

Inputs: TOPOLOGY (default xs), PRIMARY_SUBSCRIPTION_ID, SECONDARY_SUBSCRIPTION_ID,
DIAG_DIR (default ./diagnostics). Tool seam: AZ_BIN (az).
USAGE
}

diag::main() {
  local cmd="${1:-all}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    all)        diag::all ;;
    clusters)   diag::_derive; mkdir -p "$DIAG_DIR"; diag::cluster CA A; diag::cluster CB B ;;
    membership) diag::membership ;;
    names)      diag::names ;;
    help|-h|--help) diag::usage ;;
    *) log::error "unknown command: ${cmd}"; diag::usage >&2 ;;
  esac
  return 0
}

diag::main "$@"
# Best-effort collector: never fail the always() diagnostics step (AC-007).
exit 0
