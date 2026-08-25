#!/usr/bin/env bash
# =============================================================================
# setup_cross_sub_rbac_test.sh - behavioural tests for ../setup-cross-sub-rbac.sh
# (EPIC-010 / ITEM-038): explicit-subscription, run-RG-scoped cross-subscription
# RBAC for the `xs` topology, generalizing scripts/poc/setup-cross-sub-rbac.sh.
#
# Fully hermetic: `az` is a STATEFUL deterministic mock (no real cloud). The
# suite proves the cross-subscription release-safety contract:
#   * setup discovers BOTH clusters' node identities in their OWNING subscription
#     (Cluster A primary, Cluster B secondary) and grants the minimal role on the
#     PRIMARY run RG scope ONLY - never a subscription-scope grant (SEC-002);
#   * two DISTINCT subscriptions are mandatory; equal / missing IDs fail fast;
#   * EVERY Azure call carries an explicit --subscription and `az account set` is
#     never called (RD-020);
#   * assignment IDs are inventoried into the run manifest (XSUB-003) and
#     propagation is awaited with a bounded retry (NFR-003);
#   * remove deletes every recorded assignment by ID with an explicit
#     --subscription and verifies each is gone (cleanup_xs assignment removal,
#     ITEM-040 / AC-027); a lingering assignment fails verification.
#
# Directly executable; no framework. Traceability: ITEM-038, FR-026, NFR-003,
# NFR-012, RD-020, SEC-002, XSUB-003, AC-027.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SETUP_SH="${TEST_DIR}/../setup-cross-sub-rbac.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.setup_cross_sub_rbac_test_work"
MOCKBIN="${WORK}/bin"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi; }
assert_ne() { if [[ "$2" != "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected NOT [%s]\n' "$2" >&2; fi; }
assert_match() { if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2; fi; }

if [[ ! -f "$SETUP_SH" ]]; then
  printf 'FATAL setup-cross-sub-rbac.sh not found at %s\n' "$SETUP_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

# ---- stateful mock az -------------------------------------------------------
# create -> deterministic assignment id derived from (assignee|scope); delete
# marks that id gone so a subsequent list at the same (assignee,scope) is empty,
# proving remove really deletes AND verifies. MOCK_LINGER_PID keeps one principal
# assigned after delete (simulating a stuck cleanup).
cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
S="${MOCK_STATE}"; mkdir -p "$S/deleted"
vm=""; assignee=""; scope=""; role=""; sub=""; ids=""; prev=""
for a in "$@"; do
  case "$prev" in
    -n) vm="$a" ;;
    --assignee-object-id|--assignee) assignee="$a" ;;
    --scope) scope="$a" ;;
    --role) role="$a" ;;
    --subscription) sub="$a" ;;
    --ids) ids="$a" ;;
  esac
  prev="$a"
done
guid_of(){ printf '%s|%s' "$1" "$2" | sha256sum | cut -c1-32; }
case "$1 $2 $3" in
  "vm identity show")
    h="$(printf '%s' "$vm" | sha256sum | cut -c1-12)"
    printf '00000000-0000-0000-0000-%s\n' "$h" ;;
  "role assignment create")
    if [[ -n "${MOCK_FAIL_PID:-}" && "$assignee" == "$MOCK_FAIL_PID" ]]; then exit 1; fi
    if [[ "${MOCK_RBAC_CREATE_EXISTS:-0}" == "1" ]]; then echo "RoleAssignmentExists" >&2; exit 1; fi
    g="$(guid_of "$assignee" "$scope")"
    jq -n --arg id "${scope}/providers/Microsoft.Authorization/roleAssignments/${g}" \
          --arg name "$g" --arg pid "$assignee" --arg scope "$scope" \
          --arg role "${role:-Network Contributor}" \
          '{id:$id,name:$name,principalId:$pid,scope:$scope,roleDefinitionName:$role}' ;;
  "role assignment list")
    if [[ -n "$scope" ]]; then
      if [[ -n "${MOCK_FAIL_PID:-}" && "$assignee" == "$MOCK_FAIL_PID" ]]; then echo '[]'; exit 0; fi
      g="$(guid_of "$assignee" "$scope")"
      if [[ -f "$S/deleted/$g" && "$assignee" != "${MOCK_LINGER_PID:-__none__}" ]]; then echo '[]'; exit 0; fi
      if [[ "${MOCK_RBAC_PROP_EMPTY:-0}" == "1" ]]; then echo '[]'; exit 0; fi
      jq -n --arg id "${scope}/providers/Microsoft.Authorization/roleAssignments/${g}" \
            --arg name "$g" --arg s "$scope" --arg pid "$assignee" \
            '[{id:$id,name:$name,scope:$s,roleDefinitionName:"Network Contributor",principalId:$pid}]'
    else
      base="$(jq -n --arg s "/subscriptions/${sub}/resourceGroups/mock-rg" --arg pid "$assignee" \
              '{scope:$s,roleDefinitionName:"Network Contributor",principalId:$pid}')"
      if [[ "${MOCK_RBAC_LIST_SUBSCRIPTION_SCOPE:-0}" == "1" ]]; then
        extra="$(jq -n --arg s "/subscriptions/${sub}" --arg pid "$assignee" \
                '{scope:$s,roleDefinitionName:"Owner",principalId:$pid}')"
        jq -n --argjson a "$base" --argjson b "$extra" '[$a,$b]'
      else
        jq -n --argjson a "$base" '[$a]'
      fi
    fi ;;
  "role assignment delete")
    if [[ -n "$ids" ]]; then g="$(basename "$ids")"; touch "$S/deleted/$g"; fi ;;
  "account set") : ;;
  *) : ;;
esac
exit 0
AZ
chmod +x "${MOCKBIN}/az"

# run_setup <case> <subcommand> <KEY=VAL...>; sets RC, AZLOG, MANIFEST, LOG, STATE.
run_setup() {
  local name="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; MANIFEST="${casedir}/run-manifest.json"
  LOG="${casedir}/log"; STATE="${casedir}/state"
  mkdir -p "$casedir" "$STATE"; : > "$AZLOG"
  env MOCK_AZ_LOG="$AZLOG" MOCK_STATE="$STATE" AZ_BIN="${MOCKBIN}/az" \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
    GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
    MANIFEST_PATH="$MANIFEST" PROP_ATTEMPTS="2" PROP_DELAY="0" \
    REMOVE_ATTEMPTS="2" REMOVE_DELAY="0" \
    "$@" bash "$SETUP_SH" "$cmd" >"$LOG" 2>&1
  RC=$?
}
az_total()       { grep -c . "$AZLOG"; }
az_with_sub()    { grep -c -- '--subscription' "$AZLOG"; }
az_account_set() { grep -c '^account set' "$AZLOG"; }

PRIM="sub-primary-1111"
SEC="sub-secondary-2222"
XS_SCOPE='/subscriptions/sub-primary-1111/resourceGroups/pnc-e2e-xs-20260820-vahdkc-eastus2euap'

echo "== fail fast: two DISTINCT subscriptions are mandatory (FR-025/ITEM-038) =="
run_setup miss_prim setup SECONDARY_SUBSCRIPTION_ID="$SEC"
assert_eq "missing PRIMARY_SUBSCRIPTION_ID fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_setup miss_sec setup PRIMARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "missing SECONDARY_SUBSCRIPTION_ID fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_setup equal_subs setup PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "equal primary==secondary fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_setup equal_case setup PRIMARY_SUBSCRIPTION_ID="SUB-SAME" SECONDARY_SUBSCRIPTION_ID="sub-same"
assert_eq "case-only primary==secondary fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_setup ss_topo setup TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC"
assert_eq "non-xs TOPOLOGY is rejected (this script is xs-only)" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== setup (xs): cross-sub discovery, primary-RG-scoped grants only (SEC-002) =="
run_setup setup_ok setup PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC"
assert_eq "setup succeeds" "0" "$RC"
assert_match "Cluster A discovered in the primary subscription" \
  "vm identity show.*eastus2euap.*--subscription ${PRIM}" "$(grep 'vm identity show' "$AZLOG" | tr '\n' '|')"
assert_match "Cluster B discovered in the secondary subscription" \
  "vm identity show.*centraluseuap.*--subscription ${SEC}" "$(grep 'vm identity show' "$AZLOG" | tr '\n' '|')"
assert_eq "records 8 run-scoped assignments (2 clusters x 4 VMs)" "8" \
  "$(manifest::get "$MANIFEST" '.rbac.xs.assignments | length')"
assert_eq "records the minimal role" "Network Contributor" "$(manifest::get "$MANIFEST" '.rbac.xs.role')"
assert_eq "target scope is the primary run RG in the primary subscription" "$XS_SCOPE" \
  "$(manifest::get "$MANIFEST" '.rbac.xs.scope')"
assert_eq "every recorded assignment is RG-scoped" "8" \
  "$(manifest::get "$MANIFEST" '[.rbac.xs.assignments[] | select(.scope | test("^/subscriptions/[^/]+/resourceGroups/[^/]+$"))] | length')"
assert_eq "no recorded assignment is subscription-scoped (SEC-002)" "0" \
  "$(manifest::get "$MANIFEST" '[.rbac.xs.assignments[] | select(.scope | test("^/subscriptions/[^/]+$"))] | length')"
assert_eq "every grant is created in the PRIMARY subscription (RD-020)" \
  "$(grep -c 'role assignment create' "$AZLOG")" \
  "$(grep -c "role assignment create.*--subscription ${PRIM}\b" "$AZLOG")"
assert_eq "records Cluster B principals with their secondary cluster subscription" "4" \
  "$(manifest::get "$MANIFEST" "[.rbac.xs.assignments[] | select(.cluster_subscription == \"${SEC}\")] | length")"
assert_eq "no create uses a bare subscription scope" "0" \
  "$(grep -Ec -- "--scope /subscriptions/[^ /]+ " "$AZLOG")"
assert_eq "every az call carries an explicit --subscription (RD-020)" "$(az_total)" "$(az_with_sub)"
assert_eq "no 'az account set' anywhere (RD-020)" "0" "$(az_account_set)"

echo "== bounded propagation fails closed on timeout (NFR-003) =="
run_setup wait_to setup PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" MOCK_RBAC_PROP_EMPTY=1
assert_eq "setup fails closed when propagation times out" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== partial setup inventories earlier grants for always() cleanup (ITEM-040) =="
partial_vm="pnc-e2e-xs-20260820-vahdkc-centraluseuap-cp-01"
partial_hash="$(printf '%s' "$partial_vm" | sha256sum | cut -c1-12)"
partial_pid="00000000-0000-0000-0000-${partial_hash}"
run_setup partial setup PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" MOCK_FAIL_PID="$partial_pid"
assert_eq "setup fails when a later assignment cannot be created or found" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
assert_eq "the four earlier Cluster A assignment IDs remain inventoried for cleanup" "4" \
  "$(manifest::get "$MANIFEST" '.rbac.xs.assignments | length')"

echo "== a live subscription-scope grant is detected and fails (SEC-002) =="
run_setup subscope setup PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" MOCK_RBAC_LIST_SUBSCRIPTION_SCOPE=1
assert_eq "setup fails when a principal holds a subscription-scope grant" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== remove: delete every recorded assignment by ID + verify gone (AC-027) =="
# First seed a real inventory via setup, then remove against the SAME state dir.
seed_dir="${WORK}/lifecycle"; seed_manifest="${seed_dir}/run-manifest.json"; seed_state="${seed_dir}/state"
seed_az="${seed_dir}/az.log"
mkdir -p "$seed_dir" "$seed_state"; : > "$seed_az"
env MOCK_AZ_LOG="$seed_az" MOCK_STATE="$seed_state" AZ_BIN="${MOCKBIN}/az" \
  REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
  GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" MANIFEST_PATH="$seed_manifest" \
  PROP_ATTEMPTS="2" PROP_DELAY="0" \
  PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" \
  bash "$SETUP_SH" setup >/dev/null 2>&1
seeded="$(manifest::get "$seed_manifest" '.rbac.xs.assignments | length')"
assert_eq "seed produced 8 assignments to remove" "8" "$seeded"
: > "$seed_az"
env MOCK_AZ_LOG="$seed_az" MOCK_STATE="$seed_state" AZ_BIN="${MOCKBIN}/az" \
  REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
  GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" MANIFEST_PATH="$seed_manifest" \
  REMOVE_ATTEMPTS="3" REMOVE_DELAY="0" \
  PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" \
  bash "$SETUP_SH" remove >"${seed_dir}/remove.log" 2>&1
rc_remove=$?
assert_eq "remove succeeds when every assignment deletes cleanly" "0" "$rc_remove"
assert_eq "issues a delete for each of the 8 recorded assignment IDs" "8" \
  "$(grep -c 'role assignment delete' "$seed_az")"
assert_eq "every delete targets the PRIMARY subscription explicitly (RD-020)" \
  "$(grep -c 'role assignment delete' "$seed_az")" \
  "$(grep -c "role assignment delete.*--subscription ${PRIM}\b" "$seed_az")"
assert_eq "manifest records the cross-sub cleanup as verified (AC-027)" "true" \
  "$(manifest::get "$seed_manifest" '.cleanup.xs.rbac.verified')"
assert_eq "no 'az account set' during remove (RD-020)" "0" "$(grep -c '^account set' "$seed_az")"

echo "== remove FAILS verification when an assignment lingers (fail closed) =="
ling_dir="${WORK}/linger"; ling_manifest="${ling_dir}/run-manifest.json"; ling_state="${ling_dir}/state"; ling_az="${ling_dir}/az.log"
mkdir -p "$ling_dir" "$ling_state"; : > "$ling_az"
env MOCK_AZ_LOG="$ling_az" MOCK_STATE="$ling_state" AZ_BIN="${MOCKBIN}/az" \
  REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
  GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" MANIFEST_PATH="$ling_manifest" \
  PROP_ATTEMPTS="2" PROP_DELAY="0" \
  PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" \
  bash "$SETUP_SH" setup >/dev/null 2>&1
LINGER_PID="$(manifest::get "$ling_manifest" '.rbac.xs.assignments[0].principal_id')"
: > "$ling_az"
env MOCK_AZ_LOG="$ling_az" MOCK_STATE="$ling_state" AZ_BIN="${MOCKBIN}/az" \
  REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
  GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" MANIFEST_PATH="$ling_manifest" \
  REMOVE_ATTEMPTS="2" REMOVE_DELAY="0" MOCK_LINGER_PID="$LINGER_PID" \
  PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" \
  bash "$SETUP_SH" remove >"${ling_dir}/remove.log" 2>&1
rc_linger=$?
assert_eq "remove fails closed when a recorded assignment still resolves" "1" \
  "$([[ $rc_linger -ne 0 ]] && echo 1 || echo 0)"

echo
printf 'setup_cross_sub_rbac_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
