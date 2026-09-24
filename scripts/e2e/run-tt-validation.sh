#!/usr/bin/env bash
# =============================================================================
# run-tt-validation.sh - the seven transparent-tunnel same-node enforcement
# scenarios TTS-001..TTS-007 as discrete, independently-invocable functions
# (tts1..tts7), executed EXACTLY per
# docs/transparent-tunnel-same-node-enforcement-test.md (§4.1-§4.5/§5).
#
# The runner has NO path to the self-managed API server, so all kubectl runs ON
# the centraluseuap control-plane node via `az vm run-command`, per-worker CNI
# checks run ON each worker, and ASG membership is read from the runner with the
# `az rest` addressPrefixSets GET (api 2025-07-01, NOT list-effective-nsg -
# doc §4.2 warning). Every Azure call carries an explicit --subscription (RD-020).
# Each scenario records an independent pass/fail result into run-manifest.json
# (FR-015) and `all` fails if ANY scenario fails, which blocks release (FR-022).
#
#   tts1  TTS-001  every worker conflist mode=transparent-tunnel, kubelet active,
#                  /opt/cni/tt-backup-<ts> present; control-plane stays stock.
#   tts2  TTS-002  app=backend/app=frontend pod IPs are /32 members of the ASG
#                  prefix sets (addressPrefixSets REST GET).
#   tts3  TTS-003  backend-tt/frontend-tt Running on the SAME node; both IPs
#                  reconcile into their ASG prefix sets (~60s).
#   tts4  TTS-004  backend-tt -> frontend-tt ICMP 100% loss (DENIED, rule 190).
#   tts5  TTS-005  frontend-tt -> backend-tt ICMP 0% loss (ALLOWED).
#   tts6  TTS-006  tt-canary (no ASG) -> frontend-tt ICMP 0% loss (ALLOWED).
#   tts7  TTS-007  backend->frontend:8080 blocked, frontend->backend:8080 allowed,
#                  with same-node flows visible on eth0 (physical-NIC evidence).
#
# Inputs (environment): TOPOLOGY [req], REGION (default centraluseuap),
# PRIMARY_SUBSCRIPTION_ID [req], SECONDARY_SUBSCRIPTION_ID, TT_ASG_RG/TT_ASG_SUB
# (ASG scope override), MANIFEST_PATH, AZ_BIN, KUBECONFIG_ON_NODE, FIXTURE_IMAGE,
# tunables TT_RECONCILE_ATTEMPTS/TT_RECONCILE_DELAY/TT_PING_ATTEMPTS.
#
# Commands: all(default) | tts1..tts7 | names | help
#
# Traceability: ITEM-032, ITEM-033, TTS-001..007, FR-021..023, TEST-012..017,
# AC-017..022, RD-017, RD-020, CON-010, RISK-010,
# docs/transparent-tunnel-same-node-enforcement-test.md.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"

lib::require_cmds jq sort sed grep

TOPOLOGY="${TOPOLOGY:-}"
REGION="${REGION:-$NAMING_SECONDARY_REGION}"          # centraluseuap-only (CON-010)
PRIMARY_SUBSCRIPTION_ID="${PRIMARY_SUBSCRIPTION_ID:-}"
SECONDARY_SUBSCRIPTION_ID="${SECONDARY_SUBSCRIPTION_ID:-}"
TT_ASG_RG="${TT_ASG_RG:-}"
TT_ASG_SUB="${TT_ASG_SUB:-}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
AZ_BIN="${AZ_BIN:-az}"
KUBECONFIG_ON_NODE="${KUBECONFIG_ON_NODE:-/etc/kubernetes/admin.conf}"
FIXTURE_IMAGE="${FIXTURE_IMAGE:-busybox}"
APS_API_VERSION="${APS_API_VERSION:-2025-07-01}"
TT_RECONCILE_ATTEMPTS="${TT_RECONCILE_ATTEMPTS:-6}"
TT_RECONCILE_DELAY="${TT_RECONCILE_DELAY:-10}"
TT_PING_ATTEMPTS="${TT_PING_ATTEMPTS:-3}"

declare -gA N=()

ttv::_require_inputs() {
  [[ -n "$TOPOLOGY" ]]                || log::die "TOPOLOGY is required (ss|xs)"
  [[ -n "$PRIMARY_SUBSCRIPTION_ID" ]] || log::die "PRIMARY_SUBSCRIPTION_ID is required (explicit subscription; RD-020)"
}

ttv::_derive() {
  [[ "${TTV_DERIVED:-0}" == "1" ]] && return 0
  ttv::_require_inputs
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
  [[ -n "$CLUSTER_SUB" ]] || log::die "no subscription resolved for role '${N[subscription_role]}'"

  local base primary_rg
  base="$(naming::base "$NAMING_PURPOSE" "$TCODE" "$DATE_UTC" "$RUN_SUFFIX")"
  primary_rg="$(naming::rbase "$base" "$NAMING_PRIMARY_REGION")"
  ASG_SUB="${TT_ASG_SUB:-$PRIMARY_SUBSCRIPTION_ID}"
  ASG_RG="${TT_ASG_RG:-$primary_rg}"
  ASG_BACKEND_ID="/subscriptions/${ASG_SUB}/resourceGroups/${ASG_RG}/providers/Microsoft.Network/applicationSecurityGroups/asg-backend"
  ASG_FRONTEND_ID="/subscriptions/${ASG_SUB}/resourceGroups/${ASG_RG}/providers/Microsoft.Network/applicationSecurityGroups/asg-frontend"
  TT_NODE="${N[worker1_vm]}"                 # host both TT pods on one worker
  TT_WORKERS=( "${N[worker1_vm]}" "${N[worker2_vm]}" "${N[worker3_vm]}" )
  TTV_DERIVED=1
}

# ---- node seams -------------------------------------------------------------
ttv::_cp_capture() { azrun::capture "$AZ_BIN" "$CLUSTER_SUB" "${N[resource_group]}" "${N[cp_vm]}" "KUBECONFIG=${KUBECONFIG_ON_NODE} kubectl $1"; }
ttv::_cp_exec()    { azrun::exec    "$AZ_BIN" "$CLUSTER_SUB" "${N[resource_group]}" "${N[cp_vm]}" "$1"; }
ttv::_node_capture() { azrun::capture "$AZ_BIN" "$CLUSTER_SUB" "${N[resource_group]}" "$1" "$2"; }
ttv::_rest_list()  { "$AZ_BIN" rest --method get --subscription "$ASG_SUB" \
                       --url "https://management.azure.com${1}/addressPrefixSets?api-version=${APS_API_VERSION}" 2>/dev/null || true; }
ttv::_prefix_addresses() { ttv::_rest_list "$1" | jq -r --arg n "$2" \
  '.value[]? | select(.name == $n) | .properties.addressPrefixSet[]?' 2>/dev/null | sed 's#/32$##' | sort -u; }

# ---- fixtures: same-node backend-tt / frontend-tt / tt-canary (§4.3) ---------
ttv::_ensure_pods() {
  [[ "${TTV_PODS_READY:-0}" == "1" ]] && return 0
  local ns="${N[namespace]}" ov script
  ov='{"spec":{"nodeName":"'"${TT_NODE}"'"}}'
  script="$(cat <<PODS
export KUBECONFIG=${KUBECONFIG_ON_NODE}
kubectl -n ${ns} get pod backend-tt  >/dev/null 2>&1 || kubectl -n ${ns} run backend-tt  --image=${FIXTURE_IMAGE} --labels=app=backend  --restart=Never --overrides='${ov}' --command -- sleep infinity
kubectl -n ${ns} get pod frontend-tt >/dev/null 2>&1 || kubectl -n ${ns} run frontend-tt --image=${FIXTURE_IMAGE} --labels=app=frontend --restart=Never --overrides='${ov}' --command -- sleep infinity
kubectl -n ${ns} get pod tt-canary   >/dev/null 2>&1 || kubectl -n ${ns} run tt-canary   --image=${FIXTURE_IMAGE}                        --restart=Never --overrides='${ov}' --command -- sleep infinity
PODS
)"
  ttv::_cp_exec "$script" || log::die "failed to create transparent-tunnel pods on ${N[cp_vm]}"

  # Bounded wait until all three are Running; capture IPs + node placement.
  local attempt=1 json
  while true; do
    json="$(ttv::_cp_capture "-n ${ns} get pods backend-tt frontend-tt tt-canary -o json")" || true
    local running; running="$(printf '%s' "$json" | jq -r '[.items[]? | select(.status.phase=="Running")] | length' 2>/dev/null || echo 0)"
    if [[ "$running" == "3" ]]; then break; fi
    if (( attempt >= TT_RECONCILE_ATTEMPTS )); then
      log::error "transparent-tunnel pods not all Running after ${attempt} attempt(s)"; break
    fi
    sleep "$TT_RECONCILE_DELAY"; attempt=$(( attempt + 1 ))
  done
  BACKEND_IP="$(printf '%s' "$json"  | jq -r '.items[]? | select(.metadata.name=="backend-tt")  | .status.podIP // empty')"
  FRONTEND_IP="$(printf '%s' "$json" | jq -r '.items[]? | select(.metadata.name=="frontend-tt") | .status.podIP // empty')"
  BACKEND_NODE="$(printf '%s' "$json"  | jq -r '.items[]? | select(.metadata.name=="backend-tt")  | .spec.nodeName // empty')"
  FRONTEND_NODE="$(printf '%s' "$json" | jq -r '.items[]? | select(.metadata.name=="frontend-tt") | .spec.nodeName // empty')"
  [[ -n "$BACKEND_IP" && -n "$FRONTEND_IP" ]] || log::die "could not resolve backend-tt/frontend-tt pod IPs"
  TTV_PODS_READY=1
}

# ---- pod-IP readbacks + membership ------------------------------------------
ttv::_pod_ips() {  # <role> -> sorted running pod IPs (no /32)
  ttv::_cp_capture "-n ${N[namespace]} get pods -l ${N[pod_label]}=$1 --field-selector=status.phase=Running -o json" \
    | jq -r '.items[]? | select(.status.phase=="Running") | .status.podIP // empty' 2>/dev/null | sort -u
}

# ttv::_membership_ok <role> : every running pod IP for <role> is a /32 in the
# matching ASG prefix set (bounded reconcile poll; ~60s). Fails closed.
ttv::_membership_ok() {
  local role="$1" asg_id ps_name
  if [[ "$role" == backend ]]; then asg_id="$ASG_BACKEND_ID"; ps_name="${N[prefix_set_backend]}"
  else asg_id="$ASG_FRONTEND_ID"; ps_name="${N[prefix_set_frontend]}"; fi
  local attempt=1 pod_ips rest_ips ip ok
  while true; do
    pod_ips="$(ttv::_pod_ips "$role")"
    rest_ips="$(ttv::_prefix_addresses "$asg_id" "$ps_name")"
    ok=1; [[ -n "$pod_ips" ]] || ok=0
    for ip in $pod_ips; do printf '%s\n' "$rest_ips" | grep -qxF "$ip" || ok=0; done
    if (( ok == 1 )); then
      log::info "membership OK ${role}: [$(printf '%s' "$pod_ips" | tr '\n' ' ')] in ${ps_name}"; return 0; fi
    if (( attempt >= TT_RECONCILE_ATTEMPTS )); then
      log::error "membership ${role} FAILED: podIPs=[$(printf '%s' "$pod_ips" | tr '\n' ' ')] restIPs=[$(printf '%s' "$rest_ips" | tr '\n' ' ')] set=${ps_name}"
      return 1; fi
    sleep "$TT_RECONCILE_DELAY"; attempt=$(( attempt + 1 ))
  done
}

# ---- ICMP + TCP verdict probes ----------------------------------------------
# ttv::_ping_loss <src_pod> <dst_ip> -> integer packet-loss percent (bounded retry).
ttv::_ping_loss() {
  local src="$1" dst="$2" attempt=1 out loss
  while true; do
    out="$(ttv::_cp_capture "-n ${N[namespace]} exec ${src} -- ping -c 4 -W 2 ${dst}")" || true
    loss="$(printf '%s' "$out" | grep -oE '[0-9]+% packet loss' | grep -oE '[0-9]+' | tail -1)"
    if [[ -n "$loss" ]]; then printf '%s' "$loss"; return 0; fi
    (( attempt >= TT_PING_ATTEMPTS )) && { printf 'ERR'; return 1; }
    sleep 1; attempt=$(( attempt + 1 ))
  done
}

# ttv::_tcp_verdict <src_pod> <dst_ip> -> "blocked" | "allowed". Same-node TCP to
# :8080: a dropped SYN times out (blocked); a delivered SYN gets a fast RST from
# the (listener-less) pod (allowed). The node computes the verdict from nc's
# exit + elapsed time so the runner sees a stable token.
ttv::_tcp_verdict() {
  local src="$1" dst="$2" out
  out="$(ttv::_cp_capture "$(cat <<NC
export KUBECONFIG=${KUBECONFIG_ON_NODE}
__s=\$(date +%s%N 2>/dev/null || echo 0)
kubectl -n ${N[namespace]} exec ${src} -- nc -w 4 ${dst} 8080 </dev/null >/dev/null 2>&1
__r=\$?
__e=\$(date +%s%N 2>/dev/null || echo 0)
__ms=\$(( (__e - __s) / 1000000 ))
if [ "\$__r" -ne 0 ] && [ "\$__ms" -ge 3000 ]; then echo "NC_VERDICT=blocked"; else echo "NC_VERDICT=allowed"; fi
NC
)")" || true
  printf '%s' "$out" | grep -oE 'NC_VERDICT=(blocked|allowed)' | head -1 | cut -d= -f2
}

# ---- result recording -------------------------------------------------------
ttv::_record() {  # <scenario> <status> [detail]
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  manifest::put_json "$MANIFEST_PATH" "validate.tt.scenarios.$1" \
    "$(jq -n --arg n "$1" --arg s "$2" --arg d "${3:-}" '{name:$n, status:$s, detail:$d}')"
}
ttv::_finish() {  # <scenario> <rc> [detail]
  if (( $2 == 0 )); then ttv::_record "$1" pass "${3:-}"; log::info "$1 PASS ${3:-}"
  else ttv::_record "$1" fail "${3:-}"; log::error "$1 FAIL ${3:-}"; fi
  return "$2"
}

# ---- TTS-001: CNI mode activation (§4.1) ------------------------------------
ttv::tts1() {
  ttv::_derive
  local rc=0 w out check
  check='echo TT_MODE_CHECK; grep -ho '"'"'"mode":[[:space:]]*"[a-z-]*"'"'"' /etc/cni/net.d/*.conflist 2>/dev/null | head -1; systemctl is-active kubelet 2>/dev/null; ls -d /opt/cni/tt-backup-* 2>/dev/null | head -1'
  for w in "${TT_WORKERS[@]}"; do
    out="$(ttv::_node_capture "$w" "$check")" || { log::error "tts1: run-command failed on worker ${w}"; rc=1; continue; }
    printf '%s' "$out" | grep -q '"mode":[[:space:]]*"transparent-tunnel"' || { log::error "tts1: worker ${w} is NOT transparent-tunnel"; rc=1; }
    printf '%s' "$out" | grep -qw active                                   || { log::error "tts1: kubelet not active on ${w}"; rc=1; }
    printf '%s' "$out" | grep -q '/opt/cni/tt-backup-'                     || { log::error "tts1: no /opt/cni/tt-backup-<ts> on ${w}"; rc=1; }
  done
  # Control-plane MUST remain stock CNI (TTS-001 / CON-010).
  out="$(ttv::_node_capture "${N[cp_vm]}" "$check")" || out=""
  if printf '%s' "$out" | grep -q '"mode":[[:space:]]*"transparent-tunnel"'; then
    log::error "tts1: control-plane ${N[cp_vm]} is on transparent-tunnel (MUST stay stock)"; rc=1; fi
  ttv::_finish tts1 "$rc" "workers=${#TT_WORKERS[@]} cp=${N[cp_vm]}"
}

# ---- TTS-002: ASG membership via REST (§4.2) --------------------------------
ttv::tts2() {
  ttv::_derive; ttv::_ensure_pods
  local rc=0
  ttv::_membership_ok backend  || rc=1
  ttv::_membership_ok frontend || rc=1
  ttv::_finish tts2 "$rc"
}

# ---- TTS-003: same-node placement + reconcile (§4.3) ------------------------
ttv::tts3() {
  ttv::_derive; ttv::_ensure_pods
  local rc=0
  if [[ -z "$BACKEND_NODE" || "$BACKEND_NODE" != "$FRONTEND_NODE" ]]; then
    log::error "tts3: backend-tt(${BACKEND_NODE:-?}) and frontend-tt(${FRONTEND_NODE:-?}) are NOT on the same node (RISK-010)"; rc=1
  else
    log::info "tts3: backend-tt and frontend-tt co-located on ${BACKEND_NODE}"
  fi
  ttv::_membership_ok backend  || rc=1
  ttv::_membership_ok frontend || rc=1
  ttv::_finish tts3 "$rc" "node=${BACKEND_NODE}"
}

# ---- TTS-004/005/006: ICMP verdicts (§4.4/§5) -------------------------------
ttv::tts4() {
  ttv::_derive; ttv::_ensure_pods
  local loss; loss="$(ttv::_ping_loss backend-tt "$FRONTEND_IP")"
  local rc=0; [[ "$loss" == "100" ]] || { log::error "tts4: backend->frontend ICMP loss=${loss}% (expected 100% DENY)"; rc=1; }
  ttv::_finish tts4 "$rc" "loss=${loss}%"
}
ttv::tts5() {
  ttv::_derive; ttv::_ensure_pods
  local loss; loss="$(ttv::_ping_loss frontend-tt "$BACKEND_IP")"
  local rc=0; [[ "$loss" == "0" ]] || { log::error "tts5: frontend->backend ICMP loss=${loss}% (expected 0% ALLOW)"; rc=1; }
  ttv::_finish tts5 "$rc" "loss=${loss}%"
}
ttv::tts6() {
  ttv::_derive; ttv::_ensure_pods
  local loss; loss="$(ttv::_ping_loss tt-canary "$FRONTEND_IP")"
  local rc=0; [[ "$loss" == "0" ]] || { log::error "tts6: canary->frontend ICMP loss=${loss}% (expected 0% ALLOW)"; rc=1; }
  ttv::_finish tts6 "$rc" "loss=${loss}%"
}

# ---- TTS-007: TCP/8080 verdicts + physical-NIC (eth0) evidence (§4.5/§5) -----
ttv::tts7() {
  ttv::_derive; ttv::_ensure_pods
  local rc=0 pcap_out
  # Detached eth0 capture on the worker hosting both pods (survives the session).
  ttv::_node_capture "$TT_NODE" \
    "setsid bash -c 'timeout 90 tcpdump -i eth0 -w /tmp/tt-eth0.pcap \"icmp or tcp port 8080\"' </dev/null >/dev/null 2>&1 & echo PCAP_STARTED" \
    | grep -q PCAP_STARTED || log::warn "tts7: could not confirm eth0 capture start"
  local deny allow
  deny="$(ttv::_tcp_verdict backend-tt "$FRONTEND_IP")"
  allow="$(ttv::_tcp_verdict frontend-tt "$BACKEND_IP")"
  [[ "$deny" == "blocked" ]]  || { log::error "tts7: backend->frontend:8080 verdict=${deny:-?} (expected blocked)"; rc=1; }
  [[ "$allow" == "allowed" ]] || { log::error "tts7: frontend->backend:8080 verdict=${allow:-?} (expected allowed)"; rc=1; }
  # Physical-NIC evidence: the same-node flow MUST appear on eth0 (impossible
  # under stock CNI); read the capture back and require both pod IPs present.
  pcap_out="$(ttv::_node_capture "$TT_NODE" "tcpdump -tttt -nr /tmp/tt-eth0.pcap 2>/dev/null | head -50")" || pcap_out=""
  if printf '%s' "$pcap_out" | grep -qF "$BACKEND_IP" && printf '%s' "$pcap_out" | grep -qF "$FRONTEND_IP"; then
    log::info "tts7: same-node flow observed on eth0 (transparent-tunnel steering confirmed)"
  else
    log::error "tts7: same-node flow NOT observed on eth0 (${BACKEND_IP}/${FRONTEND_IP}); VFP enforcement unproven"; rc=1
  fi
  ttv::_finish tts7 "$rc" "tcp_deny=${deny} tcp_allow=${allow}"
}

ttv::all() {
  ttv::_derive
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  local rc=0 s
  for s in tts1 tts2 tts3 tts4 tts5 tts6 tts7; do "ttv::${s}" || rc=1; done
  manifest::put "$MANIFEST_PATH" validate.tt.status "$([[ $rc -eq 0 ]] && echo pass || echo fail)"
  gha::output tt_status "$([[ $rc -eq 0 ]] && echo pass || echo fail)"
  gha::summary "## Transparent-tunnel enforcement (\`validate_tt\`)"
  gha::summary ""
  gha::summary "| Scenario | Status |"
  gha::summary "|---|---|"
  for s in tts1 tts2 tts3 tts4 tts5 tts6 tts7; do
    gha::summary "| ${s} | $(manifest::get "$MANIFEST_PATH" ".validate.tt.scenarios.${s}.status // \"?\"") |"
  done
  if (( rc == 0 )); then log::info "validate_tt: TTS-001..007 all PASS"; else log::error "validate_tt: one or more scenarios FAILED"; fi
  return "$rc"
}

ttv::names() {
  ttv::_derive
  printf 'topology=%s\nregion=%s\nresource_group=%s\ncp_vm=%s\ntt_node=%s\nworkers=%s\nnamespace=%s\nasg_backend=%s\nprefix_set_backend=%s\nprefix_set_frontend=%s\n' \
    "$TCODE" "$REGION" "${N[resource_group]}" "${N[cp_vm]}" "${TT_NODE}" "${TT_WORKERS[*]}" \
    "${N[namespace]}" "$ASG_BACKEND_ID" "${N[prefix_set_backend]}" "${N[prefix_set_frontend]}"
}

ttv::usage() {
  cat <<USAGE
Usage: run-tt-validation.sh <command>

Commands:
  all              tts1..tts7 (default); fails if any scenario fails.
  tts1..tts7       Run one transparent-tunnel enforcement scenario.
  names            Print resolved cluster/ASG/prefix-set names.
  help             Show this help.
USAGE
}

ttv::main() {
  local cmd="${1:-all}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    all)  ttv::all ;;
    tts1|tts2|tts3|tts4|tts5|tts6|tts7) "ttv::${cmd}" ;;
    names) ttv::names ;;
    help|-h|--help) ttv::usage ;;
    *) log::error "unknown command: ${cmd}"; ttv::usage >&2; return 1 ;;
  esac
}

ttv::main "$@"
