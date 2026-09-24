#!/usr/bin/env bash
# Hermetic static workflow contract tests for EPIC-006.
# Traceability: ITEM-019..021, AC-010/011/013/023.
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW="${TEST_DIR}/../../../.github/workflows/e2e-validation-release.yml"
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

echo "== ITEM-019: release job consumes the validated set after the gate =="
assert_job_contains "release depends on release_gate" release 'needs: .*release_gate'
assert_job_contains "release prepares hidden transaction tags" release 'promote-release\.sh prepare'
assert_job_contains "release completes only after supply-chain attestations" release 'promote-release\.sh complete'

echo "== ITEM-020: both digests receive GitHub SLSA provenance =="
assert_job_contains "release installs cosign" release 'sigstore/cosign-installer@'
assert_job_contains "controller provenance uses the controller digest" release \
  'subject-digest: \$\{\{ needs\.build\.outputs\.image_digest \}\}'
assert_job_contains "CNI provenance uses the CNI digest" release \
  'subject-digest: \$\{\{ needs\.build_cni\.outputs\.cni_digest \}\}'
assert_job_contains "provenance is pushed to the registry" release 'push-to-registry: true'
assert_job_contains "release can mint OIDC signing identity" release 'id-token: write'
assert_job_contains "release can publish GitHub attestations" release 'attestations: write'

echo "== ITEM-021: both SBOMs and fail-safe transaction cleanup are wired =="
assert_job_contains "controller SBOM artifact is downloaded" release 'name: sbom-controller'
assert_job_contains "CNI SBOM artifact is downloaded" release 'name: sbom-cni'
assert_job_contains "transaction cleanup runs even after failure" release 'if: always\(\)'
assert_job_contains "transaction cleanup invokes abort" release 'promote-release\.sh abort'

echo
printf 'workflow_epic006_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
