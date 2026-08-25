#!/usr/bin/env bash
# =============================================================================
# build.sh - build the candidate controller image once, publish it to the
# staging ACR BY DIGEST via OIDC, generate an SBOM, and record the immutable
# digest/reference into the run manifest for every downstream job.
#
# Thin-YAML backing script for the `build` job (RD-012 / GUD-002): the workflow
# only supplies inputs, provisions tools (docker/az/syft), and performs the
# OIDC `azure/login`; all build/publish/SBOM/manifest logic lives here so it is
# unit-testable via mocks (see naming_test/build_test.sh).
#
# Build-once/promote-by-digest discipline (RD-001): the sha256 captured here is
# the ONLY thing later jobs reference (@sha256:<digest>, FR-002); release
# promotes exactly this digest without rebuild (NFR-006).
#
# Inputs (environment):
#   EVENT_NAME                 github.event_name; pull_request => no push/cloud
#   BUILD_PUSH                 explicit "true"/"false" override of the push decision
#   STAGING_ACR                staging ACR login server (e.g. pncstg.azurecr.io);
#                              REQUIRED when pushing (fixed bootstrap value, CON-008)
#   CONTROLLER_STAGING_REPO    default candidate/pod-nsg-controller (from meta)
#   CONTROLLER_STAGING_TAG     topology-neutral run tag (from meta)      [required]
#   GO_MODULE                  module the SBOM must list (default: parsed from go.mod)
#   MANIFEST_PATH              run manifest to augment (default ./run-manifest.json)
#   SBOM_PATH                  SBOM output (default ./sbom/controller.spdx.json)
#   MAKE_BIN DOCKER_BIN AZ_BIN SYFT_BIN   tool seams (default: make/docker/az/syft)
#
# Outputs (GitHub Actions step outputs): image_reference image_digest image_repo
#   image_tag image_registry pushed sbom_path manifest_path
#
# Traceability: ITEM-004 (build+digest, PR no-push), ITEM-005 (SBOM+module),
# ITEM-006 (manifest digest/reference), FR-001, FR-002, FR-016, SEC-001,
# SEC-003, NFR-003, NFR-005, NFR-006, AC-001, PRD Sections 3.1/3.5.
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
CONTROLLER_STAGING_REPO="${CONTROLLER_STAGING_REPO:-candidate/pod-nsg-controller}"
CONTROLLER_STAGING_TAG="${CONTROLLER_STAGING_TAG:-}"
STAGING_ACR="${STAGING_ACR:-}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
SBOM_PATH="${SBOM_PATH:-sbom/controller.spdx.json}"
MAKE_BIN="${MAKE_BIN:-make}"
DOCKER_BIN="${DOCKER_BIN:-docker}"
AZ_BIN="${AZ_BIN:-az}"
SYFT_BIN="${SYFT_BIN:-syft}"

# Default the required Go-module assertion from go.mod (single source of truth).
if [[ -z "${GO_MODULE:-}" ]]; then
  GO_MODULE="$(awk '/^module /{print $2; exit}' "${REPO_ROOT}/go.mod" 2>/dev/null || true)"
  GO_MODULE="${GO_MODULE:-github.com/Azure/pod-nsg-controller}"
fi

[[ -n "$CONTROLLER_STAGING_TAG" ]] || log::die "CONTROLLER_STAGING_TAG is required (provided by the meta job)"

# ---- decide whether this run pushes to the cloud (RD-009) -------------------
# PRs build to validate the Dockerfile but never touch the cloud; every other
# trigger publishes the candidate. An explicit BUILD_PUSH always wins.
if [[ -n "${BUILD_PUSH:-}" ]]; then
  case "$BUILD_PUSH" in
    true|false) push="$BUILD_PUSH" ;;
    *) log::die "BUILD_PUSH must be 'true' or 'false' (got '${BUILD_PUSH}')" ;;
  esac
elif [[ "$EVENT_NAME" == "pull_request" ]]; then
  push=false
else
  push=true
fi

# ---- resolve references -----------------------------------------------------
repo="$CONTROLLER_STAGING_REPO"
tag="$CONTROLLER_STAGING_TAG"
registry=""
if [[ "$push" == "true" ]]; then
  [[ -n "$STAGING_ACR" ]] || log::die "pushing requires STAGING_ACR (the staging ACR login server)"
  registry="$STAGING_ACR"
  build_ref="${registry}/${repo}:${tag}"
else
  build_ref="${repo}:${tag}"
fi

log::info "build: repo=${repo} tag=${tag} push=${push} registry=${registry:-<none>} module=${GO_MODULE}"

# ---- build once from Dockerfile, reusing `make docker-build` (FR-001/GUD-001)
lib::require_cmds "$MAKE_BIN"
log::info "building controller image via 'make docker-build IMG=${build_ref}'"
( cd "$REPO_ROOT" && "$MAKE_BIN" docker-build IMG="$build_ref" ) \
  || log::die "docker-build failed for ${build_ref}"

# ---- publish by digest (OIDC) and capture the immutable sha256 (FR-002) -----
digest=""
if [[ "$push" == "true" ]]; then
  lib::require_cmds "$AZ_BIN" "$DOCKER_BIN"
  # Registry name is the first label of the login server (pncstg.azurecr.io -> pncstg).
  acr_name="${registry%%.*}"
  log::info "authenticating to staging ACR '${acr_name}' via OIDC (az acr login)"
  lib::retry 3 5 -- "$AZ_BIN" acr login --name "$acr_name" \
    || log::die "az acr login failed for ${acr_name}"
  log::info "pushing candidate image ${build_ref}"
  lib::retry 3 5 -- "$DOCKER_BIN" push "$build_ref" \
    || log::die "docker push failed for ${build_ref}"
  # The registry manifest digest (RepoDigests) is the immutable, pull-by-digest ref.
  local_repo_digest="$("$DOCKER_BIN" inspect --format '{{index .RepoDigests 0}}' "$build_ref")"
  digest="$(printf '%s' "$local_repo_digest" | sed -E 's/.*@(sha256:[0-9a-f]{64})$/\1/')"
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || log::die "could not capture a valid registry digest for ${build_ref} (got '${local_repo_digest}')"
  reference="${registry}/${repo}@${digest}"
  scan_target="$reference"
else
  lib::require_cmds "$DOCKER_BIN"
  # No push: record the local image config id (a valid sha256) so the PR build
  # still emits a digest without contacting any registry (ITEM-004 validation).
  digest="$("$DOCKER_BIN" inspect --format '{{.Id}}' "$build_ref")"
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || log::die "could not capture a local image digest for ${build_ref} (got '${digest}')"
  reference="$build_ref"
  scan_target="$build_ref"
fi
log::info "candidate digest: ${digest}"
log::info "candidate reference: ${reference}"

# ---- SBOM (syft) for the candidate; assert the Go module is present (ITEM-005)
lib::require_cmds "$SYFT_BIN"
mkdir -p "$(dirname "$SBOM_PATH")"
log::info "generating SBOM for ${scan_target} -> ${SBOM_PATH}"
"$SYFT_BIN" "$scan_target" -o "spdx-json=${SBOM_PATH}" \
  || log::die "syft SBOM generation failed for ${scan_target}"
[[ -s "$SBOM_PATH" ]] || log::die "SBOM was not produced at ${SBOM_PATH}"
grep -q -F -- "$GO_MODULE" "$SBOM_PATH" \
  || log::die "SBOM at ${SBOM_PATH} does not list the Go module '${GO_MODULE}'"
log::info "SBOM lists the Go module '${GO_MODULE}'"

# ---- record the artifact into the run manifest (ITEM-006 / AC-001) ----------
[[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
manifest::record_controller_artifact "$MANIFEST_PATH" "$registry" "$repo" "$tag" "$digest" "$push"
manifest::put "$MANIFEST_PATH" artifacts.controller.sbom "$SBOM_PATH"

# ---- step outputs (threaded to deploy/validate/release in later epics) ------
gha::output image_reference "$reference"
gha::output image_digest "$digest"
gha::output image_repo "$repo"
gha::output image_tag "$tag"
gha::output image_registry "$registry"
gha::output pushed "$push"
gha::output sbom_path "$SBOM_PATH"
gha::output manifest_path "$MANIFEST_PATH"

# ---- compact job summary (no credentials/tokens) ----------------------------
gha::summary "## Candidate build (\`build\`)"
gha::summary ""
gha::summary "| Field | Value |"
gha::summary "|---|---|"
gha::summary "| Pushed to staging | ${push} |"
gha::summary "| Reference | \`${reference}\` |"
gha::summary "| Digest | \`${digest}\` |"
gha::summary "| SBOM | \`${SBOM_PATH}\` (lists \`${GO_MODULE}\`) |"

log::info "build complete: pushed=${push} reference=${reference}"
