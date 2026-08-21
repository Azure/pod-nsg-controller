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
# bytes are shipped in bounded, ordered Run Command chunks - never fetched on
# the node from a URL/credential (SEC-006/RD-016). Each worker reassembles into
# a private staging file and re-verifies size and checksum before swapping.
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
#   TT_RUN_COMMAND_MAX_BYTES (60000) TT_CHUNK_RAW_BYTES (32768)
#   TT_TRANSFER_ROOT (/var/lib/pnc-e2e-cni/<run-suffix>)
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
TT_RUN_COMMAND_MAX_BYTES="${TT_RUN_COMMAND_MAX_BYTES:-60000}"
TT_CHUNK_RAW_BYTES="${TT_CHUNK_RAW_BYTES:-32768}"
TT_AZRUN_WRAPPER_RESERVE_BYTES="${TT_AZRUN_WRAPPER_RESERVE_BYTES:-1024}"
TT_TRANSFER_ROOT="${TT_TRANSFER_ROOT:-}"

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
  : "${TT_TRANSFER_ROOT:=/var/lib/pnc-e2e-cni/${RUN_SUFFIX}}"
  if ! [[ "$TT_RUN_COMMAND_MAX_BYTES" =~ ^[0-9]+$ ]] \
    || (( TT_RUN_COMMAND_MAX_BYTES < 4096 || TT_RUN_COMMAND_MAX_BYTES > 240000 )); then
    log::die "TT_RUN_COMMAND_MAX_BYTES must be between 4096 and 240000"
  fi
  if ! [[ "$TT_CHUNK_RAW_BYTES" =~ ^[0-9]+$ ]] || (( TT_CHUNK_RAW_BYTES <= 0 )); then
    log::die "TT_CHUNK_RAW_BYTES must be a positive integer"
  fi
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
  NODE_SIZE="$(wc -c < "${PULLDIR}/${CNI_BINARY_NAME}" | tr -d ' ')"
  NODE_SHA="$CNI_BINARY_SHA256"
  TT_PULLED=1
}

# ---- bounded, ordered per-worker transfer -----------------------------------
tt::_exec_bounded() {
  local vm="$1" script="$2" bytes
  bytes="$(printf '%s' "$script" | wc -c | tr -d ' ')"
  if (( bytes + TT_AZRUN_WRAPPER_RESERVE_BYTES > TT_RUN_COMMAND_MAX_BYTES )); then
    log::error "refusing Run Command payload for ${vm}: ${bytes}+${TT_AZRUN_WRAPPER_RESERVE_BYTES} exceeds safe limit ${TT_RUN_COMMAND_MAX_BYTES}"
    return 1
  fi
  azrun::exec "$AZ_BIN" "$CLUSTER_SUB" "${N[resource_group]}" "$vm" "$script"
}

tt::_transfer_init_script() {
  cat <<NODE
set -euo pipefail
TRANSFER_ROOT='${TT_TRANSFER_ROOT}'
rm -rf "\$TRANSFER_ROOT"
install -d -m 0700 "\$TRANSFER_ROOT"
: > "\$TRANSFER_ROOT/${CNI_BINARY_NAME}.part"
chmod 0600 "\$TRANSFER_ROOT/${CNI_BINARY_NAME}.part"
NODE
}

tt::_chunk_script() {
  local index="$1" offset="$2" chunk_len="$3" chunk_sha="$4" chunk_b64="$5"
  local new_size=$(( offset + chunk_len ))
  cat <<NODE
set -euo pipefail
TRANSFER_FILE='${TT_TRANSFER_ROOT}/${CNI_BINARY_NAME}.part'
CHUNK_INDEX='${index}'
EXPECTED_OFFSET='${offset}'
CHUNK_LENGTH='${chunk_len}'
EXPECTED_SIZE='${new_size}'
CHUNK_SHA='${chunk_sha}'
[ -f "\$TRANSFER_FILE" ] || { echo "ERROR: transfer file absent for chunk \$CHUNK_INDEX"; exit 1; }
ACTUAL_SIZE="\$(wc -c < "\$TRANSFER_FILE" | tr -d ' ')"
if [ "\$ACTUAL_SIZE" = "\$EXPECTED_SIZE" ]; then
  ACTUAL_CHUNK_SHA="\$(tail -c "\$CHUNK_LENGTH" "\$TRANSFER_FILE" | sha256sum | cut -c1-64)"
  [ "\$ACTUAL_CHUNK_SHA" = "\$CHUNK_SHA" ] || { echo "ERROR: retried chunk \$CHUNK_INDEX differs"; exit 1; }
  exit 0
fi
[ "\$ACTUAL_SIZE" = "\$EXPECTED_OFFSET" ] || { echo "ERROR: out-of-order chunk \$CHUNK_INDEX (offset \$ACTUAL_SIZE != \$EXPECTED_OFFSET)"; exit 1; }
printf '%s' '${chunk_b64}' | base64 -d >> "\$TRANSFER_FILE"
ACTUAL_SIZE="\$(wc -c < "\$TRANSFER_FILE" | tr -d ' ')"
[ "\$ACTUAL_SIZE" = "\$EXPECTED_SIZE" ] || { echo "ERROR: chunk \$CHUNK_INDEX length mismatch"; exit 1; }
ACTUAL_CHUNK_SHA="\$(tail -c "\$CHUNK_LENGTH" "\$TRANSFER_FILE" | sha256sum | cut -c1-64)"
[ "\$ACTUAL_CHUNK_SHA" = "\$CHUNK_SHA" ] || { echo "ERROR: chunk \$CHUNK_INDEX checksum mismatch"; exit 1; }
NODE
}

tt::_transfer_worker() {
  local vm="$1" source="${PULLDIR}/${CNI_BINARY_NAME}"
  local index=0 offset=0 chunk_file="${PULLDIR}/.${CNI_BINARY_NAME}.chunk"
  local chunk_len chunk_sha chunk_b64 script

  tt::_exec_bounded "$vm" "$(tt::_transfer_init_script)" || return 1
  while (( offset < NODE_SIZE )); do
    dd if="$source" of="$chunk_file" bs="$TT_CHUNK_RAW_BYTES" skip="$index" count=1 status=none
    chunk_len="$(wc -c < "$chunk_file" | tr -d ' ')"
    (( chunk_len > 0 )) || { rm -f "$chunk_file"; log::error "empty transfer chunk ${index}"; return 1; }
    chunk_sha="$(sha256sum "$chunk_file" | cut -c1-64)"
    chunk_b64="$(base64 -w0 "$chunk_file")"
    script="$(tt::_chunk_script "$index" "$offset" "$chunk_len" "$chunk_sha" "$chunk_b64")"
    if ! tt::_exec_bounded "$vm" "$script"; then
      rm -f "$chunk_file"
      return 1
    fi
    offset=$(( offset + chunk_len ))
    index=$(( index + 1 ))
  done
  rm -f "$chunk_file"
  tt::_exec_bounded "$vm" "$(tt::_node_script)"
}

# ---- per-worker node install script (raw; azrun wraps it for exec) -----------
tt::_node_script() {
  local conf_b64
  conf_b64="$(base64 -w0 "${PULLDIR}/${CNI_CONFLIST_NAME}")"
  cat <<NODE
set -euo pipefail
BIN_DIR='${TT_CNI_BIN_DIR}'
CONF_DIR='${TT_CNI_CONF_DIR}'
ACTIVE_CONFLIST='${TT_ACTIVE_CONFLIST}'
BACKUP_ROOT='${TT_BACKUP_ROOT}'
RECORDED_SHA='${NODE_SHA}'
RECORDED_SIZE='${NODE_SIZE}'
TRANSFER_ROOT='${TT_TRANSFER_ROOT}'
TRANSFER_FILE="\${TRANSFER_ROOT}/${CNI_BINARY_NAME}.part"
cleanup_transfer() { rm -rf "\$TRANSFER_ROOT"; }
trap cleanup_transfer EXIT
mkdir -p "\$BIN_DIR" "\$CONF_DIR"
if [ ! -f "\$TRANSFER_FILE" ]; then
  INSTALLED_SHA="\$(sha256sum "\${BIN_DIR}/azure-vnet" 2>/dev/null | cut -c1-64 || true)"
  grep -q '"mode":[[:space:]]*"transparent-tunnel"' "\${CONF_DIR}/\${ACTIVE_CONFLIST}" 2>/dev/null \
    && [ "\$INSTALLED_SHA" = "\$RECORDED_SHA" ] \
    && { echo "TT_INSTALL_OK already-installed"; exit 0; }
  echo "ERROR: reassembled transfer file is absent"
  exit 1
fi
ACTUAL_SIZE="\$(wc -c < "\$TRANSFER_FILE" | tr -d ' ')"
[ "\$ACTUAL_SIZE" = "\$RECORDED_SIZE" ] || { echo "ERROR: reassembled azure-vnet size mismatch (\$ACTUAL_SIZE != \$RECORDED_SIZE)"; exit 1; }
ACTUAL="\$(sha256sum "\$TRANSFER_FILE" | cut -c1-64)"
[ "\$ACTUAL" = "\$RECORDED_SHA" ] || { echo "ERROR: reassembled azure-vnet checksum mismatch (\$ACTUAL != \$RECORDED_SHA)"; exit 1; }
TS="\$(date -u +%Y%m%d-%H%M%S)"
BK="\${BACKUP_ROOT}/tt-backup-\${TS}"
mkdir -p "\$BK"
# 1) Back up the current CNI binary + conflists (rollback path; §6).
cp -a "\${BIN_DIR}/azure-vnet" "\$BK"/ 2>/dev/null || true
cp -a "\${CONF_DIR}"/*.conflist "\$BK"/ 2>/dev/null || true
[ -d "\$BK" ] || { echo "ERROR: backup dir \$BK not created"; exit 1; }
echo "backup at \$BK"
# 2) Install the exact digest-verified azure-vnet bytes reassembled from chunks.
cp "\$TRANSFER_FILE" "\${BIN_DIR}/azure-vnet.new"
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
  for vm in "${TT_WORKERS[@]}"; do
    log::info "installing transparent-tunnel CNI on worker ${vm} (subscription ${CLUSTER_SUB})"
    if tt::_transfer_worker "$vm"; then
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
