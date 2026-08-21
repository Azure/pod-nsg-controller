#!/usr/bin/env bash
# =============================================================================
# install-cni-tt.sh - install the validated transparent-tunnel CNI onto every
# centraluseuap WORKER node, distributing the EXACT digest-verified bytes via
# `az vm run-command invoke` (the only node-access path; test doc §2). Generalizes
# the runbook's `tt_install.sh` (§4.1): per-worker backup to /opt/cni/tt-backup-<ts>,
# install the azure-vnet binary, swap the conflist, assert "mode":
# "transparent-tunnel", restart kubelet. The control-plane node is INTENTIONALLY
# left on stock CNI (it hosts the controller; TTS-001 / CON-010).
#
# The candidate CNI is pulled BY DIGEST from staging (OIDC) to the runner, its
# binary checksum is verified against the recorded pin (NFR-010), and the exact
# bytes are shipped inside the run-command payload (base64) - never fetched on
# the node from a URL/credential (SEC-006/RD-016). Each worker re-verifies the
# checksum before swapping, so tampered transit fails closed.
#
# Inputs (environment):
#   TOPOLOGY                 ss|xs (TT runs on the centraluseuap cluster)   [req]
#   REGION                   target region (default: centraluseuap / secondary)
#   PRIMARY_SUBSCRIPTION_ID  cluster-owning subscription (primary)          [req]
#   SECONDARY_SUBSCRIPTION_ID cluster-owning subscription for xs (default: primary)
#   MANIFEST_PATH            run manifest carrying artifacts.cni.* (default ./run-manifest.json)
#   CNI_REFERENCE/DIGEST/CNI_BINARY_SHA256  explicit CNI coordinates (else from manifest)
#   STAGING_ACR              staging ACR login server (oras pull auth)
#   CNI_ARTIFACT_DIR         SEAM: local pulled bytes (skips oras/az pull)
#   TT_CNI_BIN_DIR (/opt/cni/bin) TT_CNI_CONF_DIR (/etc/cni/net.d)
#   TT_ACTIVE_CONFLIST (10-azure.conflist) TT_BACKUP_ROOT (/opt/cni)
#   TT_KUBELET_RESTART ("systemctl restart kubelet")   node-path/command seams
#   AZ_BIN ORAS_BIN          tool seams (default az/oras)
#
# Commands: all(default) | workers | pull | render-node | names | help
# Outputs: installed_workers control_plane_excluded cni_reference cni_digest
#
# Traceability: ITEM-030, FR-019, FR-020, TTS-001, NFR-010, SEC-006, RD-016,
# CON-010, RISK-007, PRD Section 3.6 / docs/transparent-tunnel-same-node-enforcement-test.md 4.1.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"

lib::require_cmds jq base64 sha256sum

# ---- inputs -----------------------------------------------------------------
TOPOLOGY="${TOPOLOGY:-}"
REGION="${REGION:-$NAMING_SECONDARY_REGION}"     # TT is centraluseuap-only (CON-010)
PRIMARY_SUBSCRIPTION_ID="${PRIMARY_SUBSCRIPTION_ID:-}"
SECONDARY_SUBSCRIPTION_ID="${SECONDARY_SUBSCRIPTION_ID:-}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
CNI_REFERENCE="${CNI_REFERENCE:-}"
CNI_DIGEST="${CNI_DIGEST:-}"
CNI_BINARY_SHA256="${CNI_BINARY_SHA256:-}"
STAGING_ACR="${STAGING_ACR:-}"
CNI_ARTIFACT_DIR="${CNI_ARTIFACT_DIR:-}"
AZ_BIN="${AZ_BIN:-az}"
ORAS_BIN="${ORAS_BIN:-oras}"

# Node-side paths / commands embedded literally into the run-command payload.
TT_CNI_BIN_DIR="${TT_CNI_BIN_DIR:-/opt/cni/bin}"
TT_CNI_CONF_DIR="${TT_CNI_CONF_DIR:-/etc/cni/net.d}"
TT_ACTIVE_CONFLIST="${TT_ACTIVE_CONFLIST:-10-azure.conflist}"
TT_BACKUP_ROOT="${TT_BACKUP_ROOT:-/opt/cni}"
TT_KUBELET_RESTART="${TT_KUBELET_RESTART:-systemctl restart kubelet}"

CNI_BINARY_NAME="azure-vnet"
CNI_CONFLIST_NAME="azure-linux-transparent-tunnel.conflist"
PULLDIR="${CNI_PULLDIR:-${HERE}/../../.cni-install/pull}"

declare -gA N=()

tt::_require_inputs() {
  [[ -n "$TOPOLOGY" ]]                || log::die "TOPOLOGY is required (ss|xs)"
  [[ -n "$PRIMARY_SUBSCRIPTION_ID" ]] || log::die "PRIMARY_SUBSCRIPTION_ID is required (explicit subscription; RD-020)"
}

tt::_derive() {
  [[ "${TT_DERIVED:-0}" == "1" ]] && return 0
  tt::_require_inputs
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

  # CNI coordinates: explicit env wins; otherwise read what build_cni recorded.
  if [[ -f "$MANIFEST_PATH" ]]; then
    [[ -n "$CNI_REFERENCE" ]] || CNI_REFERENCE="$(manifest::get "$MANIFEST_PATH" '.artifacts.cni.reference // empty')"
    [[ -n "$CNI_DIGEST" ]]    || CNI_DIGEST="$(manifest::get "$MANIFEST_PATH" '.artifacts.cni.digest // empty')"
    [[ -n "$CNI_BINARY_SHA256" ]] || CNI_BINARY_SHA256="$(manifest::get "$MANIFEST_PATH" '.artifacts.cni.binary_sha256 // empty')"
  fi
  TT_WORKERS=( "${N[worker1_vm]}" "${N[worker2_vm]}" "${N[worker3_vm]}" )
  TT_DERIVED=1
}

# ---- pull the candidate CNI by digest to the runner and verify integrity -----
tt::_pull() {
  [[ "${TT_PULLED:-0}" == "1" ]] && return 0
  tt::_derive
  rm -rf "$PULLDIR"; mkdir -p "$PULLDIR"
  if [[ -n "$CNI_ARTIFACT_DIR" ]]; then
    log::info "using locally provided CNI artifact bytes (${CNI_ARTIFACT_DIR})"
    cp -f "${CNI_ARTIFACT_DIR}/${CNI_BINARY_NAME}"   "${PULLDIR}/${CNI_BINARY_NAME}"
    cp -f "${CNI_ARTIFACT_DIR}/${CNI_CONFLIST_NAME}" "${PULLDIR}/${CNI_CONFLIST_NAME}"
  else
    [[ -n "$CNI_REFERENCE" ]] || log::die "no CNI reference to pull (set CNI_REFERENCE or ensure artifacts.cni in ${MANIFEST_PATH})"
    [[ "$CNI_REFERENCE" == *@sha256:* ]] || log::die "CNI reference MUST be pinned by @sha256 digest (got '${CNI_REFERENCE}', FR-002)"
    lib::require_cmds "$ORAS_BIN" "$AZ_BIN"
    local registry="${CNI_REFERENCE%%/*}" acr_name
    acr_name="${STAGING_ACR%%.*}"; [[ -n "$acr_name" ]] || acr_name="${registry%%.*}"
    log::info "authenticating to staging ACR '${acr_name}' via OIDC (oras pull by digest)"
    lib::retry 3 5 -- "$AZ_BIN" acr login --name "$acr_name" || log::die "az acr login failed for ${acr_name}"
    log::info "pulling CNI artifact by digest: ${CNI_REFERENCE}"
    ( cd "$PULLDIR" && lib::retry 3 5 -- "$ORAS_BIN" pull "$CNI_REFERENCE" ) \
      || log::die "oras pull failed for ${CNI_REFERENCE}"
  fi
  [[ -s "${PULLDIR}/${CNI_BINARY_NAME}" ]]   || log::die "pulled artifact is missing ${CNI_BINARY_NAME}"
  [[ -s "${PULLDIR}/${CNI_CONFLIST_NAME}" ]] || log::die "pulled artifact is missing ${CNI_CONFLIST_NAME}"

  # Integrity: the pulled binary MUST match the recorded checksum (NFR-010).
  local actual; actual="$(sha256sum "${PULLDIR}/${CNI_BINARY_NAME}" | cut -c1-64)"
  if [[ -n "$CNI_BINARY_SHA256" ]]; then
    [[ "$actual" == "$CNI_BINARY_SHA256" ]] \
      || log::die "pulled CNI binary checksum mismatch: recorded=${CNI_BINARY_SHA256} actual=${actual} (NFR-010/SEC-006)"
    log::info "pulled CNI binary checksum verified (${actual})"
  else
    CNI_BINARY_SHA256="$actual"
    log::warn "no recorded CNI checksum found; using pulled-binary checksum for node verification (${actual})"
  fi
  grep -q '"mode":[[:space:]]*"transparent-tunnel"' "${PULLDIR}/${CNI_CONFLIST_NAME}" \
    || log::die "pulled conflist does not declare \"mode\": \"transparent-tunnel\""
  # The exact bytes are shipped inline in the run-command payload (SEC-006/RD-016).
  # `az vm run-command invoke --scripts` has a payload ceiling (~256KB); a large
  # azure-vnet binary may exceed it and require chunked transfer or a runtime-
  # scoped read-only SAS pull. Surface this at runtime rather than failing late.
  local _bin_sz; _bin_sz="$(wc -c < "${PULLDIR}/${CNI_BINARY_NAME}")"
  if (( _bin_sz > 262144 )); then
    log::warn "azure-vnet is ${_bin_sz} bytes; inline az vm run-command distribution has a ~256KB payload limit - chunked transfer may be required for a production-sized binary"
  fi
  NODE_SHA="$CNI_BINARY_SHA256"
  TT_PULLED=1
}

# ---- per-worker node install script (raw; azrun wraps it for exec) -----------
tt::_node_script() {
  local bin_b64 conf_b64
  bin_b64="$(base64 -w0 "${PULLDIR}/${CNI_BINARY_NAME}")"
  conf_b64="$(base64 -w0 "${PULLDIR}/${CNI_CONFLIST_NAME}")"
  cat <<NODE
set -euo pipefail
BIN_DIR='${TT_CNI_BIN_DIR}'
CONF_DIR='${TT_CNI_CONF_DIR}'
ACTIVE_CONFLIST='${TT_ACTIVE_CONFLIST}'
BACKUP_ROOT='${TT_BACKUP_ROOT}'
RECORDED_SHA='${NODE_SHA}'
mkdir -p "\$BIN_DIR" "\$CONF_DIR"
TS="\$(date -u +%Y%m%d-%H%M%S)"
BK="\${BACKUP_ROOT}/tt-backup-\${TS}"
mkdir -p "\$BK"
# 1) Back up the current CNI binary + conflists (rollback path; §6).
cp -a "\${BIN_DIR}/azure-vnet" "\$BK"/ 2>/dev/null || true
cp -a "\${CONF_DIR}"/*.conflist "\$BK"/ 2>/dev/null || true
[ -d "\$BK" ] || { echo "ERROR: backup dir \$BK not created"; exit 1; }
echo "backup at \$BK"
# 2) Install the exact digest-verified azure-vnet bytes shipped in this payload.
printf '%s' '${bin_b64}' | base64 -d > "\${BIN_DIR}/azure-vnet.new"
ACTUAL="\$(sha256sum "\${BIN_DIR}/azure-vnet.new" | cut -c1-64)"
[ "\$ACTUAL" = "\$RECORDED_SHA" ] || { echo "ERROR: distributed azure-vnet checksum mismatch (\$ACTUAL != \$RECORDED_SHA)"; rm -f "\${BIN_DIR}/azure-vnet.new"; exit 1; }
chmod +x "\${BIN_DIR}/azure-vnet.new"
mv "\${BIN_DIR}/azure-vnet.new" "\${BIN_DIR}/azure-vnet"
# 3) Swap the active conflist to the transparent-tunnel one and assert the mode.
printf '%s' '${conf_b64}' | base64 -d > "\${CONF_DIR}/\${ACTIVE_CONFLIST}"
grep -q '"mode":[[:space:]]*"transparent-tunnel"' "\${CONF_DIR}/\${ACTIVE_CONFLIST}" \
  || { echo "ERROR: active conflist is not transparent-tunnel"; exit 1; }
# 4) Restart kubelet so new sandboxes use the new mode.
${TT_KUBELET_RESTART}
echo "TT_INSTALL_OK mode=transparent-tunnel backup=\${BK}"
NODE
}

# ---- install onto every worker (control-plane excluded, TTS-001) -------------
tt::workers() {
  tt::_pull
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  manifest::put "$MANIFEST_PATH" validate.tt.cni_reference "${CNI_REFERENCE:-<local>}"
  manifest::put "$MANIFEST_PATH" validate.tt.cni_digest "${CNI_DIGEST:-}"
  local vm rc=0 installed=0 script
  script="$(tt::_node_script)"
  for vm in "${TT_WORKERS[@]}"; do
    log::info "installing transparent-tunnel CNI on worker ${vm} (subscription ${CLUSTER_SUB})"
    if azrun::exec "$AZ_BIN" "$CLUSTER_SUB" "${N[resource_group]}" "$vm" "$script"; then
      manifest::put_json "$MANIFEST_PATH" "validate.tt.install.${vm}" \
        "$(jq -n --arg s ok '{status:$s}')"
      installed=$(( installed + 1 ))
    else
      manifest::put_json "$MANIFEST_PATH" "validate.tt.install.${vm}" \
        "$(jq -n --arg s fail '{status:$s}')"
      log::error "transparent-tunnel install FAILED on ${vm}"
      rc=1
    fi
  done
  # The control-plane node is deliberately NOT installed (TTS-001 / CON-010).
  log::info "control-plane ${N[cp_vm]} intentionally left on stock CNI (TTS-001)"
  manifest::put "$MANIFEST_PATH" validate.tt.control_plane_excluded "${N[cp_vm]}"
  manifest::put "$MANIFEST_PATH" validate.tt.workers_installed "$installed"

  gha::output installed_workers "$installed"
  gha::output control_plane_excluded "${N[cp_vm]}"
  gha::output cni_reference "${CNI_REFERENCE:-<local>}"
  gha::output cni_digest "${CNI_DIGEST:-}"
  gha::summary "## Transparent-tunnel CNI install (\`validate_tt\`)"
  gha::summary ""
  gha::summary "| Field | Value |"
  gha::summary "|---|---|"
  gha::summary "| Workers installed | ${installed}/${#TT_WORKERS[@]} |"
  gha::summary "| Control-plane | \`${N[cp_vm]}\` (stock CNI, excluded) |"
  gha::summary "| CNI reference | \`${CNI_REFERENCE:-<local>}\` |"
  if (( rc == 0 )); then log::info "transparent-tunnel installed on ${installed} worker(s)"; else log::error "one or more worker installs failed"; fi
  return "$rc"
}

# ---- render-node: print the raw node script (debug / sandbox tests) ----------
tt::render_node() {
  tt::_pull
  local idx="${TT_WORKER_INDEX:-1}"
  (( idx >= 1 && idx <= ${#TT_WORKERS[@]} )) || log::die "TT_WORKER_INDEX must be 1..${#TT_WORKERS[@]}"
  log::info "rendering node install script for worker ${TT_WORKERS[$(( idx - 1 ))]}" >&2
  tt::_node_script
}

tt::names() {
  tt::_derive
  printf 'topology=%s\nregion=%s\nresource_group=%s\ncluster_subscription=%s\ncp_vm=%s\nworkers=%s\ncni_reference=%s\n' \
    "$TCODE" "$REGION" "${N[resource_group]}" "$CLUSTER_SUB" "${N[cp_vm]}" "${TT_WORKERS[*]}" "${CNI_REFERENCE:-<local>}"
}

tt::usage() {
  cat <<USAGE
Usage: install-cni-tt.sh <command>

Commands:
  all | workers   Install transparent-tunnel on every centraluseuap worker
                  (control-plane excluded, TTS-001) using digest-verified bytes.
  pull            Pull+verify the candidate CNI artifact to the runner only.
  render-node     Print the raw per-worker node install script (TT_WORKER_INDEX).
  names           Print resolved cluster/worker/CNI names.
  help            Show this help.
USAGE
}

tt::main() {
  local cmd="${1:-all}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    all|workers)  tt::workers ;;
    pull)         tt::_pull; log::info "pull+verify OK" ;;
    render-node)  tt::render_node ;;
    names)        tt::names ;;
    help|-h|--help) tt::usage ;;
    *) log::error "unknown command: ${cmd}"; tt::usage >&2; return 1 ;;
  esac
}

tt::main "$@"
