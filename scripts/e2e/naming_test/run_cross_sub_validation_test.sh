#!/usr/bin/env bash
# =============================================================================
# run_cross_sub_validation_test.sh - behavioural tests for
# ../run-cross-sub-validation.sh (EPIC-010 / ITEM-039): XSUB-001..003 plus the
# topology-keyed Tests 1-4 on the `xs` topology.
#
# Fully hermetic. `az` is a STATEFUL deterministic mock that stands in for BOTH
# self-managed clusters, a healthy controller, IMDS, and cross-subscription ARM:
#   * `get nodes` reports 4 Ready nodes per cluster (MOCK_NODES_NOTREADY breaks it);
#   * the in-cluster IMDS/ARM probe returns a token (never surfaced) and 200s from
#     each ARM GET (MOCK_IMDS_FAIL / MOCK_ARM_FAIL make the mandatory checks fail);
#   * scale / get pods / get podasgmappings / addressPrefixSets REST reflect a
#     consistent controller so val::all (Tests 1-4) PASSES on xs.
#
# The suite proves: two distinct subscriptions are required; a skipped/failed
# IMDS or ARM check FAILS validate_cross_subscription (never a warning, FR-027);
# Cluster A reads its LOCAL primary RG while Cluster B crosses the subscription
# boundary to read the primary RG AND both shared ASGs; the ARM token is NEVER
# written to the manifest or logs (sanitized evidence, NFR-012); Tests 1-4 run
# keyed to xs; and the aggregate gate result is recorded for the release gate.
#
# Traceability: ITEM-039, FR-025, FR-027, FR-028, XSUB-001, XSUB-002, XSUB-003,
# TEST-018, TEST-019, AC-024, AC-025, AC-026, NFR-012, RD-020.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
XSVAL_SH="${TEST_DIR}/../run-cross-sub-validation.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.run_cross_sub_validation_test_work"
MOCKBIN="${WORK}/bin"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi; }
assert_match() { if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2; fi; }
assert_nomatch() { if [[ "$3" =~ $2 ]]; then fail "$1"; printf '        value UNEXPECTEDLY matched /%s/\n' "$2" >&2; else pass "$1"; fi; }

if [[ ! -f "$XSVAL_SH" ]]; then
  printf 'FATAL run-cross-sub-validation.sh not found at %s\n' "$XSVAL_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

# Secret token the mock's IMDS returns; the suite asserts it NEVER leaks.
SECRET_TOKEN="eyJ0b2tlblNlY3JldERvTm90TGVhay0xMjM0NTY3ODkw"
A_RG="pnc-e2e-xs-20260820-vahdkc-eastus2euap"
B_RG="pnc-e2e-xs-20260820-vahdkc-centraluseuap"

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
emit_items(){ local cl="$1" role="$2" n b r i ip; n="$(count_of "${cl}-${role}")"
  if [ "$cl" = A ]; then b=3; else b=4; fi
  if [ "$role" = backend ]; then r=10; else r=50; fi
  printf '{"items":['; i=0
  while [ "$i" -lt "$n" ]; do ip="10.${b}.1.$((r+i))"; [ "$i" -gt 0 ] && printf ','
    printf '{"status":{"phase":"Running","podIP":"%s"}}' "$ip"; i=$((i+1)); done
  printf ']}'; }
prefixset(){ local cl="$1" role="$2" n b r i ip rg ns name first; n="$(count_of "${cl}-${role}")"
  if [ "$cl" = A ]; then rg="$MOCK_A_RG"; ns=test-apps; b=3; else rg="$MOCK_B_RG"; ns=default; b=4; fi
  if [ "$role" = backend ]; then r=10; else r=50; fi
  name="${rg}-${ns}-${role}-asg-mapping"
  [ "$n" -le 0 ] && return 0
  printf '{"name":"%s","properties":{"provisioningState":"Succeeded","addressPrefixSet":[' "$name"
  first=1; i=0
  while [ "$i" -lt "$n" ]; do ip="10.${b}.1.$((r+i))"; [ "$first" = 1 ] || printf ','; printf '"%s/32"' "$ip"; first=0; i=$((i+1)); done
  printf ']}}'; }
case "$1 $2" in
  "rest --method")
    case "$url" in *asg-frontend*) role=frontend ;; *) role=backend ;; esac
    a="$(prefixset A "$role")"; b="$(prefixset B "$role")"
    parts="$a"; if [ -n "$b" ]; then if [ -n "$parts" ]; then parts="$parts,$b"; else parts="$b"; fi; fi
    printf '{"value":[%s]}' "$parts"; exit 0 ;;
  "vm run-command")
    [ "$query" = "value[0].message" ] || exit 0
    case "$*" in *centraluseuap*) cl=B ;; *) cl=A ;; esac
    # --- XSUB-001 in-cluster IMDS + cross-subscription ARM probe ---
    if [ "${scripts#*PNC_XSUB_PROBE}" != "$scripts" ]; then
      if [ "${MOCK_IMDS_FAIL:-}" = "$cl" ] || [ "${MOCK_IMDS_FAIL:-}" = both ]; then
        printf '[stdout]\nIMDS=fail\nXSUB_PROBE_RESULT=fail\n[stderr]\n'; exit 0
      fi
      printf '[stdout]\nIMDS=ok\n'
      # Cluster A reads only its LOCAL primary RG; Cluster B reads the primary RG
      # AND both shared ASGs (cross-subscription).
      if [ "$cl" = A ]; then targets="primary_rg"; else targets="primary_rg asg_backend asg_frontend"; fi
      res=pass
      for t in $targets; do
        if [ "${MOCK_ARM_FAIL:-}" = "$cl" ] || { [ "${MOCK_ARM_FAIL_TARGET:-}" = "$t" ] && [ "$cl" = B ]; }; then
          printf 'ARM name=%s code=403\n' "$t"; res=fail
        else
          printf 'ARM name=%s code=200\n' "$t"
        fi
      done
      printf 'XSUB_PROBE_RESULT=%s\n[stderr]\n' "$res"; exit 0
    fi
    # --- node health ---
    if [ "${scripts#*get nodes}" != "$scripts" ]; then
      ready=4; [ "${MOCK_NODES_NOTREADY:-}" = "$cl" ] && ready=3
      i=0; printf '[stdout]\n'; while [ "$i" -lt "$ready" ]; do printf 'node-%s   Ready    control-plane   1d   v1.31.0\n' "$i"; i=$((i+1)); done
      printf '[stderr]\n'; exit 0
    fi
    # --- wrapped mutation (scale) ---
    if [ "${scripts#*__AZRUN_OK__}" != "$scripts" ]; then
      op="$(printf '%s' "$scripts" | grep -o 'PNC_OP scale [AB] [a-z]* [0-9]*' | head -1)"
      if [ -n "$op" ]; then set -- $op; echo "$5" > "$S/${3}-${4}"; fi
      printf '[stdout]\n__AZRUN_OK__\n[stderr]\n'; exit 0
    fi
    # --- Tests 1-4 read-backs ---
    if [ "${scripts#*get podasgmappings}" != "$scripts" ]; then
      case "$scripts" in *frontend-asg-mapping*) role=frontend ;; *) role=backend ;; esac
      n="$(count_of "${cl}-${role}")"
      printf '[stdout]\n{"status":{"mappingStatuses":[{"asgSyncState":"Synced","matchedPods":%s}]}}\n[stderr]\n' "$n"
    elif [ "${scripts#*get pods}" != "$scripts" ]; then
      case "$*$scripts" in *frontend*) role=frontend ;; *) role=backend ;; esac
      printf '[stdout]\n%s\n[stderr]\n' "$(emit_items "$cl" "$role")"
    elif [ "${scripts#*logs}" != "$scripts" ]; then
      printf '[stdout]\nreconcile complete\n[stderr]\n'
    else printf '[stdout]\nok\n[stderr]\n'; fi
    exit 0 ;;
  "account set") exit 0 ;;
  *) exit 0 ;;
esac
AZ
chmod +x "${MOCKBIN}/az"

# run_xs <case> <subcommand> <KEY=VAL...>; sets RC, AZLOG, MANIFEST, LOG, STATE.
run_xs() {
  local name="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; MANIFEST="${casedir}/run-manifest.json"
  LOG="${casedir}/log"; STATE="${casedir}/state"
  mkdir -p "$casedir" "$STATE"; : > "$AZLOG"
  if [[ "$cmd" == "all" ]]; then
    manifest::init "$MANIFEST"
    manifest::put_json "$MANIFEST" rbac.xs "$(jq -n \
      --arg s "/subscriptions/sub-primary-1111/resourceGroups/${A_RG}" \
      '{role:"Network Contributor",scope:$s,
        assignments:[{principal_id:"p1",scope:$s,assignment_id:"a1"},
                     {principal_id:"p2",scope:$s,assignment_id:"a2"}]}')"
  fi
  env \
    MOCK_AZ_LOG="$AZLOG" MOCK_STATE="$STATE" AZ_BIN="${MOCKBIN}/az" \
    MOCK_A_RG="$A_RG" MOCK_B_RG="$B_RG" MOCK_SECRET_TOKEN="$SECRET_TOKEN" \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" \
    DATE_UTC="20260820" GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
    MANIFEST_PATH="$MANIFEST" \
    RECONCILE_ATTEMPTS="2" RECONCILE_DELAY="0" AZRUN_ATTEMPTS="2" AZRUN_DELAY="0" \
    PROBE_READY_ATTEMPTS="2" PROBE_READY_DELAY="0" \
    "$@" bash "$XSVAL_SH" "$cmd" >"$LOG" 2>&1
  RC=$?
}
az_total()       { grep -c . "$AZLOG"; }
az_with_sub()    { grep -c -- '--subscription' "$AZLOG"; }
az_account_set() { grep -c '^account set' "$AZLOG"; }

PRIM="sub-primary-1111"
SEC="sub-secondary-2222"

echo "== fail fast: two DISTINCT subscriptions are mandatory (FR-025) =="
run_xs miss_sec xsub1 PRIMARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "missing SECONDARY_SUBSCRIPTION_ID fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_xs equal_subs xsub1 PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "equal primary==secondary fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_xs equal_case xsub1 PRIMARY_SUBSCRIPTION_ID="SUB-SAME" SECONDARY_SUBSCRIPTION_ID="sub-same"
assert_eq "case-only primary==secondary fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== XSUB-001: distinct subs, both clusters Ready, IMDS+ARM preflight (TEST-018) =="
run_xs x1_ok xsub1 PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC"
assert_eq "xsub1 passes with healthy clusters + IMDS + cross-sub ARM" "0" "$RC"
assert_eq "xsub1 recorded pass" "pass" "$(manifest::get "$MANIFEST" '.validate.xs.xsub.xsub1.status')"
assert_eq "manifest proves the two subscriptions differ" "true" \
  "$(manifest::get "$MANIFEST" '.validate.xs.xsub.xsub1.distinct_subscriptions')"
assert_match "an IMDS/ARM probe pod runs in-cluster on Cluster A's control-plane node" \
  'run-command invoke -g '"$A_RG"' -n '"$A_RG"'-cp-01' "$(grep 'PNC_XSUB_PROBE' "$AZLOG" | tr '\n' '|')"
assert_match "an IMDS/ARM probe pod runs in-cluster on Cluster B's control-plane node" \
  'run-command invoke -g '"$B_RG"' -n '"$B_RG"'-cp-01' "$(grep 'PNC_XSUB_PROBE' "$AZLOG" | tr '\n' '|')"
# Cluster B crosses the subscription boundary to read the PRIMARY RG and BOTH ASGs.
clusterB_probe="$(grep 'PNC_XSUB_PROBE' "$AZLOG" | grep "${B_RG}-cp-01")"
assert_match "Cluster B probes the cross-subscription primary run RG" \
  "subscriptions/${PRIM}/resourceGroups/${A_RG}\b" "$clusterB_probe"
assert_match "Cluster B probes the shared asg-backend cross-subscription" \
  "applicationSecurityGroups/asg-backend" "$clusterB_probe"
assert_match "Cluster B probes the shared asg-frontend cross-subscription" \
  "applicationSecurityGroups/asg-frontend" "$clusterB_probe"

echo "== sanitized evidence: the ARM token NEVER leaks (NFR-012) =="
assert_nomatch "the IMDS access token is absent from the manifest" "$SECRET_TOKEN" "$(cat "$MANIFEST")"
assert_nomatch "the IMDS access token is absent from the job log" "$SECRET_TOKEN" "$(cat "$LOG")"
assert_nomatch "the IMDS access token is absent from the az call log" "$SECRET_TOKEN" "$(cat "$AZLOG")"
assert_match "the node-side ARM request authenticates with the IMDS token" \
  'AUTH_SCHEME=B\\earer' "$(cat "$XSVAL_SH")"
assert_nomatch "the ARM request does not use a redaction placeholder as its credential" \
  'Authorization: \*\*\*\*\*\*' "$(cat "$XSVAL_SH")"

echo "== a skipped/failed IMDS token is a FAILURE, not a warning (FR-027) =="
run_xs x1_imds_a xsub1 PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" MOCK_IMDS_FAIL=A
assert_eq "xsub1 FAILS when Cluster A cannot obtain an IMDS token" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_xs x1_imds_b xsub1 PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" MOCK_IMDS_FAIL=B
assert_eq "xsub1 FAILS when Cluster B cannot obtain an IMDS token" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== a failed cross-subscription ARM GET fails xsub1 (FR-027) =="
run_xs x1_arm_b xsub1 PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" MOCK_ARM_FAIL=B
assert_eq "xsub1 FAILS when Cluster B cross-sub ARM GET is denied" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_xs x1_arm_asg xsub1 PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" MOCK_ARM_FAIL_TARGET=asg_backend
assert_eq "xsub1 FAILS when the cross-sub ASG read is denied" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== unhealthy nodes fail the preflight (XSUB-001) =="
run_xs x1_nodes xsub1 PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" MOCK_NODES_NOTREADY=B
assert_eq "xsub1 FAILS when a cluster is not fully Ready" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== XSUB-002: Tests 1-4 run keyed to xs and pass (TEST-019/AC-026) =="
run_xs x2_ok xsub2 PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC"
assert_eq "xsub2 passes (Cluster B reconciles into primary-sub ASGs)" "0" "$RC"
assert_eq "Tests 1-4 recorded under the xs topology key" "pass" \
  "$(manifest::get "$MANIFEST" '.validate.xs.status')"
assert_eq "xsub2 recorded pass" "pass" "$(manifest::get "$MANIFEST" '.validate.xs.xsub.xsub2.status')"

echo "== XSUB-003: recorded runtime RBAC must be run-RG-scoped (containment) =="
# Seed a GOOD inventory then assert xsub3 accepts it; a subscription-scoped grant
# in the inventory must FAIL the containment check.
missing_dir="${WORK}/x3_missing"; missing_manifest="${missing_dir}/run-manifest.json"; mkdir -p "$missing_dir"
manifest::init "$missing_manifest"
env AZ_BIN="${MOCKBIN}/az" MANIFEST_PATH="$missing_manifest" \
  REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
  GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
  PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" \
  bash "$XSVAL_SH" xsub3 >/dev/null 2>&1
rc_x3_missing=$?
assert_eq "xsub3 FAILS when the mandatory RBAC assignment inventory is absent" "1" \
  "$([[ $rc_x3_missing -ne 0 ]] && echo 1 || echo 0)"

good_dir="${WORK}/x3_good"; good_manifest="${good_dir}/run-manifest.json"; mkdir -p "$good_dir"
manifest::init "$good_manifest"
manifest::put_json "$good_manifest" rbac.xs "$(jq -n \
  --arg s "/subscriptions/${PRIM}/resourceGroups/${A_RG}" \
  '{role:"Network Contributor",scope:$s,subscription:"p",
    assignments:[{principal_id:"p1",scope:$s,assignment_id:"a1"},
                 {principal_id:"p2",scope:$s,assignment_id:"a2"}]}')"
env AZ_BIN="${MOCKBIN}/az" MANIFEST_PATH="$good_manifest" \
  REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
  GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
  PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" \
  bash "$XSVAL_SH" xsub3 >/dev/null 2>&1
rc_x3_good=$?
assert_eq "xsub3 passes for run-RG-scoped recorded grants" "0" "$rc_x3_good"
assert_eq "xsub3 records the inventory size" "2" \
  "$(manifest::get "$good_manifest" '.validate.xs.xsub.xsub3.assignments_recorded')"
bad_dir="${WORK}/x3_bad"; bad_manifest="${bad_dir}/run-manifest.json"; mkdir -p "$bad_dir"
manifest::init "$bad_manifest"
manifest::put_json "$bad_manifest" rbac.xs "$(jq -n \
  '{role:"Network Contributor",scope:"/subscriptions/x",subscription:"p",
    assignments:[{principal_id:"p1",scope:"/subscriptions/x",assignment_id:"a1"}]}')"
env AZ_BIN="${MOCKBIN}/az" MANIFEST_PATH="$bad_manifest" \
  REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
  GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
  PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" \
  bash "$XSVAL_SH" xsub3 >/dev/null 2>&1
rc_x3_bad=$?
assert_eq "xsub3 FAILS when a recorded grant is subscription-scoped (SEC-002)" "1" \
  "$([[ $rc_x3_bad -ne 0 ]] && echo 1 || echo 0)"

echo "== all: XSUB-001..003 + Tests 1-4; aggregate recorded for the release gate =="
run_xs all_ok all PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC"
assert_eq "validate_cross_subscription passes end-to-end" "0" "$RC"
assert_eq "aggregate cross-subscription status recorded pass" "pass" \
  "$(manifest::get "$MANIFEST" '.validate.xs.cross_subscription')"
assert_eq "no 'az account set' anywhere (RD-020)" "0" "$(az_account_set)"
assert_eq "EVERY az call carries an explicit --subscription (RD-020)" "$(az_total)" "$(az_with_sub)"
run_xs all_imds all PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" MOCK_IMDS_FAIL=B
assert_eq "a failed XSUB-001 fails the whole cross-subscription gate" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
assert_eq "aggregate recorded fail" "fail" "$(manifest::get "$MANIFEST" '.validate.xs.cross_subscription')"

echo
printf 'run_cross_sub_validation_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
