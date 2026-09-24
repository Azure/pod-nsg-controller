#!/usr/bin/env bash
# =============================================================================
# build_test.sh - behavioural tests for the `build` job logic in ../build.sh
# (EPIC-002 / ITEM-004, ITEM-005, ITEM-006).
#
# Fully hermetic: docker, make, az, and syft are replaced by deterministic mock
# executables so the suite exercises build.sh's orchestration (build reuse,
# OIDC push-by-digest, sha256 capture, SBOM + Go-module assertion, run-manifest
# recording, PR-vs-push branching, and fail-fast paths) with NO real cloud,
# container runtime, or SBOM tooling. Directly executable; no framework.
#
# Traceability: ITEM-004 (build/digest, PR no-push), ITEM-005 (SBOM + module),
# ITEM-006 (manifest digest/reference), FR-001/FR-002/FR-016, AC-001, NFR-005.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_SH="${TEST_DIR}/../build.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.build_test_work"
MOCKBIN="${WORK}/bin"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() {
  if [[ "$2" == "$3" ]]; then pass "$1"; else
    fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2
  fi
}
assert_match() {
  if [[ "$3" =~ $2 ]]; then pass "$1"; else
    fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2
  fi
}

if [[ ! -f "$BUILD_SH" ]]; then
  printf 'FATAL build.sh not found at %s\n' "$BUILD_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

# ---- mock tooling ----------------------------------------------------------
cat > "${MOCKBIN}/make" <<'MK'
#!/usr/bin/env bash
echo "$*" >> "${MOCK_LOG_DIR}/make.log"
exit 0
MK
cat > "${MOCKBIN}/docker" <<'DK'
#!/usr/bin/env bash
echo "$*" >> "${MOCK_LOG_DIR}/docker.log"
cmd="${1:-}"; shift || true
case "$cmd" in
  push) exit 0 ;;
  inspect)
    fmt=""; ref=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --format) fmt="$2"; shift 2 ;;
        *) ref="$1"; shift ;;
      esac
    done
    hex="$(printf '%s' "$ref" | sha256sum | cut -c1-64)"
    case "$fmt" in
      *RepoDigests*) printf '%s@sha256:%s\n' "${ref%:*}" "$hex" ;;
      *) printf 'sha256:%s\n' "$hex" ;;
    esac
    ;;
  *) exit 0 ;;
esac
DK
cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
echo "$*" >> "${MOCK_LOG_DIR}/az.log"
exit 0
AZ
cat > "${MOCKBIN}/syft" <<'SY'
#!/usr/bin/env bash
echo "$*" >> "${MOCK_LOG_DIR}/syft.log"
out=""
for a in "$@"; do
  case "$a" in spdx-json=*) out="${a#spdx-json=}" ;; esac
done
[[ -z "$out" ]] && { echo "mock syft: missing spdx-json= target" >&2; exit 2; }
mkdir -p "$(dirname "$out")"
if [[ "${MOCK_SYFT_OMIT_MODULE:-0}" == "1" ]]; then
  printf '{"artifacts":[{"name":"golang.org/x/net","type":"go-module"}]}\n' > "$out"
else
  printf '{"artifacts":[{"name":"github.com/Azure/pod-nsg-controller","type":"go-module","version":"(devel)"}]}\n' > "$out"
fi
exit 0
SY
chmod +x "${MOCKBIN}"/*

# run_build <tag> <KEY=VAL...> ; sets globals RC, MANIFEST, SBOM, OUT, LOGS.
run_build() {
  local tag="$1"; shift
  local casedir="${WORK}/${tag}"
  LOGS="${casedir}/logs"; MANIFEST="${casedir}/run-manifest.json"
  SBOM="${casedir}/sbom/controller.spdx.json"; OUT="${casedir}/out"
  mkdir -p "$LOGS" "$(dirname "$SBOM")"
  # Emulate the manifest handed over by the meta job (must be preserved).
  manifest::init "$MANIFEST"
  manifest::put "$MANIFEST" run.run_suffix vahdkc
  env \
    MOCK_LOG_DIR="$LOGS" \
    MAKE_BIN="${MOCKBIN}/make" DOCKER_BIN="${MOCKBIN}/docker" \
    AZ_BIN="${MOCKBIN}/az" SYFT_BIN="${MOCKBIN}/syft" \
    CONTROLLER_STAGING_REPO="candidate/pod-nsg-controller" \
    CONTROLLER_STAGING_TAG="run-20260820-vahdkc" \
    MANIFEST_PATH="$MANIFEST" SBOM_PATH="$SBOM" \
    GITHUB_OUTPUT="$OUT" GITHUB_STEP_SUMMARY="${casedir}/summary.md" \
    "$@" bash "$BUILD_SH" >"${casedir}/log" 2>&1
  RC=$?
}
out_val() { grep -E "^$1=" "$OUT" 2>/dev/null | tail -1 | cut -d= -f2-; }

echo "== PR build: no push, no cloud, still emits a digest (ITEM-004) =="
run_build pr EVENT_NAME="pull_request"
assert_eq "PR build succeeds" "0" "$RC"
assert_eq "PR reuses 'make docker-build' with local IMG (no registry)" \
  "docker-build IMG=candidate/pod-nsg-controller:run-20260820-vahdkc" \
  "$(cat "${LOGS}/make.log" 2>/dev/null)"
assert_eq "PR does NOT authenticate to Azure (no az calls)" "0" \
  "$([[ -f "${LOGS}/az.log" ]] && wc -l < "${LOGS}/az.log" || echo 0)"
if [[ -f "${LOGS}/docker.log" ]] && grep -q '^push' "${LOGS}/docker.log"; then
  fail "PR must NOT docker push"; else pass "PR does NOT docker push"; fi
assert_eq "PR output pushed=false" "false" "$(out_val pushed)"
assert_match "PR emits a sha256 digest output (ITEM-004)" '^sha256:[0-9a-f]{64}$' "$(out_val image_digest)"
assert_eq "PR reference is the bare repo:tag" \
  "candidate/pod-nsg-controller:run-20260820-vahdkc" "$(out_val image_reference)"
assert_eq "PR SBOM artifact exists" "yes" "$([[ -s "$SBOM" ]] && echo yes || echo no)"
assert_eq "PR manifest records pushed=false" "false" "$(manifest::get "$MANIFEST" '.artifacts.controller.pushed')"
assert_eq "PR manifest preserves meta run.* keys" "vahdkc" "$(manifest::get "$MANIFEST" '.run.run_suffix')"

echo "== push build: OIDC login + push by digest, capture sha256 (ITEM-004/FR-002) =="
run_build push EVENT_NAME="workflow_dispatch" STAGING_ACR="pncstg.azurecr.io"
assert_eq "push build succeeds" "0" "$RC"
assert_eq "push reuses 'make docker-build' with registry-qualified IMG" \
  "docker-build IMG=pncstg.azurecr.io/candidate/pod-nsg-controller:run-20260820-vahdkc" \
  "$(cat "${LOGS}/make.log" 2>/dev/null)"
if grep -q 'acr login --name pncstg' "${LOGS}/az.log" 2>/dev/null; then
  pass "push authenticates to staging ACR via az acr login"; else fail "push must az acr login to registry name"; fi
if grep -q '^push pncstg.azurecr.io/candidate/pod-nsg-controller:run-20260820-vahdkc' "${LOGS}/docker.log" 2>/dev/null; then
  pass "push runs docker push of the candidate ref"; else fail "push must docker push the candidate ref"; fi
assert_eq "push output pushed=true" "true" "$(out_val pushed)"
assert_match "push captures immutable sha256 digest" '^sha256:[0-9a-f]{64}$' "$(out_val image_digest)"
assert_match "push reference is BY @sha256 digest (FR-002)" \
  '^pncstg\.azurecr\.io/candidate/pod-nsg-controller@sha256:[0-9a-f]{64}$' "$(out_val image_reference)"
assert_eq "push manifest reference == output reference" \
  "$(out_val image_reference)" "$(manifest::get "$MANIFEST" '.artifacts.controller.reference')"
assert_eq "push manifest pushed=true" "true" "$(manifest::get "$MANIFEST" '.artifacts.controller.pushed')"
assert_eq "exactly ONE controller artifact recorded (AC-001)" "1" "$(manifest::get "$MANIFEST" '.artifacts | length')"
assert_eq "manifest records the SBOM path (ITEM-005/006)" \
  "$SBOM" "$(manifest::get "$MANIFEST" '.artifacts.controller.sbom')"

echo "== SBOM lists the Go module github.com/Azure/pod-nsg-controller (ITEM-005) =="
if grep -q 'github.com/Azure/pod-nsg-controller' "$SBOM"; then
  pass "SBOM lists the controller Go module"; else fail "SBOM must list the Go module"; fi

echo "== SBOM missing the Go module is a hard failure (ITEM-005) =="
run_build nomodule EVENT_NAME="workflow_dispatch" STAGING_ACR="pncstg.azurecr.io" MOCK_SYFT_OMIT_MODULE="1"
assert_eq "build fails when SBOM omits the Go module" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== push mode requires a staging ACR (fail fast) =="
run_build no_acr BUILD_PUSH="true" STAGING_ACR=""
assert_eq "push without STAGING_ACR fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== BUILD_PUSH=false overrides even on a non-PR event =="
run_build override EVENT_NAME="workflow_dispatch" BUILD_PUSH="false" STAGING_ACR="pncstg.azurecr.io"
assert_eq "override build succeeds" "0" "$RC"
assert_eq "override does NOT push (no az login)" "0" \
  "$([[ -f "${LOGS}/az.log" ]] && wc -l < "${LOGS}/az.log" || echo 0)"
assert_eq "override output pushed=false" "false" "$(out_val pushed)"

echo
printf 'build_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
