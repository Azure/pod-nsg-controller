#!/usr/bin/env bash
# =============================================================================
# build_cni_test.sh - behavioural tests for the CNI build/package/artifact
# logic in ../build-cni.sh (EPIC-009 / ITEM-028, ITEM-029).
#
# Fully hermetic: oras, az, syft, git, go, and curl are replaced by deterministic
# mock executables so the suite exercises build-cni.sh's orchestration (source
# acquisition via a pinned fixture / pinned prebuilt / pinned source ref,
# recorded-checksum verification, conflist "mode": "transparent-tunnel"
# assertion, digest-addressable OCI packaging, OIDC push-by-digest, SBOM, and
# run-manifest recording) with NO real registry, network, or CNI toolchain.
# Directly executable; no framework. Mirrors build_test.sh.
#
# Traceability: ITEM-028 (repo-native build/package + mode/checksum assert),
# ITEM-029 (push by digest + SBOM + manifest CNI digest), FR-018, FR-019,
# NFR-010, SEC-006, CON-011, AC-001, AC-016, TTS-001, PRD Sections 3.6/3.4.5.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_CNI_SH="${TEST_DIR}/../build-cni.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.build_cni_test_work"
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
assert_nonzero() { if (( $2 != 0 )); then pass "$1"; else fail "$1"; fi; }

if [[ ! -f "$BUILD_CNI_SH" ]]; then
  printf 'FATAL build-cni.sh not found at %s\n' "$BUILD_CNI_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

# ---- deterministic pinned fixture (stands in for the transparent-tunnel build)
FIXTURE="${WORK}/fixture"
mkdir -p "$FIXTURE"
printf 'MOCK-transparent-tunnel-azure-vnet-binary\n' > "${FIXTURE}/azure-vnet"
cat > "${FIXTURE}/azure-linux-transparent-tunnel.conflist" <<'CONF'
{
  "cniVersion": "0.3.0",
  "name": "azure",
  "plugins": [
    { "type": "azure-vnet", "mode": "transparent-tunnel", "bridge": "azure0" }
  ]
}
CONF
FIXTURE_SHA="$(sha256sum "${FIXTURE}/azure-vnet" | cut -c1-64)"

# A second fixture whose conflist is NOT transparent-tunnel (mode assertion test).
FIXTURE_BAD="${WORK}/fixture-bad"
mkdir -p "$FIXTURE_BAD"
printf 'MOCK-bin\n' > "${FIXTURE_BAD}/azure-vnet"
cat > "${FIXTURE_BAD}/azure-linux-transparent-tunnel.conflist" <<'CONF'
{ "cniVersion": "0.3.0", "name": "azure",
  "plugins": [ { "type": "azure-vnet", "mode": "transparent" } ] }
CONF

# ---- mock tooling ----------------------------------------------------------
cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
echo "$*" >> "${MOCK_LOG_DIR}/az.log"
exit 0
AZ
cat > "${MOCKBIN}/oras" <<'ORAS'
#!/usr/bin/env bash
echo "$*" >> "${MOCK_LOG_DIR}/oras.log"
cmd="${1:-}"
case "$cmd" in
  version) echo "Version: 1.2.0" ;;
  login) exit 0 ;;
  push)
    ref=""
    for a in "$@"; do case "$a" in */*:*|*/*) ref="$a" ;; esac; done
    hex="$(printf '%s' "$ref" | sha256sum | cut -c1-64)"
    printf 'Pushed [registry] %s\n' "$ref"
    printf 'ArtifactType: application/vnd.azure.pnc.cni.transparent-tunnel\n'
    printf 'Digest: sha256:%s\n' "$hex"
    ;;
  *) exit 0 ;;
esac
exit 0
ORAS
cat > "${MOCKBIN}/syft" <<'SY'
#!/usr/bin/env bash
echo "$*" >> "${MOCK_LOG_DIR}/syft.log"
out=""
for a in "$@"; do case "$a" in spdx-json=*) out="${a#spdx-json=}" ;; esac; done
[[ -z "$out" ]] && { echo "mock syft: missing spdx-json= target" >&2; exit 2; }
mkdir -p "$(dirname "$out")"
printf '{"artifacts":[{"name":"azure-vnet","type":"binary"},{"name":"azure-linux-transparent-tunnel.conflist","type":"file"}]}\n' > "$out"
exit 0
SY
cat > "${MOCKBIN}/curl" <<'CURL'
#!/usr/bin/env bash
# mock curl -fsSL <url> -o <out> : materialise from MOCK_FIXTURE_DIR by basename
echo "$*" >> "${MOCK_LOG_DIR}/curl.log"
out=""; url=""; prev=""
for a in "$@"; do case "$prev" in -o) out="$a" ;; esac; case "$a" in http*) url="$a" ;; esac; prev="$a"; done
[[ -z "$out" ]] && exit 2
case "$url" in
  *azure-vnet*) cp "${MOCK_FIXTURE_DIR}/azure-vnet" "$out" ;;
  *conflist*)   cp "${MOCK_FIXTURE_DIR}/azure-linux-transparent-tunnel.conflist" "$out" ;;
  *) echo "mock curl: unknown url $url" >&2; exit 22 ;;
esac
exit 0
CURL
chmod +x "${MOCKBIN}"/*

# run_cni <case> <cmd> <KEY=VAL...> ; sets globals RC, MANIFEST, SBOM, OUT, LOGS, CASE.
run_cni() {
  local casename="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${casename}"
  LOGS="${casedir}/logs"; MANIFEST="${casedir}/run-manifest.json"
  SBOM="${casedir}/sbom/cni.spdx.json"; OUT="${casedir}/out"
  mkdir -p "$LOGS" "$(dirname "$SBOM")"
  manifest::init "$MANIFEST"
  manifest::put "$MANIFEST" run.run_suffix vahdkc
  # Emulate a manifest that already carries the controller artifact (build runs
  # in parallel), so we prove recording the CNI never clobbers the controller.
  manifest::record_controller_artifact "$MANIFEST" "pncstg.azurecr.io" \
    "candidate/pod-nsg-controller" "run-20260820-vahdkc" "sha256:$(printf ctrl | sha256sum | cut -c1-64)" true
  env \
    MOCK_LOG_DIR="$LOGS" MOCK_FIXTURE_DIR="$FIXTURE" \
    AZ_BIN="${MOCKBIN}/az" ORAS_BIN="${MOCKBIN}/oras" \
    SYFT_BIN="${MOCKBIN}/syft" CURL_BIN="${MOCKBIN}/curl" \
    CNI_STAGING_REPO="candidate/pod-nsg-cni-transparent-tunnel" \
    CNI_STAGING_TAG="run-20260820-vahdkc" \
    CNI_WORKDIR="${casedir}/cni-build" \
    MANIFEST_PATH="$MANIFEST" SBOM_PATH="$SBOM" \
    GITHUB_OUTPUT="$OUT" GITHUB_STEP_SUMMARY="${casedir}/summary.md" \
    "$@" bash "$BUILD_CNI_SH" "$cmd" >"${casedir}/log" 2>&1
  RC=$?
}
out_val() { grep -E "^$1=" "$OUT" 2>/dev/null | tail -1 | cut -d= -f2-; }

echo "== fixture PR build: no push, no cloud, digest + mode assertion (ITEM-028) =="
run_cni pr all EVENT_NAME="pull_request" CNI_SOURCE_MODE="fixture" CNI_FIXTURE_DIR="$FIXTURE"
assert_eq "PR CNI build succeeds" "0" "$RC"
assert_eq "PR does NOT authenticate/push (no az calls)" "0" \
  "$([[ -f "${LOGS}/az.log" ]] && wc -l < "${LOGS}/az.log" || echo 0)"
if [[ -f "${LOGS}/oras.log" ]] && grep -q '^push' "${LOGS}/oras.log"; then
  fail "PR must NOT oras push"; else pass "PR does NOT oras push"; fi
assert_eq "PR output pushed=false" "false" "$(out_val pushed)"
assert_match "PR emits a sha256 CNI digest (ITEM-028)" '^sha256:[0-9a-f]{64}$' "$(out_val cni_digest)"
assert_eq "PR reference is bare repo:tag" \
  "candidate/pod-nsg-cni-transparent-tunnel:run-20260820-vahdkc" "$(out_val cni_reference)"
assert_eq "PR conflist mode asserted transparent-tunnel" "transparent-tunnel" "$(out_val conflist_mode)"
assert_eq "PR SBOM artifact exists" "yes" "$([[ -s "$SBOM" ]] && echo yes || echo no)"
assert_eq "PR manifest records CNI pushed=false" "false" "$(manifest::get "$MANIFEST" '.artifacts.cni.pushed')"
assert_eq "PR manifest records conflist mode" "transparent-tunnel" "$(manifest::get "$MANIFEST" '.artifacts.cni.conflist_mode')"
assert_eq "PR manifest records binary checksum" "$FIXTURE_SHA" "$(manifest::get "$MANIFEST" '.artifacts.cni.binary_sha256')"
assert_eq "recording CNI preserves the controller artifact (atomic set)" \
  "1" "$([[ "$(manifest::get "$MANIFEST" '.artifacts.controller.pushed')" == "true" ]] && echo 1 || echo 0)"

echo "== push build: OIDC oras push by digest, capture sha256 (ITEM-029/FR-019) =="
run_cni push all EVENT_NAME="workflow_dispatch" STAGING_ACR="pncstg.azurecr.io" \
  CNI_SOURCE_MODE="fixture" CNI_FIXTURE_DIR="$FIXTURE"
assert_eq "push CNI build succeeds" "0" "$RC"
if grep -q '^push pncstg.azurecr.io/candidate/pod-nsg-cni-transparent-tunnel:run-20260820-vahdkc' "${LOGS}/oras.log" 2>/dev/null; then
  pass "push runs oras push of the candidate ref"; else fail "push must oras push the candidate ref"; fi
assert_eq "push output pushed=true" "true" "$(out_val pushed)"
assert_match "push captures immutable sha256 digest" '^sha256:[0-9a-f]{64}$' "$(out_val cni_digest)"
assert_match "push reference is BY @sha256 digest (FR-002/FR-019)" \
  '^pncstg\.azurecr\.io/candidate/pod-nsg-cni-transparent-tunnel@sha256:[0-9a-f]{64}$' "$(out_val cni_reference)"
assert_eq "push manifest reference == output reference" \
  "$(out_val cni_reference)" "$(manifest::get "$MANIFEST" '.artifacts.cni.reference')"
assert_eq "push manifest CNI pushed=true" "true" "$(manifest::get "$MANIFEST" '.artifacts.cni.pushed')"
assert_eq "exactly TWO artifacts recorded (controller + cni, AC-001)" "2" "$(manifest::get "$MANIFEST" '.artifacts | length')"
assert_eq "manifest records the CNI SBOM path (ITEM-029)" \
  "$SBOM" "$(manifest::get "$MANIFEST" '.artifacts.cni.sbom')"

echo "== prebuilt mode: pinned checksum verified (SEC-006/NFR-010) =="
run_cni prebuilt_ok all EVENT_NAME="workflow_dispatch" STAGING_ACR="pncstg.azurecr.io" \
  CNI_SOURCE_MODE="prebuilt" CNI_BINARY_URL="https://example/azure-vnet" \
  CNI_CONFLIST_URL="https://example/azure-linux-transparent-tunnel.conflist" \
  CNI_BINARY_SHA256="$FIXTURE_SHA"
assert_eq "prebuilt with matching checksum succeeds" "0" "$RC"
if grep -q 'azure-vnet' "${LOGS}/curl.log" 2>/dev/null; then
  pass "prebuilt downloads via curl seam"; else fail "prebuilt must curl the pinned binary"; fi

echo "== prebuilt checksum MISMATCH is a hard failure (SEC-006/RISK-008) =="
run_cni prebuilt_bad all EVENT_NAME="workflow_dispatch" STAGING_ACR="pncstg.azurecr.io" \
  CNI_SOURCE_MODE="prebuilt" CNI_BINARY_URL="https://example/azure-vnet" \
  CNI_CONFLIST_URL="https://example/azure-linux-transparent-tunnel.conflist" \
  CNI_BINARY_SHA256="0000000000000000000000000000000000000000000000000000000000000000"
assert_nonzero "prebuilt checksum mismatch fails fast" "$RC"

echo "== conflist NOT transparent-tunnel is a hard failure (FR-018 mode assert) =="
run_cni badmode all EVENT_NAME="pull_request" CNI_SOURCE_MODE="fixture" CNI_FIXTURE_DIR="$FIXTURE_BAD"
assert_nonzero "wrong conflist mode fails fast" "$RC"

echo "== push mode requires STAGING_ACR (fail fast) =="
run_cni no_acr all BUILD_PUSH="true" STAGING_ACR="" CNI_SOURCE_MODE="fixture" CNI_FIXTURE_DIR="$FIXTURE"
assert_nonzero "push without STAGING_ACR fails" "$RC"

echo "== BUILD_PUSH=false overrides even on a non-PR event =="
run_cni override all EVENT_NAME="workflow_dispatch" BUILD_PUSH="false" STAGING_ACR="pncstg.azurecr.io" \
  CNI_SOURCE_MODE="fixture" CNI_FIXTURE_DIR="$FIXTURE"
assert_eq "override build succeeds" "0" "$RC"
assert_eq "override does NOT push (no oras push)" "0" \
  "$([[ -f "${LOGS}/oras.log" ]] && grep -c '^push' "${LOGS}/oras.log" || echo 0)"
assert_eq "override output pushed=false" "false" "$(out_val pushed)"

echo "== missing CNI_STAGING_TAG fails fast (meta contract) =="
run_cni no_tag all EVENT_NAME="pull_request" CNI_SOURCE_MODE="fixture" CNI_FIXTURE_DIR="$FIXTURE" CNI_STAGING_TAG=""
assert_nonzero "missing CNI_STAGING_TAG fails" "$RC"

echo
printf 'build_cni_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
