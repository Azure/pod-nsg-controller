#!/usr/bin/env bash
# =============================================================================
# promote_release_test.sh - behavioural tests for the artifact-set atomicity +
# validate_tt release-gate SEAM in ../promote-release.sh (EPIC-009 / ITEM-035).
#
# Scope: ITEM-035 introduces the promotion/release INTEGRATION SEAM that (a)
# promotes the CNI digest ALONGSIDE the controller digest under ONE ${SEMVER},
# by digest, with no rebuild, ATOMICALLY (both or neither), and (b) requires
# validate_tt success. The full EPIC-006 promotion logic - cosign signing, SBOM
# attach, SLSA provenance (ITEM-020), anonymous-pull enable/verify (ITEM-021),
# immutability + moving tags (ITEM-019) - is intentionally NOT exercised here.
#
# Fully hermetic: az (`acr import`) and oras (`copy`) are mocks. The suite proves
# AC-023 (released set == validated set) and TEST-009 (a TTS failure blocks BOTH
# promotions).
#
# Traceability: ITEM-035, FR-024, NFR-011, AC-023, AC-009, TEST-009, RD-013/014,
# PRD Section 3.6.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROMOTE_SH="${TEST_DIR}/../promote-release.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.promote_release_test_work"
MOCKBIN="${WORK}/bin"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi; }
assert_match() { if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2; fi; }
assert_nonzero() { if (( $2 != 0 )); then pass "$1"; else fail "$1"; fi; }

if [[ ! -f "$PROMOTE_SH" ]]; then
  printf 'FATAL promote-release.sh not found at %s\n' "$PROMOTE_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

CTRL_SHA="sha256:$(printf ctrl | sha256sum | cut -c1-64)"
CNI_SHA="sha256:$(printf cni | sha256sum | cut -c1-64)"

cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
case "$1 $2" in
  "acr import") [ -n "${MOCK_IMPORT_FAIL:-}" ] && exit 1; exit 0 ;;
  "acr login")  exit 0 ;;
  *) exit 0 ;;
esac
AZ
cat > "${MOCKBIN}/oras" <<'ORAS'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_ORAS_LOG}"
case "${1:-}" in
  copy) [ -n "${MOCK_COPY_FAIL:-}" ] && exit 1; echo "Copied"; exit 0 ;;
  *) exit 0 ;;
esac
ORAS
chmod +x "${MOCKBIN}/az" "${MOCKBIN}/oras"

# seed_manifest <tt_status> [--no-cni] [--no-ctrl]
seed_manifest() {
  local tt="$1"; shift || true
  manifest::init "$MANIFEST"
  local no_cni=0 no_ctrl=0
  for a in "$@"; do case "$a" in --no-cni) no_cni=1;; --no-ctrl) no_ctrl=1;; esac; done
  (( no_ctrl == 0 )) && manifest::record_controller_artifact "$MANIFEST" pncstg.azurecr.io candidate/pod-nsg-controller run-x "$CTRL_SHA" true
  (( no_cni == 0 ))  && manifest::record_cni_artifact "$MANIFEST" pncstg.azurecr.io candidate/pod-nsg-cni-transparent-tunnel run-x "$CNI_SHA" true
  [[ -n "$tt" ]] && manifest::put "$MANIFEST" validate.tt.status "$tt"
}

run_promote() {
  local name="$1"; shift
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; ORASLOG="${casedir}/oras.log"; MANIFEST="${casedir}/run-manifest.json"; LOG="${casedir}/log"
  mkdir -p "$casedir"; : > "$AZLOG"; : > "$ORASLOG"
  SEED_TT="${SEED_TT-pass}"; SEED_ARGS="${SEED_ARGS:-}"
  # shellcheck disable=SC2086
  seed_manifest "$SEED_TT" $SEED_ARGS
  env \
    MOCK_AZ_LOG="$AZLOG" MOCK_ORAS_LOG="$ORASLOG" AZ_BIN="${MOCKBIN}/az" ORAS_BIN="${MOCKBIN}/oras" \
    PUBLIC_ACR="pncpub.azurecr.io" STAGING_ACR="pncstg.azurecr.io" \
    MANIFEST_PATH="$MANIFEST" \
    "$@" bash "$PROMOTE_SH" release >"$LOG" 2>"${LOG}.err"
  RC=$?
}

echo "== happy path: BOTH artifacts promoted by digest under ONE semver (AC-023) =="
SEED_TT=pass run_promote ok RELEASE_VERSION=v1.2.3
assert_eq "release seam succeeds when validated" "0" "$RC"
assert_match "controller promoted by digest via az acr import" \
  "acr import .*candidate/pod-nsg-controller@${CTRL_SHA}" "$(tr '\n' '|' < "$AZLOG")"
assert_match "controller lands under the semver tag" 'pod-nsg-controller:v1.2.3' "$(tr '\n' '|' < "$AZLOG")"
assert_match "CNI promoted by digest via oras copy" \
  "copy .*candidate/pod-nsg-cni-transparent-tunnel@${CNI_SHA}" "$(tr '\n' '|' < "$ORASLOG")"
assert_match "CNI lands under the SAME semver tag (set atomicity, NFR-011)" \
  'pod-nsg-cni-transparent-tunnel:v1.2.3' "$(tr '\n' '|' < "$ORASLOG")"
assert_eq "manifest records the released version" "v1.2.3" "$(manifest::get "$MANIFEST" '.release.version')"
assert_eq "released controller digest == validated candidate digest (AC-023)" \
  "$CTRL_SHA" "$(manifest::get "$MANIFEST" '.release.controller.digest')"
assert_eq "released CNI digest == validated candidate digest (AC-023)" \
  "$CNI_SHA" "$(manifest::get "$MANIFEST" '.release.cni.digest')"
assert_eq "manifest marks the set atomic" "true" "$(manifest::get "$MANIFEST" '.release.set_atomic')"

echo "== a TTS/validate_tt failure blocks BOTH promotions (TEST-009 / AC-009) =="
SEED_TT=fail run_promote ttsfail RELEASE_VERSION=v1.2.3
assert_nonzero "release is blocked when validate_tt failed" "$RC"
assert_eq "controller was NOT imported" "0" "$(grep -c 'acr import' "$AZLOG")"
assert_eq "CNI was NOT copied" "0" "$(grep -c 'copy' "$ORASLOG")"

echo "== validate_tt status absent fails closed =="
SEED_TT="" run_promote ttsabsent RELEASE_VERSION=v1.2.3
assert_nonzero "release is blocked when validate_tt result is missing" "$RC"
assert_eq "no promotion attempted (controller)" "0" "$(grep -c 'acr import' "$AZLOG")"

echo "== atomicity: a missing CNI digest blocks the controller promotion too =="
SEED_TT=pass SEED_ARGS="--no-cni" run_promote nocni RELEASE_VERSION=v1.2.3
assert_nonzero "release fails when the CNI digest is missing (NFR-011)" "$RC"
assert_eq "controller NOT imported when the set is incomplete" "0" "$(grep -c 'acr import' "$AZLOG")"

echo "== atomicity: a missing controller digest blocks release =="
SEED_TT=pass SEED_ARGS="--no-ctrl" run_promote noctrl RELEASE_VERSION=v1.2.3
assert_nonzero "release fails when the controller digest is missing" "$RC"

echo "== a missing semver fails fast =="
SEED_TT=pass run_promote nover RELEASE_VERSION=""
assert_nonzero "release requires a semantic version" "$RC"

echo "== VALIDATE_TT_STATUS env override gates the seam (job needs result) =="
SEED_TT=pass run_promote envfail RELEASE_VERSION=v1.2.3 VALIDATE_TT_STATUS=failure
assert_nonzero "explicit validate_tt=failure blocks release even if manifest says pass" "$RC"
assert_eq "no promotion under an env gate failure" "0" "$(grep -c 'acr import' "$AZLOG")"

# ---------------------------------------------------------------------------
# EPIC-010 / ITEM-040: the release gate ALSO requires validate_cross_subscription
# and cleanup_xs WHEN the xs topology is in scope (release forces ss,xs). The
# ss-only seam above is unchanged because those manifests never include xs.
# ---------------------------------------------------------------------------
# seed_xs_manifest <validate_xs> <cleanup_xs>  ('-' omits a key)
seed_xs_manifest() {
  local vx="$1" cx="$2"
  manifest::init "$MANIFEST"
  manifest::record_controller_artifact "$MANIFEST" pncstg.azurecr.io candidate/pod-nsg-controller run-x "$CTRL_SHA" true
  manifest::record_cni_artifact "$MANIFEST" pncstg.azurecr.io candidate/pod-nsg-cni-transparent-tunnel run-x "$CNI_SHA" true
  manifest::put "$MANIFEST" validate.tt.status pass
  manifest::put_json "$MANIFEST" run.validation_topologies '["ss","xs"]'
  [[ "$vx" != "-" ]] && manifest::put "$MANIFEST" validate.xs.cross_subscription "$vx"
  [[ "$cx" != "-" ]] && manifest::put "$MANIFEST" cleanup.xs.status "$cx"
  return 0
}
run_xs_promote() {
  local name="$1"; shift
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; ORASLOG="${casedir}/oras.log"; MANIFEST="${casedir}/run-manifest.json"; LOG="${casedir}/log"
  mkdir -p "$casedir"; : > "$AZLOG"; : > "$ORASLOG"
  seed_xs_manifest "$1" "$2"; shift 2
  env MOCK_AZ_LOG="$AZLOG" MOCK_ORAS_LOG="$ORASLOG" AZ_BIN="${MOCKBIN}/az" ORAS_BIN="${MOCKBIN}/oras" \
    PUBLIC_ACR="pncpub.azurecr.io" STAGING_ACR="pncstg.azurecr.io" MANIFEST_PATH="$MANIFEST" \
    "$@" bash "$PROMOTE_SH" release >"$LOG" 2>"${LOG}.err"
  RC=$?
}

echo "== xs gate: release requires validate_cross_subscription AND cleanup_xs (ITEM-040) =="
run_xs_promote xs_ok pass pass RELEASE_VERSION=v1.2.3
assert_eq "release succeeds when xs validation + cleanup both pass" "0" "$RC"
assert_match "controller still promoted by digest" \
  "acr import .*candidate/pod-nsg-controller@${CTRL_SHA}" "$(tr '\n' '|' < "$AZLOG")"

run_xs_promote xs_valfail fail pass RELEASE_VERSION=v1.2.3
assert_nonzero "a failed validate_cross_subscription blocks release (AC-009/FR-025)" "$RC"
assert_eq "no controller promotion when xs validation failed" "0" "$(grep -c 'acr import' "$AZLOG")"
assert_eq "no CNI promotion when xs validation failed" "0" "$(grep -c 'copy' "$ORASLOG")"

run_xs_promote xs_cleanfail pass fail RELEASE_VERSION=v1.2.3
assert_nonzero "a failed cleanup_xs blocks release (RD-007/AC-008)" "$RC"
assert_eq "no promotion when cleanup_xs failed" "0" "$(grep -c 'acr import' "$AZLOG")"

run_xs_promote xs_valabsent - pass RELEASE_VERSION=v1.2.3
assert_nonzero "an ABSENT validate_cross_subscription fails closed" "$RC"
run_xs_promote xs_cleanabsent pass - RELEASE_VERSION=v1.2.3
assert_nonzero "an ABSENT cleanup_xs fails closed" "$RC"

echo "== xs gate: env overrides gate even when the manifest says pass =="
run_xs_promote xs_env_val pass pass RELEASE_VERSION=v1.2.3 VALIDATE_XS_STATUS=failure
assert_nonzero "explicit validate_cross_subscription=failure blocks release" "$RC"
run_xs_promote xs_env_clean pass pass RELEASE_VERSION=v1.2.3 CLEANUP_XS_STATUS=failure
assert_nonzero "explicit cleanup_xs=failure blocks release" "$RC"

echo "== ITEM-018: complete release gate fails closed for every required result =="
COMPLETE_ENV=(
  REQUIRE_COMPLETE=1
  LINT_STATUS=success
  VALIDATE_SS_STATUS=success
  VALIDATE_TT_STATUS=success
  CLEANUP_SS_STATUS=success
  VALIDATE_XS_STATUS=success
  CLEANUP_XS_STATUS=success
)
run_xs_promote complete_ok pass pass RELEASE_VERSION=v1.2.3 "${COMPLETE_ENV[@]}"
assert_eq "complete gate opens only when every required result passes" "0" "$RC"
for failed_gate in LINT_STATUS VALIDATE_SS_STATUS VALIDATE_TT_STATUS CLEANUP_SS_STATUS VALIDATE_XS_STATUS CLEANUP_XS_STATUS; do
  failure_env=("${COMPLETE_ENV[@]}")
  for i in "${!failure_env[@]}"; do
    [[ "${failure_env[i]}" == "${failed_gate}="* ]] && failure_env[i]="${failed_gate}=failure"
  done
  run_xs_promote "complete_${failed_gate}" pass pass RELEASE_VERSION=v1.2.3 "${failure_env[@]}"
  assert_nonzero "forced ${failed_gate} failure blocks release" "$RC"
  assert_eq "forced ${failed_gate} failure promotes neither artifact" "0" "$(grep -c 'acr import' "$AZLOG")"
done

echo
printf 'promote_release_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
