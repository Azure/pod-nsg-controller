#!/usr/bin/env bash
# =============================================================================
# provision_tt_fixtures_test.sh - behavioural tests for ../provision-tt-fixtures.sh
# (EPIC-009 / ITEM-031, FR-021).
#
# Fully hermetic. `az` is a deterministic mock that records `network nsg rule
# create` invocations AND captures the on-node `kubectl apply` payload (the
# PodASGMappings). The suite proves the two documented enforcement rules are
# created on the centraluseuap subnet NSG with the EXACT priorities / actions /
# source+dest ASGs / ports (§3 of the test doc), the app=backend/app=frontend
# PodASGMappings are applied in namespace `default`, every Azure call carries an
# explicit --subscription (RD-020), and a rule-create failure fails closed.
#
# Traceability: ITEM-031, FR-021, REQ-004, CON-003, RD-020, CON-010, PRD 3.6 /
# docs/transparent-tunnel-same-node-enforcement-test.md 3.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIX_SH="${TEST_DIR}/../provision-tt-fixtures.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.provision_tt_fixtures_test_work"
MOCKBIN="${WORK}/bin"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi; }
assert_match() { if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2; fi; }
assert_nonzero() { if (( $2 != 0 )); then pass "$1"; else fail "$1"; fi; }

if [[ ! -f "$FIX_SH" ]]; then
  printf 'FATAL provision-tt-fixtures.sh not found at %s\n' "$FIX_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

B_RG="pnc-e2e-ss-20260820-vahdkc-centraluseuap"       # centraluseuap cluster RG
NSG="${B_RG}-nsg"
CP="${B_RG}-cp-01"
PRIMARY_RG="pnc-e2e-ss-20260820-vahdkc-eastus2euap"   # shared ASGs live here

cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
vm=""; scripts=""; rulename=""; prev=""
for a in "$@"; do
  case "$prev" in -n) if [ "$1 $2" = "vm run-command" ]; then vm="$a"; else rulename="$a"; fi ;; --scripts) scripts="$a" ;; esac
  prev="$a"
done
case "$1 $2" in
  "network nsg")   # nsg rule create
    if [ -n "${MOCK_RULE_FAIL:-}" ] && [ "$rulename" = "${MOCK_RULE_FAIL}" ]; then
      echo "mock: forced failure creating $rulename" >&2; exit 1; fi
    exit 0 ;;
  "vm run-command")
    [ -n "$vm" ] && printf '%s' "$scripts" > "${MOCK_CAP}/${vm}.script"
    printf '[stdout]\n__AZRUN_OK__\n[stderr]\n'; exit 0 ;;
  *) exit 0 ;;
esac
AZ
chmod +x "${MOCKBIN}/az"

run_fix() {
  local name="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; CAP="${casedir}/cap"; MANIFEST="${casedir}/run-manifest.json"; LOG="${casedir}/log"
  mkdir -p "$casedir" "$CAP"; : > "$AZLOG"; manifest::init "$MANIFEST"
  env \
    MOCK_AZ_LOG="$AZLOG" MOCK_CAP="$CAP" AZ_BIN="${MOCKBIN}/az" \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" \
    DATE_UTC="20260820" GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
    MANIFEST_PATH="$MANIFEST" AZRUN_ATTEMPTS="2" AZRUN_DELAY="0" \
    TTFX_RETRY_ATTEMPTS="1" TTFX_RETRY_DELAY="0" \
    "$@" bash "$FIX_SH" "$cmd" >"$LOG" 2>"${LOG}.err"
  RC=$?
}
azline() { grep -- "$1" "$AZLOG" | tr '\n' '|'; }

echo "== fail fast on missing inputs =="
run_fix miss_topo all PRIMARY_SUBSCRIPTION_ID=sub-a TOPOLOGY=""
assert_nonzero "missing TOPOLOGY fails" "$RC"
run_fix miss_sub all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=""
assert_nonzero "missing PRIMARY_SUBSCRIPTION_ID fails" "$RC"

echo "== creates rule 190 Deny backend->frontend + rule 200 Allow frontend->backend:8080 (FR-021) =="
run_fix ok all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary
assert_eq "fixtures provisioning succeeds" "0" "$RC"
DENY="$(azline DenyBackendToFrontend)"
assert_match "DenyBackendToFrontend is created on the centraluseuap NSG" "nsg-name ${NSG}" "$DENY"
assert_match "Deny rule has priority 190" 'priority 190' "$DENY"
assert_match "Deny rule action is Deny" 'access Deny' "$DENY"
assert_match "Deny rule source ASG is asg-backend" 'source-asgs [^|]*asg-backend' "$DENY"
assert_match "Deny rule destination ASG is asg-frontend" 'destination-asgs [^|]*asg-frontend' "$DENY"
assert_match "Deny rule carries the explicit NSG subscription (RD-020)" 'subscription sub-primary' "$DENY"
ALLOW="$(azline AllowFrontendToBackend)"
assert_match "AllowFrontendToBackend priority 200" 'priority 200' "$ALLOW"
assert_match "Allow rule action is Allow" 'access Allow' "$ALLOW"
assert_match "Allow rule protocol is Tcp" 'protocol Tcp' "$ALLOW"
assert_match "Allow rule destination port is 8080" 'destination-port-ranges [^|]*8080' "$ALLOW"
assert_match "Allow rule source ASG is asg-frontend" 'source-asgs [^|]*asg-frontend' "$ALLOW"
assert_match "Allow rule destination ASG is asg-backend" 'destination-asgs [^|]*asg-backend' "$ALLOW"

echo "== applies app=backend/app=frontend PodASGMappings in namespace default (CON-003) =="
S="$(cat "${CAP}/${CP}.script" 2>/dev/null)"
assert_eq "mappings applied on the centraluseuap control-plane node" "yes" \
  "$([[ -n "$S" ]] && echo yes || echo no)"
assert_match "applies via kubectl" 'kubectl apply -f -' "$S"
# The manifests are shipped base64-encoded (safe transport, like deploy-controller.sh);
# decode the payload before asserting on the rendered YAML.
MAP="$(grep -oE '[A-Za-z0-9+/=]{200,}' <<<"$S" | head -1 | base64 -d 2>/dev/null)"
assert_match "namespace is default" 'namespace: default' "$MAP"
assert_match "backend mapping selects app: backend" 'app: backend' "$MAP"
assert_match "frontend mapping selects app: frontend" 'app: frontend' "$MAP"
assert_match "backend-asg-mapping is present" 'backend-asg-mapping' "$MAP"
assert_match "frontend-asg-mapping is present" 'frontend-asg-mapping' "$MAP"
assert_match "mappings reference the asg-backend resource id" "applicationSecurityGroups/asg-backend" "$MAP"
assert_match "mappings reference the asg-frontend resource id" "applicationSecurityGroups/asg-frontend" "$MAP"
assert_match "ASGs resolve under the primary RG (shared, FR-028)" "resourceGroups/${PRIMARY_RG}" "$MAP"

echo "== manifest records the enforcement fixtures =="
assert_match "manifest records rule 190" '190' "$(manifest::get "$MANIFEST" '.validate.tt.fixtures.nsg_rules')"
assert_match "manifest records rule 200" '200' "$(manifest::get "$MANIFEST" '.validate.tt.fixtures.nsg_rules')"
assert_eq "manifest records the NSG under test" "$NSG" "$(manifest::get "$MANIFEST" '.validate.tt.fixtures.nsg')"

echo "== a rule-create failure fails closed (FR-021) =="
run_fix denyfail nsg-rules TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary MOCK_RULE_FAIL=DenyBackendToFrontend
assert_nonzero "fixtures provisioning fails when a rule cannot be created" "$RC"

echo "== TT_ASG_RG / TT_ASG_SUB override the ASG scope (cross-region seam) =="
run_fix override all TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-primary \
  TT_ASG_RG=custom-asg-rg TT_ASG_SUB=asg-sub
DENY2="$(azline DenyBackendToFrontend)"
assert_match "override ASG RG is honored in the rule ASG ids" 'resourceGroups/custom-asg-rg' "$DENY2"

echo
printf 'provision_tt_fixtures_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
