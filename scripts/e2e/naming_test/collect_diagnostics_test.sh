#!/usr/bin/env bash
# =============================================================================
# collect_diagnostics_test.sh - behavioural tests for ../collect-diagnostics.sh
# (EPIC-010 / ITEM-040): the cross-subscription (xs) diagnostics collector used
# by the always() diagnostics_xs job.
#
# Fully hermetic: `az` is a deterministic mock. The suite proves:
#   * BEST-EFFORT collection always exits 0 (AC-007) even when a node capture
#     fails, so the always() job never aborts;
#   * evidence is captured from BOTH clusters (controller logs, PodASGMapping
#     status, pod IPs, node status) each in its OWNING subscription, plus the
#     shared-ASG addressPrefixSets membership from the primary subscription;
#   * captured evidence is SANITIZED - a bearer/access token in controller logs
#     is redacted, never written to an artifact (NFR-012);
#   * every Azure call carries an explicit --subscription and `az account set`
#     is never used (RD-020).
#
# Traceability: ITEM-040, FR-006, AC-007, NFR-012, RD-020.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIAG_SH="${TEST_DIR}/../collect-diagnostics.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.collect_diagnostics_test_work"
MOCKBIN="${WORK}/bin"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi; }
assert_match() { if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2; fi; }
assert_nomatch() { if [[ "$3" =~ $2 ]]; then fail "$1"; printf '        value UNEXPECTEDLY matched /%s/\n' "$2" >&2; else pass "$1"; fi; }

if [[ ! -f "$DIAG_SH" ]]; then
  printf 'FATAL collect-diagnostics.sh not found at %s\n' "$DIAG_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

SECRET_TOKEN="eyJ0b2tlblNlY3JldERvTm90TGVhay1ESUFH"

cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
scripts=""; query=""; url=""; prev=""
for a in "$@"; do case "$prev" in --scripts) scripts="$a" ;; --query) query="$a" ;; --url) url="$a" ;; esac; prev="$a"; done
case "$1 $2" in
  "rest --method")
    printf '{"value":[{"name":"ps-a","properties":{"provisioningState":"Succeeded","addressPrefixSet":["10.3.1.10/32"]}}]}' ;;
  "vm run-command")
    [ "$query" = "value[0].message" ] || exit 0
    case "$*" in *centraluseuap*) cl=B ;; *) cl=A ;; esac
    if [ -n "${MOCK_TRANSPORT_FAIL:-}" ] && [ "${MOCK_TRANSPORT_FAIL}" = "$cl" ]; then exit 1; fi
    if [ "${scripts#*get nodes}" != "$scripts" ]; then
      printf '[stdout]\nnode-0 Ready control-plane\nnode-1 Ready worker\n[stderr]\n'
    elif [ "${scripts#*get podasgmappings}" != "$scripts" ]; then
      printf '[stdout]\n{"items":[{"metadata":{"name":"backend-asg-mapping"},"status":{"mappingStatuses":[{"asgSyncState":"Synced"}]}}]}\n[stderr]\n'
    elif [ "${scripts#*logs}" != "$scripts" ]; then
      printf '[stdout]\nreconcile ok Authorization: Bearer %s done\n[stderr]\n' "$MOCK_SECRET_TOKEN"
    elif [ "${scripts#*get pods}" != "$scripts" ]; then
      printf '[stdout]\nbackend-0 10.3.1.10 node-1 Running\n[stderr]\n'
    else printf '[stdout]\nok\n[stderr]\n'; fi ;;
  "account set") : ;;
  *) : ;;
esac
exit 0
AZ
chmod +x "${MOCKBIN}/az"

run_diag() {
  local name="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; MANIFEST="${casedir}/run-manifest.json"; LOG="${casedir}/log"; DIAG="${casedir}/diagnostics"
  mkdir -p "$casedir"; : > "$AZLOG"
  env MOCK_AZ_LOG="$AZLOG" AZ_BIN="${MOCKBIN}/az" MOCK_SECRET_TOKEN="$SECRET_TOKEN" \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" DATE_UTC="20260820" \
    GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
    MANIFEST_PATH="$MANIFEST" DIAG_DIR="$DIAG" AZRUN_ATTEMPTS="1" AZRUN_DELAY="0" \
    "$@" bash "$DIAG_SH" "$cmd" >"$LOG" 2>&1
  RC=$?
}
az_account_set() { grep -c '^account set' "$AZLOG"; }

PRIM="sub-primary-1111"
SEC="sub-secondary-2222"

echo "== diagnostics_xs: best-effort collection from BOTH clusters (AC-007) =="
run_diag all all PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC"
assert_eq "collection exits 0 (best-effort, always())" "0" "$RC"
assert_eq "Cluster A node status captured" "1" "$([[ -s "${WORK}/all/diagnostics/clusterA-nodes.txt" ]] && echo 1 || echo 0)"
assert_eq "Cluster B node status captured" "1" "$([[ -s "${WORK}/all/diagnostics/clusterB-nodes.txt" ]] && echo 1 || echo 0)"
assert_eq "Cluster A controller logs captured" "1" "$([[ -s "${WORK}/all/diagnostics/clusterA-controller-logs.txt" ]] && echo 1 || echo 0)"
assert_eq "Cluster B controller logs captured" "1" "$([[ -s "${WORK}/all/diagnostics/clusterB-controller-logs.txt" ]] && echo 1 || echo 0)"
assert_eq "Cluster A PodASGMapping status captured" "1" "$([[ -s "${WORK}/all/diagnostics/clusterA-podasgmappings.json" ]] && echo 1 || echo 0)"
assert_eq "Cluster B PodASGMapping status captured" "1" "$([[ -s "${WORK}/all/diagnostics/clusterB-podasgmappings.json" ]] && echo 1 || echo 0)"
assert_eq "shared-ASG membership captured" "1" "$([[ -s "${WORK}/all/diagnostics/asg-membership.json" ]] && echo 1 || echo 0)"
assert_match "Cluster A is read in the PRIMARY subscription" \
  "run-command invoke .*eastus2euap.* --subscription ${PRIM}\b|--subscription ${PRIM}\b.*eastus2euap" "$(grep 'run-command' "$AZLOG" | tr '\n' '|')"
assert_match "Cluster B is read in the SECONDARY subscription" \
  "run-command invoke .*centraluseuap.* --subscription ${SEC}\b|--subscription ${SEC}\b.*centraluseuap" "$(grep 'run-command' "$AZLOG" | tr '\n' '|')"
assert_match "ASG membership REST GET uses the addressPrefixSets api (CON-001)" \
  'addressPrefixSets\?api-version=2025-07-01' "$(grep 'rest' "$AZLOG" | tr '\n' '|')"
assert_eq "manifest records the diagnostics_xs directory" "1" \
  "$([[ -n "$(manifest::get "$MANIFEST" '.diagnostics.xs.dir // empty')" ]] && echo 1 || echo 0)"
assert_eq "no 'az account set' anywhere (RD-020)" "0" "$(az_account_set)"

echo "== sanitized evidence: a bearer/access token is redacted (NFR-012) =="
assert_nomatch "the token is NOT written to Cluster A controller logs" "$SECRET_TOKEN" "$(cat "${WORK}/all/diagnostics/clusterA-controller-logs.txt")"
assert_nomatch "the token is NOT present anywhere under the diagnostics dir" "$SECRET_TOKEN" "$(cat "${WORK}"/all/diagnostics/* 2>/dev/null)"

echo "== best-effort: a failing node capture does NOT abort collection (AC-007) =="
run_diag partial all PRIMARY_SUBSCRIPTION_ID="$PRIM" SECONDARY_SUBSCRIPTION_ID="$SEC" MOCK_TRANSPORT_FAIL=A
assert_eq "collection still exits 0 when Cluster A is unreachable" "0" "$RC"
assert_eq "Cluster B evidence is still captured despite Cluster A failing" "1" \
  "$([[ -s "${WORK}/partial/diagnostics/clusterB-nodes.txt" ]] && echo 1 || echo 0)"

echo "== ITEM-015: same-subscription diagnostics include CNI/network evidence =="
run_diag ss all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID="$PRIM"
assert_eq "same-subscription collection exits 0" "0" "$RC"
assert_match "both ss clusters are read with the explicit primary subscription" \
  "run-command invoke .*--subscription ${PRIM}\b" "$(grep 'run-command' "$AZLOG" | tr '\n' '|')"
assert_eq "same-subscription manifest entry is recorded" "1" \
  "$([[ -n "$(manifest::get "$MANIFEST" '.diagnostics.ss.dir // empty')" ]] && echo 1 || echo 0)"
assert_eq "collect-cni-diagnostics.sh is invoked into the diagnostics artifact" "1" \
  "$([[ -s "${WORK}/ss/diagnostics/cni/controller-logs.txt" ]] && echo 1 || echo 0)"

echo
printf 'collect_diagnostics_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
