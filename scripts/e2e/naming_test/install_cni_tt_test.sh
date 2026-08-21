#!/usr/bin/env bash
# =============================================================================
# install_cni_tt_test.sh - behavioural tests for ../install-cni-tt.sh
# (EPIC-009 / ITEM-030, TTS-001 / FR-019 / FR-020).
#
# Fully hermetic. `az` is a deterministic mock standing in for the ONLY node
# access path (`az vm run-command invoke`); it captures the per-VM install
# scripts so the suite proves (a) transparent-tunnel is distributed to every
# centraluseuap WORKER but NEVER the control-plane (TTS-001), (b) the exact
# digest-verified bytes are shipped in the run-command payload (no node-side
# network/credential, SEC-006/RD-016), (c) the runner refuses to distribute when
# the pulled bytes do not match the recorded checksum (NFR-010), and (d) the
# rendered node script actually backs up, checksum-verifies, swaps the conflist
# (mode assertion), and restarts kubelet when executed in a sandbox fake-node.
#
# Traceability: ITEM-030, FR-019, FR-020, TTS-001, NFR-010, SEC-006, RD-016,
# CON-010, RISK-007, PRD Section 3.6 / docs/transparent-tunnel-same-node-enforcement-test.md 4.1.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_SH="${TEST_DIR}/../install-cni-tt.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.install_cni_tt_test_work"
MOCKBIN="${WORK}/bin"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi; }
assert_match() { if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2; fi; }
assert_nomatch() { if [[ "$3" =~ $2 ]]; then fail "$1"; printf '        value unexpectedly matched /%s/\n' "$2" >&2; else pass "$1"; fi; }
assert_nonzero() { if (( $2 != 0 )); then pass "$1"; else fail "$1"; fi; }

if [[ ! -f "$INSTALL_SH" ]]; then
  printf 'FATAL install-cni-tt.sh not found at %s\n' "$INSTALL_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

# Deterministic centraluseuap (Cluster B) names for the documented run context.
B_RG="pnc-e2e-ss-20260820-vahdkc-centraluseuap"
W1="${B_RG}-worker-01"; W2="${B_RG}-worker-02"; W3="${B_RG}-worker-03"
CP="${B_RG}-cp-01"

# Pulled CNI artifact bytes (as if oras-pulled by digest to the runner).
ART="${WORK}/artifact"
mkdir -p "$ART"
printf 'MOCK-tt-azure-vnet-bytes\n' > "${ART}/azure-vnet"
cat > "${ART}/azure-linux-transparent-tunnel.conflist" <<'CONF'
{ "cniVersion":"0.3.0","name":"azure",
  "plugins":[ {"type":"azure-vnet","mode":"transparent-tunnel","bridge":"azure0"} ] }
CONF
BIN_SHA="$(sha256sum "${ART}/azure-vnet" | cut -c1-64)"

# ---- mock az: capture per-VM run-command scripts, return success sentinel ----
cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
vm=""; scripts=""; prev=""
for a in "$@"; do case "$prev" in -n) vm="$a" ;; --scripts) scripts="$a" ;; esac; prev="$a"; done
case "$1 $2" in
  "vm run-command")
    [ -n "$vm" ] && printf '%s' "$scripts" > "${MOCK_CAP}/${vm}.script"
    printf '[stdout]\n__AZRUN_OK__\n[stderr]\n'; exit 0 ;;
  "acr login") exit 0 ;;
  *) exit 0 ;;
esac
AZ
chmod +x "${MOCKBIN}/az"

# run_install <case> <cmd> <KEY=VAL...>; sets RC, AZLOG, CAP, MANIFEST, LOG, OUT.
run_install() {
  local name="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; CAP="${casedir}/cap"; MANIFEST="${casedir}/run-manifest.json"
  LOG="${casedir}/log"; OUT="${casedir}/out"
  mkdir -p "$casedir" "$CAP"; : > "$AZLOG"
  manifest::init "$MANIFEST"
  # Seed the CNI artifact coordinates as build_cni would (recorded checksum).
  manifest::record_cni_artifact "$MANIFEST" "pncstg.azurecr.io" \
    "candidate/pod-nsg-cni-transparent-tunnel" "run-20260820-vahdkc" \
    "sha256:$(printf cni | sha256sum | cut -c1-64)" true
  manifest::put "$MANIFEST" artifacts.cni.binary_sha256 "${SEED_SHA:-$BIN_SHA}"
  env \
    MOCK_AZ_LOG="$AZLOG" MOCK_CAP="$CAP" AZ_BIN="${MOCKBIN}/az" \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" \
    DATE_UTC="20260820" GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
    MANIFEST_PATH="$MANIFEST" CNI_ARTIFACT_DIR="$ART" \
    AZRUN_ATTEMPTS="2" AZRUN_DELAY="0" \
    GITHUB_OUTPUT="$OUT" GITHUB_STEP_SUMMARY="${casedir}/summary.md" \
    "$@" bash "$INSTALL_SH" "$cmd" >"$LOG" 2>"${LOG}.err"
  RC=$?
}
out_val() { grep -E "^$1=" "$OUT" 2>/dev/null | tail -1 | cut -d= -f2-; }

echo "== fail fast on missing inputs =="
run_install miss_topo all PRIMARY_SUBSCRIPTION_ID=sub-a TOPOLOGY=""
assert_nonzero "missing TOPOLOGY fails" "$RC"
run_install miss_sub all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=""
assert_nonzero "missing PRIMARY_SUBSCRIPTION_ID fails" "$RC"

echo "== install distributes TT to every worker, EXCLUDES control-plane (TTS-001) =="
run_install ok all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary
assert_eq "install succeeds" "0" "$RC"
assert_eq "worker-01 received an install script" "yes" "$([[ -f "${CAP}/${W1}.script" ]] && echo yes || echo no)"
assert_eq "worker-02 received an install script" "yes" "$([[ -f "${CAP}/${W2}.script" ]] && echo yes || echo no)"
assert_eq "worker-03 received an install script" "yes" "$([[ -f "${CAP}/${W3}.script" ]] && echo yes || echo no)"
assert_eq "control-plane node is NOT touched (stays stock CNI, TTS-001)" "no" \
  "$([[ -f "${CAP}/${CP}.script" ]] && echo yes || echo no)"
assert_match "every run-command carries an explicit --subscription (RD-020)" \
  'run-command .*--subscription sub-primary' "$(grep 'run-command' "$AZLOG" | tr '\n' '|')"
assert_eq "install recorded per-worker in manifest" "ok" \
  "$(manifest::get "$MANIFEST" ".validate.tt.install.\"${W1}\".status")"
assert_eq "output reports 3 workers installed" "3" "$(out_val installed_workers)"
assert_eq "output reports the excluded control-plane node" "$CP" "$(out_val control_plane_excluded)"

echo "== the worker payload ships exact bytes, checksum, mode, backup, kubelet restart =="
S="$(cat "${CAP}/${W1}.script")"
assert_match "payload backs up existing CNI to /opt/cni/tt-backup-<ts>" 'tt-backup-' "$S"
assert_match "payload decodes base64 bytes on the node (no node-side curl)" 'base64 -d' "$S"
assert_nomatch "payload does NOT fetch from any URL (SEC-006)" 'curl|https?://' "$S"
assert_match "payload verifies the recorded sha256 on the node (NFR-010)" "$BIN_SHA" "$S"
assert_match "payload asserts conflist mode transparent-tunnel (FR-020)" 'transparent-tunnel' "$S"
assert_match "payload restarts kubelet" 'systemctl restart kubelet' "$S"
assert_match "payload installs the azure-vnet binary" 'azure-vnet' "$S"
assert_match "payload targets the real /opt/cni/bin path" '/opt/cni/bin' "$S"

echo "== runner refuses to distribute when pulled bytes != recorded checksum (NFR-010) =="
SEED_SHA="0000000000000000000000000000000000000000000000000000000000000000" \
  run_install badsum all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary
assert_nonzero "checksum mismatch between pulled bytes and manifest fails fast" "$RC"
assert_eq "no worker was touched on a checksum failure" "no" \
  "$([[ -f "${CAP}/${W1}.script" ]] && echo yes || echo no)"

echo "== rendered node script actually installs when executed in a sandbox fake-node =="
SANDBOX="${WORK}/node"
mkdir -p "${SANDBOX}/opt/cni/bin" "${SANDBOX}/etc/cni/net.d"
printf 'OLD-stock-azure-vnet\n' > "${SANDBOX}/opt/cni/bin/azure-vnet"
printf '{ "name":"azure","plugins":[{"type":"azure-vnet","mode":"transparent"}] }\n' \
  > "${SANDBOX}/etc/cni/net.d/10-azure.conflist"
run_install render render-node TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary \
  TT_WORKER_INDEX=1 \
  TT_CNI_BIN_DIR="${SANDBOX}/opt/cni/bin" TT_CNI_CONF_DIR="${SANDBOX}/etc/cni/net.d" \
  TT_BACKUP_ROOT="${SANDBOX}/opt/cni" TT_KUBELET_RESTART=":"
assert_eq "render-node succeeds" "0" "$RC"
# The node script is printed to LOG; execute it in the sandbox.
bash "$LOG" >"${WORK}/node_run.out" 2>&1; NODE_RC=$?
assert_eq "node install script runs clean in the sandbox" "0" "$NODE_RC"
assert_eq "installed azure-vnet bytes equal the pulled artifact" \
  "$(sha256sum "${ART}/azure-vnet" | cut -c1-64)" \
  "$(sha256sum "${SANDBOX}/opt/cni/bin/azure-vnet" | cut -c1-64)"
assert_match "active conflist is now transparent-tunnel" 'transparent-tunnel' \
  "$(cat "${SANDBOX}/etc/cni/net.d/10-azure.conflist")"
if ls -d "${SANDBOX}"/opt/cni/tt-backup-* >/dev/null 2>&1; then
  pass "a /opt/cni/tt-backup-<ts> directory was created"; else fail "backup dir must be created"; fi
if grep -rq 'OLD-stock-azure-vnet' "${SANDBOX}"/opt/cni/tt-backup-*/ 2>/dev/null; then
  pass "the prior azure-vnet binary was backed up (rollback path)"; else fail "prior binary must be backed up"; fi

echo "== node script REJECTS tampered bytes (checksum bite, sandbox) =="
SANDBOX2="${WORK}/node2"; mkdir -p "${SANDBOX2}/opt/cni/bin" "${SANDBOX2}/etc/cni/net.d"
printf 'OLD\n' > "${SANDBOX2}/opt/cni/bin/azure-vnet"
run_install render2 render-node TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary TT_WORKER_INDEX=1 \
  TT_CNI_BIN_DIR="${SANDBOX2}/opt/cni/bin" TT_CNI_CONF_DIR="${SANDBOX2}/etc/cni/net.d" \
  TT_BACKUP_ROOT="${SANDBOX2}/opt/cni" TT_KUBELET_RESTART=":"
# Corrupt the recorded sha embedded in the script to simulate tampered transit.
sed 's/'"$BIN_SHA"'/deadbeef/' "$LOG" > "${WORK}/tampered.sh"
bash "${WORK}/tampered.sh" >/dev/null 2>&1; TRC=$?
assert_nonzero "node script fails when the distributed bytes fail the checksum" "$TRC"

echo
printf 'install_cni_tt_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
