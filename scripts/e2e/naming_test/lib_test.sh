#!/usr/bin/env bash
# =============================================================================
# lib_test.sh - smoke tests for the shared foundation helpers in ../lib.sh.
#
# Focused on the run-manifest emit foundation (used by the `meta` job) and the
# retry/backoff helper. Directly executable; no external framework required.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.lib_test_work"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() {
  if [[ "$2" == "$3" ]]; then pass "$1"; else
    fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2
  fi
}

if [[ ! -f "$LIB_SH" ]]; then
  printf 'FATAL lib.sh not found at %s\n' "$LIB_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$WORK"
MANIFEST="${WORK}/run-manifest.json"

echo "== manifest init/put/put_json/merge/get =="
manifest::init "$MANIFEST"
assert_eq "init creates empty object" "{}" "$(jq -c '.' "$MANIFEST")"

manifest::put "$MANIFEST" run.run_suffix vahdkc
assert_eq "put sets nested string" "vahdkc" "$(manifest::get "$MANIFEST" '.run.run_suffix')"

manifest::put_json "$MANIFEST" release.requested true
assert_eq "put_json sets nested boolean" "true" "$(manifest::get "$MANIFEST" '.release.requested')"

manifest::merge "$MANIFEST" '{"names":{"ss":{"base":"pnc-e2e-ss-20260820-vahdkc"}}}'
assert_eq "merge adds a subtree" "pnc-e2e-ss-20260820-vahdkc" "$(manifest::get "$MANIFEST" '.names.ss.base')"
assert_eq "merge preserves prior keys" "vahdkc" "$(manifest::get "$MANIFEST" '.run.run_suffix')"
assert_eq "manifest remains valid JSON" "0" "$(jq empty "$MANIFEST"; echo $?)"

echo "== retry/backoff =="
attempts_file="${WORK}/attempts"
echo 0 > "$attempts_file"
flaky() {
  local n; n=$(<"$attempts_file"); n=$((n + 1)); echo "$n" > "$attempts_file"
  (( n >= 3 ))
}
if lib::retry 5 0 -- flaky; then pass "retry succeeds once command recovers"; else fail "retry should have succeeded"; fi
assert_eq "retry stopped at first success" "3" "$(<"$attempts_file")"

if lib::retry 2 0 -- false >/dev/null 2>&1; then fail "retry should fail after exhausting attempts"; else pass "retry returns non-zero after exhaustion"; fi

echo "== gha helpers degrade to stdout when unset =="
assert_eq "gha::output prints key=value locally" "foo=bar" "$(GITHUB_OUTPUT='' gha::output foo bar)"

echo "== manifest::record_controller_artifact (EPIC-002 / ITEM-006) =="
# Push mode: the immutable digest reference (@sha256) is what downstream jobs consume.
ART="${WORK}/artifact.json"
manifest::init "$ART"
manifest::record_controller_artifact "$ART" "pncstg.azurecr.io" "candidate/pod-nsg-controller" \
  "run-20260820-vahdkc" "sha256:$(printf 'a%.0s' $(seq 1 64))" true
assert_eq "push: reference is by @sha256 digest" \
  "pncstg.azurecr.io/candidate/pod-nsg-controller@sha256:$(printf 'a%.0s' $(seq 1 64))" \
  "$(manifest::get "$ART" '.artifacts.controller.reference')"
assert_eq "push: pushed=true recorded" "true" "$(manifest::get "$ART" '.artifacts.controller.pushed')"
assert_eq "push: digest recorded" "sha256:$(printf 'a%.0s' $(seq 1 64))" \
  "$(manifest::get "$ART" '.artifacts.controller.digest')"
assert_eq "push: repo recorded" "candidate/pod-nsg-controller" \
  "$(manifest::get "$ART" '.artifacts.controller.repo')"
assert_eq "push: exactly one controller artifact recorded (AC-001)" "1" \
  "$(manifest::get "$ART" '.artifacts | length')"

# No-push mode with a registry present: reference falls back to registry/repo:tag.
manifest::init "$ART"
manifest::record_controller_artifact "$ART" "pncstg.azurecr.io" "candidate/pod-nsg-controller" \
  "run-20260820-vahdkc" "sha256:deadbeef" false
assert_eq "no-push(registry): reference is by tag" \
  "pncstg.azurecr.io/candidate/pod-nsg-controller:run-20260820-vahdkc" \
  "$(manifest::get "$ART" '.artifacts.controller.reference')"
assert_eq "no-push(registry): pushed=false recorded" "false" \
  "$(manifest::get "$ART" '.artifacts.controller.pushed')"

# PR local mode (no registry): reference is the bare repo:tag.
manifest::init "$ART"
manifest::record_controller_artifact "$ART" "" "candidate/pod-nsg-controller" \
  "run-20260820-vahdkc" "sha256:localid" false
assert_eq "pr-local: reference is bare repo:tag" \
  "candidate/pod-nsg-controller:run-20260820-vahdkc" \
  "$(manifest::get "$ART" '.artifacts.controller.reference')"
assert_eq "pr-local: local digest still recorded" "sha256:localid" \
  "$(manifest::get "$ART" '.artifacts.controller.digest')"
assert_eq "pr-local: manifest remains valid JSON" "0" "$(jq empty "$ART"; echo $?)"

# Recording preserves prior manifest keys (build augments meta's manifest in place).
manifest::init "$ART"
manifest::put "$ART" run.run_suffix vahdkc
manifest::record_controller_artifact "$ART" "pncstg.azurecr.io" "candidate/pod-nsg-controller" \
  "run-20260820-vahdkc" "sha256:abc" true
assert_eq "record preserves prior run.* keys" "vahdkc" "$(manifest::get "$ART" '.run.run_suffix')"

echo "== lib::validate_canary_region (EPIC-003 / ITEM-010 / CON-001) =="
assert_eq "accepts eastus2euap"   "eastus2euap"   "$(lib::validate_canary_region eastus2euap)"
assert_eq "accepts centraluseuap" "centraluseuap" "$(lib::validate_canary_region centraluseuap)"
assert_eq "normalizes case + surrounding space" "eastus2euap" "$(lib::validate_canary_region '  EastUS2EUAP ')"
if lib::validate_canary_region westus2 >/dev/null 2>&1; then
  fail "non-canary westus2 must be rejected (CON-001)"; else pass "rejects non-canary region westus2"; fi
if lib::validate_canary_region '' >/dev/null 2>&1; then
  fail "empty region must be rejected"; else pass "rejects empty region"; fi

echo "== lib::csv_to_json_array (EPIC-003 / ITEM-010 provision matrix) =="
assert_eq "two regions -> JSON array" '["eastus2euap","centraluseuap"]' \
  "$(lib::csv_to_json_array 'eastus2euap,centraluseuap')"
assert_eq "trims spaces and drops trailing empty" '["eastus2euap","centraluseuap"]' \
  "$(lib::csv_to_json_array ' eastus2euap , centraluseuap ,')"
assert_eq "single element" '["eastus2euap"]' "$(lib::csv_to_json_array 'eastus2euap')"
assert_eq "empty string -> []" '[]' "$(lib::csv_to_json_array '')"

echo "== lib::az_vm_quota_preflight (EPIC-003 / ITEM-010 / RISK-003) =="
QWORK="${WORK}/quota"; QBIN="${QWORK}/bin"; mkdir -p "$QBIN"
QAZLOG="${QWORK}/az.log"
cat > "${QBIN}/az" <<'AZ'
#!/usr/bin/env bash
echo "$*" >> "${MOCK_AZ_LOG}"
fam_limit="${MOCK_QUOTA_FAMILY_LIMIT:-100}"; fam_used="${MOCK_QUOTA_FAMILY_USED:-0}"
tot_limit="${MOCK_QUOTA_TOTAL_LIMIT:-200}";  tot_used="${MOCK_QUOTA_TOTAL_USED:-0}"
fam='{"currentValue":'"$fam_used"',"limit":'"$fam_limit"',"name":{"value":"standardDSv5Family","localizedValue":"Standard DSv5 Family vCPUs"},"unit":"Count"}'
tot='{"currentValue":'"$tot_used"',"limit":'"$tot_limit"',"name":{"value":"cores","localizedValue":"Total Regional vCPUs"},"unit":"Count"}'
if [[ "${MOCK_QUOTA_OMIT_FAMILY:-0}" == "1" ]]; then printf '[%s]\n' "$tot"; else printf '[%s,%s]\n' "$fam" "$tot"; fi
exit 0
AZ
chmod +x "${QBIN}/az"

# Mock knobs are exported at top level (no subshells) so the mock child sees
# them and shellcheck stays clean; each case mutates one knob then resets it.
export MOCK_AZ_LOG="$QAZLOG"
export MOCK_QUOTA_FAMILY_LIMIT=100 MOCK_QUOTA_FAMILY_USED=0
export MOCK_QUOTA_TOTAL_LIMIT=200 MOCK_QUOTA_TOTAL_USED=0 MOCK_QUOTA_OMIT_FAMILY=0

: > "$QAZLOG"
if lib::az_vm_quota_preflight "${QBIN}/az" sub-123 eastus2euap standardDSv5Family 16 >/dev/null 2>&1; then
  pass "sufficient quota passes"; else fail "sufficient quota should pass"; fi
assert_eq "quota preflight passes explicit --subscription" "1" "$(grep -c -- '--subscription sub-123' "$QAZLOG")"
assert_eq "quota preflight passes --location region"       "1" "$(grep -c -- '--location eastus2euap' "$QAZLOG")"

MOCK_QUOTA_FAMILY_LIMIT=10
if lib::az_vm_quota_preflight "${QBIN}/az" sub eastus2euap standardDSv5Family 16 >/dev/null 2>&1; then
  fail "insufficient family quota should gate provisioning"; else pass "insufficient family quota fails (gates provisioning)"; fi
MOCK_QUOTA_FAMILY_LIMIT=100

MOCK_QUOTA_TOTAL_LIMIT=8
if lib::az_vm_quota_preflight "${QBIN}/az" sub eastus2euap standardDSv5Family 16 >/dev/null 2>&1; then
  fail "insufficient total regional vCPUs should fail"; else pass "insufficient total regional vCPUs fails"; fi
MOCK_QUOTA_TOTAL_LIMIT=200

MOCK_QUOTA_OMIT_FAMILY=1
if lib::az_vm_quota_preflight "${QBIN}/az" sub eastus2euap standardDSv5Family 16 >/dev/null 2>&1; then
  fail "missing family usage entry should fail closed"; else pass "missing family usage entry fails closed"; fi
MOCK_QUOTA_OMIT_FAMILY=0

if lib::az_vm_quota_preflight "${QBIN}/az" sub eastus2euap standardDSv5Family notanumber >/dev/null 2>&1; then
  fail "non-integer required vCPUs should be rejected"; else pass "rejects non-integer required vCPUs"; fi

echo "== azrun node-exec via 'az vm run-command' (EPIC-004 / ITEM-011) =="
# The runner has no path to the cluster API server, so kubectl runs ON the
# control-plane node through az vm run-command. This mock reproduces the POC
# message envelope ([stdout]..[stderr]) and the extension's "always exit 0"
# behaviour, so azrun::exec's sentinel-based failure detection is exercised.
AWORK="${WORK}/azrun"; ABIN="${AWORK}/bin"; mkdir -p "$ABIN"
AAZLOG="${AWORK}/az.log"
cat > "${ABIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
[[ "${MOCK_TRANSPORT_FAIL:-0}" == "1" ]] && exit 7
scripts=""; query=""; prev=""
for a in "$@"; do
  case "$prev" in --scripts) scripts="$a" ;; --query) query="$a" ;; esac
  prev="$a"
done
[[ "$1 $2" == "vm run-command" && "$query" == "value[0].message" ]] || exit 0
if [[ "$scripts" == *__AZRUN_OK__* ]]; then
  if [[ -n "${MOCK_NODE_FAIL_MATCH:-}" && "$scripts" == *"${MOCK_NODE_FAIL_MATCH}"* ]]; then
    printf '[stdout]\n__AZRUN_FAIL__ rc=1\nsimulated node failure\n[stderr]\n'
  else
    printf '[stdout]\n__AZRUN_OK__\n[stderr]\n'
  fi
else
  printf '[stdout]\nREADBACK:%s\n[stderr]\n' "${MOCK_READBACK:-hello}"
fi
exit 0
AZ
chmod +x "${ABIN}/az"
export MOCK_AZ_LOG="$AAZLOG"; : > "$AAZLOG"

assert_eq "azrun::_extract_stdout parses the [stdout] envelope" "line1
line2" "$(azrun::_extract_stdout "$(printf '[stdout]\nline1\nline2\n[stderr]\n')")"
assert_eq "azrun::_extract_stdout passes unframed text through" "raw" \
  "$(azrun::_extract_stdout "raw")"

out="$(MOCK_READBACK=four-ready azrun::capture "${ABIN}/az" sub-cp rg-a vm-cp 'kubectl get nodes')"
assert_eq "azrun::capture returns the node stdout" "READBACK:four-ready" "$out"
assert_eq "azrun::capture carries an explicit --subscription" "1" \
  "$(grep -c -- '--subscription sub-cp' "$AAZLOG")"
assert_eq "azrun::capture targets the control-plane vm/rg" "1" \
  "$(grep -c -- 'run-command invoke -g rg-a -n vm-cp' "$AAZLOG")"

: > "$AAZLOG"
if azrun::exec "${ABIN}/az" sub-cp rg-a vm-cp 'kubectl apply -f -' >/dev/null 2>&1; then
  pass "azrun::exec returns 0 when the node sentinel reports success"
else fail "azrun::exec should succeed on node success"; fi
assert_eq "azrun::exec wraps the script and carries --subscription" "1" \
  "$(grep -c -- '--subscription sub-cp' "$AAZLOG")"

# MOCK_NODE_FAIL_MATCH is read from the environment by the mock, forcing the
# node script's sentinel to report failure even though `az` still exits 0.
: > "$AAZLOG"
if MOCK_NODE_FAIL_MATCH=broken azrun::exec "${ABIN}/az" sub-cp rg-a vm-cp 'kubectl apply -f broken' >/dev/null 2>&1; then
  fail "azrun::exec must detect node failure despite az exit 0"
else pass "azrun::exec fails closed when the node script fails"; fi

: > "$AAZLOG"
if MOCK_TRANSPORT_FAIL=1 AZRUN_ATTEMPTS=2 AZRUN_DELAY=0 \
     azrun::capture "${ABIN}/az" sub-cp rg-a vm-cp 'kubectl get nodes' >/dev/null 2>&1; then
  fail "azrun::capture should fail on transport error"
else pass "azrun transport failure is bounded-retried then fails closed"; fi
assert_eq "azrun retried the bounded number of transport attempts" "2" \
  "$(grep -c -- 'run-command invoke' "$AAZLOG")"

echo
printf 'lib_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
