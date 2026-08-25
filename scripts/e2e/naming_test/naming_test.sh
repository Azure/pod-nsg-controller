#!/usr/bin/env bash
# =============================================================================
# naming_test.sh - deterministic-naming test suite (PRD ITEM-003 / TEST-003).
#
# Pure-shell tests for scripts/e2e/naming.sh. No external test framework is
# required (bats is intentionally NOT a dependency of this repository); the
# suite is a directly executable script that self-reports pass/fail counts and
# exits non-zero if any assertion fails.
#
# Coverage (PRD Section 3.4 / TEST-003 / AC-012):
#   * Determinism             - same inputs always yield identical names.
#   * Known-answer vectors     - RUN_SUFFIX matches an independently computed value.
#   * Topology separation      - ss names never equal xs names for one run/region.
#   * Charset / length limits  - every emitted name honours its provider bounds.
#   * fit() truncation         - deterministic, length-exact, collision-resistant.
#   * Prefix-set invariant     - len(RG)+1+len(ns)+1+len(mapping) <= 80 (3.4.4).
#   * Golden snapshot          - core names match a hand-authored golden file.
#   * self-check               - naming.sh self-check exits 0.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMING_SH="${TEST_DIR}/../naming.sh"
GOLDEN_FILE="${TEST_DIR}/golden/core-names.txt"

PASS=0
FAIL=0

red()   { printf '\033[31m%s\033[0m' "$1"; }
green() { printf '\033[32m%s\033[0m' "$1"; }

pass() { PASS=$((PASS + 1)); printf '  %s %s\n' "$(green PASS)" "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  %s %s\n' "$(red FAIL)" "$1" >&2; }

# assert_eq <description> <expected> <actual>
assert_eq() {
  local desc="$1" expected="$2" actual="$3"
  if [[ "$expected" == "$actual" ]]; then
    pass "$desc"
  else
    fail "$desc"
    printf '        expected: [%s]\n' "$expected" >&2
    printf '        actual:   [%s]\n' "$actual" >&2
  fi
}

# assert_match <description> <ERE-regex> <actual>
assert_match() {
  local desc="$1" regex="$2" actual="$3"
  if [[ "$actual" =~ $regex ]]; then
    pass "$desc"
  else
    fail "$desc"
    printf '        value [%s] did not match /%s/\n' "$actual" "$regex" >&2
  fi
}

# assert_len_le <description> <max> <value>
assert_len_le() {
  local desc="$1" max="$2" value="$3"
  if (( ${#value} <= max )); then
    pass "$desc (len=${#value} <= $max)"
  else
    fail "$desc (len=${#value} > $max): $value"
  fi
}

# assert_ok <description> -- <command...>   (expects success)
assert_ok() {
  local desc="$1"; shift; [[ "${1:-}" == "--" ]] && shift
  if "$@" >/dev/null 2>&1; then pass "$desc"; else fail "$desc (command failed: $*)"; fi
}

# assert_fails <description> -- <command...>  (expects non-zero exit)
assert_fails() {
  local desc="$1"; shift; [[ "${1:-}" == "--" ]] && shift
  if "$@" >/dev/null 2>&1; then fail "$desc (expected failure but succeeded: $*)"; else pass "$desc"; fi
}

# ---------------------------------------------------------------------------
if [[ ! -f "$NAMING_SH" ]]; then
  printf '%s naming library not found at %s\n' "$(red FATAL)" "$NAMING_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$NAMING_SH"

# Fixed, deterministic inputs used throughout the suite.
REPO="Azure/pod-nsg-controller"
RUN_ID="10293847561"
DATE_UTC="20260820"
SUFFIX="vahdkc"   # = naming::run_suffix "$REPO" "$RUN_ID" 1  (independently verified)

echo "== primitives: run_suffix (determinism + known-answer vectors) =="
assert_eq "run_suffix known vector (attempt 1)" "vahdkc" "$(naming::run_suffix "$REPO" "$RUN_ID" 1)"
assert_eq "run_suffix known vector (attempt 2)" "7fivbk" "$(naming::run_suffix "$REPO" "$RUN_ID" 2)"
assert_eq "run_suffix known vector (run 999)"   "v8d4ni" "$(naming::run_suffix "$REPO" 999 1)"
assert_eq "run_suffix known vector (run 1)"     "azb0bz" "$(naming::run_suffix "$REPO" 1 1)"
assert_eq "run_suffix is deterministic" \
  "$(naming::run_suffix "$REPO" "$RUN_ID" 1)" "$(naming::run_suffix "$REPO" "$RUN_ID" 1)"
assert_match "run_suffix charset/length [0-9a-z]{6}" '^[0-9a-z]{6}$' "$(naming::run_suffix "$REPO" "$RUN_ID" 1)"
assert_match "run_suffix charset/length (large ids)" '^[0-9a-z]{6}$' "$(naming::run_suffix "$REPO" 999999999999 7)"
if [[ "$(naming::run_suffix "$REPO" "$RUN_ID" 1)" != "$(naming::run_suffix "$REPO" "$RUN_ID" 2)" ]]; then
  pass "run_suffix differs across run attempts"
else
  fail "run_suffix should differ across run attempts"
fi

# Optional independent cross-check against a Python reference implementation.
if command -v python3 >/dev/null 2>&1; then
  py_suffix="$(python3 - "$REPO" "$RUN_ID" 1 <<'PY'
import hashlib, sys
repo, rid, att = sys.argv[1], sys.argv[2], sys.argv[3]
d = "0123456789abcdefghijklmnopqrstuvwxyz"
n = int(hashlib.sha256(f"{repo}:{rid}:{att}".encode()).hexdigest()[0:8], 16) % (36 ** 6)
s = ""
while n:
    s = d[n % 36] + s
    n //= 36
print((s or "0").rjust(6, "0"))
PY
)"
  assert_eq "run_suffix matches Python reference" "$py_suffix" "$(naming::run_suffix "$REPO" "$RUN_ID" 1)"
fi

echo "== primitives: normalize / alnum =="
assert_eq "normalize lowercases + dashes non-alnum" "eastus2-euap" "$(naming::normalize 'EastUS2_EUAP!!')"
assert_eq "normalize collapses + strips dashes"     "a-b-c"        "$(naming::normalize '__A--b__c__')"
assert_eq "alnum removes non-alnum + lowercases"    "pnce2ess"     "$(naming::alnum 'PNC-e2e_ss')"

echo "== base / rbase tokens =="
base_ss="$(naming::base pnc-e2e ss "$DATE_UTC" "$SUFFIX")"
base_xs="$(naming::base pnc-e2e xs "$DATE_UTC" "$SUFFIX")"
assert_eq "BASE(ss)" "pnc-e2e-ss-20260820-vahdkc" "$base_ss"
assert_eq "BASE(xs)" "pnc-e2e-xs-20260820-vahdkc" "$base_xs"
assert_eq "RBASE(ss,eastus2euap)"   "pnc-e2e-ss-20260820-vahdkc-eastus2euap"   "$(naming::rbase "$base_ss" eastus2euap)"
assert_eq "RBASE(xs,centraluseuap)" "pnc-e2e-xs-20260820-vahdkc-centraluseuap" "$(naming::rbase "$base_xs" centraluseuap)"
assert_len_le "RBASE respects 44-char cap" 44 "$(naming::rbase "$base_xs" centraluseuap)"

echo "== topology separation (ss names never equal xs names) =="
if [[ "$base_ss" != "$base_xs" ]]; then pass "BASE differs across topologies"; else fail "BASE must differ across topologies"; fi
for region in eastus2euap centraluseuap; do
  r_ss="$(naming::rbase "$base_ss" "$region")"
  r_xs="$(naming::rbase "$base_xs" "$region")"
  if [[ "$r_ss" != "$r_xs" ]]; then pass "RBASE differs across topologies ($region)"; else fail "RBASE must differ across topologies ($region)"; fi
done

echo "== storage account (alnum, global, <=24) =="
assert_eq "storage_account(xs) matches spec example" "pnce2exs20260820k3f9q2" "$(naming::storage_account pnc-e2e xs 20260820 k3f9q2)"
sa="$(naming::storage_account pnc-e2e ss "$DATE_UTC" "$SUFFIX")"
assert_eq "storage_account(ss)" "pnce2ess20260820vahdkc" "$sa"
assert_match "storage_account charset ^[a-z0-9]+$" '^[a-z0-9]+$' "$sa"
assert_len_le "storage_account <= 24" 24 "$sa"

echo "== staging run tag (topology-neutral, <=128) =="
assert_eq "staging run tag" "run-20260820-vahdkc" "$(naming::staging_run_tag "$DATE_UTC" "$SUFFIX")"
assert_match "staging run tag charset" '^[A-Za-z0-9_][A-Za-z0-9_.-]*$' "$(naming::staging_run_tag "$DATE_UTC" "$SUFFIX")"

echo "== fit() truncation (deterministic, length-exact, collision-resistant) =="
assert_eq "fit() leaves short names unchanged" "abc" "$(naming::fit abc 44)"
boundary="$(printf 'a%.0s' $(seq 1 44))"          # exactly 44 chars
assert_eq "fit() leaves boundary (==max) unchanged" "$boundary" "$(naming::fit "$boundary" 44)"
prefix39="$(printf 'a%.0s' $(seq 1 39))"
longA="${prefix39}xxxxxxxxxxxx"                    # 51 chars, shares 39-char prefix
longB="${prefix39}yyyyyyyyyyyy"                    # 51 chars, shares 39-char prefix
fitA="$(naming::fit "$longA" 44)"
fitB="$(naming::fit "$longB" 44)"
assert_eq  "fit() output is exactly maxLen"        "44" "${#fitA}"
hA="$(printf '%s' "$longA" | sha256sum | cut -c1-4)"   # independent hash cross-check
assert_eq  "fit() assembly = name[0:maxLen-5]-hash4" "${prefix39}-${hA}" "$fitA"
assert_eq  "fit() is deterministic"                "$fitA" "$(naming::fit "$longA" 44)"
if [[ "$fitA" != "$fitB" ]]; then pass "fit() distinguishes shared-prefix names"; else fail "fit() must distinguish shared-prefix names"; fi

echo "== address-prefix-set invariant (3.4.4: RG+ns+mapping <= 80) =="
rg_worst="$(naming::rbase "$base_xs" centraluseuap)"   # longest default RG (40)
assert_eq "prefix_set_name lowercases RG + joins parts" \
  "pnc-e2e-xs-20260820-vahdkc-centraluseuap-default-frontend-asg-mapping" \
  "$(naming::prefix_set_name "$rg_worst" default frontend-asg-mapping)"
for ns in test-apps default; do
  for mapping in backend-asg-mapping frontend-asg-mapping; do
    assert_len_le "prefix_set len ($ns/$mapping)" 80 "$(naming::prefix_set_name "$rg_worst" "$ns" "$mapping")"
    assert_ok "prefix-set invariant holds ($ns/$mapping)" -- naming::validate_prefix_set_invariant "$rg_worst" "$ns" "$mapping"
  done
done
rg_toolong="$(printf 'r%.0s' $(seq 1 60))"
assert_fails "prefix-set invariant rejects oversized RG" -- naming::validate_prefix_set_invariant "$rg_toolong" test-apps frontend-asg-mapping

echo "== name validators (charset + length, fail-fast) =="
assert_ok    "validate_name accepts a valid RBASE" -- naming::validate_name "$rg_worst" 1 90 '^[a-z0-9-]+$' resource-group
assert_fails "validate_name rejects over-length"   -- naming::validate_name "$(printf 'a%.0s' $(seq 1 91))" 1 90 '^[a-z0-9-]+$' resource-group
assert_fails "validate_name rejects bad charset"   -- naming::validate_name 'Bad_Name!' 1 90 '^[a-z0-9-]+$' resource-group

echo "== golden snapshot (core names) =="
if [[ -f "$GOLDEN_FILE" ]]; then
  flat="$(REPO="$REPO" RUN_ID="$RUN_ID" RUN_ATTEMPT=1 DATE_UTC="$DATE_UTC" bash "$NAMING_SH" dump-flat 2>/dev/null)"
  missing=0
  while IFS= read -r line; do
    [[ -z "$line" || "$line" == \#* ]] && continue
    if ! grep -Fxq -- "$line" <<<"$flat"; then
      missing=$((missing + 1))
      printf '        golden line absent from dump-flat: [%s]\n' "$line" >&2
    fi
  done < "$GOLDEN_FILE"
  if (( missing == 0 )); then pass "dump-flat contains every golden core name"; else fail "dump-flat missing $missing golden core name(s)"; fi
else
  fail "golden file missing at $GOLDEN_FILE"
fi

echo "== naming.sh self-check =="
assert_ok "self-check exits 0" -- env REPO="$REPO" RUN_ID="$RUN_ID" RUN_ATTEMPT=1 DATE_UTC="$DATE_UTC" bash "$NAMING_SH" self-check

# ---------------------------------------------------------------------------
echo
if (( FAIL == 0 )); then fail_str="$(green 0)"; else fail_str="$(red "$FAIL")"; fi
printf 'naming_test: %s passed, %s failed\n' "$(green "$PASS")" "$fail_str"
(( FAIL == 0 ))
