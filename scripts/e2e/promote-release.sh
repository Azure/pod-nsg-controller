#!/usr/bin/env bash
# Promote, secure, and verify the controller+CNI release as one digest-pinned set.
# Traceability: ITEM-019..021, FR-008..010/016/024, NFR-009/NFR-011,
# AC-010/011/013/023, RD-013/RD-014.
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
source "${HERE}/lib.sh"

lib::require_cmds jq

RELEASE_VERSION="${RELEASE_VERSION:-}"
PUBLIC_ACR="${PUBLIC_ACR:-}"
STAGING_ACR="${STAGING_ACR:-}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
CONTROLLER_PUBLIC_REPO="${CONTROLLER_PUBLIC_REPO:-pod-nsg-controller}"
CNI_PUBLIC_REPO="${CNI_PUBLIC_REPO:-pod-nsg-cni-transparent-tunnel}"
RELEASE_INDEX_REPO="${RELEASE_INDEX_REPO:-pod-nsg-release-index}"
RELEASE_INDEX_ARTIFACT_TYPE="${RELEASE_INDEX_ARTIFACT_TYPE:-application/vnd.azure.pod-nsg.release-index.v1+json}"
RELEASE_INDEX_ATTESTATION_TYPE="${RELEASE_INDEX_ATTESTATION_TYPE:-https://networking.azure.com/attestations/pod-nsg-release-index/v1}"
RELEASE_INDEX_PATH="${RELEASE_INDEX_PATH:-$(dirname "$MANIFEST_PATH")/release-index.json}"
CONTROLLER_SBOM_PATH="${CONTROLLER_SBOM_PATH:-sbom/controller.spdx.json}"
CNI_SBOM_PATH="${CNI_SBOM_PATH:-sbom/cni.spdx.json}"
MOVING_TAGS="${MOVING_TAGS:-}"
RELEASE_TRANSACTION_TAG="${RELEASE_TRANSACTION_TAG:-_release-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}}"
PROVENANCE_AVAILABLE="${PROVENANCE_AVAILABLE:-false}"
COSIGN_CERTIFICATE_IDENTITY_REGEXP="${COSIGN_CERTIFICATE_IDENTITY_REGEXP:-^https://github.com/${GITHUB_REPOSITORY:-Azure/pod-nsg-controller}/}"
COSIGN_OIDC_ISSUER="${COSIGN_OIDC_ISSUER:-https://token.actions.githubusercontent.com}"
VALIDATE_TT_STATUS="${VALIDATE_TT_STATUS:-}"
VALIDATE_XS_STATUS="${VALIDATE_XS_STATUS:-}"
CLEANUP_XS_STATUS="${CLEANUP_XS_STATUS:-}"
LINT_STATUS="${LINT_STATUS:-}"
VALIDATE_SS_STATUS="${VALIDATE_SS_STATUS:-}"
CLEANUP_SS_STATUS="${CLEANUP_SS_STATUS:-}"
REQUIRE_XS="${REQUIRE_XS:-0}"
REQUIRE_COMPLETE="${REQUIRE_COMPLETE:-0}"
AZ_BIN="${AZ_BIN:-az}"
ORAS_BIN="${ORAS_BIN:-oras}"
COSIGN_BIN="${COSIGN_BIN:-cosign}"
DOCKER_BIN="${DOCKER_BIN:-docker}"

promote::_derive() {
  [[ "${PROMOTE_DERIVED:-0}" == "1" ]] && return 0
  [[ -f "$MANIFEST_PATH" ]] || log::die "manifest not found at ${MANIFEST_PATH}"
  CTRL_DIGEST="$(manifest::get "$MANIFEST_PATH" '.artifacts.controller.digest // empty')"
  CTRL_REFERENCE="$(manifest::get "$MANIFEST_PATH" '.artifacts.controller.reference // empty')"
  CNI_DIGEST="$(manifest::get "$MANIFEST_PATH" '.artifacts.cni.digest // empty')"
  CNI_REFERENCE="$(manifest::get "$MANIFEST_PATH" '.artifacts.cni.reference // empty')"
  TT_STATUS_MANIFEST="$(manifest::get "$MANIFEST_PATH" '.validate.tt.status // empty')"
  XS_STATUS_MANIFEST="$(manifest::get "$MANIFEST_PATH" '.validate.xs.cross_subscription // empty')"
  CLEANUP_XS_STATUS_MANIFEST="$(manifest::get "$MANIFEST_PATH" '.cleanup.xs.status // empty')"
  XS_IN_SCOPE_MANIFEST="$(manifest::get "$MANIFEST_PATH" '[.run.validation_topologies[]? | select(. == "xs")] | length')"
  XS_IN_SCOPE_MANIFEST="${XS_IN_SCOPE_MANIFEST//[^0-9]/}"
  XS_IN_SCOPE_MANIFEST="${XS_IN_SCOPE_MANIFEST:-0}"
  PROMOTE_DERIVED=1
}

promote::_require_release_inputs() {
  promote::_derive
  [[ "$RELEASE_VERSION" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$ ]] \
    || log::die "RELEASE_VERSION '${RELEASE_VERSION}' is not a valid semantic version"
  [[ -n "$PUBLIC_ACR" ]] || log::die "PUBLIC_ACR is required"
  [[ "$RELEASE_TRANSACTION_TAG" =~ ^_[A-Za-z0-9_.-]+$ ]] \
    || log::die "invalid RELEASE_TRANSACTION_TAG '${RELEASE_TRANSACTION_TAG}'"
}

promote::gate() {
  promote::_derive
  local rc=0 label status
  if [[ "$REQUIRE_COMPLETE" == "1" ]]; then
    for label in lint validate_ss cleanup_ss; do
      case "$label" in
        lint) status="$LINT_STATUS" ;;
        validate_ss) status="$VALIDATE_SS_STATUS" ;;
        cleanup_ss) status="$CLEANUP_SS_STATUS" ;;
      esac
      case "$status" in
        pass|success|Success) ;;
        "") log::error "release gate: ${label} result is absent"; rc=1 ;;
        *) log::error "release gate: ${label} did not pass (${status})"; rc=1 ;;
      esac
    done
  fi

  local tt="${VALIDATE_TT_STATUS:-$TT_STATUS_MANIFEST}"
  case "$tt" in
    pass|success|Success) ;;
    "") log::error "release gate: validate_tt result is absent"; rc=1 ;;
    *) log::error "release gate: validate_tt did not pass (${tt})"; rc=1 ;;
  esac
  [[ "$CTRL_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || { log::error "release gate: controller digest missing/invalid"; rc=1; }
  [[ "$CNI_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || { log::error "release gate: CNI digest missing/invalid"; rc=1; }

  local xs_required=0 xs cx
  [[ -n "$VALIDATE_XS_STATUS" || -n "$CLEANUP_XS_STATUS" ]] && xs_required=1
  (( XS_IN_SCOPE_MANIFEST > 0 )) && xs_required=1
  [[ "$REQUIRE_XS" == "1" ]] && xs_required=1
  if (( xs_required == 1 )); then
    xs="${VALIDATE_XS_STATUS:-$XS_STATUS_MANIFEST}"
    cx="${CLEANUP_XS_STATUS:-$CLEANUP_XS_STATUS_MANIFEST}"
    case "$xs" in
      pass|success|Success) ;;
      "") log::error "release gate: cross-subscription validation result is absent"; rc=1 ;;
      *) log::error "release gate: cross-subscription validation did not pass (${xs})"; rc=1 ;;
    esac
    case "$cx" in
      pass|success|Success) ;;
      "") log::error "release gate: cross-subscription cleanup result is absent"; rc=1 ;;
      *) log::error "release gate: cross-subscription cleanup did not pass (${cx})"; rc=1 ;;
    esac
  fi
  return "$rc"
}

promote::_tag_digest() {
  local repo="$1" tag="$2" out rc
  if out="$("$AZ_BIN" acr repository show --name "${PUBLIC_ACR%%.*}" \
    --image "${repo}:${tag}" --query digest -o tsv 2>&1)"; then
    printf '%s\n' "$out"
    return 0
  fi
  rc=$?
  if grep -Eqi 'manifest.?not.?found|manifest unknown|not found|does not exist' <<<"$out"; then
    return 1
  fi
  log::error "could not verify tag state for ${repo}:${tag}: ${out:-az exited ${rc}}"
  return 2
}

promote::_delete_tag() {
  local repo="$1" tag="$2" rc
  "$AZ_BIN" acr repository delete --name "${PUBLIC_ACR%%.*}" \
    --image "${repo}:${tag}" --yes >/dev/null 2>&1 \
    || log::warn "tag delete returned non-zero for ${repo}:${tag}; verifying absence"
  if promote::_tag_digest "$repo" "$tag" >/dev/null; then
    log::error "tag still exists after delete: ${repo}:${tag}"
    return 1
  else
    rc=$?
  fi
  (( rc == 1 )) && return 0
  return "$rc"
}

promote::_assert_version_absent() {
  local repo="$1" rc
  if promote::_tag_digest "$repo" "$RELEASE_VERSION" >/dev/null; then
    log::die "immutable version already exists: ${PUBLIC_ACR}/${repo}:${RELEASE_VERSION}"
  else
    rc=$?
  fi
  (( rc == 1 )) \
    || log::die "could not prove immutable version absence: ${PUBLIC_ACR}/${repo}:${RELEASE_VERSION}"
}

promote::_delete_prepared_tags() {
  local rc=0
  promote::_delete_tag "$CONTROLLER_PUBLIC_REPO" "$RELEASE_TRANSACTION_TAG" || rc=1
  promote::_delete_tag "$CNI_PUBLIC_REPO" "$RELEASE_TRANSACTION_TAG" || rc=1
  promote::_delete_tag "$RELEASE_INDEX_REPO" "$RELEASE_TRANSACTION_TAG" || rc=1
  return "$rc"
}

promote::_remote_digest() {
  "$ORAS_BIN" manifest fetch --descriptor "$1" | jq -r '.digest // empty'
}

promote::_assert_digest() {
  local reference="$1" expected="$2" actual
  actual="$(promote::_remote_digest "$reference")"
  [[ "$actual" == "$expected" ]] \
    || log::die "digest mismatch for ${reference}: expected ${expected}, got ${actual:-<empty>}"
}

promote::_validate_moving_tags() {
  local version="${RELEASE_VERSION#v}" major minor allowed tag
  IFS=. read -r major minor _ <<<"$version"
  allowed="latest,v${major},v${major}.${minor}"
  [[ -z "$MOVING_TAGS" ]] && return 0
  IFS=, read -ra tags <<<"$MOVING_TAGS"
  for tag in "${tags[@]}"; do
    [[ -n "$tag" && ",$allowed," == *",$tag,"* ]] \
      || log::die "moving tag '${tag}' is not allowed; choose from ${allowed}"
  done
}

promote::_login() {
  lib::require_cmds "$AZ_BIN" "$ORAS_BIN"
  lib::retry 3 5 -- "$AZ_BIN" acr login --name "${PUBLIC_ACR%%.*}"
  if [[ -n "$STAGING_ACR" ]]; then
    lib::retry 3 5 -- "$AZ_BIN" acr login --name "${STAGING_ACR%%.*}"
  fi
}

promote::_prepare_exit() {
  local rc="$?"
  if (( rc != 0 )); then
    log::warn "release preparation failed; removing all transaction tags"
    promote::_delete_prepared_tags
  fi
}

promote::prepare() {
  promote::_require_release_inputs
  promote::gate || log::die "release blocked by validation/cleanup gate"
  promote::_validate_moving_tags
  trap promote::_prepare_exit EXIT

  # Check BOTH immutable tags before the first registry write.
  promote::_assert_version_absent "$CONTROLLER_PUBLIC_REPO"
  promote::_assert_version_absent "$CNI_PUBLIC_REPO"
  promote::_assert_version_absent "$RELEASE_INDEX_REPO"
  promote::_login

  local ctrl_src="${CTRL_REFERENCE:-${STAGING_ACR}/candidate/pod-nsg-controller@${CTRL_DIGEST}}"
  local cni_src="${CNI_REFERENCE:-${STAGING_ACR}/candidate/pod-nsg-cni-transparent-tunnel@${CNI_DIGEST}}"
  local ctrl_tx="${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}:${RELEASE_TRANSACTION_TAG}"
  local cni_tx="${PUBLIC_ACR}/${CNI_PUBLIC_REPO}:${RELEASE_TRANSACTION_TAG}"

  promote::_delete_tag "$CONTROLLER_PUBLIC_REPO" "$RELEASE_TRANSACTION_TAG"
  promote::_delete_tag "$CNI_PUBLIC_REPO" "$RELEASE_TRANSACTION_TAG"
  promote::_delete_tag "$RELEASE_INDEX_REPO" "$RELEASE_TRANSACTION_TAG"
  lib::retry 3 5 -- "$AZ_BIN" acr import --name "${PUBLIC_ACR%%.*}" \
    --source "$ctrl_src" --image "${CONTROLLER_PUBLIC_REPO}:${RELEASE_TRANSACTION_TAG}" ||
    log::die "controller transaction promotion failed"
  if ! lib::retry 3 5 -- "$ORAS_BIN" copy "$cni_src" "$cni_tx"; then
    promote::_delete_tag "$CONTROLLER_PUBLIC_REPO" "$RELEASE_TRANSACTION_TAG"
    log::die "CNI transaction promotion failed; controller transaction tag rolled back"
  fi
  if ! promote::_assert_digest "$ctrl_tx" "$CTRL_DIGEST" ||
    ! promote::_assert_digest "$cni_tx" "$CNI_DIGEST"; then
    promote::_delete_tag "$CONTROLLER_PUBLIC_REPO" "$RELEASE_TRANSACTION_TAG"
    promote::_delete_tag "$CNI_PUBLIC_REPO" "$RELEASE_TRANSACTION_TAG"
    log::die "transaction digest verification failed; both transaction tags rolled back"
  fi

  manifest::put "$MANIFEST_PATH" release.version "$RELEASE_VERSION"
  manifest::put "$MANIFEST_PATH" release.transaction_tag "$RELEASE_TRANSACTION_TAG"
  manifest::put "$MANIFEST_PATH" release.prepared true
  manifest::put_json "$MANIFEST_PATH" release.controller "$(jq -n \
    --arg d "$CTRL_DIGEST" --arg r "${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}" --arg s "$ctrl_src" \
    '{digest:$d, repository:$r, source:$s}')"
  manifest::put_json "$MANIFEST_PATH" release.cni "$(jq -n \
    --arg d "$CNI_DIGEST" --arg r "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}" --arg s "$cni_src" \
    '{digest:$d, repository:$r, source:$s}')"
  gha::output controller_subject "${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}"
  gha::output cni_subject "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}"
  gha::output controller_digest "$CTRL_DIGEST"
  gha::output cni_digest "$CNI_DIGEST"
}

promote::secure() {
  promote::_require_release_inputs
  [[ "$(manifest::get "$MANIFEST_PATH" '.release.prepared // false')" == "true" ]] \
    || log::die "release transaction is not prepared"
  [[ -s "$CONTROLLER_SBOM_PATH" ]] || log::die "controller SBOM missing at ${CONTROLLER_SBOM_PATH}"
  [[ -s "$CNI_SBOM_PATH" ]] || log::die "CNI SBOM missing at ${CNI_SBOM_PATH}"
  lib::require_cmds "$COSIGN_BIN"

  local ctrl="${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}@${CTRL_DIGEST}"
  local cni="${PUBLIC_ACR}/${CNI_PUBLIC_REPO}@${CNI_DIGEST}"
  "$COSIGN_BIN" sign --yes "$ctrl"
  "$COSIGN_BIN" sign --yes "$cni"
  "$COSIGN_BIN" attach sbom --sbom "$CONTROLLER_SBOM_PATH" "$ctrl"
  "$COSIGN_BIN" attach sbom --sbom "$CNI_SBOM_PATH" "$cni"
  manifest::put "$MANIFEST_PATH" release.supply_chain.signed true
  manifest::put "$MANIFEST_PATH" release.supply_chain.sboms_attached true
}

promote::_verify_supply_chain() {
  [[ "$PROVENANCE_AVAILABLE" == "true" ]] \
    || log::die "SLSA provenance has not been emitted for both subjects"
  local ref
  for ref in \
    "${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}@${CTRL_DIGEST}" \
    "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}@${CNI_DIGEST}"; do
    "$COSIGN_BIN" verify \
      --certificate-identity-regexp "$COSIGN_CERTIFICATE_IDENTITY_REGEXP" \
      --certificate-oidc-issuer "$COSIGN_OIDC_ISSUER" "$ref" >/dev/null
    "$COSIGN_BIN" tree "$ref" | grep -qi 'sbom' \
      || log::die "SBOM attachment missing for ${ref}"
    "$COSIGN_BIN" verify-attestation --type slsaprovenance \
      --certificate-identity-regexp "$COSIGN_CERTIFICATE_IDENTITY_REGEXP" \
      --certificate-oidc-issuer "$COSIGN_OIDC_ISSUER" "$ref" >/dev/null
  done
}

promote::_publish_version() {
  local ctrl_digest_ref="${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}@${CTRL_DIGEST}"
  local cni_digest_ref="${PUBLIC_ACR}/${CNI_PUBLIC_REPO}@${CNI_DIGEST}"
  "$AZ_BIN" acr import --name "${PUBLIC_ACR%%.*}" --source "$ctrl_digest_ref" \
    --image "${CONTROLLER_PUBLIC_REPO}:${RELEASE_VERSION}"
  if ! "$ORAS_BIN" copy "$cni_digest_ref" \
    "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}:${RELEASE_VERSION}"; then
    promote::_delete_tag "$CONTROLLER_PUBLIC_REPO" "$RELEASE_VERSION"
    log::die "CNI version publication failed; partial controller version rolled back"
  fi
}

promote::_rollback_version() {
  local rc=0
  promote::_delete_tag "$CONTROLLER_PUBLIC_REPO" "$RELEASE_VERSION" || rc=1
  promote::_delete_tag "$CNI_PUBLIC_REPO" "$RELEASE_VERSION" || rc=1
  promote::_delete_tag "$RELEASE_INDEX_REPO" "$RELEASE_VERSION" || rc=1
  return "$rc"
}

promote::_verify_anonymous_pulls() {
  local verify_dir="${RELEASE_VERIFY_DIR:-$(dirname "$MANIFEST_PATH")/.release-verify-${RELEASE_TRANSACTION_TAG#_}}"
  mkdir -p "${verify_dir}/docker"
  printf '{}\n' >"${verify_dir}/oras-config.json"
  DOCKER_CONFIG="${verify_dir}/docker" "$DOCKER_BIN" logout "$PUBLIC_ACR"
  DOCKER_CONFIG="${verify_dir}/docker" "$DOCKER_BIN" pull \
    "${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}:${RELEASE_VERSION}"
  "$ORAS_BIN" pull --registry-config "${verify_dir}/oras-config.json" \
    "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}:${RELEASE_VERSION}" -o "${verify_dir}/cni"
}

promote::_post_publish_checks() {
  promote::_assert_digest "${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}:${RELEASE_VERSION}" "$CTRL_DIGEST"
  promote::_assert_digest "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}:${RELEASE_VERSION}" "$CNI_DIGEST"
  "$AZ_BIN" acr update --name "${PUBLIC_ACR%%.*}" --anonymous-pull-enabled true >/dev/null
  [[ "$("$AZ_BIN" acr show --name "${PUBLIC_ACR%%.*}" --query anonymousPullEnabled -o tsv)" == "true" ]] \
    || log::die "public ACR anonymous pull is not enabled"
  promote::_verify_anonymous_pulls
}

promote::_publish_moving_tags() {
  [[ -z "$MOVING_TAGS" ]] && return 0
  local tag ctrl_old cni_old rc
  PROMOTE_MOVING_TOUCHED=()
  PROMOTE_CTRL_PREVIOUS=()
  PROMOTE_CNI_PREVIOUS=()
  IFS=, read -ra tags <<<"$MOVING_TAGS"
  for tag in "${tags[@]}"; do
    if ctrl_old="$(promote::_tag_digest "$CONTROLLER_PUBLIC_REPO" "$tag")"; then
      :
    else
      rc=$?; (( rc == 1 )) || return "$rc"; ctrl_old=""
    fi
    if cni_old="$(promote::_tag_digest "$CNI_PUBLIC_REPO" "$tag")"; then
      :
    else
      rc=$?; (( rc == 1 )) || return "$rc"; cni_old=""
    fi
    PROMOTE_MOVING_TOUCHED+=("$tag")
    PROMOTE_CTRL_PREVIOUS+=("$ctrl_old")
    PROMOTE_CNI_PREVIOUS+=("$cni_old")
    if ! "$AZ_BIN" acr import --name "${PUBLIC_ACR%%.*}" \
      --source "${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}@${CTRL_DIGEST}" \
      --image "${CONTROLLER_PUBLIC_REPO}:${tag}" ||
      ! "$ORAS_BIN" copy "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}@${CNI_DIGEST}" \
        "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}:${tag}" ||
      ! promote::_assert_digest "${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}:${tag}" "$CTRL_DIGEST" ||
      ! promote::_assert_digest "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}:${tag}" "$CNI_DIGEST"; then
      promote::_rollback_moving_tags
      return 1
    fi
  done
}

promote::_rollback_moving_tags() {
  local i tag rollback_rc=0
  for ((i=${#PROMOTE_MOVING_TOUCHED[@]} - 1; i >= 0; i--)); do
    tag="${PROMOTE_MOVING_TOUCHED[i]}"
    if [[ -n "${PROMOTE_CTRL_PREVIOUS[i]}" ]]; then
      "$AZ_BIN" acr import --name "${PUBLIC_ACR%%.*}" \
        --source "${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}@${PROMOTE_CTRL_PREVIOUS[i]}" \
        --image "${CONTROLLER_PUBLIC_REPO}:${tag}" || rollback_rc=1
    else
      promote::_delete_tag "$CONTROLLER_PUBLIC_REPO" "$tag" || rollback_rc=1
    fi
    if [[ -n "${PROMOTE_CNI_PREVIOUS[i]}" ]]; then
      "$ORAS_BIN" copy "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}@${PROMOTE_CNI_PREVIOUS[i]}" \
        "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}:${tag}" || rollback_rc=1
    else
      promote::_delete_tag "$CNI_PUBLIC_REPO" "$tag" || rollback_rc=1
    fi
  done
  PROMOTE_MOVING_TOUCHED=()
  (( rollback_rc == 0 )) || log::error "one or more moving tags could not be restored"
  return "$rollback_rc"
}

promote::_write_release_index() {
  jq -n \
    --arg version "$RELEASE_VERSION" \
    --arg ctrl_repo "${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}" \
    --arg ctrl_digest "$CTRL_DIGEST" \
    --arg cni_repo "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}" \
    --arg cni_digest "$CNI_DIGEST" \
    '{
      schemaVersion: 1,
      version: $version,
      artifacts: {
        controller: {repository: $ctrl_repo, digest: $ctrl_digest},
        cni: {repository: $cni_repo, digest: $cni_digest}
      }
    }' >"$RELEASE_INDEX_PATH"
}

promote::_publish_release_index() {
  promote::_write_release_index
  local tx="${PUBLIC_ACR}/${RELEASE_INDEX_REPO}:${RELEASE_TRANSACTION_TAG}"
  local push_json expected config_dir
  push_json="$("$ORAS_BIN" push --format json \
    --artifact-type "$RELEASE_INDEX_ARTIFACT_TYPE" "$tx" \
    "${RELEASE_INDEX_PATH}:${RELEASE_INDEX_ARTIFACT_TYPE}")" \
    || log::die "release-index transaction publication failed"
  expected="$(printf '%s' "$push_json" | jq -r '.manifest.digest // .descriptor.digest // .digest // empty')"
  [[ "$expected" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || log::die "release-index push did not return a valid digest"
  promote::_assert_digest "$tx" "$expected"

  local index_ref="${PUBLIC_ACR}/${RELEASE_INDEX_REPO}@${expected}"
  "$COSIGN_BIN" sign --yes "$index_ref"
  "$COSIGN_BIN" attest --yes --predicate "$RELEASE_INDEX_PATH" \
    --type "$RELEASE_INDEX_ATTESTATION_TYPE" "$index_ref"
  "$COSIGN_BIN" verify \
    --certificate-identity-regexp "$COSIGN_CERTIFICATE_IDENTITY_REGEXP" \
    --certificate-oidc-issuer "$COSIGN_OIDC_ISSUER" "$index_ref" >/dev/null
  "$COSIGN_BIN" verify-attestation --type "$RELEASE_INDEX_ATTESTATION_TYPE" \
    --certificate-identity-regexp "$COSIGN_CERTIFICATE_IDENTITY_REGEXP" \
    --certificate-oidc-issuer "$COSIGN_OIDC_ISSUER" "$index_ref" >/dev/null

  config_dir="${RELEASE_VERIFY_DIR:-$(dirname "$MANIFEST_PATH")/.release-verify-${RELEASE_TRANSACTION_TAG#_}}"
  mkdir -p "$config_dir"
  printf '{}\n' >"${config_dir}/oras-config.json"
  "$ORAS_BIN" manifest fetch --registry-config "${config_dir}/oras-config.json" "$tx" >/dev/null

  "$ORAS_BIN" copy "$index_ref" "${PUBLIC_ACR}/${RELEASE_INDEX_REPO}:${RELEASE_VERSION}"
  promote::_assert_digest "${PUBLIC_ACR}/${RELEASE_INDEX_REPO}:${RELEASE_VERSION}" "$expected"
  RELEASE_INDEX_DIGEST="$expected"
  manifest::put_json "$MANIFEST_PATH" release.index "$(jq -n \
    --arg d "$expected" --arg v "$RELEASE_VERSION" \
    --arg r "${PUBLIC_ACR}/${RELEASE_INDEX_REPO}" \
    --arg cd "$CTRL_DIGEST" --arg cr "${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}" \
    --arg nd "$CNI_DIGEST" --arg nr "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}" \
    '{digest:$d,version:$v,repository:$r,
      controller:{digest:$cd,repository:$cr},
      cni:{digest:$nd,repository:$nr}}')"
}

promote::_rollback_precommit() {
  promote::_rollback_moving_tags || true
  promote::_rollback_version
  promote::_delete_prepared_tags
}

promote::_complete_exit() {
  local rc="$?"
  if (( rc != 0 )) && [[ "${PROMOTE_INDEX_COMMITTED:-0}" != "1" ]]; then
    log::warn "release failed before canonical index commit; removing all prepared tags"
    promote::_rollback_precommit
  fi
}

promote::complete() {
  promote::_require_release_inputs
  [[ "$(manifest::get "$MANIFEST_PATH" '.release.prepared // false')" == "true" ]] \
    || log::die "release transaction is not prepared"
  promote::_assert_version_absent "$CONTROLLER_PUBLIC_REPO"
  promote::_assert_version_absent "$CNI_PUBLIC_REPO"
  promote::_assert_version_absent "$RELEASE_INDEX_REPO"
  PROMOTE_INDEX_COMMITTED=0
  PROMOTE_MOVING_TOUCHED=()
  PROMOTE_CTRL_PREVIOUS=()
  PROMOTE_CNI_PREVIOUS=()
  trap promote::_complete_exit EXIT
  promote::_assert_digest "${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}:${RELEASE_TRANSACTION_TAG}" "$CTRL_DIGEST"
  promote::_assert_digest "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}:${RELEASE_TRANSACTION_TAG}" "$CNI_DIGEST"
  promote::_verify_supply_chain
  promote::_publish_version
  if ! promote::_post_publish_checks; then
    promote::_rollback_version
    log::die "published set failed digest/anonymous verification; both version tags rolled back"
  fi
  if ! promote::_publish_moving_tags; then
    promote::_rollback_version
    log::die "moving-tag publication failed; both version tags rolled back"
  fi
  promote::_publish_release_index
  PROMOTE_INDEX_COMMITTED=1
  manifest::put "$MANIFEST_PATH" release.supply_chain.provenance_verified true
  manifest::put "$MANIFEST_PATH" release.anonymous_pull_verified true
  manifest::put "$MANIFEST_PATH" release.set_atomic true
  manifest::put "$MANIFEST_PATH" release.complete true
  manifest::put_json "$MANIFEST_PATH" release.moving_tags "$(jq -nc \
    --arg tags "$MOVING_TAGS" '$tags | split(",") | map(select(length > 0))')"
  promote::_delete_prepared_tags
  gha::output release_version "$RELEASE_VERSION"
  gha::output controller_release "${PUBLIC_ACR}/${CONTROLLER_PUBLIC_REPO}:${RELEASE_VERSION}"
  gha::output cni_release "${PUBLIC_ACR}/${CNI_PUBLIC_REPO}:${RELEASE_VERSION}"
  gha::output release_index "${PUBLIC_ACR}/${RELEASE_INDEX_REPO}:${RELEASE_VERSION}"
  gha::output release_index_digest "$RELEASE_INDEX_DIGEST"
}

promote::abort() {
  promote::_require_release_inputs
  promote::_delete_prepared_tags
  rm -f "$RELEASE_INDEX_PATH"
}

promote::names() {
  promote::_derive
  printf 'release_version=%s\ncontroller_digest=%s\ncni_digest=%s\npublic_acr=%s\n' \
    "$RELEASE_VERSION" "$CTRL_DIGEST" "$CNI_DIGEST" "$PUBLIC_ACR"
}

promote::usage() {
  cat <<'USAGE'
Usage: promote-release.sh <gate|prepare|secure|complete|abort|names>

  gate      Fail closed unless all configured validation gates and both digests pass.
  prepare   Check immutability, promote both digests to transaction tags, verify bytes.
  secure    Keyless-sign both digests and attach both SBOMs.
  complete  Verify supply chain, prepare version/moving tags, then atomically commit the signed release index.
  abort     Remove transaction tags without touching a completed semantic version.
USAGE
}

promote::main() {
  local cmd="${1:-}"
  case "$cmd" in
    gate) promote::gate ;;
    prepare) promote::prepare ;;
    secure) promote::secure ;;
    complete) promote::complete ;;
    abort) promote::abort ;;
    names) promote::names ;;
    help|-h|--help|"") promote::usage ;;
    *) log::error "unknown command: ${cmd}"; promote::usage >&2; return 1 ;;
  esac
}

promote::main "$@"
