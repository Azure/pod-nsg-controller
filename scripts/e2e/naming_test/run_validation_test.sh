#!/usr/bin/env bash
# =============================================================================
# run_validation_test.sh - behavioural tests for ../run-validation.sh
# (EPIC-004 / ITEM-013 Tests 1-4 from docs/multi-cluster-test-setup.md).
#
# Fully hermetic. `az` is a STATEFUL deterministic mock that stands in for BOTH
# self-managed clusters AND a healthy controller: scale operations (kubectl on
# the control-plane node via `az vm run-command`) mutate per-(cluster,role) pod
# state; `get pods`/`get podasgmappings` and the `az rest` addressPrefixSets GET
# reflect that state consistently, so a correctly-implemented validator PASSES.
# Failure-injection knobs (MOCK_NOT_SYNCED, MOCK_DROP_IP, MOCK_412,
# MOCK_B_PREFIX_LINGERS) simulate specific controller defects and MUST make the
# corresponding documented pass criterion fail - proving the assertions bite.
#
# Traceability: ITEM-013, ITEM-014, FR-005, REQ-001, TEST-004..007, AC-003..006,
# RD-020, PRD Section 3.3 (runner has no API path) / docs/multi-cluster-test-setup.md.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VAL_SH="${TEST_DIR}/../run-validation.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.run_validation_test_work"
MOCKBIN="${WORK}/bin"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi; }
assert_ne() { if [[ "$2" != "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected NOT [%s]\n' "$2" >&2; fi; }
assert_match() { if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2; fi; }

if [[ ! -f "$VAL_SH" ]]; then
  printf 'FATAL run-validation.sh not found at %s\n' "$VAL_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

A_RG="pnc-e2e-ss-20260820-vahdkc-eastus2euap"
B_RG="pnc-e2e-ss-20260820-vahdkc-centraluseuap"

# ---- stateful mock az -------------------------------------------------------
cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
S="${MOCK_STATE}"; mkdir -p "$S"
scripts=""; query=""; url=""; prev=""
for a in "$@"; do
  case "$prev" in --scripts) scripts="$a" ;; --query) query="$a" ;; --url) url="$a" ;; esac
  prev="$a"
done
count_of(){ local f="$S/$1"; if [ -f "$f" ]; then cat "$f"; else echo 0; fi; }
emit_items(){ # cluster role -> {"items":[...]}
  local cl="$1" role="$2" n b r i ip; n="$(count_of "${cl}-${role}")"
  if [ "$cl" = A ]; then b=3; else b=4; fi
  if [ "$role" = backend ]; then r=10; else r=50; fi
  printf '{"items":['; i=0
  while [ "$i" -lt "$n" ]; do ip="10.${b}.1.$((r+i))"; [ "$i" -gt 0 ] && printf ','
    printf '{"status":{"phase":"Running","podIP":"%s"}}' "$ip"; i=$((i+1)); done
  printf ']}'
}
prefixset(){ # cluster role -> prints one JSON object, or nothing (absent)
  local cl="$1" role="$2" n b r i ip rg ns name first drop; n="$(count_of "${cl}-${role}")"
  if [ "$cl" = A ]; then rg="$MOCK_A_RG"; ns=test-apps; b=3; else rg="$MOCK_B_RG"; ns=default; b=4; fi
  if [ "$role" = backend ]; then r=10; else r=50; fi
  name="${rg}-${ns}-${role}-asg-mapping"
  if [ "$n" -le 0 ]; then
    if [ "$cl" = B ] && [ "${MOCK_B_PREFIX_LINGERS:-0}" = 1 ]; then
      printf '{"name":"%s","properties":{"provisioningState":"Succeeded","addressPrefixSet":["10.%s.1.99/32"]}}' "$name" "$b"
    fi
    return 0
  fi
  drop=0; [ "${MOCK_DROP_IP:-}" = "${cl}-${role}" ] && drop=1
  printf '{"name":"%s","properties":{"provisioningState":"Succeeded","addressPrefixSet":[' "$name"
  first=1; i=0
  while [ "$i" -lt "$n" ]; do
    if [ "$drop" = 1 ] && [ "$i" -eq 0 ]; then i=$((i+1)); continue; fi
    ip="10.${b}.1.$((r+i))"; [ "$first" = 1 ] || printf ','; printf '"%s/32"' "$ip"; first=0; i=$((i+1))
  done
  printf ']}}'
}
case "$1 $2" in
  "rest --method")
    case "$url" in *asg-frontend*) role=frontend ;; *) role=backend ;; esac
    a="$(prefixset A "$role")"; b="$(prefixset B "$role")"
    parts="$a"; if [ -n "$b" ]; then if [ -n "$parts" ]; then parts="$parts,$b"; else parts="$b"; fi; fi
    printf '{"value":[%s]}' "$parts"; exit 0 ;;
  "vm run-command")
    [ "$query" = "value[0].message" ] || exit 0
    if [ "${scripts#*__AZRUN_OK__}" != "$scripts" ]; then      # wrapped mutation
      if [ -n "${MOCK_NODE_FAIL_MATCH:-}" ] && [ "${scripts#*${MOCK_NODE_FAIL_MATCH}}" != "$scripts" ]; then
        printf '[stdout]\n__AZRUN_FAIL__ rc=1\n[stderr]\n'; exit 0
      fi
      op="$(printf '%s' "$scripts" | grep -o 'PNC_OP scale [AB] [a-z]* [0-9]*' | head -1)"
      if [ -n "$op" ]; then set -- $op; echo "$5" > "$S/${3}-${4}"; fi
      printf '[stdout]\n__AZRUN_OK__\n[stderr]\n'; exit 0
    fi
    case "$*" in *centraluseuap*) cl=B ;; *) cl=A ;; esac
    if [ "${scripts#*get podasgmappings}" != "$scripts" ]; then
      case "$scripts" in *frontend-asg-mapping*) role=frontend ;; *) role=backend ;; esac
      n="$(count_of "${cl}-${role}")"; st=Synced
      [ "${MOCK_NOT_SYNCED:-}" = "${cl}-${role}" ] && st=OutOfSync
      printf '[stdout]\n{"status":{"mappingStatuses":[{"asgSyncState":"%s","matchedPods":%s}]}}\n[stderr]\n' "$st" "$n"
    elif [ "${scripts#*get pods}" != "$scripts" ]; then
      case "$*$scripts" in *frontend*) role=frontend ;; *) role=backend ;; esac
      printf '[stdout]\n%s\n[stderr]\n' "$(emit_items "$cl" "$role")"
    elif [ "${scripts#*logs}" != "$scripts" ]; then
      if [ "${MOCK_412:-}" = "$cl" ]; then printf '[stdout]\nE update: 412 PreconditionFailed\n[stderr]\n'
      else printf '[stdout]\nreconcile complete\n[stderr]\n'; fi
    else printf '[stdout]\nok\n[stderr]\n'; fi
    exit 0 ;;
  "account set") exit 0 ;;
  *) exit 0 ;;
esac
AZ
chmod +x "${MOCKBIN}/az"

# run_val <case> <subcommand> <KEY=VAL...>; sets RC, AZLOG, MANIFEST, LOG, STATE.
run_val() {
  local name="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; MANIFEST="${casedir}/run-manifest.json"
  LOG="${casedir}/log"; STATE="${casedir}/state"
  mkdir -p "$casedir" "$STATE"; : > "$AZLOG"
  env \
    MOCK_AZ_LOG="$AZLOG" MOCK_STATE="$STATE" AZ_BIN="${MOCKBIN}/az" \
    MOCK_A_RG="$A_RG" MOCK_B_RG="$B_RG" \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" \
    DATE_UTC="20260820" GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
    MANIFEST_PATH="$MANIFEST" \
    RECONCILE_ATTEMPTS="2" RECONCILE_DELAY="0" AZRUN_ATTEMPTS="2" AZRUN_DELAY="0" \
    "$@" bash "$VAL_SH" "$cmd" >"$LOG" 2>&1
  RC=$?
}
az_total()       { grep -c . "$AZLOG"; }
az_with_sub()    { grep -c -- '--subscription' "$AZLOG"; }
az_account_set() { grep -c '^account set' "$AZLOG"; }

echo "== fail fast on missing mandatory inputs (ITEM-013) =="
run_val miss_topo test1 PRIMARY_SUBSCRIPTION_ID=sub-a
assert_eq "missing TOPOLOGY fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_val miss_sub test1 TOPOLOGY=ss
assert_eq "missing PRIMARY_SUBSCRIPTION_ID fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== Test 1 (single-cluster scale-up): healthy PASS, defects FAIL =="
run_val t1_ok test1 TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary
assert_eq "test1 passes against a healthy controller" "0" "$RC"
assert_eq "test1 recorded as pass in the manifest" "pass" \
  "$(manifest::get "$MANIFEST" '.validate.ss.tests.test1.status')"
assert_match "prefix sets are verified via the addressPrefixSets REST GET (api 2025-07-01)" \
  'rest .*addressPrefixSets\?api-version=2025-07-01' "$(tr '\n' '|' < "$AZLOG")"
assert_match "REST verification carries the PRIMARY --subscription" \
  'rest .*--subscription sub-primary' "$(grep -- 'rest ' "$AZLOG" | tr '\n' '|')"
run_val t1_notsynced test1 TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary MOCK_NOT_SYNCED=A-backend
assert_eq "test1 FAILS when a mapping is not Synced" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
assert_eq "test1 recorded as fail in the manifest" "fail" \
  "$(manifest::get "$MANIFEST" '.validate.ss.tests.test1.status')"
run_val t1_dropip test1 TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary MOCK_DROP_IP=A-frontend
assert_eq "test1 FAILS when a pod IP is missing from the prefix set" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== Test 2 (multi-cluster concurrent writes): 2 prefix sets/ASG, zero 412 =="
run_val t2_ok test2 TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary
assert_eq "test2 passes (both clusters synced, 2 prefix sets/ASG)" "0" "$RC"
assert_eq "test2 recorded pass" "pass" "$(manifest::get "$MANIFEST" '.validate.ss.tests.test2.status')"
run_val t2_412 test2 TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary MOCK_412=A
assert_eq "test2 FAILS on a 412 PreconditionFailed in controller logs (RISK-005)" "1" \
  "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== Test 3 (scale-down + cleanup): Cluster B prefix sets removed =="
run_val t3_ok test3 TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary
assert_eq "test3 passes (B prefix sets removed on scale-to-zero)" "0" "$RC"
run_val t3_linger test3 TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary MOCK_B_PREFIX_LINGERS=1
assert_eq "test3 FAILS when a scaled-down cluster's prefix set lingers" "1" \
  "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== Test 4 (parallel scale-up stress): 35 IPs across 4 prefix sets =="
run_val t4_ok test4 TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary
assert_eq "test4 passes (A=25 + B=10 across 4 prefix sets)" "0" "$RC"
run_val t4_drop test4 TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary MOCK_DROP_IP=B-backend
assert_eq "test4 FAILS when the stress set loses an IP" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== all: full suite + invariants =="
run_val all_ok all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary
assert_eq "all four tests pass against a healthy controller" "0" "$RC"
assert_eq "aggregate status recorded pass" "pass" "$(manifest::get "$MANIFEST" '.validate.ss.status')"
assert_eq "no 'az account set' anywhere (RD-020)" "0" "$(az_account_set)"
assert_eq "EVERY az call carries an explicit --subscription (RD-020)" "$(az_total)" "$(az_with_sub)"
assert_match "kubectl scale runs on the control-plane node via run-command" \
  'run-command invoke -g pnc-e2e-ss-20260820-vahdkc-eastus2euap-cp-01|run-command invoke -g pnc-e2e-ss-20260820-vahdkc-eastus2euap -n pnc-e2e-ss-20260820-vahdkc-eastus2euap-cp-01' \
  "$(grep 'run-command invoke' "$AZLOG" | tr '\n' '|')"
run_val all_fail all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary MOCK_412=B
assert_eq "any failing test fails the aggregate suite" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
assert_eq "aggregate status recorded fail" "fail" "$(manifest::get "$MANIFEST" '.validate.ss.status')"

echo
printf 'run_validation_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
