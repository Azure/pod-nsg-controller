#!/usr/bin/env bash
# =============================================================================
# meta_test.sh - behavioural tests for the `meta` job logic in ../meta.sh.
#
# Verifies the release/topology validation and cross-subscription ID rules
# required by ITEM-002 (CON-009, FR-025, CON-001, semver), plus the happy-path
# run-manifest foundation. Directly executable; no external framework required.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
META_SH="${TEST_DIR}/../meta.sh"
WORK="${TEST_DIR}/.meta_test_work"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }

if [[ ! -f "$META_SH" ]]; then
  printf 'FATAL meta.sh not found at %s\n' "$META_SH" >&2
  exit 1
fi

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$WORK"

# run_meta <manifest-tag> <KEY=VALUE...> ; sets globals RC and MANIFEST.
run_meta() {
  local tag="$1"; shift
  MANIFEST="${WORK}/${tag}.json"
  env \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" \
    GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" DATE_UTC="20260820" \
    MANIFEST_PATH="$MANIFEST" \
    GITHUB_OUTPUT="${WORK}/${tag}.out" GITHUB_STEP_SUMMARY="${WORK}/${tag}.sum" \
    "$@" bash "$META_SH" >"${WORK}/${tag}.log" 2>&1
  RC=$?
}

echo "== happy path (ss,xs, no release) =="
run_meta happy INPUT_TOPOLOGIES="ss,xs" INPUT_RELEASE="false"
if (( RC == 0 )); then pass "meta succeeds on ss,xs"; else fail "meta should succeed (rc=$RC)"; fi
if [[ "$(jq -c '.run.validation_topologies' "$MANIFEST" 2>/dev/null)" == '["ss","xs"]' ]]; then
  pass "manifest records both topologies"; else fail "manifest topologies wrong"; fi
if [[ "$(jq -r '.release.requested' "$MANIFEST" 2>/dev/null)" == "false" ]]; then
  pass "manifest release.requested=false"; else fail "release.requested should be false"; fi
if [[ "$(jq -r '.names.xs.centraluseuap.subscription_role' "$MANIFEST" 2>/dev/null)" == "secondary" ]]; then
  pass "xs centraluseuap role=secondary"; else fail "xs role map wrong"; fi
if grep -q '^validation_topologies=ss,xs$' "${WORK}/happy.out"; then
  pass "step output validation_topologies set"; else fail "missing step output"; fi

echo "== release must include BOTH topologies (CON-009) =="
run_meta rel_ss_only INPUT_TOPOLOGIES="ss" INPUT_RELEASE="true" INPUT_RELEASE_VERSION="v1.2.3"
if (( RC != 0 )); then pass "release with only ss fails"; else fail "release+ss-only should fail"; fi

echo "== release requires a version (FR-008) =="
run_meta rel_no_ver INPUT_TOPOLOGIES="ss,xs" INPUT_RELEASE="true"
if (( RC != 0 )); then pass "release without version fails"; else fail "release w/o version should fail"; fi

echo "== release happy path (ss,xs + version + distinct IDs) =="
run_meta rel_ok INPUT_TOPOLOGIES="ss,xs" INPUT_RELEASE="true" INPUT_RELEASE_VERSION="v0.1.0" \
  PRIMARY_SUBSCRIPTION_ID="aaa" SECONDARY_SUBSCRIPTION_ID="bbb"
if (( RC == 0 )); then pass "release succeeds with full inputs"; else fail "release should succeed (rc=$RC)"; fi
if [[ "$(jq -r '.release.version' "$MANIFEST" 2>/dev/null)" == "v0.1.0" ]]; then
  pass "manifest records release version"; else fail "release version not recorded"; fi

echo "== xs requires DISTINCT subscription IDs (FR-025) =="
run_meta xs_equal INPUT_TOPOLOGIES="ss,xs" \
  PRIMARY_SUBSCRIPTION_ID="dup-id" SECONDARY_SUBSCRIPTION_ID="dup-id"
if (( RC != 0 )); then pass "xs with equal IDs fails"; else fail "xs equal IDs should fail"; fi

echo "== invalid inputs fail fast =="
run_meta bad_region INPUT_REGIONS="westus2"
if (( RC != 0 )); then pass "non-canary region rejected (CON-001)"; else fail "bad region should fail"; fi
run_meta bad_topo INPUT_TOPOLOGIES="ss,zz"
if (( RC != 0 )); then pass "invalid topology token rejected"; else fail "bad topology should fail"; fi
run_meta bad_ver INPUT_TOPOLOGIES="ss,xs" INPUT_RELEASE="true" INPUT_RELEASE_VERSION="1.2"
if (( RC != 0 )); then pass "malformed semver rejected"; else fail "bad semver should fail"; fi

echo "== tag push derives release version =="
run_meta tag_push INPUT_TOPOLOGIES="ss,xs" REF_TYPE="tag" REF_NAME="v1.4.0" \
  PRIMARY_SUBSCRIPTION_ID="aaa" SECONDARY_SUBSCRIPTION_ID="bbb"
if (( RC == 0 )) && [[ "$(jq -r '.release.version' "$MANIFEST" 2>/dev/null)" == "v1.4.0" ]]; then
  pass "tag event forces release and derives version"; else fail "tag-driven release wrong (rc=$RC)"; fi

echo
printf 'meta_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
