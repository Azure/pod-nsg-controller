#!/usr/bin/env bash
# Hermetic tests for final staging artifact/token cleanup.
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLEANUP_SH="${TEST_DIR}/../cleanup-artifacts.sh"
WORK="${TEST_DIR}/.artifact_cleanup_test_work"
MOCKBIN="${WORK}/bin"
PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi; }
assert_match() { if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2; fi; }

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

cat >"${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${MOCK_LOG}"
state="${MOCK_STATE}"; mkdir -p "$state"
case "$1 $2 $3" in
  "acr repository delete")
    image=""; while (($#)); do [[ "$1" == "--image" ]] && image="$2"; shift || true; done
    touch "${state}/tag-$(printf '%s' "$image" | tr '/' '_')"; exit 0 ;;
  "acr repository show")
    image=""; while (($#)); do [[ "$1" == "--image" ]] && image="$2"; shift || true; done
    if [[ -n "${MOCK_VERIFY_ERROR:-}" ]]; then
      printf '%s\n' '(AuthorizationFailed) denied' >&2
      exit 1
    fi
    if [[ -f "${state}/tag-$(printf '%s' "$image" | tr '/' '_')" ]]; then
      printf '%s\n' '(ManifestNotFound) manifest unknown' >&2
      exit 1
    fi
    [[ -n "${MOCK_TAG_LINGERS:-}" ]] && exit 0
    printf '%s\n' '(ManifestNotFound) manifest unknown' >&2
    exit 1 ;;
  "acr token delete")
    name=""; while (($#)); do [[ "$1" == "--name" ]] && name="$2"; shift || true; done
    touch "${state}/token-${name}"; exit 0 ;;
  "acr token show")
    name=""; while (($#)); do [[ "$1" == "--name" ]] && name="$2"; shift || true; done
    [[ -n "${MOCK_TOKEN_LINGERS:-}" ]] && exit 0
    if [[ -f "${state}/token-${name}" ]]; then
      printf '%s\n' '(ResourceNotFound) resource not found' >&2
      exit 1
    fi
    printf '%s\n' '(ResourceNotFound) resource not found' >&2
    exit 1 ;;
esac
exit 0
AZ
chmod +x "${MOCKBIN}/az"

run_cleanup() {
  local name="$1"; shift
  local dir="${WORK}/${name}"
  LOG="${dir}/az.log"; MANIFEST="${dir}/manifest.json"
  mkdir -p "$dir"; : >"$LOG"
  env AZ_BIN="${MOCKBIN}/az" MOCK_LOG="$LOG" MOCK_STATE="${dir}/state" \
    REPO=Azure/pod-nsg-controller RUN_ID=10293847561 RUN_ATTEMPT=1 DATE_UTC=20260820 \
    GIT_SHA=8504b2e1c3a9 GIT_REF=refs/heads/test \
    STAGING_ACR=pncstg.azurecr.io STAGING_ACR_SUBSCRIPTION_ID=sub-primary \
    CONTROLLER_STAGING_TAG=run-20260820-vahdkc CNI_STAGING_TAG=run-20260820-vahdkc \
    VALIDATION_TOPOLOGIES=ss,xs MANIFEST_PATH="$MANIFEST" VERIFY_ATTEMPTS=2 VERIFY_DELAY=0 \
    "$@" bash "$CLEANUP_SH" cleanup >"${dir}/out" 2>"${dir}/err"
  RC=$?
}

echo "== final cleanup deletes and verifies both candidates and all run tokens =="
run_cleanup happy
assert_eq "cleanup succeeds" 0 "$RC"
assert_eq "both per-run candidate tags are deleted" 2 "$(grep -c '^acr repository delete' "$LOG")"
assert_eq "all four ss/xs regional pull tokens are deleted" 4 "$(grep -c '^acr token delete' "$LOG")"
assert_eq "every registry call uses explicit staging subscription context" \
  "$(grep -c . "$LOG")" "$(grep -c -- '--subscription sub-primary' "$LOG")"
assert_eq "manifest records verified artifact cleanup" pass \
  "$(jq -r '.cleanup.artifacts.status' "$MANIFEST")"
assert_match "controller candidate is targeted exactly" \
  'candidate/pod-nsg-controller:run-20260820-vahdkc' "$(cat "$LOG")"
assert_match "CNI candidate is targeted exactly" \
  'candidate/pod-nsg-cni-transparent-tunnel:run-20260820-vahdkc' "$(cat "$LOG")"

echo "== cleanup fails closed when registry state remains =="
run_cleanup linger MOCK_TOKEN_LINGERS=1
assert_eq "lingering token fails cleanup" 1 "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
assert_eq "manifest exposes cleanup failure" fail \
  "$(jq -r '.cleanup.artifacts.status' "$MANIFEST")"

echo "== cleanup never accepts an ambiguous registry error as absence =="
run_cleanup verify_error MOCK_VERIFY_ERROR=1
assert_eq "authorization failure fails cleanup" 1 "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
assert_eq "ambiguous verification failure is recorded" fail \
  "$(jq -r '.cleanup.artifacts.status' "$MANIFEST")"

echo
printf 'artifact_cleanup_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
