#!/usr/bin/env bash
# =============================================================================
# run_tt_validation_test.sh - behavioural tests for ../run-tt-validation.sh
# (EPIC-009 / ITEM-032, TTS-001..TTS-007).
#
# Fully hermetic. `az` is a STATEFUL deterministic mock standing in for BOTH the
# centraluseuap workers (conflist mode / kubelet / backup on tts1) AND the
# control-plane node's kubectl (pod create/get, exec ping/nc) AND the ARM
# addressPrefixSets REST GET (tts2/tts3 membership). A correctly-implemented
# validator PASSES against a healthy data plane; per-scenario failure-injection
# knobs make exactly the matching documented pass criterion FAIL, proving the
# assertions bite:
#   MOCK_TT_WORKER_STOCK=<vm>  a worker still on stock CNI        -> tts1
#   MOCK_TT_CP_TT              control-plane wrongly on TT        -> tts1
#   MOCK_TT_NO_MEMBERSHIP      ASG prefix set empty               -> tts2
#   MOCK_TT_DIFF_NODE          pods land on different nodes       -> tts3
#   MOCK_TT_DENY_NOT_ENFORCED  backend->frontend ICMP allowed     -> tts4
#   MOCK_TT_ALLOW_BLOCKED      frontend->backend ICMP blocked     -> tts5
#   MOCK_TT_TCP_NOT_BLOCKED    backend->frontend:8080 not blocked -> tts7
#
# Traceability: ITEM-032, TTS-001..007, FR-021..023, TEST-012..017, AC-017..022,
# RD-020, docs/transparent-tunnel-same-node-enforcement-test.md 4.1-4.5/5.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TTV_SH="${TEST_DIR}/../run-tt-validation.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.run_tt_validation_test_work"
MOCKBIN="${WORK}/bin"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi; }
assert_pass() { assert_eq "$1" "0" "$2"; }
assert_fail() { if (( $2 != 0 )); then pass "$1"; else fail "$1"; fi; }
assert_match() { if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2; fi; }

if [[ ! -f "$TTV_SH" ]]; then
  printf 'FATAL run-tt-validation.sh not found at %s\n' "$TTV_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

B_RG="pnc-e2e-ss-20260820-vahdkc-centraluseuap"
NODE="${B_RG}-worker-01"

# ---- stateful mock az -------------------------------------------------------
cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
vm=""; scripts=""; url=""; query=""; prev=""
for a in "$@"; do
  case "$prev" in -n) vm="$a" ;; --scripts) scripts="$a" ;; --url) url="$a" ;; --query) query="$a" ;; esac
  prev="$a"
done
NODE="${MOCK_NODE}"; B_RG="${MOCK_B_RG}"
BACKEND_IP=10.244.1.10; FRONTEND_IP=10.244.1.20; CANARY_IP=10.244.1.30

emit() { printf '[stdout]\n%s\n[stderr]\n' "$1"; }

case "$1 $2" in
  "rest --method")
    if [ -n "${MOCK_TT_NO_MEMBERSHIP:-}" ]; then printf '{"value":[]}'; exit 0; fi
    case "$url" in
      *asg-backend*)  printf '{"value":[{"name":"%s-default-backend-asg-mapping","properties":{"provisioningState":"Succeeded","addressPrefixSet":["%s/32"]}}]}'  "$B_RG" "$BACKEND_IP" ;;
      *asg-frontend*) printf '{"value":[{"name":"%s-default-frontend-asg-mapping","properties":{"provisioningState":"Succeeded","addressPrefixSet":["%s/32"]}}]}' "$B_RG" "$FRONTEND_IP" ;;
      *) printf '{"value":[]}' ;;
    esac
    exit 0 ;;
  "vm run-command")
    [ "$query" = "value[0].message" ] || { emit ok; exit 0; }
    # ---- worker / control-plane conflist mode + kubelet + backup (tts1) ----
    if [ "${scripts#*TT_MODE_CHECK}" != "$scripts" ]; then
      case "$vm" in
        *-cp-01)
          if [ -n "${MOCK_TT_CP_TT:-}" ]; then emit '"mode": "transparent-tunnel"'; else emit '"mode": "transparent"'; fi ;;
        *)
          mode='"mode": "transparent-tunnel"'; kubelet=active; backup="/opt/cni/tt-backup-20260820-000000"
          [ "${MOCK_TT_WORKER_STOCK:-}" = "$vm" ] && mode='"mode": "transparent"'
          [ "${MOCK_TT_KUBELET_DOWN:-}" = "$vm" ] && kubelet=inactive
          [ "${MOCK_TT_NO_BACKUP:-}" = "$vm" ] && backup=""
          printf '[stdout]\n%s\n%s\n%s\n[stderr]\n' "$mode" "$kubelet" "$backup" ;;
      esac
      exit 0
    fi
    # ---- pcap start / read on the worker (tts7) ----
    if [ "${scripts#*tcpdump -i eth0 -w}" != "$scripts" ]; then emit "PCAP_STARTED"; exit 0; fi
    if [ "${scripts#*tcpdump}" != "$scripts" ] && [ "${scripts#*-nr}" != "$scripts" ]; then
      printf '[stdout]\n%s > %s ICMP echo request\n%s.44 > %s.8080 Flags [S]\n[stderr]\n' \
        "$BACKEND_IP" "$FRONTEND_IP" "$FRONTEND_IP" "$BACKEND_IP"; exit 0
    fi
    # ---- wrapped mutations (pod create, pcap start via exec) ----
    if [ "${scripts#*__AZRUN_OK__}" != "$scripts" ]; then emit "__AZRUN_OK__"; exit 0; fi
    # ---- kubectl get pods -o json ----
    if [ "${scripts#*get pods}" != "$scripts" ]; then
      fnode="$NODE"; [ -n "${MOCK_TT_DIFF_NODE:-}" ] && fnode="${B_RG}-worker-02"
      if [ "${scripts#*app=backend}" != "$scripts" ]; then
        emit "$(printf '{"items":[{"metadata":{"name":"backend-tt","labels":{"app":"backend"}},"status":{"phase":"Running","podIP":"%s"},"spec":{"nodeName":"%s"}}]}' "$BACKEND_IP" "$NODE")"
      elif [ "${scripts#*app=frontend}" != "$scripts" ]; then
        emit "$(printf '{"items":[{"metadata":{"name":"frontend-tt","labels":{"app":"frontend"}},"status":{"phase":"Running","podIP":"%s"},"spec":{"nodeName":"%s"}}]}' "$FRONTEND_IP" "$fnode")"
      else
        emit "$(printf '{"items":[{"metadata":{"name":"backend-tt"},"status":{"phase":"Running","podIP":"%s"},"spec":{"nodeName":"%s"}},{"metadata":{"name":"frontend-tt"},"status":{"phase":"Running","podIP":"%s"},"spec":{"nodeName":"%s"}},{"metadata":{"name":"tt-canary"},"status":{"phase":"Running","podIP":"%s"},"spec":{"nodeName":"%s"}}]}' "$BACKEND_IP" "$NODE" "$FRONTEND_IP" "$fnode" "$CANARY_IP" "$NODE")"
      fi
      exit 0
    fi
    # ---- kubectl exec ping (tts4/5/6) ----
    if [ "${scripts#*ping}" != "$scripts" ]; then
      loss=0
      case "$scripts" in
        *backend-tt*ping*"$FRONTEND_IP"*) loss=100; [ -n "${MOCK_TT_DENY_NOT_ENFORCED:-}" ] && loss=0 ;;   # deny path
        *frontend-tt*ping*"$BACKEND_IP"*) loss=0;   [ -n "${MOCK_TT_ALLOW_BLOCKED:-}" ] && loss=100 ;;      # allow reverse
        *tt-canary*ping*"$FRONTEND_IP"*)  loss=0 ;;                                                          # control
      esac
      emit "$(printf '4 packets transmitted, %s received, %s%% packet loss' "$(( (100-loss)*4/100 ))" "$loss")"
      exit 0
    fi
    # ---- kubectl exec nc :8080 (tts7) ----
    if [ "${scripts#*nc -w}" != "$scripts" ]; then
      verdict=allowed
      case "$scripts" in
        *backend-tt*"$FRONTEND_IP"*8080*) verdict=blocked; [ -n "${MOCK_TT_TCP_NOT_BLOCKED:-}" ] && verdict=allowed ;;
        *frontend-tt*"$BACKEND_IP"*8080*) verdict=allowed ;;
      esac
      emit "NC_VERDICT=${verdict}"
      exit 0
    fi
    emit "ok"; exit 0 ;;
  "account set") exit 0 ;;
  *) exit 0 ;;
esac
AZ
chmod +x "${MOCKBIN}/az"

run_ttv() {
  local name="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; MANIFEST="${casedir}/run-manifest.json"; LOG="${casedir}/log"; OUT="${casedir}/out"
  mkdir -p "$casedir"; : > "$AZLOG"; manifest::init "$MANIFEST"
  env \
    MOCK_AZ_LOG="$AZLOG" MOCK_NODE="$NODE" MOCK_B_RG="$B_RG" AZ_BIN="${MOCKBIN}/az" \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" \
    DATE_UTC="20260820" GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
    MANIFEST_PATH="$MANIFEST" AZRUN_ATTEMPTS="2" AZRUN_DELAY="0" \
    TT_RECONCILE_ATTEMPTS="2" TT_RECONCILE_DELAY="0" TT_PING_ATTEMPTS="2" \
    GITHUB_OUTPUT="$OUT" GITHUB_STEP_SUMMARY="${casedir}/summary.md" \
    "$@" bash "$TTV_SH" "$cmd" >"$LOG" 2>"${LOG}.err"
  RC=$?
}
st() { manifest::get "$MANIFEST" ".validate.tt.scenarios.$1.status"; }

echo "== fail fast on missing inputs =="
run_ttv miss tts1 PRIMARY_SUBSCRIPTION_ID=sub-a TOPOLOGY=""
assert_fail "missing TOPOLOGY fails" "$RC"

BASE=(TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary)

echo "== TTS-001 CNI mode activation (workers TT, kubelet active, backup; CP stock) =="
run_ttv tts1_ok tts1 "${BASE[@]}"
assert_pass "tts1 passes on a healthy install" "$RC"
assert_eq "tts1 recorded pass" "pass" "$(st tts1)"
assert_match "tts1 checks every worker conflist mode (TT_MODE_CHECK marker)" 'TT_MODE_CHECK' "$(tr '\n' '|' < "$AZLOG")"
run_ttv tts1_stock tts1 "${BASE[@]}" MOCK_TT_WORKER_STOCK="${B_RG}-worker-02"
assert_fail "tts1 FAILS when a worker is still stock CNI" "$RC"
run_ttv tts1_cp tts1 "${BASE[@]}" MOCK_TT_CP_TT=1
assert_fail "tts1 FAILS when the control-plane is NOT stock (TTS-001)" "$RC"
run_ttv tts1_nb tts1 "${BASE[@]}" MOCK_TT_NO_BACKUP="${B_RG}-worker-03"
assert_fail "tts1 FAILS when a worker has no /opt/cni/tt-backup-<ts>" "$RC"

echo "== TTS-002 ASG membership via addressPrefixSets REST (NOT effective-nsg) =="
run_ttv tts2_ok tts2 "${BASE[@]}"
assert_pass "tts2 passes when pod IPs are in their ASG prefix sets" "$RC"
assert_match "tts2 verifies membership via addressPrefixSets REST (api 2025-07-01)" \
  'rest .*addressPrefixSets\?api-version=2025-07-01' "$(tr '\n' '|' < "$AZLOG")"
assert_match "tts2 must NOT use list-effective-nsg (doc warning)" '' "$(grep -c 'list-effective-nsg' "$AZLOG")"
run_ttv tts2_no tts2 "${BASE[@]}" MOCK_TT_NO_MEMBERSHIP=1
assert_fail "tts2 FAILS when membership is absent from the ASG prefix set" "$RC"

echo "== TTS-003 same-node placement + reconcile =="
run_ttv tts3_ok tts3 "${BASE[@]}"
assert_pass "tts3 passes when both pods are Running on the SAME node" "$RC"
run_ttv tts3_diff tts3 "${BASE[@]}" MOCK_TT_DIFF_NODE=1
assert_fail "tts3 FAILS when pods land on different nodes (RISK-010)" "$RC"

echo "== TTS-004 same-node deny enforced (backend->frontend ICMP 100% loss) =="
run_ttv tts4_ok tts4 "${BASE[@]}"
assert_pass "tts4 passes when backend->frontend ICMP is 100% loss (DENIED)" "$RC"
run_ttv tts4_open tts4 "${BASE[@]}" MOCK_TT_DENY_NOT_ENFORCED=1
assert_fail "tts4 FAILS when the deny is not enforced (0% loss)" "$RC"

echo "== TTS-005 reverse allowed (frontend->backend ICMP 0% loss) =="
run_ttv tts5_ok tts5 "${BASE[@]}"
assert_pass "tts5 passes when frontend->backend ICMP is 0% loss (ALLOWED)" "$RC"
run_ttv tts5_block tts5 "${BASE[@]}" MOCK_TT_ALLOW_BLOCKED=1
assert_fail "tts5 FAILS when the allowed reverse path is blocked" "$RC"

echo "== TTS-006 non-member control allowed (tt-canary->frontend 0% loss) =="
run_ttv tts6_ok tts6 "${BASE[@]}"
assert_pass "tts6 passes when the non-member canary reaches frontend (0% loss)" "$RC"

echo "== TTS-007 TCP/8080 verdicts + physical-NIC (eth0) evidence =="
run_ttv tts7_ok tts7 "${BASE[@]}"
assert_pass "tts7 passes: backend->frontend:8080 blocked, frontend->backend:8080 allowed, eth0 evidence" "$RC"
assert_match "tts7 captures on the physical NIC eth0" 'tcpdump -i eth0 -w' "$(tr '\n' '|' < "$AZLOG")"
run_ttv tts7_open tts7 "${BASE[@]}" MOCK_TT_TCP_NOT_BLOCKED=1
assert_fail "tts7 FAILS when backend->frontend:8080 is not blocked" "$RC"

echo "== all: every scenario recorded; healthy run passes and is gate-ready =="
run_ttv all_ok all "${BASE[@]}"
assert_pass "all scenarios pass on a healthy data plane" "$RC"
for s in tts1 tts2 tts3 tts4 tts5 tts6 tts7; do
  assert_eq "manifest records ${s}=pass" "pass" "$(st "$s")"
done
assert_eq "aggregate validate.tt.status=pass" "pass" "$(manifest::get "$MANIFEST" '.validate.tt.status')"
run_ttv all_fail all "${BASE[@]}" MOCK_TT_DENY_NOT_ENFORCED=1
assert_fail "a single TTS failure fails validate_tt (blocks release, AC-009)" "$RC"
assert_eq "aggregate status is fail" "fail" "$(manifest::get "$MANIFEST" '.validate.tt.status')"

echo
printf 'run_tt_validation_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
