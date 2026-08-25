#!/usr/bin/env bash
# =============================================================================
# collect-cni-diagnostics.sh - BEST-EFFORT transparent-tunnel CNI / network
# diagnostics collector (FR-023). Captures, on every run (pass or fail), the
# evidence needed to debug a transparent-tunnel enforcement failure into an
# uploadable directory:
#   * per-worker active conflist "mode", kubelet status, /opt/cni/tt-backup-<ts>
#     presence, `ip rule` policy-routing tables, route tables, and fwmark config
#   * pod -> host `azv*` veth mapping (`ip route get`)
#   * the `eth0` capture from §4.5 (retrieved from the node's /tmp/tt-eth0.pcap)
#   * ASG `addressPrefixSets` membership (REST GET, NOT list-effective-nsg)
#   * controller logs
#
# Collection is BEST-EFFORT: individual node captures may fail (a node may be
# gone, cleanup may have run), and that MUST NOT abort collection or fail the
# always()-diagnostics step (AC-007). This script therefore does NOT use
# errexit and always exits 0. Every Azure call carries an explicit --subscription
# (RD-020). Designed to be invoked standalone or from the diagnostics job
# (EPIC-005 / ITEM-015) and from validate_tt's always() step (ITEM-034).
#
# Inputs (environment): TOPOLOGY [req], REGION (default centraluseuap),
# PRIMARY_SUBSCRIPTION_ID [req], SECONDARY_SUBSCRIPTION_ID, TT_ASG_RG/TT_ASG_SUB,
# CNI_DIAG_DIR (default ./cni-diagnostics), MANIFEST_PATH, AZ_BIN,
# KUBECONFIG_ON_NODE, CONTROLLER_NAMESPACE, CONTROLLER_LOG_SELECTOR.
#
# Commands: all(default) | workers | veth | pcap | membership | controller-logs | names | help
#
# Traceability: ITEM-034, FR-023, AC-007, TTS-007, RD-020, CON-010,
# docs/transparent-tunnel-same-node-enforcement-test.md 4.5.
# =============================================================================
set -uo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"

lib::require_cmds jq base64 sed

TOPOLOGY="${TOPOLOGY:-}"
REGION="${REGION:-$NAMING_SECONDARY_REGION}"
PRIMARY_SUBSCRIPTION_ID="${PRIMARY_SUBSCRIPTION_ID:-}"
SECONDARY_SUBSCRIPTION_ID="${SECONDARY_SUBSCRIPTION_ID:-}"
TT_ASG_RG="${TT_ASG_RG:-}"
TT_ASG_SUB="${TT_ASG_SUB:-}"
CNI_DIAG_DIR="${CNI_DIAG_DIR:-cni-diagnostics}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
AZ_BIN="${AZ_BIN:-az}"
KUBECONFIG_ON_NODE="${KUBECONFIG_ON_NODE:-/etc/kubernetes/admin.conf}"
CONTROLLER_NAMESPACE="${CONTROLLER_NAMESPACE:-pod-nsg-controller-system}"
CONTROLLER_LOG_SELECTOR="${CONTROLLER_LOG_SELECTOR:-app.kubernetes.io/name=pod-nsg-controller}"
APS_API_VERSION="${APS_API_VERSION:-2025-07-01}"

declare -gA N=()

cnidiag::_require_inputs() {
  [[ -n "$TOPOLOGY" ]]                || log::die "TOPOLOGY is required (ss|xs)"
  [[ -n "$PRIMARY_SUBSCRIPTION_ID" ]] || log::die "PRIMARY_SUBSCRIPTION_ID is required (explicit subscription; RD-020)"
}

cnidiag::_derive() {
  [[ "${CNIDIAG_DERIVED:-0}" == "1" ]] && return 0
  cnidiag::_require_inputs
  naming::_load_context
  TCODE="$(naming::topology_code "$TOPOLOGY")" || log::die "invalid TOPOLOGY '${TOPOLOGY}' (want ss|xs)"
  : "${SECONDARY_SUBSCRIPTION_ID:=$PRIMARY_SUBSCRIPTION_ID}"
  local pairs k v
  pairs="$(naming::_region_pairs "$TCODE" "$REGION")" || log::die "name generation failed for ${TCODE}/${REGION}"
  N=()
  while IFS='=' read -r k v; do [[ -n "$k" ]] && N["$k"]="$v"; done <<< "$pairs"
  case "${N[subscription_role]}" in
    primary)   CLUSTER_SUB="$PRIMARY_SUBSCRIPTION_ID" ;;
    secondary) CLUSTER_SUB="$SECONDARY_SUBSCRIPTION_ID" ;;
    *) log::die "unexpected subscription role '${N[subscription_role]}'" ;;
  esac
  local base primary_rg
  base="$(naming::base "$NAMING_PURPOSE" "$TCODE" "$DATE_UTC" "$RUN_SUFFIX")"
  primary_rg="$(naming::rbase "$base" "$NAMING_PRIMARY_REGION")"
  ASG_SUB="${TT_ASG_SUB:-$PRIMARY_SUBSCRIPTION_ID}"
  ASG_RG="${TT_ASG_RG:-$primary_rg}"
  TT_NODE="${N[worker1_vm]}"
  TT_WORKERS=( "${N[worker1_vm]}" "${N[worker2_vm]}" "${N[worker3_vm]}" )
  mkdir -p "$CNI_DIAG_DIR"
  CNIDIAG_DERIVED=1
}

cnidiag::_capture() { azrun::capture "$AZ_BIN" "$CLUSTER_SUB" "${N[resource_group]}" "$1" "$2"; }

# ---- per-worker CNI + policy-routing state ----------------------------------
cnidiag::workers() {
  cnidiag::_derive
  local w out
  local check
  check='echo CNI_DIAG_WORKER; grep -ho '"'"'"mode":[[:space:]]*"[a-z-]*"'"'"' /etc/cni/net.d/*.conflist 2>/dev/null | head -1; systemctl is-active kubelet 2>/dev/null; ls -d /opt/cni/tt-backup-* 2>/dev/null; echo "---iprule---"; ip rule 2>/dev/null; echo "---iproute---"; ip route show table all 2>/dev/null; echo "---fwmark---"; iptables -t mangle -S 2>/dev/null'
  for w in "${TT_WORKERS[@]}"; do
    if out="$(cnidiag::_capture "$w" "$check")" && [[ -n "$out" ]]; then
      printf '%s\n' "$out" > "${CNI_DIAG_DIR}/worker-${w}.txt"
      log::info "collected CNI diagnostics for worker ${w}"
    else
      log::warn "could not collect CNI diagnostics for worker ${w} (best-effort)"
    fi
  done
}

# ---- pod -> host azv* veth mapping (ip route get) ---------------------------
cnidiag::veth() {
  cnidiag::_derive
  local pods_json backend_ip frontend_ip out
  pods_json="$(cnidiag::_capture "${N[cp_vm]}" "echo CNI_DIAG_PODS; KUBECONFIG=${KUBECONFIG_ON_NODE} kubectl -n ${N[namespace]} get pods backend-tt frontend-tt -o json 2>/dev/null")" || pods_json=""
  backend_ip="$(printf '%s' "$pods_json"  | jq -r '.items[]? | select(.metadata.name=="backend-tt")  | .status.podIP // empty' 2>/dev/null)"
  frontend_ip="$(printf '%s' "$pods_json" | jq -r '.items[]? | select(.metadata.name=="frontend-tt") | .status.podIP // empty' 2>/dev/null)"
  if [[ -z "$backend_ip" && -z "$frontend_ip" ]]; then
    log::warn "no transparent-tunnel pod IPs available for veth mapping (best-effort)"; return 0
  fi
  if out="$(cnidiag::_capture "$TT_NODE" "echo CNI_DIAG_VETH; ip route get ${backend_ip:-0.0.0.0} 2>/dev/null; ip route get ${frontend_ip:-0.0.0.0} 2>/dev/null")" && [[ -n "$out" ]]; then
    printf '%s\n' "$out" > "${CNI_DIAG_DIR}/veth.txt"
    log::info "collected pod->host azv* veth mapping on ${TT_NODE}"
  else
    log::warn "could not collect veth mapping on ${TT_NODE} (best-effort)"
  fi
}

# ---- eth0 pcap retrieval (/tmp/tt-eth0.pcap from §4.5) -----------------------
cnidiag::pcap() {
  cnidiag::_derive
  local b64
  b64="$(cnidiag::_capture "$TT_NODE" "echo CNI_DIAG_PCAP; if [ -f /tmp/tt-eth0.pcap ]; then base64 -w0 /tmp/tt-eth0.pcap; fi")" || b64=""
  # Strip the marker line; keep only the base64 payload.
  b64="$(printf '%s' "$b64" | grep -v 'CNI_DIAG_PCAP' | tr -d '[:space:]')"
  if [[ -n "$b64" ]]; then
    if printf '%s' "$b64" | base64 -d > "${CNI_DIAG_DIR}/tt-eth0.pcap" 2>/dev/null; then
      log::info "retrieved eth0 capture -> ${CNI_DIAG_DIR}/tt-eth0.pcap ($(wc -c < "${CNI_DIAG_DIR}/tt-eth0.pcap") bytes)"
    else
      log::warn "eth0 pcap payload could not be decoded (best-effort)"; rm -f "${CNI_DIAG_DIR}/tt-eth0.pcap"
    fi
  else
    log::warn "no /tmp/tt-eth0.pcap present on ${TT_NODE} (best-effort)"
  fi
}

# ---- ASG addressPrefixSets membership (REST; NOT effective-nsg) -------------
cnidiag::membership() {
  cnidiag::_derive
  local asg out combined='{}'
  for asg in asg-backend asg-frontend; do
    out="$("$AZ_BIN" rest --method get --subscription "$ASG_SUB" \
      --url "https://management.azure.com/subscriptions/${ASG_SUB}/resourceGroups/${ASG_RG}/providers/Microsoft.Network/applicationSecurityGroups/${asg}/addressPrefixSets?api-version=${APS_API_VERSION}" 2>/dev/null || true)"
    if [[ -n "$out" ]] && printf '%s' "$out" | jq -e . >/dev/null 2>&1; then
      combined="$(printf '%s' "$combined" | jq --arg a "$asg" --argjson v "$out" '.[$a]=$v' 2>/dev/null || printf '%s' "$combined")"
    else
      log::warn "could not read ${asg} addressPrefixSets (best-effort)"
    fi
  done
  printf '%s\n' "$combined" > "${CNI_DIAG_DIR}/asg-membership.json"
  log::info "collected ASG addressPrefixSets membership (REST, api ${APS_API_VERSION})"
}

# ---- controller logs --------------------------------------------------------
cnidiag::controller_logs() {
  cnidiag::_derive
  local out
  if out="$(cnidiag::_capture "${N[cp_vm]}" "echo CNI_DIAG_LOGS; KUBECONFIG=${KUBECONFIG_ON_NODE} kubectl -n ${CONTROLLER_NAMESPACE} logs -l ${CONTROLLER_LOG_SELECTOR} --tail=200 --all-containers 2>/dev/null")" && [[ -n "$out" ]]; then
    printf '%s\n' "$out" > "${CNI_DIAG_DIR}/controller-logs.txt"
    log::info "collected controller logs"
  else
    log::warn "could not collect controller logs (best-effort)"
    : > "${CNI_DIAG_DIR}/controller-logs.txt"
  fi
}

cnidiag::all() {
  cnidiag::_derive
  cnidiag::workers
  cnidiag::veth
  cnidiag::pcap
  cnidiag::membership
  cnidiag::controller_logs
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  manifest::put_json "$MANIFEST_PATH" diagnostics.tt "$(jq -n --arg d "$CNI_DIAG_DIR" \
    '{collected:true, dir:$d}')" 2>/dev/null || true
  gha::output cni_diag_dir "$CNI_DIAG_DIR"
  log::info "transparent-tunnel diagnostics collected under ${CNI_DIAG_DIR} (best-effort)"
  return 0
}

cnidiag::names() {
  cnidiag::_derive
  printf 'topology=%s\nregion=%s\nresource_group=%s\ncp_vm=%s\ntt_node=%s\nworkers=%s\ndiag_dir=%s\n' \
    "$TCODE" "$REGION" "${N[resource_group]}" "${N[cp_vm]}" "$TT_NODE" "${TT_WORKERS[*]}" "$CNI_DIAG_DIR"
}

cnidiag::usage() {
  cat <<USAGE
Usage: collect-cni-diagnostics.sh <command>

Commands:
  all               Collect every FR-023 evidence type (default; best-effort).
  workers           Per-worker conflist mode/kubelet/backup/ip rule/route/fwmark.
  veth              Pod -> host azv* veth mapping (ip route get).
  pcap              Retrieve the eth0 /tmp/tt-eth0.pcap capture.
  membership        ASG addressPrefixSets membership (REST).
  controller-logs   Controller logs.
  names             Print resolved names.
  help              Show this help.
USAGE
}

cnidiag::main() {
  local cmd="${1:-all}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    all)             cnidiag::all ;;
    workers)         cnidiag::workers ;;
    veth)            cnidiag::veth ;;
    pcap)            cnidiag::pcap ;;
    membership)      cnidiag::membership ;;
    controller-logs) cnidiag::controller_logs ;;
    names)           cnidiag::names ;;
    help|-h|--help)  cnidiag::usage ;;
    *) log::error "unknown command: ${cmd}"; cnidiag::usage >&2; return 1 ;;
  esac
  return 0
}

cnidiag::main "$@"
