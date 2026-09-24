#!/usr/bin/env bash
# Static workflow contracts for final release-pipeline review fixes.
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW="${TEST_DIR}/../../../.github/workflows/e2e-validation-release.yml"
PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_contains() { if grep -Eq -- "$2" "$WORKFLOW"; then pass "$1"; else fail "$1"; fi; }
assert_job_contains() {
  local name="$1" job="$2" pattern="$3"
  if awk -v job="$job" '$0 == "jobs:" {in_jobs=1; next} in_jobs && $0 == "  " job ":" {inside=1; next} inside && /^  [A-Za-z0-9_-]+:$/ {exit} inside {print}' \
    "$WORKFLOW" | grep -Eq -- "$pattern"; then pass "$name"; else fail "$name"; fi
}
assert_input_type() {
  local name="$1" input="$2" type="$3"
  if awk -v input="$input" '$0 == "      " input ":" {inside=1; next} inside && /^      [A-Za-z0-9_-]+:$/ {exit} inside {print}' \
    "$WORKFLOW" | grep -Eq "type: ${type}"; then pass "$name"; else fail "$name"; fi
}

echo "== PR trigger covers all controller/CNI changes =="
if awk '/^  pull_request:/{inside=1; next} inside && /^  [A-Za-z_]+:/{exit} inside {print}' "$WORKFLOW" | grep -q 'paths:'; then
  fail "pull_request has no restrictive paths filter"
else
  pass "pull_request has no restrictive paths filter"
fi

echo "== FR-012 typed dispatch inputs and propagation =="
assert_input_type "run_full_validation is boolean" run_full_validation boolean
assert_input_type "moving_tags is string" moving_tags string
assert_input_type "keep_resources_on_failure is boolean" keep_resources_on_failure boolean
assert_job_contains "meta receives run_full_validation" meta 'INPUT_RUN_FULL_VALIDATION:'
assert_job_contains "meta receives moving tags" meta 'INPUT_MOVING_TAGS:'
assert_job_contains "release receives per-run moving tags from meta" release \
  'MOVING_TAGS: \$\{\{ needs\.meta\.outputs\.moving_tags \}\}'
assert_job_contains "ss retention is conditioned on upstream failure" cleanup_ss \
  'KEEP_RESOURCES:.*needs\.meta\.outputs\.keep_resources_on_failure.*failure'
assert_job_contains "xs retention is conditioned on upstream failure" cleanup_xs \
  'KEEP_RESOURCES:.*needs\.meta\.outputs\.keep_resources_on_failure.*failure'

echo "== final artifact cleanup runs after topology and release terminal paths =="
assert_job_contains "artifact cleanup is always-on" artifact_cleanup 'if: \$\{\{ always\(\)'
assert_job_contains "artifact cleanup waits for both topology cleanups and release" artifact_cleanup \
  'needs: .*cleanup_ss.*cleanup_xs.*release'
assert_job_contains "artifact cleanup has explicit staging ACR context" artifact_cleanup \
  'STAGING_ACR: \$\{\{ vars\.E2E_STAGING_ACR \}\}'
assert_job_contains "artifact cleanup invokes cleanup script" artifact_cleanup \
  'cleanup-artifacts\.sh cleanup'
assert_job_contains "summary waits for artifact cleanup" summary 'needs: .*artifact_cleanup'

echo
printf 'workflow_final_review_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
