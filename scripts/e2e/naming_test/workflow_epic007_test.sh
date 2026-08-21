#!/usr/bin/env bash
# Hermetic static workflow contract tests for EPIC-007.
# Traceability: ITEM-022..025, AC-014, FR-013..FR-015, RD-008/RD-009.
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW="${TEST_DIR}/../../../.github/workflows/e2e-validation-release.yml"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_contains() {
  local name="$1" pattern="$2"
  if grep -Eq -- "$pattern" "$WORKFLOW"; then pass "$name"; else fail "$name"; fi
}
assert_job_contains() {
  local name="$1" job="$2" pattern="$3"
  if awk -v job="$job" '
      $0 == "jobs:" {in_jobs=1; next}
      in_jobs && $0 == "  " job ":" {inside=1; next}
      inside && /^  [A-Za-z0-9_-]+:$/ {exit}
      inside {print}
    ' "$WORKFLOW" | grep -Eq -- "$pattern"; then pass "$name"; else fail "$name"; fi
}
assert_input_contains() {
  local name="$1" input="$2" pattern="$3"
  if awk -v input="$input" '
      $0 == "      " input ":" {inside=1; next}
      inside && /^      [A-Za-z0-9_-]+:$/ {exit}
      inside {print}
    ' "$WORKFLOW" | grep -Eq -- "$pattern"; then pass "$name"; else fail "$name"; fi
}
assert_job_excludes() {
  local name="$1" job="$2" pattern="$3"
  if awk -v job="$job" '
      $0 == "jobs:" {in_jobs=1; next}
      in_jobs && $0 == "  " job ":" {inside=1; next}
      inside && /^  [A-Za-z0-9_-]+:$/ {exit}
      inside {print}
    ' "$WORKFLOW" | grep -Eq -- "$pattern"; then fail "$name"; else pass "$name"; fi
}

echo "== ITEM-022: typed trigger matrix and fail-closed release inputs =="
assert_contains "nightly schedule trigger exists" '^  schedule:$'
assert_contains "published release trigger exists" '^  release:$'
assert_contains "v-tag trigger exists" 'tags: \["v\*"\]'
assert_input_contains "topology input is typed choice" validation_topologies 'type: choice'
assert_input_contains "region input is typed choice" regions 'type: choice'
assert_job_contains "meta resolves configured primary subscription" meta \
  "PRIMARY_SUBSCRIPTION_ID:.*vars\\.E2E_PRIMARY_SUBSCRIPTION_ID"
assert_job_contains "meta resolves configured secondary subscription" meta \
  "SECONDARY_SUBSCRIPTION_ID:.*vars\\.E2E_SECONDARY_SUBSCRIPTION_ID"

echo "== ITEM-023: environments and least-privilege event separation =="
for job in build build_cni provision rbac deploy validate_multicluster validate_tt \
  preflight_xs provision_xs_primary provision_xs_secondary rbac_xs deploy_xs \
  validate_cross_subscription diagnostics_ss cleanup_ss diagnostics_xs cleanup_xs; do
  assert_job_contains "${job} uses azure-e2e" "$job" 'environment: azure-e2e'
done
for job in release_gate release; do
  assert_job_contains "${job} uses public-release" "$job" 'environment: public-release'
done
assert_job_excludes "PR controller build has no OIDC permission" build_pr 'id-token: write'
assert_job_excludes "PR CNI build has no OIDC permission" build_cni_pr 'id-token: write'
assert_job_contains "PR controller build is PR-only" build_pr "if:.*github\\.event_name == 'pull_request'"
assert_job_contains "PR CNI build is PR-only" build_cni_pr "if:.*github\\.event_name == 'pull_request'"

echo "== ITEM-024: event-specific concurrency and bounded execution =="
assert_contains "cloud runs share a serialized concurrency group" "github\\.event_name == 'pull_request'.*pnc-e2e-pr-.*pnc-e2e-cloud"
assert_contains "only PR fast checks cancel in progress" "cancel-in-progress:.*github\\.event_name == 'pull_request'"
assert_job_contains "summary has a timeout" summary 'timeout-minutes:'

echo "== ITEM-025: always-on auditable manifest finalization =="
assert_job_contains "summary always runs" summary 'if: \$\{\{ always\(\) \}\}'
assert_job_contains "summary downloads manifest fragments" summary 'pattern: run-manifest-\*'
assert_job_contains "summary invokes finalizer" summary 'finalize-manifest\.sh'
assert_job_contains "summary uploads final run manifest" summary 'name: run-manifest-final'

echo
printf 'workflow_epic007_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
