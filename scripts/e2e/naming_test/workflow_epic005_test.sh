#!/usr/bin/env bash
# Hermetic static contract tests for EPIC-005 workflow wiring.
# Traceability: ITEM-015..018, AC-007, AC-008, FR-006..008, FR-017.
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW="${TEST_DIR}/../../../.github/workflows/e2e-validation-release.yml"
REAPER="${TEST_DIR}/../../../.github/workflows/e2e-reaper.yml"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_job_contains() {
  local name="$1" job="$2" pattern="$3"
  if awk -v job="$job" '
      $0 == "jobs:" {in_jobs=1; next}
      in_jobs && $0 == "  " job ":" {inside=1; next}
      inside && /^  [A-Za-z0-9_-]+:$/ {exit}
      inside {print}
    ' "$WORKFLOW" | grep -Eq -- "$pattern"; then pass "$name"; else fail "$name"; fi
}

echo "== ITEM-015: same-subscription diagnostics are always retained =="
assert_job_contains "diagnostics_ss uses always()" diagnostics_ss 'if: \$\{\{ always\(\)'
assert_job_contains "diagnostics_ss invokes the shared collector" diagnostics_ss 'collect-diagnostics\.sh all'
assert_job_contains "diagnostics_ss artifacts are retained 30 days" diagnostics_ss 'retention-days: 30'

echo "== ITEM-016: same-subscription cleanup is always-on and release-aware =="
assert_job_contains "cleanup_ss uses always()" cleanup_ss 'if: \$\{\{ always\(\)'
assert_job_contains "cleanup_ss invokes topology-aware teardown" cleanup_ss 'teardown\.sh cleanup'
assert_job_contains "cleanup_ss receives release status" cleanup_ss 'RELEASE_REQUESTED:'

echo "== ITEM-017: scheduled dual-subscription reaper uses hardened reap =="
if grep -Eq 'schedule:' "$REAPER" && grep -Eq 'teardown\.sh reap' "$REAPER"; then
  pass "dual-subscription reaper is scheduled"
else
  fail "dual-subscription reaper is scheduled"
fi

echo "== ITEM-018: every required gate is a release_gate dependency =="
for gate in naming-tests validate_multicluster validate_tt cleanup_ss validate_cross_subscription cleanup_xs; do
  assert_job_contains "release gate depends on ${gate}" release_gate \
    "needs: .*${gate}"
done
assert_job_contains "release gate evaluates complete statuses" release_gate 'REQUIRE_COMPLETE=1'

echo
printf 'workflow_epic005_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
