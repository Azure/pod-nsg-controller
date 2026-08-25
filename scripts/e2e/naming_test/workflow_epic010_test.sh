#!/usr/bin/env bash
# Hermetic static contract tests for EPIC-010 workflow wiring.
# Traceability: ITEM-036, ITEM-037, ITEM-038, ITEM-040, AC-024, AC-027.
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
      $0 == "  " job ":" {inside=1; next}
      inside && /^  [A-Za-z0-9_-]+:$/ {exit}
      inside {print}
    ' "$WORKFLOW" | grep -Eq -- "$pattern"; then
    pass "$name"
  else
    fail "$name"
  fi
}

echo "== ITEM-036: tenant-aware preflight before provisioning =="
assert_contains "primary subscription validation checks the primary tenant" \
  'lib::az_subscription_validate az "\$\{PRIMARY_SUBSCRIPTION_ID\}" "\$\{PRIMARY_TENANT_ID\}"'
assert_contains "secondary subscription validation checks the secondary tenant" \
  'lib::az_subscription_validate az "\$\{SECONDARY_SUBSCRIPTION_ID\}" "\$\{SECONDARY_TENANT_ID\}"'
assert_contains "xs primary provisioning depends on preflight and prior ss cleanup" \
  'needs: \[meta, preflight_xs, cleanup_ss\]'

echo "== ITEM-037/038: manifest inventory is passed through the xs chain =="
assert_job_contains "secondary provisioning downloads the primary xs manifest" provision_xs_secondary \
  'name: run-manifest-provision-xs-eastus2euap'
assert_job_contains "rbac downloads the combined secondary provisioning manifest" rbac_xs \
  'name: run-manifest-provision-xs-centraluseuap'
assert_job_contains "deploy downloads the RBAC assignment inventory" deploy_xs \
  'name: run-manifest-rbac-xs'
assert_job_contains "validation downloads the deploy manifest" validate_cross_subscription \
  'name: run-manifest-deploy-xs'

echo "== ITEM-040: cleanup receives assignment IDs and remains always-on =="
assert_job_contains "cleanup_xs uses always()" cleanup_xs \
  'if: \$\{\{ always\(\) && github\.event_name != '\''pull_request'\'''
assert_job_contains "cleanup downloads the RBAC assignment inventory" cleanup_xs \
  'Download RBAC assignment inventory for verified cleanup'
assert_job_contains "cleanup tolerates a missing RBAC artifact after an upstream failure" cleanup_xs \
  'continue-on-error: true'

echo
printf 'workflow_epic010_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
