#!/usr/bin/env bash
# =============================================================================
# collect_cni_diagnostics_test.sh - behavioural tests for ../collect-cni-diagnostics.sh
# (EPIC-009 / ITEM-034, FR-023).
#
# Fully hermetic. `az` is a deterministic mock for the worker/control-plane
# run-command captures and the addressPrefixSets REST GET. The suite proves the
# collector captures ALL FR-023 evidence (per-worker conflist mode + kubelet +
# backup presence, ip rule / policy route tables / fwmark, azv* veth mapping,
# the eth0 /tmp/tt-eth0.pcap retrieval, ASG membership, controller logs) into an
# uploadable directory, and that collection is BEST-EFFORT: a failing node
# capture never aborts the run (diagnostics must appear on pass AND forced-fail).
#
# Traceability: ITEM-034, FR-023, AC-007, TTS-007, RD-020,
# docs/transparent-tunnel-same-node-enforcement-test.md 4.5.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIAG_SH="${TEST_DIR}/../collect-cni-diagnostics.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.collect_cni_diagnostics_test_work"
MOCKBIN="${WORK}/bin"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi; }
assert_file_has() { if [[ -f "$2" ]] && grep -q -- "$3" "$2"; then pass "$1"; else fail "$1"; printf '        %s missing or lacks /%s/\n' "$2" "$3" >&2; fi; }
assert_exists() { if [[ -s "$2" ]]; then pass "$1"; else fail "$1"; printf '        %s missing/empty\n' "$2" >&2; fi; }

if [[ ! -f "$DIAG_SH" ]]; then
  printf 'FATAL collect-cni-diagnostics.sh not found at %s\n' "$DIAG_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

B_RG="pnc-e2e-ss-20260820-vahdkc-centraluseuap"
W1="${B_RG}-worker-01"

cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
vm=""; scripts=""; url=""; query=""; prev=""
for a in "$@"; do case "$prev" in -n) vm="$a";; --scripts) scripts="$a";; --url) url="$a";; --query) query="$a";; esac; prev="$a"; done
emit(){ printf '[stdout]\n%s\n[stderr]\n' "$1"; }
case "$1 $2" in
  "rest --method")
    case "$url" in
      *asg-backend*)  printf '{"value":[{"name":"%s-default-backend-asg-mapping","properties":{"addressPrefixSet":["10.244.1.10/32"]}}]}'  "${MOCK_B_RG}" ;;
      *asg-frontend*) printf '{"value":[{"name":"%s-default-frontend-asg-mapping","properties":{"addressPrefixSet":["10.244.1.20/32"]}}]}' "${MOCK_B_RG}" ;;
      *) printf '{"value":[]}' ;;
    esac; exit 0 ;;
  "vm run-command")
    [ "$query" = "value[0].message" ] || { emit ok; exit 0; }
    if [ -n "${MOCK_DIAG_FAIL:-}" ] && [ "$vm" = "${MOCK_DIAG_FAIL}" ]; then exit 1; fi   # transport failure (best-effort)
    case "$scripts" in
      *CNI_DIAG_WORKER*) printf '[stdout]\n"mode": "transparent-tunnel"\nactive\n/opt/cni/tt-backup-20260820-000000\n---iprule---\n0:\tfrom all lookup local\n100:\tfrom all fwmark 0x1 lookup 200\n---iproute---\ndefault via 10.0.0.1 dev eth0\n---fwmark---\n-A PREROUTING -j MARK --set-xmark 0x1\n[stderr]\n' ;;
      *CNI_DIAG_VETH*)   printf '[stdout]\n10.244.1.10 dev azv1a2b3c src 10.0.0.4\n10.244.1.20 dev azv4d5e6f src 10.0.0.4\n[stderr]\n' ;;
      *CNI_DIAG_PCAP*)   printf '[stdout]\n%s\n[stderr]\n' "$(printf 'PCAPDATA-eth0-samenode' | base64 -w0)" ;;
      *CNI_DIAG_PODS*)   emit '{"items":[{"metadata":{"name":"backend-tt"},"status":{"podIP":"10.244.1.10"}},{"metadata":{"name":"frontend-tt"},"status":{"podIP":"10.244.1.20"}}]}' ;;
      *CNI_DIAG_LOGS*)   emit 'reconcile complete; asgSyncState Synced' ;;
      *) emit ok ;;
    esac; exit 0 ;;
  *) exit 0 ;;
esac
AZ
chmod +x "${MOCKBIN}/az"

run_diag() {
  local name="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; MANIFEST="${casedir}/run-manifest.json"; LOG="${casedir}/log"; DIAG="${casedir}/diag"
  mkdir -p "$casedir"; : > "$AZLOG"; manifest::init "$MANIFEST"
  env \
    MOCK_AZ_LOG="$AZLOG" MOCK_B_RG="$B_RG" AZ_BIN="${MOCKBIN}/az" \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" \
    DATE_UTC="20260820" GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
    MANIFEST_PATH="$MANIFEST" CNI_DIAG_DIR="$DIAG" AZRUN_ATTEMPTS="2" AZRUN_DELAY="0" \
    "$@" bash "$DIAG_SH" "$cmd" >"$LOG" 2>"${LOG}.err"
  RC=$?
}

echo "== collects ALL FR-023 evidence into an uploadable directory =="
run_diag ok all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary
assert_eq "collector exits 0 (best-effort)" "0" "$RC"
assert_file_has "per-worker conflist mode captured" "${DIAG}/worker-${W1}.txt" 'transparent-tunnel'
assert_file_has "per-worker kubelet status captured" "${DIAG}/worker-${W1}.txt" 'active'
assert_file_has "per-worker backup-dir presence captured" "${DIAG}/worker-${W1}.txt" '/opt/cni/tt-backup-'
assert_file_has "ip rule / policy routing captured" "${DIAG}/worker-${W1}.txt" 'fwmark'
assert_file_has "azv* veth mapping captured (ip route get)" "${DIAG}/veth.txt" 'azv'
assert_exists "eth0 pcap retrieved from /tmp/tt-eth0.pcap" "${DIAG}/tt-eth0.pcap"
assert_file_has "ASG membership captured via REST" "${DIAG}/asg-membership.json" 'backend-asg-mapping'
assert_file_has "controller logs captured" "${DIAG}/controller-logs.txt" 'reconcile'
assert_eq "manifest records diagnostics collection" "yes" \
  "$([[ "$(manifest::get "$MANIFEST" '.diagnostics.tt.collected // empty')" == "true" ]] && echo yes || echo no)"

echo "== pcap decodes to the exact captured bytes =="
assert_file_has "pcap content is the retrieved capture" "${DIAG}/tt-eth0.pcap" 'PCAPDATA-eth0-samenode'

echo "== best-effort: a failing worker capture does NOT abort collection (AC-007) =="
run_diag partial all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary MOCK_DIAG_FAIL="${W1}"
assert_eq "collector still exits 0 when a node capture fails" "0" "$RC"
assert_exists "membership still collected despite the worker failure" "${DIAG}/asg-membership.json"
assert_exists "controller logs still collected despite the worker failure" "${DIAG}/controller-logs.txt"

echo "== az REST membership uses the addressPrefixSets API (not effective-nsg) =="
assert_eq "no list-effective-nsg used" "0" "$(grep -c 'list-effective-nsg' "$AZLOG")"

echo
printf 'collect_cni_diagnostics_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
