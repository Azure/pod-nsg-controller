#!/usr/bin/env bash
# =============================================================================
# teardown_test.sh - behavioural tests for ../teardown.sh (EPIC-010 / ITEM-040):
# cross-subscription (xs) cleanup and the dual-subscription reaper seam.
#
# Fully hermetic: `az` is a STATEFUL deterministic mock. The suite proves the
# cross-subscription cleanup contract (XSUB-003 / AC-027 / TEST-020):
#   * BOTH run resource groups are deleted with an EXPLICIT --subscription in
#     their OWNING subscription (A in primary, B in secondary), and deletion is
#     VERIFIED not-found; a lingering RG fails closed;
#   * recorded run role assignments are removed (via setup-cross-sub-rbac remove)
#     so neither subscription retains a run-VM grant;
#   * `az account set` is never used (RD-020);
#   * debug resource retention is FORBIDDEN for release runs (RD-007);
#   * the reaper deletes EXPIRED run-tagged RGs in each subscription while
#     leaving live (recent) run RGs untouched.
#
# Traceability: ITEM-040, FR-007, FR-017, XSUB-003, AC-008, AC-027, TEST-008,
# TEST-020, RD-007, RD-020.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEARDOWN_SH="${TEST_DIR}/../teardown.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.teardown_test_work"
MOCKBIN="${WORK}/bin"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi; }
assert_match() { if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2; fi; }

if [[ ! -f "$TEARDOWN_SH" ]]; then
  printf 'FATAL teardown.sh not found at %s\n' "$TEARDOWN_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

A_RG="pnc-e2e-xs-20260820-vahdkc-eastus2euap"
B_RG="pnc-e2e-xs-20260820-vahdkc-centraluseuap"
TODAY="$(date -u +%Y%m%d)"

cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
S="${MOCK_STATE}"; mkdir -p "$S/deleted"
name=""; sub=""; ids=""; assignee=""; scope=""; role=""; prev=""
for a in "$@"; do
  case "$prev" in
    --name|-n) name="$a" ;;
    --subscription) sub="$a" ;;
    --ids) ids="$a" ;;
    --assignee-object-id|--assignee) assignee="$a" ;;
    --scope) scope="$a" ;;
    --role) role="$a" ;;
  esac
  prev="$a"
done
guid_of(){ printf '%s|%s' "$1" "$2" | sha256sum | cut -c1-32; }
case "$1 $2" in
  "group delete") touch "$S/deleted/${name}"; exit 0 ;;
  "group exists")
    if [[ -f "$S/deleted/${name}" && "$name" != "${MOCK_GROUP_LINGERS:-__none__}" ]]; then echo "false"; else echo "true"; fi
    exit 0 ;;
  "group list")
    # Two run RGs per subscription: one EXPIRED (old date), one LIVE (today).
    jq -n --arg exp "${MOCK_EXPIRED_RG:-pnc-e2e-xs-20200101-old0001-eastus2euap}" \
          --arg live "${MOCK_LIVE_RG:-pnc-e2e-xs-TODAY-live001-eastus2euap}" \
          --arg today "${MOCK_TODAY}" '
      [ {name:$exp,  tags:{"validation-purpose":"pnc-e2e","managed-by":"github-actions","validation-date-utc":"20200101"}},
        {name:$live, tags:{"validation-purpose":"pnc-e2e","managed-by":"github-actions","validation-date-utc":$today}},
        {name:"unrelated-rg", tags:{"owner":"someone-else"}} ]'
    exit 0 ;;
  "role assignment delete")
    if [[ -n "$ids" ]]; then g="$(basename "$ids")"; touch "$S/deleted/ra-$g"; fi; exit 0 ;;
  "role assignment list")
    if [[ -n "$scope" ]]; then
      g="$(guid_of "$assignee" "$scope")"
      if [[ -f "$S/deleted/ra-$g" ]]; then echo '[]'; else
        jq -n --arg s "$scope" --arg pid "$assignee" '[{scope:$s,roleDefinitionName:"Network Contributor",principalId:$pid}]'
      fi
    else echo '[]'; fi
    exit 0 ;;
  "account set") exit 0 ;;
  *) exit 0 ;;
esac
AZ
chmod +x "${MOCKBIN}/az"

# run_td <case> <subcommand> <KEY=VAL...>; sets RC, AZLOG, MANIFEST, LOG, STATE.
run_td() {
  local name="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; MANIFEST="${casedir}/run-manifest.json"
  LOG="${casedir}/log"; STATE="${casedir}/state"
  mkdir -p "$casedir" "$STATE"; : > "$AZLOG"
  env MOCK_AZ_LOG="$AZLOG" MOCK_STATE="$STATE" AZ_BIN="${MOCKBIN}/az" MOCK_TODAY="$TODAY" \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
    GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
    MANIFEST_PATH="$MANIFEST" \
    DELETE_ATTEMPTS="2" DELETE_DELAY="0" VERIFY_ATTEMPTS="2" VERIFY_DELAY="0" \
    REMOVE_ATTEMPTS="2" REMOVE_DELAY="0" \
    "$@" bash "$TEARDOWN_SH" "$cmd" >"$LOG" 2>&1
  RC=$?
}
az_total()       { grep -c . "$AZLOG"; }
az_with_sub()    { grep -c -- '--subscription' "$AZLOG"; }
az_account_set() { grep -c '^account set' "$AZLOG"; }

PRIM="sub-primary-1111"
SEC="sub-secondary-2222"

echo "== fail fast: xs cleanup requires two DISTINCT subscriptions (FR-025) =="
run_td miss_sec cleanup PRIMARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "missing SECONDARY_SUBSCRIPTION_ID fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_td equal cleanup PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "equal primary==secondary fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_td equal_case cleanup PRIMARY_SUBSCRIPTION_ID="SUB-SAME" SECONDARY_SUBSCRIPTION_ID="sub-same"
assert_eq "case-only primary==secondary fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== cleanup (xs): delete BOTH RGs in their owning subscription + verify (TEST-020) =="
run_td cl_ok cleanup PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC"
assert_eq "cleanup succeeds when both RGs delete + verify not-found" "0" "$RC"
assert_match "Cluster A run RG is deleted in the PRIMARY subscription" \
  "group delete.*--name ${A_RG}\b.*--subscription ${PRIM}\b|group delete.*--subscription ${PRIM}\b.*--name ${A_RG}\b" \
  "$(grep 'group delete' "$AZLOG" | tr '\n' '|')"
assert_match "Cluster B run RG is deleted in the SECONDARY subscription" \
  "group delete.*--name ${B_RG}\b.*--subscription ${SEC}\b|group delete.*--subscription ${SEC}\b.*--name ${B_RG}\b" \
  "$(grep 'group delete' "$AZLOG" | tr '\n' '|')"
assert_match "deletion is VERIFIED with 'group exists' in the primary subscription" \
  "group exists.*--name ${A_RG}\b.*--subscription ${PRIM}\b" "$(grep 'group exists' "$AZLOG" | tr '\n' '|')"
assert_match "deletion is VERIFIED with 'group exists' in the secondary subscription" \
  "group exists.*--name ${B_RG}\b.*--subscription ${SEC}\b" "$(grep 'group exists' "$AZLOG" | tr '\n' '|')"
assert_eq "manifest records cleanup status pass" "pass" "$(manifest::get "$MANIFEST" '.cleanup.xs.status')"
assert_eq "manifest records both RGs absent" "2" \
  "$(manifest::get "$MANIFEST" '[.cleanup.xs.resource_groups[] | select(.absent == true)] | length')"
assert_eq "no 'az account set' anywhere (RD-020)" "0" "$(az_account_set)"
assert_eq "every az call carries an explicit --subscription (RD-020)" "$(az_total)" "$(az_with_sub)"

echo "== a lingering RG fails cleanup closed (verified not-found is mandatory) =="
run_td cl_linger cleanup PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" MOCK_GROUP_LINGERS="$B_RG"
assert_eq "cleanup FAILS when a run RG still exists after delete" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
assert_eq "manifest records cleanup status fail" "fail" "$(manifest::get "$MANIFEST" '.cleanup.xs.status')"

echo "== cleanup removes recorded run role assignments (XSUB-003 / AC-027) =="
seed_dir="${WORK}/withrbac"; seed_manifest="${seed_dir}/run-manifest.json"; seed_state="${seed_dir}/state"; seed_az="${seed_dir}/az.log"
mkdir -p "$seed_dir" "$seed_state"; : > "$seed_az"
manifest::init "$seed_manifest"
manifest::put_json "$seed_manifest" rbac.xs "$(jq -n \
  --arg s "/subscriptions/${PRIM}/resourceGroups/${A_RG}" \
  '{role:"Network Contributor",scope:$s,subscription:"p",
    assignments:[{principal_id:"p1",vm:"v1",region:"eastus2euap",cluster_subscription:"'"$PRIM"'",assignment_id:($s+"/providers/Microsoft.Authorization/roleAssignments/aaaa"),scope:$s},
                 {principal_id:"p2",vm:"v2",region:"centraluseuap",cluster_subscription:"'"$SEC"'",assignment_id:($s+"/providers/Microsoft.Authorization/roleAssignments/bbbb"),scope:$s}]}')"
env MOCK_AZ_LOG="$seed_az" MOCK_STATE="$seed_state" AZ_BIN="${MOCKBIN}/az" MOCK_TODAY="$TODAY" \
  REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
  GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" MANIFEST_PATH="$seed_manifest" \
  DELETE_ATTEMPTS="2" DELETE_DELAY="0" VERIFY_ATTEMPTS="2" VERIFY_DELAY="0" REMOVE_ATTEMPTS="3" REMOVE_DELAY="0" \
  PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" \
  bash "$TEARDOWN_SH" cleanup >"${seed_dir}/log" 2>&1
rc_rbac=$?
assert_eq "cleanup with recorded assignments succeeds" "0" "$rc_rbac"
assert_eq "each recorded assignment is deleted by id" "2" "$(grep -c 'role assignment delete' "$seed_az")"
assert_eq "manifest records the assignment cleanup verified (AC-027)" "true" \
  "$(manifest::get "$seed_manifest" '.cleanup.xs.rbac.verified')"

echo "== debug retention is FORBIDDEN for release runs (RD-007) =="
run_td keep_rel cleanup PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" KEEP_RESOURCES=true RELEASE_REQUESTED=true
assert_eq "release + KEEP_RESOURCES is rejected" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
assert_eq "no RG is deleted when the run is rejected" "0" "$(grep -c 'group delete' "$AZLOG")"
run_td keep_dbg cleanup PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" KEEP_RESOURCES=true RELEASE_REQUESTED=false
assert_eq "non-release debug retention is allowed (skips deletion)" "0" "$RC"
assert_eq "debug retention deletes nothing" "0" "$(grep -c 'group delete' "$AZLOG")"
assert_eq "manifest records cleanup skipped" "skipped" "$(manifest::get "$MANIFEST" '.cleanup.xs.status')"

echo "== reaper: delete EXPIRED run-tagged RGs in BOTH subscriptions; keep live =="
run_td reap reap PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" \
  MOCK_EXPIRED_RG="pnc-e2e-xs-20200101-old0001-eastus2euap" MOCK_LIVE_RG="pnc-e2e-xs-live-run"
assert_eq "reap succeeds" "0" "$RC"
assert_eq "reaper lists run RGs in BOTH subscriptions" "2" \
  "$(( $(grep -c "group list.*--subscription ${PRIM}\b" "$AZLOG") + $(grep -c "group list.*--subscription ${SEC}\b" "$AZLOG") ))"
assert_eq "reaper deletes the EXPIRED RG in each subscription (2 total)" "2" \
  "$(grep -c 'group delete.*pnc-e2e-xs-20200101-old0001-eastus2euap' "$AZLOG")"
assert_eq "reaper does NOT delete the live (recent) run RG" "0" \
  "$(grep -c 'group delete.*pnc-e2e-xs-live-run' "$AZLOG")"
assert_eq "reaper never touches an unrelated RG" "0" "$(grep -c 'group delete.*unrelated-rg' "$AZLOG")"
assert_eq "reaper uses no 'az account set' (RD-020)" "0" "$(az_account_set)"

echo
printf 'teardown_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
