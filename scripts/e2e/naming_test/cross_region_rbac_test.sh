#!/usr/bin/env bash
# =============================================================================
# cross_region_rbac_test.sh - behavioural tests for ../cross-region-rbac.sh
# (EPIC-003 / ITEM-009): same-subscription, run-scoped, cross-region RBAC.
#
# Fully hermetic: `az` is a deterministic mock (no real cloud, no principals).
# The suite proves the release-safety contract:
#   * both clusters' node identities are granted the minimal role on the
#     PRIMARY run RG scope ONLY - never a subscription-scope grant (ITEM-009);
#   * EVERY Azure call carries an explicit --subscription; discovery targets
#     each cluster's owning subscription; grants target the primary subscription;
#   * `az account set` is never called (RD-020 / RISK-013);
#   * propagation is awaited with a bounded retry that fails closed on timeout;
#   * assignment IDs are recorded into the run manifest (XSUB-003 inventory).
#
# Directly executable; no framework. Traceability: ITEM-009, FR-026, NFR-003,
# NFR-012, RD-020, XSUB-003, RISK-011, RISK-013.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RBAC_SH="${TEST_DIR}/../cross-region-rbac.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.rbac_test_work"
MOCKBIN="${WORK}/bin"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() {
  if [[ "$2" == "$3" ]]; then pass "$1"; else
    fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2
  fi
}
assert_match() {
  if [[ "$3" =~ $2 ]]; then pass "$1"; else
    fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2
  fi
}

if [[ ! -f "$RBAC_SH" ]]; then
  printf 'FATAL cross-region-rbac.sh not found at %s\n' "$RBAC_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
vm=""; assignee=""; scope=""; role=""; sub=""; prev=""
for a in "$@"; do
  case "$prev" in
    -n) vm="$a" ;;
    --assignee-object-id|--assignee) assignee="$a" ;;
    --scope) scope="$a" ;;
    --role) role="$a" ;;
    --subscription) sub="$a" ;;
  esac
  prev="$a"
done
case "$1 $2 $3" in
  "vm identity show")
    h="$(printf '%s' "$vm" | sha256sum | cut -c1-12)"
    printf '00000000-0000-0000-0000-%s\n' "$h" ;;
  "role assignment create")
    if [[ "${MOCK_RBAC_CREATE_EXISTS:-0}" == "1" ]]; then echo "RoleAssignmentExists" >&2; exit 1; fi
    g="$(printf '%s|%s' "$assignee" "$scope" | sha256sum | cut -c1-32)"
    guid="${g:0:8}-${g:8:4}-${g:12:4}-${g:16:4}-${g:20:12}"
    jq -n --arg id "${scope}/providers/Microsoft.Authorization/roleAssignments/${guid}" \
          --arg name "$guid" --arg pid "$assignee" --arg scope "$scope" \
          --arg role "${role:-Network Contributor}" \
          '{id:$id,name:$name,principalId:$pid,scope:$scope,roleDefinitionName:$role}' ;;
  "role assignment list")
    if [[ -n "$scope" ]]; then
      if [[ "${MOCK_RBAC_PROP_EMPTY:-0}" == "1" ]]; then echo '[]'; else
        g="$(printf '%s|%s' "$assignee" "$scope" | sha256sum | cut -c1-32)"
        jq -n --arg id "${scope}/providers/Microsoft.Authorization/roleAssignments/${g}" \
              --arg name "$g" --arg s "$scope" --arg pid "$assignee" \
              '[{id:$id,name:$name,scope:$s,roleDefinitionName:"Network Contributor",principalId:$pid}]'
      fi
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
  "role assignment delete") : ;;
  "account set") : ;;
  *) : ;;
esac
exit 0
AZ
chmod +x "${MOCKBIN}/az"

# run_rbac <case> <subcommand> <KEY=VAL...> ; sets RC, AZLOG, MANIFEST, LOG.
run_rbac() {
  local name="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; MANIFEST="${casedir}/run-manifest.json"; LOG="${casedir}/log"
  mkdir -p "$casedir"; : > "$AZLOG"
  env MOCK_AZ_LOG="$AZLOG" AZ_BIN="${MOCKBIN}/az" \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
    GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
    MANIFEST_PATH="$MANIFEST" PROP_ATTEMPTS="2" PROP_DELAY="0" \
    "$@" bash "$RBAC_SH" "$cmd" >"$LOG" 2>&1
  RC=$?
}
az_total()       { grep -c . "$AZLOG"; }
az_with_sub()    { grep -c -- '--subscription' "$AZLOG"; }
az_account_set() { grep -c '^account set' "$AZLOG"; }

PRIM="sub-primary-1111"
SEC="sub-secondary-2222"
SS_SCOPE='/subscriptions/sub-primary-1111/resourceGroups/pnc-e2e-ss-20260820-vahdkc-eastus2euap'
XS_SCOPE='/subscriptions/sub-primary-1111/resourceGroups/pnc-e2e-xs-20260820-vahdkc-eastus2euap'

echo "== fail fast on missing inputs (ITEM-009) =="
run_rbac miss_prim discover TOPOLOGY=ss
assert_eq "missing PRIMARY_SUBSCRIPTION_ID fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_rbac miss_topo discover PRIMARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "missing TOPOLOGY fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_rbac xs_no_sec discover TOPOLOGY=xs PRIMARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "xs without SECONDARY_SUBSCRIPTION_ID fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== discover (ss): both clusters' node principals; explicit --subscription =="
run_rbac disc_ss discover TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "discover succeeds" "0" "$RC"
assert_eq "discovers 8 node principals (2 clusters x 4 VMs)" "8" "$(grep -c '^principal_id=' "$LOG")"
assert_eq "no 'az account set' during discovery (RD-020)" "0" "$(az_account_set)"
assert_eq "every discovery az call is explicit-subscription" "$(az_total)" "$(az_with_sub)"

echo "== grant (ss): RG-scoped 'Network Contributor' on the PRIMARY run RG only (ITEM-009) =="
run_rbac grant_ss grant TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "grant succeeds" "0" "$RC"
assert_eq "records 8 run-scoped assignments" "8" "$(manifest::get "$MANIFEST" '.rbac.ss.assignments | length')"
assert_eq "records the minimal role" "Network Contributor" "$(manifest::get "$MANIFEST" '.rbac.ss.role')"
assert_eq "target scope is the primary run RG" "$SS_SCOPE" "$(manifest::get "$MANIFEST" '.rbac.ss.scope')"
assert_eq "every recorded assignment is RG-scoped (no subscription scope)" "8" \
  "$(manifest::get "$MANIFEST" '[.rbac.ss.assignments[] | select(.scope | test("^/subscriptions/[^/]+/resourceGroups/[^/]+$"))] | length')"
assert_eq "no recorded assignment is subscription-scoped" "0" \
  "$(manifest::get "$MANIFEST" '[.rbac.ss.assignments[] | select(.scope | test("^/subscriptions/[^/]+$"))] | length')"
assert_eq "every 'role assignment create' targets the primary RG scope" \
  "$(grep -c 'role assignment create' "$AZLOG")" \
  "$(grep -c "role assignment create.*--scope ${SS_SCOPE}\b" "$AZLOG")"
# A bare subscription scope would be "--scope /subscriptions/<id> " with no
# "/resourceGroups/" segment; exclude '/' from the id class so RG scopes don't match.
assert_eq "no create uses a bare subscription scope" "0" \
  "$(grep -Ec -- "--scope /subscriptions/[^ /]+ " "$AZLOG")"
assert_match "assignment IDs are recorded (XSUB-003 inventory)" \
  'roleAssignments/[0-9a-f-]+' "$(manifest::get "$MANIFEST" '.rbac.ss.assignments[0].assignment_id')"

echo "== wait: bounded propagation (NFR-003) =="
run_rbac wait_ok wait TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "wait succeeds when the assignment is visible" "0" "$RC"
run_rbac wait_to wait TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID="$PRIM" MOCK_RBAC_PROP_EMPTY=1
assert_eq "wait fails closed when propagation times out" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== assert-scope: rejects any subscription-scope grant (ITEM-009) =="
# seed a GOOD manifest, then assert on it
good_dir="${WORK}/assert_good"; good_manifest="${good_dir}/run-manifest.json"
mkdir -p "$good_dir"
manifest::init "$good_manifest"
manifest::put_json "$good_manifest" rbac.ss "$(jq -n --arg s "$SS_SCOPE" \
  '{role:"Network Contributor",scope:$s,subscription:"p",assignments:[{scope:$s,assignment_id:"x"}]}')"
if env AZ_BIN="${MOCKBIN}/az" TOPOLOGY=ss MANIFEST_PATH="$good_manifest" \
     REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
     bash "$RBAC_SH" assert-scope >/dev/null 2>&1; then
  pass "assert-scope passes for RG-scoped assignments"; else fail "assert-scope should pass for RG-scoped assignments"; fi
bad_manifest="${good_dir}/bad.json"
manifest::init "$bad_manifest"
manifest::put_json "$bad_manifest" rbac.ss "$(jq -n \
  '{role:"Network Contributor",scope:"/subscriptions/x",subscription:"p",assignments:[{scope:"/subscriptions/x",assignment_id:"y"}]}')"
if env AZ_BIN="${MOCKBIN}/az" TOPOLOGY=ss MANIFEST_PATH="$bad_manifest" \
     REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
     bash "$RBAC_SH" assert-scope >/dev/null 2>&1; then
  fail "assert-scope should fail for a subscription-scope grant"; else pass "assert-scope fails for a subscription-scope grant"; fi

echo "== all (ss): full flow; detects a stray subscription-scope grant =="
run_rbac all_ss all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "all succeeds" "0" "$RC"
assert_eq "all records 8 assignments" "8" "$(manifest::get "$MANIFEST" '.rbac.ss.assignments | length')"
assert_eq "all explicit-subscription invariant holds" "$(az_total)" "$(az_with_sub)"
assert_eq "all: no 'az account set'" "0" "$(az_account_set)"
run_rbac all_ss_bad all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID="$PRIM" MOCK_RBAC_LIST_SUBSCRIPTION_SCOPE=1
assert_eq "all fails when a principal holds a subscription-scope grant" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== idempotent grant: existing assignment is reused (RISK-004) =="
run_rbac grant_exists grant TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID="$PRIM" MOCK_RBAC_CREATE_EXISTS=1
assert_eq "grant succeeds when assignments already exist" "0" "$RC"
assert_eq "still records 8 assignments via idempotent lookup" "8" \
  "$(manifest::get "$MANIFEST" '.rbac.ss.assignments | length')"

echo "== xs: discovery crosses subscriptions; grants stay on the primary RG =="
run_rbac disc_xs discover TOPOLOGY=xs PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC"
assert_eq "xs discover succeeds" "0" "$RC"
assert_match "Cluster A discovered in the primary subscription" \
  "vm identity show.*eastus2euap.*--subscription ${PRIM}" "$(grep 'vm identity show' "$AZLOG" | tr '\n' '|')"
assert_match "Cluster B discovered in the secondary subscription" \
  "vm identity show.*centraluseuap.*--subscription ${SEC}" "$(grep 'vm identity show' "$AZLOG" | tr '\n' '|')"
run_rbac grant_xs grant TOPOLOGY=xs PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC"
assert_eq "xs grant target scope is the primary RG in the primary subscription" "$XS_SCOPE" \
  "$(manifest::get "$MANIFEST" '.rbac.xs.scope')"
assert_eq "xs grants are all created in the primary subscription" \
  "$(grep -c 'role assignment create' "$AZLOG")" \
  "$(grep -c "role assignment create.*--subscription ${PRIM}\b" "$AZLOG")"
assert_eq "xs records Cluster B principals with their secondary cluster subscription" "4" \
  "$(manifest::get "$MANIFEST" "[.rbac.xs.assignments[] | select(.cluster_subscription == \"${SEC}\")] | length")"

echo
printf 'cross_region_rbac_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
