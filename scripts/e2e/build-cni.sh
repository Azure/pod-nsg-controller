#!/usr/bin/env bash
# =============================================================================
# build-cni.sh - build/package the transparent-tunnel CNI artifact ONCE, publish
# it to the staging ACR BY DIGEST via OIDC, generate an SBOM, and record the
# immutable CNI digest/reference into the run manifest for validate_tt + release.
#
# Repository-native backing script for the `cni-build`/`cni-package`/
# `cni-artifact` Make targets (FR-018 / GUD-002): the Makefile and the
# `build_cni` workflow job only supply inputs and OIDC login; all
# acquisition/checksum/mode-assert/package/push/SBOM/manifest logic lives here so
# it is unit-testable via mocks (see naming_test/build_cni_test.sh).
#
# This repository contains NO CNI source (CON-011): the transparent-tunnel
# `azure-vnet` binary + `azure-linux-transparent-tunnel.conflist` originate from
# the external azure-container-networking transparent-tunnel build. This script
# PINS that source and makes the artifact reproducible/auditable (NFR-010) via
# three acquisition modes, in the PRD's order of preference (RD-015):
#   * source   (primary)  - git clone a PINNED azure-container-networking ref and
#                           `go build` the linux/amd64 azure-vnet; copy the
#                           conflist from the pinned tree.
#   * prebuilt (fallback) - fetch a PINNED prebuilt azure-vnet + conflist and
#                           verify a RECORDED sha256 (SEC-006). No committed
#                           SAS/credential; the URL is a run-time pinned input.
#   * fixture  (dry-run)  - assemble from a local CNI_FIXTURE_DIR (offline Make
#                           dry runs / hermetic tests); no network.
# Every mode asserts the conflist declares `"mode": "transparent-tunnel"` at
# build time (FR-018) and records the binary checksum (NFR-010).
#
# Build-once/promote-by-digest discipline (RD-001/RD-014): the sha256 captured
# here is the ONLY thing validate_tt/release reference (@sha256:<digest>, FR-002);
# release promotes exactly this CNI digest with the controller digest, atomically,
# without rebuild (NFR-011).
#
# Inputs (environment):
#   EVENT_NAME              github.event_name; pull_request => no push/cloud
#   BUILD_PUSH              explicit "true"/"false" override of the push decision
#   STAGING_ACR             staging ACR login server (e.g. pncstg.azurecr.io);
#                           REQUIRED when pushing (fixed bootstrap value, CON-008)
#   CNI_STAGING_REPO        default candidate/pod-nsg-cni-transparent-tunnel
#   CNI_STAGING_TAG         topology-neutral run tag (from meta)        [required]
#   CNI_SOURCE_MODE         source | prebuilt | fixture (default: source)
#   CNI_SOURCE_REPO/REF     PINNED azure-container-networking repo + ref (source)
#   CNI_BINARY_URL/CONFLIST_URL  PINNED prebuilt locations (prebuilt)
#   CNI_BINARY_SHA256       RECORDED checksum: REQUIRED (prebuilt), optional else
#   CNI_FIXTURE_DIR         local dir with azure-vnet + conflist (fixture)
#   CNI_ARTIFACT_TYPE       OCI artifactType (default application/vnd.azure.pnc.cni.transparent-tunnel)
#   CNI_WORKDIR             build/package scratch (default ./.cni-build)
#   MANIFEST_PATH           run manifest to augment (default ./run-manifest.json)
#   SBOM_PATH               SBOM output (default ./sbom/cni.spdx.json)
#   AZ_BIN ORAS_BIN SYFT_BIN GIT_BIN GO_BIN CURL_BIN   tool seams
#
# Commands: build | package | artifact | all(default) | names | help
# Outputs (artifact/all step outputs): cni_reference cni_digest cni_repo cni_tag
#   cni_registry pushed conflist_mode sbom_path manifest_path
#
# Traceability: ITEM-028 (repo-native build/package + mode/checksum assert),
# ITEM-029 (push by digest + SBOM + manifest CNI digest), FR-018, FR-019,
# NFR-010, NFR-011, SEC-006, CON-011, AC-001, AC-016, TTS-001, PRD Section 3.6.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"

lib::require_cmds jq sha256sum

# ---- inputs -----------------------------------------------------------------
EVENT_NAME="${EVENT_NAME:-${GITHUB_EVENT_NAME:-}}"
CNI_STAGING_REPO="${CNI_STAGING_REPO:-candidate/pod-nsg-cni-transparent-tunnel}"
CNI_STAGING_TAG="${CNI_STAGING_TAG:-}"
STAGING_ACR="${STAGING_ACR:-}"
CNI_SOURCE_MODE="${CNI_SOURCE_MODE:-source}"
# Pinned azure-container-networking transparent-tunnel source (ASSUMPTION-005;
# EPIC-008 records how the exact ref/checksum is obtained and updated). The pin
# lives here as repository configuration, NOT a runtime knob (Simplicity §Config).
CNI_SOURCE_REPO="${CNI_SOURCE_REPO:-https://github.com/Azure/azure-container-networking}"
CNI_SOURCE_REF="${CNI_SOURCE_REF:-v1.6.6}"
CNI_SOURCE_CMD_PATH="${CNI_SOURCE_CMD_PATH:-azure-vnet}"
CNI_SOURCE_CONFLIST_PATH="${CNI_SOURCE_CONFLIST_PATH:-cni/azure-linux-transparent-tunnel.conflist}"
CNI_BINARY_URL="${CNI_BINARY_URL:-}"
CNI_CONFLIST_URL="${CNI_CONFLIST_URL:-}"
CNI_BINARY_SHA256="${CNI_BINARY_SHA256:-}"
CNI_FIXTURE_DIR="${CNI_FIXTURE_DIR:-}"
CNI_ARTIFACT_TYPE="${CNI_ARTIFACT_TYPE:-application/vnd.azure.pnc.cni.transparent-tunnel}"
CNI_WORKDIR="${CNI_WORKDIR:-${REPO_ROOT}/.cni-build}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
SBOM_PATH="${SBOM_PATH:-sbom/cni.spdx.json}"
AZ_BIN="${AZ_BIN:-az}"
ORAS_BIN="${ORAS_BIN:-oras}"
SYFT_BIN="${SYFT_BIN:-syft}"
GIT_BIN="${GIT_BIN:-git}"
GO_BIN="${GO_BIN:-go}"
CURL_BIN="${CURL_BIN:-curl}"

CNI_BINARY_NAME="azure-vnet"
CNI_CONFLIST_NAME="azure-linux-transparent-tunnel.conflist"
STAGE_DIR="${CNI_WORKDIR}/stage"       # acquired binary + conflist
ARTIFACT_DIR="${CNI_WORKDIR}/artifact" # exact bytes packaged into the OCI artifact
META_FILE="${CNI_WORKDIR}/build.env"   # state shared across build/package/artifact

# ---- decide whether this run pushes to the cloud (RD-009) -------------------
cni::_decide_push() {
  if [[ -n "${BUILD_PUSH:-}" ]]; then
    case "$BUILD_PUSH" in
      true|false) printf '%s' "$BUILD_PUSH" ;;
      *) log::die "BUILD_PUSH must be 'true' or 'false' (got '${BUILD_PUSH}')" ;;
    esac
  elif [[ "$EVENT_NAME" == "pull_request" ]]; then
    printf 'false'
  else
    printf 'true'
  fi
}

# ---- acquisition (source | prebuilt | fixture) ------------------------------
cni::_acquire_fixture() {
  [[ -n "$CNI_FIXTURE_DIR" ]] || log::die "CNI_SOURCE_MODE=fixture requires CNI_FIXTURE_DIR"
  [[ -f "${CNI_FIXTURE_DIR}/${CNI_BINARY_NAME}" ]] \
    || log::die "fixture missing ${CNI_BINARY_NAME} in ${CNI_FIXTURE_DIR}"
  [[ -f "${CNI_FIXTURE_DIR}/${CNI_CONFLIST_NAME}" ]] \
    || log::die "fixture missing ${CNI_CONFLIST_NAME} in ${CNI_FIXTURE_DIR}"
  cp -f "${CNI_FIXTURE_DIR}/${CNI_BINARY_NAME}"   "${STAGE_DIR}/${CNI_BINARY_NAME}"
  cp -f "${CNI_FIXTURE_DIR}/${CNI_CONFLIST_NAME}" "${STAGE_DIR}/${CNI_CONFLIST_NAME}"
}

cni::_acquire_prebuilt() {
  [[ -n "$CNI_BINARY_URL"   ]] || log::die "CNI_SOURCE_MODE=prebuilt requires CNI_BINARY_URL (pinned)"
  [[ -n "$CNI_CONFLIST_URL" ]] || log::die "CNI_SOURCE_MODE=prebuilt requires CNI_CONFLIST_URL (pinned)"
  # SEC-006: a pinned prebuilt MUST carry a recorded checksum to verify against.
  [[ -n "$CNI_BINARY_SHA256" ]] || log::die "CNI_SOURCE_MODE=prebuilt requires CNI_BINARY_SHA256 (recorded checksum; SEC-006)"
  lib::require_cmds "$CURL_BIN"
  log::info "fetching pinned prebuilt CNI binary + conflist (checksum-verified; no committed credential, SEC-006)"
  lib::retry 3 5 -- "$CURL_BIN" -fsSL "$CNI_BINARY_URL"   -o "${STAGE_DIR}/${CNI_BINARY_NAME}" \
    || log::die "failed to fetch pinned CNI binary"
  lib::retry 3 5 -- "$CURL_BIN" -fsSL "$CNI_CONFLIST_URL" -o "${STAGE_DIR}/${CNI_CONFLIST_NAME}" \
    || log::die "failed to fetch pinned CNI conflist"
}

cni::_acquire_source() {
  lib::require_cmds "$GIT_BIN" "$GO_BIN"
  local src="${CNI_WORKDIR}/src"
  rm -rf "$src"; mkdir -p "$src"
  log::info "cloning pinned azure-container-networking ${CNI_SOURCE_REF} (CON-011/RD-015)"
  lib::retry 3 5 -- "$GIT_BIN" clone --depth 1 --branch "$CNI_SOURCE_REF" "$CNI_SOURCE_REPO" "$src" \
    || log::die "failed to clone pinned CNI source ${CNI_SOURCE_REPO}@${CNI_SOURCE_REF}"
  log::info "building linux/amd64 ${CNI_BINARY_NAME} from pinned source"
  ( cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      "$GO_BIN" build -o "${STAGE_DIR}/${CNI_BINARY_NAME}" "./${CNI_SOURCE_CMD_PATH}" ) \
    || log::die "go build of ${CNI_BINARY_NAME} failed from pinned source"
  [[ -f "${src}/${CNI_SOURCE_CONFLIST_PATH}" ]] \
    || log::die "pinned source is missing ${CNI_SOURCE_CONFLIST_PATH}"
  cp -f "${src}/${CNI_SOURCE_CONFLIST_PATH}" "${STAGE_DIR}/${CNI_CONFLIST_NAME}"
}

# cni::_assert_mode : the conflist MUST declare "mode": "transparent-tunnel"
# (FR-018), mirroring the runbook's grep and additionally validating JSON.
cni::_assert_mode() {
  local cf="${STAGE_DIR}/${CNI_CONFLIST_NAME}"
  jq -e '.' "$cf" >/dev/null 2>&1 || log::die "conflist ${cf} is not valid JSON"
  grep -q '"mode":[[:space:]]*"transparent-tunnel"' "$cf" \
    || log::die "conflist does not declare \"mode\": \"transparent-tunnel\" (FR-018 build-time assertion failed)"
}

# ---- build: acquire + checksum + mode assert (make cni-build / ITEM-028) -----
cni::build() {
  [[ -n "$CNI_STAGING_TAG" ]] || log::die "CNI_STAGING_TAG is required (provided by the meta job)"
  rm -rf "$STAGE_DIR"; mkdir -p "$STAGE_DIR"
  case "$CNI_SOURCE_MODE" in
    source)   cni::_acquire_source ;;
    prebuilt) cni::_acquire_prebuilt ;;
    fixture)  cni::_acquire_fixture ;;
    *) log::die "invalid CNI_SOURCE_MODE '${CNI_SOURCE_MODE}' (want source|prebuilt|fixture)" ;;
  esac
  [[ -s "${STAGE_DIR}/${CNI_BINARY_NAME}" ]]   || log::die "acquired ${CNI_BINARY_NAME} is empty"
  chmod +x "${STAGE_DIR}/${CNI_BINARY_NAME}" 2>/dev/null || true

  # Recorded-checksum verification (SEC-006/NFR-010). A pin (CNI_BINARY_SHA256)
  # is REQUIRED for prebuilt and, when present in any mode, MUST match.
  local actual_sha
  actual_sha="$(sha256sum "${STAGE_DIR}/${CNI_BINARY_NAME}" | cut -c1-64)"
  if [[ -n "$CNI_BINARY_SHA256" ]]; then
    [[ "$actual_sha" == "$CNI_BINARY_SHA256" ]] \
      || log::die "CNI binary checksum mismatch: recorded=${CNI_BINARY_SHA256} actual=${actual_sha} (SEC-006/RISK-008)"
    log::info "CNI binary checksum verified against recorded pin (${actual_sha})"
  else
    log::info "recording built CNI binary checksum for provenance (${actual_sha})"
  fi

  cni::_assert_mode
  log::info "conflist asserts \"mode\": \"transparent-tunnel\" (mode=${CNI_SOURCE_MODE}, ref=${CNI_SOURCE_REF})"

  mkdir -p "$(dirname "$META_FILE")"
  {
    printf 'CNI_BINARY_SHA256=%s\n' "$actual_sha"
    printf 'CNI_SOURCE_MODE=%s\n'   "$CNI_SOURCE_MODE"
    printf 'CNI_SOURCE_REF=%s\n'    "$CNI_SOURCE_REF"
  } > "$META_FILE"
  log::info "cni-build OK: ${STAGE_DIR}/${CNI_BINARY_NAME} + ${CNI_CONFLIST_NAME}"
}

# ---- package: assemble the exact artifact bytes + content digest -------------
# The content digest is a deterministic sha256 over the sorted (name,filehash)
# pairs, so a no-push (PR/local) run still emits a stable digest without a
# registry, and two runs of the same bytes agree (NFR-010).
cni::package() {
  [[ -s "${STAGE_DIR}/${CNI_BINARY_NAME}" && -s "${STAGE_DIR}/${CNI_CONFLIST_NAME}" ]] \
    || log::die "cni-package requires cni-build outputs in ${STAGE_DIR} (run 'build' first)"
  rm -rf "$ARTIFACT_DIR"; mkdir -p "$ARTIFACT_DIR"
  cp -f "${STAGE_DIR}/${CNI_BINARY_NAME}"   "${ARTIFACT_DIR}/${CNI_BINARY_NAME}"
  cp -f "${STAGE_DIR}/${CNI_CONFLIST_NAME}" "${ARTIFACT_DIR}/${CNI_CONFLIST_NAME}"
  local content_digest
  content_digest="sha256:$( ( cd "$ARTIFACT_DIR" && \
      find . -type f -printf '%P\n' | LC_ALL=C sort | while IFS= read -r f; do
        printf '%s  %s\n' "$(sha256sum "$f" | cut -c1-64)" "$f"
      done ) | sha256sum | cut -c1-64 )"
  printf 'CNI_CONTENT_DIGEST=%s\n' "$content_digest" >> "$META_FILE"
  log::info "cni-package OK: content digest ${content_digest}"
}

# ---- artifact: push-by-digest (OIDC) + SBOM + manifest (make cni-artifact) ----
cni::artifact() {
  [[ -d "$ARTIFACT_DIR" ]] || log::die "cni-artifact requires cni-package output (run 'package' first)"
  [[ -f "$META_FILE" ]]    || log::die "cni-artifact requires build/package state (${META_FILE} missing)"
  # shellcheck source=/dev/null
  source "$META_FILE"

  local push repo tag registry digest reference conflist_mode="transparent-tunnel"
  push="$(cni::_decide_push)"
  repo="$CNI_STAGING_REPO"; tag="$CNI_STAGING_TAG"; registry=""
  [[ -n "$tag" ]] || log::die "CNI_STAGING_TAG is required (provided by the meta job)"

  if [[ "$push" == "true" ]]; then
    [[ -n "$STAGING_ACR" ]] || log::die "pushing requires STAGING_ACR (the staging ACR login server)"
    registry="$STAGING_ACR"
    lib::require_cmds "$AZ_BIN" "$ORAS_BIN"
    local acr_name="${registry%%.*}" push_ref="${registry}/${repo}:${tag}"
    log::info "authenticating to staging ACR '${acr_name}' via OIDC (az acr login)"
    lib::retry 3 5 -- "$AZ_BIN" acr login --name "$acr_name" \
      || log::die "az acr login failed for ${acr_name}"
    log::info "pushing digest-addressable CNI OCI artifact ${push_ref}"
    local out
    out="$( ( cd "$ARTIFACT_DIR" && lib::retry 3 5 -- "$ORAS_BIN" push "$push_ref" \
              --artifact-type "$CNI_ARTIFACT_TYPE" \
              "${CNI_BINARY_NAME}:application/vnd.azure.pnc.cni.binary" \
              "${CNI_CONFLIST_NAME}:application/vnd.azure.pnc.cni.conflist" ) )" \
      || log::die "oras push failed for ${push_ref}"
    printf '%s\n' "$out"
    digest="$(printf '%s' "$out" | grep -oE 'sha256:[0-9a-f]{64}' | head -1)"
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] \
      || log::die "could not capture a valid pushed OCI digest for ${push_ref}"
    reference="${registry}/${repo}@${digest}"
  else
    # No push: use the deterministic package content digest (ITEM-028 validation).
    digest="${CNI_CONTENT_DIGEST:-}"
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] \
      || log::die "no valid CNI content digest from package step (got '${digest:-<none>}')"
    reference="${repo}:${tag}"
  fi
  log::info "candidate CNI digest: ${digest}"
  log::info "candidate CNI reference: ${reference}"

  # SBOM (syft) over the exact artifact bytes.
  lib::require_cmds "$SYFT_BIN"
  mkdir -p "$(dirname "$SBOM_PATH")"
  log::info "generating SBOM for the CNI artifact -> ${SBOM_PATH}"
  "$SYFT_BIN" "dir:${ARTIFACT_DIR}" -o "spdx-json=${SBOM_PATH}" \
    || log::die "syft SBOM generation failed for ${ARTIFACT_DIR}"
  [[ -s "$SBOM_PATH" ]] || log::die "SBOM was not produced at ${SBOM_PATH}"
  grep -q -F -- "$CNI_BINARY_NAME" "$SBOM_PATH" \
    || log::die "SBOM at ${SBOM_PATH} does not list the CNI binary '${CNI_BINARY_NAME}'"

  # Record the CNI artifact into the run manifest (ITEM-029 / AC-001 / AC-016).
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  manifest::record_cni_artifact "$MANIFEST_PATH" "$registry" "$repo" "$tag" "$digest" "$push"
  manifest::put "$MANIFEST_PATH" artifacts.cni.sbom "$SBOM_PATH"
  manifest::put "$MANIFEST_PATH" artifacts.cni.conflist_mode "$conflist_mode"
  manifest::put "$MANIFEST_PATH" artifacts.cni.binary_sha256 "${CNI_BINARY_SHA256:-}"
  manifest::put "$MANIFEST_PATH" artifacts.cni.source_mode "${CNI_SOURCE_MODE:-}"
  manifest::put "$MANIFEST_PATH" artifacts.cni.source_ref "${CNI_SOURCE_REF:-}"

  # Step outputs (threaded to validate_tt install + release).
  gha::output cni_reference "$reference"
  gha::output cni_digest "$digest"
  gha::output cni_repo "$repo"
  gha::output cni_tag "$tag"
  gha::output cni_registry "$registry"
  gha::output pushed "$push"
  gha::output conflist_mode "$conflist_mode"
  gha::output sbom_path "$SBOM_PATH"
  gha::output manifest_path "$MANIFEST_PATH"

  gha::summary "## Candidate CNI artifact (\`build_cni\`)"
  gha::summary ""
  gha::summary "| Field | Value |"
  gha::summary "|---|---|"
  gha::summary "| Pushed to staging | ${push} |"
  gha::summary "| Reference | \`${reference}\` |"
  gha::summary "| Digest | \`${digest}\` |"
  gha::summary "| Conflist mode | \`${conflist_mode}\` |"
  gha::summary "| Source | \`${CNI_SOURCE_MODE}\` @ \`${CNI_SOURCE_REF}\` |"
  gha::summary "| SBOM | \`${SBOM_PATH}\` (lists \`${CNI_BINARY_NAME}\`) |"

  log::info "cni-artifact complete: pushed=${push} reference=${reference}"
}

cni::names() {
  printf 'staging_repo=%s\nstaging_tag=%s\nsource_mode=%s\nsource_ref=%s\nworkdir=%s\n' \
    "$CNI_STAGING_REPO" "$CNI_STAGING_TAG" "$CNI_SOURCE_MODE" "$CNI_SOURCE_REF" "$CNI_WORKDIR"
}

cni::usage() {
  cat <<USAGE
Usage: build-cni.sh <command>

Commands:
  build      Acquire azure-vnet + conflist (source|prebuilt|fixture), verify the
             recorded checksum, and assert "mode": "transparent-tunnel".
  package    Assemble the exact artifact bytes and compute a content digest.
  artifact   Push the digest-addressable OCI artifact to staging (OIDC), emit an
             SBOM, and record the CNI digest/reference into the run manifest.
  all        build -> package -> artifact (default).
  names      Print resolved CNI staging names / source pin.
  help       Show this help.

Pin (repository configuration; EPIC-008 records updates): CNI_SOURCE_REPO,
CNI_SOURCE_REF, CNI_BINARY_SHA256. Tool seams: AZ_BIN ORAS_BIN SYFT_BIN GIT_BIN
GO_BIN CURL_BIN.
USAGE
}

cni::main() {
  local cmd="${1:-all}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    build)    cni::build ;;
    package)  cni::package ;;
    artifact) cni::artifact ;;
    all)      cni::build; cni::package; cni::artifact ;;
    names)    cni::names ;;
    help|-h|--help) cni::usage ;;
    *) log::error "unknown command: ${cmd}"; cni::usage >&2; return 1 ;;
  esac
}

cni::main "$@"
