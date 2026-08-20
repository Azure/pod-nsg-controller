#!/usr/bin/env bash
# =============================================================================
# lib_test.sh - smoke tests for the shared foundation helpers in ../lib.sh.
#
# Focused on the run-manifest emit foundation (used by the `meta` job) and the
# retry/backoff helper. Directly executable; no external framework required.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.lib_test_work"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() {
  if [[ "$2" == "$3" ]]; then pass "$1"; else
    fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2
  fi
}

if [[ ! -f "$LIB_SH" ]]; then
  printf 'FATAL lib.sh not found at %s\n' "$LIB_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$WORK"
MANIFEST="${WORK}/run-manifest.json"

echo "== manifest init/put/put_json/merge/get =="
manifest::init "$MANIFEST"
assert_eq "init creates empty object" "{}" "$(jq -c '.' "$MANIFEST")"

manifest::put "$MANIFEST" run.run_suffix vahdkc
assert_eq "put sets nested string" "vahdkc" "$(manifest::get "$MANIFEST" '.run.run_suffix')"

manifest::put_json "$MANIFEST" release.requested true
assert_eq "put_json sets nested boolean" "true" "$(manifest::get "$MANIFEST" '.release.requested')"

manifest::merge "$MANIFEST" '{"names":{"ss":{"base":"pnc-e2e-ss-20260820-vahdkc"}}}'
assert_eq "merge adds a subtree" "pnc-e2e-ss-20260820-vahdkc" "$(manifest::get "$MANIFEST" '.names.ss.base')"
assert_eq "merge preserves prior keys" "vahdkc" "$(manifest::get "$MANIFEST" '.run.run_suffix')"
assert_eq "manifest remains valid JSON" "0" "$(jq empty "$MANIFEST"; echo $?)"

echo "== retry/backoff =="
attempts_file="${WORK}/attempts"
echo 0 > "$attempts_file"
flaky() {
  local n; n=$(<"$attempts_file"); n=$((n + 1)); echo "$n" > "$attempts_file"
  (( n >= 3 ))
}
if lib::retry 5 0 -- flaky; then pass "retry succeeds once command recovers"; else fail "retry should have succeeded"; fi
assert_eq "retry stopped at first success" "3" "$(<"$attempts_file")"

if lib::retry 2 0 -- false >/dev/null 2>&1; then fail "retry should fail after exhausting attempts"; else pass "retry returns non-zero after exhaustion"; fi

echo "== gha helpers degrade to stdout when unset =="
assert_eq "gha::output prints key=value locally" "foo=bar" "$(GITHUB_OUTPUT='' gha::output foo bar)"

echo "== manifest::record_controller_artifact (EPIC-002 / ITEM-006) =="
# Push mode: the immutable digest reference (@sha256) is what downstream jobs consume.
ART="${WORK}/artifact.json"
manifest::init "$ART"
manifest::record_controller_artifact "$ART" "pncstg.azurecr.io" "candidate/pod-nsg-controller" \
  "run-20260820-vahdkc" "sha256:$(printf 'a%.0s' $(seq 1 64))" true
assert_eq "push: reference is by @sha256 digest" \
  "pncstg.azurecr.io/candidate/pod-nsg-controller@sha256:$(printf 'a%.0s' $(seq 1 64))" \
  "$(manifest::get "$ART" '.artifacts.controller.reference')"
assert_eq "push: pushed=true recorded" "true" "$(manifest::get "$ART" '.artifacts.controller.pushed')"
assert_eq "push: digest recorded" "sha256:$(printf 'a%.0s' $(seq 1 64))" \
  "$(manifest::get "$ART" '.artifacts.controller.digest')"
assert_eq "push: repo recorded" "candidate/pod-nsg-controller" \
  "$(manifest::get "$ART" '.artifacts.controller.repo')"
assert_eq "push: exactly one controller artifact recorded (AC-001)" "1" \
  "$(manifest::get "$ART" '.artifacts | length')"

# No-push mode with a registry present: reference falls back to registry/repo:tag.
manifest::init "$ART"
manifest::record_controller_artifact "$ART" "pncstg.azurecr.io" "candidate/pod-nsg-controller" \
  "run-20260820-vahdkc" "sha256:deadbeef" false
assert_eq "no-push(registry): reference is by tag" \
  "pncstg.azurecr.io/candidate/pod-nsg-controller:run-20260820-vahdkc" \
  "$(manifest::get "$ART" '.artifacts.controller.reference')"
assert_eq "no-push(registry): pushed=false recorded" "false" \
  "$(manifest::get "$ART" '.artifacts.controller.pushed')"

# PR local mode (no registry): reference is the bare repo:tag.
manifest::init "$ART"
manifest::record_controller_artifact "$ART" "" "candidate/pod-nsg-controller" \
  "run-20260820-vahdkc" "sha256:localid" false
assert_eq "pr-local: reference is bare repo:tag" \
  "candidate/pod-nsg-controller:run-20260820-vahdkc" \
  "$(manifest::get "$ART" '.artifacts.controller.reference')"
assert_eq "pr-local: local digest still recorded" "sha256:localid" \
  "$(manifest::get "$ART" '.artifacts.controller.digest')"
assert_eq "pr-local: manifest remains valid JSON" "0" "$(jq empty "$ART"; echo $?)"

# Recording preserves prior manifest keys (build augments meta's manifest in place).
manifest::init "$ART"
manifest::put "$ART" run.run_suffix vahdkc
manifest::record_controller_artifact "$ART" "pncstg.azurecr.io" "candidate/pod-nsg-controller" \
  "run-20260820-vahdkc" "sha256:abc" true
assert_eq "record preserves prior run.* keys" "vahdkc" "$(manifest::get "$ART" '.run.run_suffix')"

echo
printf 'lib_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
