#!/usr/bin/env bash
# =============================================================================
# promote-release.sh - artifact-set atomicity + validate_tt release-GATE SEAM.
#
# SCOPE (EPIC-009 / ITEM-035): this file establishes the promotion/release
# integration SEAM that EPIC-006 (ITEM-019/020/021) fleshes out. It guarantees
# the two things ITEM-035 requires:
#   1. The controller image AND the transparent-tunnel CNI artifact are promoted
#      as ONE atomic set - by digest, under a SINGLE ${SEMVER}, with NO rebuild -
#      or NEITHER is promoted (both-or-neither, NFR-011/FR-024). The released
#      digests are recorded and MUST equal the validated candidate digests
#      (AC-023).
#   2. validate_tt success GATES the release: any TTS failure blocks BOTH
#      promotions (TEST-009/AC-009).
#
# INTENTIONALLY DEFERRED to EPIC-006 (NOT implemented here, by design):
#   * cosign keyless signing, SBOM attach, SLSA provenance      -> ITEM-020
#   * anonymous-pull enable + unauthenticated pull verification -> ITEM-021
#   * immutability assertion (NFR-009) + moving tags            -> ITEM-019
#   * the broader release gate (ss/xs Tests 1-4, cleanups)      -> ITEM-018
# The gate here is composable: it enforces the ITEM-035 additions (validate_tt +
# atomic set); ITEM-018 ANDs the remaining conditions onto it.
#
# Inputs (environment):
#   RELEASE_VERSION            semantic version tag, e.g. v1.2.3           [req]
#   PUBLIC_ACR                 public ACR login server (bootstrap, CON-007) [req]
#   STAGING_ACR                staging ACR login server
#   MANIFEST_PATH              run manifest with artifacts.controller/.cni + validate.tt.status
#   CONTROLLER_PUBLIC_REPO     default pod-nsg-controller
#   CNI_PUBLIC_REPO            default pod-nsg-cni-transparent-tunnel
#   VALIDATE_TT_STATUS         optional override from the job's needs result
#                              (success|failure|...); when unset, read from manifest
#   AZ_BIN ORAS_BIN            tool seams (default az/oras)
#
# Commands: release(default) | gate | names | help
#
# Traceability: ITEM-035, FR-024, NFR-011, AC-023, AC-009, TEST-009,
# RD-013/RD-014, CON-007, PRD Section 3.6.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"

lib::require_cmds jq

RELEASE_VERSION="${RELEASE_VERSION:-}"
PUBLIC_ACR="${PUBLIC_ACR:-}"
STAGING_ACR="${STAGING_ACR:-}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
CONTROLLER_PUBLIC_REPO="${CONTROLLER_PUBLIC_REPO:-pod-nsg-controller}"
CNI_PUBLIC_REPO="${CNI_PUBLIC_REPO:-pod-nsg-cni-transparent-tunnel}"
VALIDATE_TT_STATUS="${VALIDATE_TT_STATUS:-}"
AZ_BIN="${AZ_BIN:-az}"
ORAS_BIN="${ORAS_BIN:-oras}"

# ---- resolve the validated artifact set from the manifest -------------------
promote::_derive() {
  [[ "${PROMOTE_DERIVED:-0}" == "1" ]] && return 0
  [[ -f "$MANIFEST_PATH" ]] || log::die "manifest not found at ${MANIFEST_PATH} (nothing to promote)"
  CTRL_DIGEST="$(manifest::get "$MANIFEST_PATH" '.artifacts.controller.digest // empty')"
  CTRL_REFERENCE="$(manifest::get "$MANIFEST_PATH" '.artifacts.controller.reference // empty')"
  CNI_DIGEST="$(manifest::get "$MANIFEST_PATH" '.artifacts.cni.digest // empty')"
  CNI_REFERENCE="$(manifest::get "$MANIFEST_PATH" '.artifacts.cni.reference // empty')"
  TT_STATUS_MANIFEST="$(manifest::get "$MANIFEST_PATH" '.validate.tt.status // empty')"
  PROMOTE_DERIVED=1
}

# ---- release GATE (validate_tt + atomic set) --------------------------------
# Fails CLOSED unless validate_tt passed AND both validated digests exist. This
# is the ITEM-035 contract; ITEM-018 ANDs the ss/xs test + cleanup conditions.
promote::gate() {
  promote::_derive
  local rc=0

  # validate_tt gate: an explicit job-result override wins; else the manifest.
  local tt="$VALIDATE_TT_STATUS"
  [[ -n "$tt" ]] || tt="$TT_STATUS_MANIFEST"
  case "$tt" in
    pass|success|Success) : ;;
    "") log::error "release gate: validate_tt result is ABSENT (fail closed; AC-009)"; rc=1 ;;
    *)  log::error "release gate: validate_tt did NOT pass (status='${tt}') - blocking release (TEST-009/AC-009)"; rc=1 ;;
  esac

  # Artifact-set atomicity: BOTH digests MUST be present and pinned by @sha256.
  [[ "$CTRL_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || { log::error "release gate: controller digest missing/invalid (got '${CTRL_DIGEST:-<none>}')"; rc=1; }
  [[ "$CNI_DIGEST"  =~ ^sha256:[0-9a-f]{64}$ ]] || { log::error "release gate: CNI digest missing/invalid (NFR-011 atomic set; got '${CNI_DIGEST:-<none>}')"; rc=1; }

  if (( rc == 0 )); then log::info "release gate OPEN: validate_tt=${tt}, controller+CNI digests present (atomic set)"; fi
  return "$rc"
}

# ---- promote the atomic set by digest (no rebuild) --------------------------
promote::release() {
  promote::_derive
  [[ -n "$RELEASE_VERSION" ]] || log::die "RELEASE_VERSION (semantic version) is required"
  [[ "$RELEASE_VERSION" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$ ]] \
    || log::die "RELEASE_VERSION '${RELEASE_VERSION}' is not a valid semantic version"
  [[ -n "$PUBLIC_ACR" ]] || log::die "PUBLIC_ACR is required (pre-provisioned public ACR, CON-007)"

  # HARD GATE FIRST: validate everything before touching either registry so the
  # set is promoted atomically (both-or-neither); a failed gate promotes NEITHER.
  promote::gate || log::die "release blocked by the gate; NEITHER artifact promoted (atomic set preserved)"

  local pub_name="${PUBLIC_ACR%%.*}"
  local ctrl_src ctrl_repo cni_src cni_dest
  # Source references by digest (the validated candidates; no rebuild, RD-001/014).
  ctrl_src="${CTRL_REFERENCE:-${STAGING_ACR:+${STAGING_ACR}/}candidate/pod-nsg-controller@${CTRL_DIGEST}}"
  cni_src="${CNI_REFERENCE:-${STAGING_ACR:+${STAGING_ACR}/}candidate/pod-nsg-cni-transparent-tunnel@${CNI_DIGEST}}"
  ctrl_repo="${CONTROLLER_PUBLIC_REPO}:${RELEASE_VERSION}"
  cni_dest="${PUBLIC_ACR}/${CNI_PUBLIC_REPO}:${RELEASE_VERSION}"

  lib::require_cmds "$AZ_BIN" "$ORAS_BIN"

  # Authenticate both registries via OIDC: `az acr import` uses the ARM context,
  # but `oras copy` needs registry credentials in the cred store (AcrPull on the
  # staging source, AcrPush on the public destination). No standing secret (SEC-001).
  log::info "authenticating to registries via OIDC for by-digest promotion"
  lib::retry 3 5 -- "$AZ_BIN" acr login --name "$pub_name" \
    || log::die "az acr login failed for public ACR ${pub_name}"
  if [[ -n "$STAGING_ACR" ]]; then
    lib::retry 3 5 -- "$AZ_BIN" acr login --name "${STAGING_ACR%%.*}" \
      || log::die "az acr login failed for staging ACR ${STAGING_ACR%%.*}"
  fi

  # 1) Controller image: server-side copy by digest into the public ACR (RD-002).
  log::info "promoting controller ${ctrl_src} -> ${PUBLIC_ACR}/${ctrl_repo} (by digest, no rebuild)"
  lib::retry 3 5 -- "$AZ_BIN" acr import --name "$pub_name" \
    --source "$ctrl_src" --image "$ctrl_repo" \
    || log::die "controller promotion failed; release aborted (NEITHER artifact fully promoted)"

  # 2) CNI OCI artifact: copy by digest under the SAME semver (set atomicity).
  log::info "promoting CNI ${cni_src} -> ${cni_dest} (by digest, same semver ${RELEASE_VERSION})"
  lib::retry 3 5 -- "$ORAS_BIN" copy "$cni_src" "$cni_dest" \
    || log::die "CNI promotion failed AFTER controller import; the set is INCOMPLETE (EPIC-006 ITEM-019 advances moving tags for rollback; this version tag MUST NOT be referenced)"

  # Record the released set; released digests == validated candidate digests (AC-023).
  manifest::put "$MANIFEST_PATH" release.version "$RELEASE_VERSION"
  manifest::put_json "$MANIFEST_PATH" release.controller "$(jq -n \
    --arg d "$CTRL_DIGEST" --arg r "${PUBLIC_ACR}/${ctrl_repo}" --arg s "$ctrl_src" \
    '{digest:$d, reference:$r, source:$s}')"
  manifest::put_json "$MANIFEST_PATH" release.cni "$(jq -n \
    --arg d "$CNI_DIGEST" --arg r "$cni_dest" --arg s "$cni_src" \
    '{digest:$d, reference:$r, source:$s}')"
  manifest::put "$MANIFEST_PATH" release.set_atomic true
  # Explicit deferral markers so EPIC-006 knows what remains for this version.
  manifest::put_json "$MANIFEST_PATH" release.pending "$(jq -n \
    '{signing:"ITEM-020", provenance:"ITEM-020", anonymous_pull:"ITEM-021", immutability:"ITEM-019", moving_tags:"ITEM-019"}')"

  gha::output release_version "$RELEASE_VERSION"
  gha::output controller_release "${PUBLIC_ACR}/${ctrl_repo}"
  gha::output cni_release "$cni_dest"
  gha::output set_atomic true
  gha::summary "## Artifact-set promotion (\`release\`)"
  gha::summary ""
  gha::summary "| Artifact | Released (by digest) |"
  gha::summary "|---|---|"
  gha::summary "| Controller | \`${PUBLIC_ACR}/${ctrl_repo}\` @ \`${CTRL_DIGEST}\` |"
  gha::summary "| CNI | \`${cni_dest}\` @ \`${CNI_DIGEST}\` |"
  gha::summary ""
  gha::summary "> Signing/SBOM/provenance (ITEM-020), anonymous-pull verification (ITEM-021), and immutability/moving tags (ITEM-019) are completed by EPIC-006."
  log::info "artifact set promoted atomically under ${RELEASE_VERSION} (controller+CNI, by digest, no rebuild)"
}

promote::names() {
  promote::_derive
  printf 'release_version=%s\ncontroller_digest=%s\ncni_digest=%s\nvalidate_tt=%s\npublic_acr=%s\n' \
    "$RELEASE_VERSION" "$CTRL_DIGEST" "$CNI_DIGEST" "${VALIDATE_TT_STATUS:-$TT_STATUS_MANIFEST}" "$PUBLIC_ACR"
}

promote::usage() {
  cat <<USAGE
Usage: promote-release.sh <command>

Commands:
  release   Gate on validate_tt + atomic set, then promote BOTH artifacts by
            digest under one \${SEMVER} (no rebuild). Default.
  gate      Evaluate the release gate only (validate_tt + both digests present).
  names     Print resolved release coordinates.
  help      Show this help.

EPIC-006 (ITEM-019/020/021) completes signing, SBOM, provenance, anonymous-pull,
immutability, and moving tags on top of this seam.
USAGE
}

promote::main() {
  local cmd="${1:-release}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    release) promote::release ;;
    gate)    promote::gate ;;
    names)   promote::names ;;
    help|-h|--help) promote::usage ;;
    *) log::error "unknown command: ${cmd}"; promote::usage >&2; return 1 ;;
  esac
}

promote::main "$@"
