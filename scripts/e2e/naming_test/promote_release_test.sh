#!/usr/bin/env bash
# Hermetic behavioral coverage for EPIC-006 release promotion.
# Traceability: ITEM-019..021, AC-010/011/013/023, NFR-009/NFR-011.
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROMOTE_SH="${TEST_DIR}/../promote-release.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.promote_release_test_work"
MOCKBIN="${WORK}/bin"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() {
  if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi
}
assert_match() {
  if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2; fi
}
assert_nonzero() { if (( $2 != 0 )); then pass "$1"; else fail "$1"; fi; }

# shellcheck source=/dev/null
source "$LIB_SH"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

CTRL_SHA="sha256:$(printf ctrl | sha256sum | cut -c1-64)"
CNI_SHA="sha256:$(printf cni | sha256sum | cut -c1-64)"

cat >"${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${MOCK_LOG}"
case "$1 $2" in
  "acr repository")
    case "$3" in
      show)
        image=""; while (($#)); do [[ "$1" == "--image" ]] && image="$2"; shift || true; done
        if [[ ",${MOCK_EXISTING_TAGS:-}," == *",${image},"* ]]; then
          printf '%s\n' "${MOCK_EXISTING_DIGEST:-sha256:existing}"
          exit 0
        fi
        exit 1
        ;;
      delete) exit 0 ;;
    esac
    ;;
  "acr import")
    [[ "${MOCK_IMPORT_FAIL_IMAGE:-}" && "$*" == *"${MOCK_IMPORT_FAIL_IMAGE}"* ]] && exit 1
    exit 0
    ;;
  "acr update") exit 0 ;;
  "acr show") printf '%s\n' "${MOCK_ANONYMOUS_STATE:-true}"; exit 0 ;;
  "acr login") exit 0 ;;
esac
exit 0
AZ

cat >"${MOCKBIN}/oras" <<'ORAS'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${MOCK_LOG}"
case "${1:-}" in
  copy)
    [[ "${MOCK_COPY_FAIL_DEST:-}" && "$*" == *"${MOCK_COPY_FAIL_DEST}"* ]] && exit 1
    exit 0
    ;;
  manifest)
    ref="${*: -1}"
    if [[ "$ref" == *pod-nsg-controller* ]]; then printf '{"digest":"%s"}\n' "${MOCK_CTRL_REMOTE:-$CTRL_SHA}"
    else printf '{"digest":"%s"}\n' "${MOCK_CNI_REMOTE:-$CNI_SHA}"; fi
    ;;
  pull) [[ -n "${MOCK_ORAS_PULL_FAIL:-}" ]] && exit 1; exit 0 ;;
esac
ORAS

cat >"${MOCKBIN}/cosign" <<'COSIGN'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${MOCK_LOG}"
case "${1:-}" in
  sign|attach) exit 0 ;;
  verify)
    [[ -n "${MOCK_COSIGN_VERIFY_FAIL:-}" ]] && exit 1
    printf '{"critical":{"identity":{"docker-reference":"ok"}}}\n'
    ;;
  verify-attestation)
    [[ -n "${MOCK_ATTEST_VERIFY_FAIL:-}" ]] && exit 1
    printf '{"payload":"verified"}\n'
    ;;
  tree) printf 'SBOM: sha256:attached\n' ;;
esac
COSIGN

cat >"${MOCKBIN}/docker" <<'DOCKER'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${MOCK_LOG}"
case "${1:-}" in
  logout) exit 0 ;;
  pull) [[ -n "${MOCK_DOCKER_PULL_FAIL:-}" ]] && exit 1; exit 0 ;;
esac
DOCKER
chmod +x "${MOCKBIN}/az" "${MOCKBIN}/oras" "${MOCKBIN}/cosign" "${MOCKBIN}/docker"

seed_manifest() {
  manifest::init "$MANIFEST"
  manifest::record_controller_artifact "$MANIFEST" pncstg.azurecr.io candidate/pod-nsg-controller run-x "$CTRL_SHA" true
  manifest::record_cni_artifact "$MANIFEST" pncstg.azurecr.io candidate/pod-nsg-cni-transparent-tunnel run-x "$CNI_SHA" true
  manifest::put "$MANIFEST" validate.tt.status pass
}

new_case() {
  local name="$1"
  CASEDIR="${WORK}/${name}"
  MANIFEST="${CASEDIR}/run-manifest.json"
  LOG="${CASEDIR}/commands.log"
  mkdir -p "$CASEDIR"
  : >"$LOG"
  printf '{"name":"controller"}\n' >"${CASEDIR}/controller.spdx.json"
  printf '{"name":"cni"}\n' >"${CASEDIR}/cni.spdx.json"
  seed_manifest
}

run_cmd() {
  local cmd="$1"; shift
  env MOCK_LOG="$LOG" CTRL_SHA="$CTRL_SHA" CNI_SHA="$CNI_SHA" \
    AZ_BIN="${MOCKBIN}/az" ORAS_BIN="${MOCKBIN}/oras" COSIGN_BIN="${MOCKBIN}/cosign" DOCKER_BIN="${MOCKBIN}/docker" \
    PUBLIC_ACR=pncpub.azurecr.io STAGING_ACR=pncstg.azurecr.io RELEASE_VERSION=v1.2.3 \
    RELEASE_TRANSACTION_TAG=_release-test CONTROLLER_SBOM_PATH="${CASEDIR}/controller.spdx.json" \
    CNI_SBOM_PATH="${CASEDIR}/cni.spdx.json" MANIFEST_PATH="$MANIFEST" \
    COSIGN_CERTIFICATE_IDENTITY_REGEXP='^https://github.com/Azure/pod-nsg-controller/' \
    "$@" bash "$PROMOTE_SH" "$cmd" >"${CASEDIR}/${cmd}.out" 2>"${CASEDIR}/${cmd}.err"
  RC=$?
}

echo "== ITEM-019: immutable, digest-preserving two-phase promotion =="
new_case happy
run_cmd prepare
assert_eq "prepare succeeds" 0 "$RC"
assert_match "controller candidate imported by exact digest to transaction tag" \
  "acr import .*candidate/pod-nsg-controller@${CTRL_SHA}.*pod-nsg-controller:_release-test" "$(cat "$LOG")"
assert_match "CNI candidate copied by exact digest to transaction tag" \
  "copy .*candidate/pod-nsg-cni-transparent-tunnel@${CNI_SHA}.*pod-nsg-cni-transparent-tunnel:_release-test" "$(cat "$LOG")"
assert_eq "prepare does not publish semantic version" 0 \
  "$(grep -Ec '^(acr import|copy).*:v1\.2\.3' "$LOG")"

run_cmd secure
assert_eq "both released digests are keyless signed" 2 "$(grep -c '^sign --yes ' "$LOG")"
assert_eq "both SBOMs are attached" 2 "$(grep -c '^attach sbom ' "$LOG")"

run_cmd complete PROVENANCE_AVAILABLE=true MOVING_TAGS=latest,v1,v1.2
assert_eq "complete succeeds after verification" 0 "$RC"
assert_eq "both signatures are verified fail-closed" 2 "$(grep -c '^verify --certificate-' "$LOG")"
assert_eq "both SLSA attestations are verified fail-closed" 2 "$(grep -c '^verify-attestation --type slsaprovenance ' "$LOG")"
assert_match "controller semantic version published from verified digest" \
  "acr import .*pod-nsg-controller@${CTRL_SHA}.*pod-nsg-controller:v1.2.3" "$(cat "$LOG")"
assert_match "CNI semantic version published from verified digest" \
  "copy .*pod-nsg-cni-transparent-tunnel@${CNI_SHA}.*pod-nsg-cni-transparent-tunnel:v1.2.3" "$(cat "$LOG")"
assert_eq "released controller digest equals validated digest" "$CTRL_SHA" "$(manifest::get "$MANIFEST" '.release.controller.digest')"
assert_eq "released CNI digest equals validated digest" "$CNI_SHA" "$(manifest::get "$MANIFEST" '.release.cni.digest')"
assert_eq "release is marked complete only after all checks" true "$(manifest::get "$MANIFEST" '.release.complete')"

echo "== prior release gates remain mandatory =="
COMPLETE_GATES=(
  REQUIRE_COMPLETE=1 REQUIRE_XS=1
  LINT_STATUS=success VALIDATE_SS_STATUS=success VALIDATE_TT_STATUS=success
  CLEANUP_SS_STATUS=success VALIDATE_XS_STATUS=success CLEANUP_XS_STATUS=success
)
new_case complete_gate
run_cmd prepare "${COMPLETE_GATES[@]}"
assert_eq "all prior validation and cleanup gates permit prepare" 0 "$RC"
run_cmd abort
for failed_gate in LINT_STATUS VALIDATE_SS_STATUS VALIDATE_TT_STATUS CLEANUP_SS_STATUS VALIDATE_XS_STATUS CLEANUP_XS_STATUS; do
  new_case "gate_${failed_gate}"
  gate_env=("${COMPLETE_GATES[@]}")
  for i in "${!gate_env[@]}"; do
    [[ "${gate_env[i]}" == "${failed_gate}="* ]] && gate_env[i]="${failed_gate}=failure"
  done
  run_cmd prepare "${gate_env[@]}"
  assert_nonzero "forced ${failed_gate} failure blocks prepare" "$RC"
  assert_eq "forced ${failed_gate} failure writes no transaction artifact" 0 \
    "$(grep -Ec '^(acr import|copy)' "$LOG")"
done

echo "== AC-013: an existing immutable version blocks all writes =="
new_case immutable
run_cmd prepare MOCK_EXISTING_TAGS='pod-nsg-controller:v1.2.3'
assert_nonzero "existing controller version fails" "$RC"
assert_eq "immutable failure imports nothing" 0 "$(grep -c '^acr import' "$LOG")"
assert_eq "immutable failure copies nothing" 0 "$(grep -c '^copy' "$LOG")"

new_case immutable_cni
run_cmd prepare MOCK_EXISTING_TAGS='pod-nsg-cni-transparent-tunnel:v1.2.3'
assert_nonzero "existing CNI version fails" "$RC"
assert_eq "CNI immutability is checked before writes" 0 "$(grep -c '^acr import' "$LOG")"

echo "== NFR-011: partial prepare/finalize is rolled back before complete =="
new_case prepare_rollback
run_cmd prepare MOCK_COPY_FAIL_DEST='pod-nsg-cni-transparent-tunnel:_release-test'
assert_nonzero "CNI staging failure fails prepare" "$RC"
assert_match "controller transaction tag is rolled back" \
  'acr repository delete .*pod-nsg-controller:_release-test' "$(cat "$LOG")"
assert_eq "no semantic version is published after prepare failure" 0 \
  "$(grep -Ec '^(acr import|copy).*:v1\.2\.3' "$LOG")"

new_case digest_mismatch
run_cmd prepare MOCK_CTRL_REMOTE="sha256:$(printf wrong | sha256sum | cut -c1-64)"
assert_nonzero "public digest mismatch fails prepare" "$RC"
assert_match "digest mismatch removes controller transaction tag" \
  'acr repository delete .*pod-nsg-controller:_release-test' "$(cat "$LOG")"
assert_match "digest mismatch removes CNI transaction tag" \
  'acr repository delete .*pod-nsg-cni-transparent-tunnel:_release-test' "$(cat "$LOG")"

new_case final_rollback
run_cmd prepare
run_cmd secure
run_cmd complete PROVENANCE_AVAILABLE=true MOCK_COPY_FAIL_DEST='pod-nsg-cni-transparent-tunnel:v1.2.3'
assert_nonzero "second semantic publication failure fails release" "$RC"
assert_match "partial controller semantic tag is rolled back" \
  'acr repository delete .*pod-nsg-controller:v1.2.3' "$(cat "$LOG")"
assert_eq "failed release is never marked complete" "" "$(manifest::get "$MANIFEST" '.release.complete // empty')"

echo "== ITEM-020: supply-chain verification fails closed =="
new_case verify_fail
run_cmd prepare
run_cmd secure
run_cmd complete PROVENANCE_AVAILABLE=true MOCK_COSIGN_VERIFY_FAIL=1
assert_nonzero "signature verification failure blocks release" "$RC"
assert_eq "signature failure publishes no semantic tag" 0 \
  "$(grep -Ec '^(acr import|copy).*:v1\.2\.3' "$LOG")"

new_case attest_fail
run_cmd prepare
run_cmd secure
run_cmd complete PROVENANCE_AVAILABLE=true MOCK_ATTEST_VERIFY_FAIL=1
assert_nonzero "provenance verification failure blocks release" "$RC"
assert_eq "attestation failure publishes no semantic tag" 0 \
  "$(grep -Ec '^(acr import|copy).*:v1\.2\.3' "$LOG")"

new_case moving_rollback
run_cmd prepare
run_cmd secure
run_cmd complete PROVENANCE_AVAILABLE=true MOVING_TAGS=latest \
  MOCK_COPY_FAIL_DEST='pod-nsg-cni-transparent-tunnel:latest'
assert_nonzero "partial moving-tag advancement fails release" "$RC"
assert_match "controller moving tag is rolled back with its CNI peer" \
  'acr repository delete .*pod-nsg-controller:latest' "$(cat "$LOG")"
assert_match "moving-tag failure rolls back controller version" \
  'acr repository delete .*pod-nsg-controller:v1.2.3' "$(cat "$LOG")"
assert_match "moving-tag failure rolls back CNI version" \
  'acr repository delete .*pod-nsg-cni-transparent-tunnel:v1.2.3' "$(cat "$LOG")"

echo "== ITEM-021: anonymous configuration and unauthenticated dual pulls =="
assert_match "public ACR anonymous pull is enabled" 'acr update .*--anonymous-pull-enabled true' "$(cat "${WORK}/happy/commands.log")"
assert_match "anonymous setting is verified" 'acr show .*anonymousPullEnabled' "$(cat "${WORK}/happy/commands.log")"
assert_match "docker credentials are cleared before controller pull" \
  'logout pncpub.azurecr.io.*pull pncpub.azurecr.io/pod-nsg-controller:v1.2.3' "$(tr '\n' '|' <"${WORK}/happy/commands.log")"
assert_match "CNI semantic tag is pulled without registry credentials" \
  'pull --registry-config .*pncpub.azurecr.io/pod-nsg-cni-transparent-tunnel:v1.2.3' "$(cat "${WORK}/happy/commands.log")"

new_case pull_fail
run_cmd prepare
run_cmd secure
run_cmd complete PROVENANCE_AVAILABLE=true MOCK_ORAS_PULL_FAIL=1
assert_nonzero "anonymous CNI pull failure fails closed" "$RC"
assert_match "failed anonymous verification rolls back controller version" \
  'acr repository delete .*pod-nsg-controller:v1.2.3' "$(cat "$LOG")"
assert_match "failed anonymous verification rolls back CNI version" \
  'acr repository delete .*pod-nsg-cni-transparent-tunnel:v1.2.3' "$(cat "$LOG")"

echo
printf 'promote_release_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
