#!/usr/bin/env bash
# Hermetic static contracts for EPIC-008 documentation and lint wiring.
# Traceability: ITEM-026, ITEM-027, TEST-001.
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${TEST_DIR}/../../.."
WORKFLOW="${REPO_ROOT}/.github/workflows/e2e-validation-release.yml"
README="${REPO_ROOT}/README.md"
BOOTSTRAP="${REPO_ROOT}/docs/projects/infrastructure-e2e-validation-pipeline/bootstrap.md"
MAKEFILE="${REPO_ROOT}/Makefile"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_contains() {
  local name="$1" file="$2" pattern="$3"
  if grep -Eq -- "$pattern" "$file"; then pass "$name"; else fail "$name"; fi
}
assert_job_contains() {
  local name="$1" job="$2" pattern="$3"
  if awk -v job="$job" '
      $0 == "jobs:" {in_jobs=1; next}
      in_jobs && $0 == "  " job ":" {inside=1; next}
      inside && /^  [A-Za-z0-9_-]+:$/ {exit}
      inside {print}
    ' "$WORKFLOW" | grep -Eq -- "$pattern"; then
    pass "$name"
  else
    fail "$name"
  fi
}

echo "== ITEM-026: durable bootstrap surfaces =="
assert_contains "bootstrap documents distinct primary and secondary identities" "$BOOTSTRAP" \
  'two distinct subscriptions and two'
assert_contains "bootstrap documents azure-e2e environment" "$BOOTSTRAP" \
  "^### \`azure-e2e\`$"
assert_contains "bootstrap documents public-release approvals" "$BOOTSTRAP" \
  "^### \`public-release\`$"
assert_contains "bootstrap checks both subscriptions" "$BOOTSTRAP" \
  'lib::assert_distinct_subscriptions'
assert_contains "bootstrap documents CNI pin update policy" "$BOOTSTRAP" \
  '^## 7\. CNI source pin and checksum update policy$'

echo "== ITEM-027: ShellCheck and public artifact operator contract =="
assert_job_contains "lint gate runs ShellCheck over shipped E2E scripts" naming-tests \
  'run: shellcheck scripts/e2e/\*\.sh'
assert_contains "Makefile exposes cni-artifact" "$MAKEFILE" \
  '^cni-artifact: cni-package'
assert_contains "README documents make cni-artifact" "$README" \
  '^make cni-artifact$'
assert_contains "README documents exact controller pull path" "$README" \
  'docker pull "\$\{PUBLIC_ACR\}/pod-nsg-controller:\$\{SEMVER\}"'
assert_contains "README documents exact CNI ORAS pull path" "$README" \
  'oras pull "\$\{PUBLIC_ACR\}/pod-nsg-cni-transparent-tunnel:\$\{SEMVER\}"'

echo
printf 'workflow_epic008_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
