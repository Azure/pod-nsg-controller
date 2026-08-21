#!/usr/bin/env bash
# =============================================================================
# meta.sh - compute deterministic run metadata for the E2E pipeline `meta` job.
#
# Pure computation (no cloud access). Captures DATE_UTC once, derives the run
# suffix and every ss/xs name map via naming.sh, validates release/topology and
# cross-subscription input constraints, and writes the run-manifest foundation
# plus GitHub Actions step outputs and a job summary.
#
# Inputs (environment, all optional with sensible defaults / GitHub fallbacks):
#   REPO RUN_ID RUN_ATTEMPT GIT_SHA GIT_REF                (naming context)
#   INPUT_TOPOLOGIES  (default "ss,xs")   INPUT_REGIONS (default canary pair)
#   INPUT_RELEASE (true|false)            INPUT_RELEASE_VERSION
#   INPUT_RUN_FULL_VALIDATION (true|false) INPUT_MOVING_TAGS
#   INPUT_KEEP_RESOURCES_ON_FAILURE (true|false)
#   PRIMARY_SUBSCRIPTION_ID  SECONDARY_SUBSCRIPTION_ID
#   EVENT_NAME REF_TYPE REF_NAME          (release-trigger detection)
#   MANIFEST_PATH     (default ./run-manifest.json)
#
# Traceability: ITEM-002, FR-011, FR-012, FR-015, FR-025, CON-001, CON-009,
# NFR-006, PRD Sections 3.4 / 9.1.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"
NAMING_SH="${HERE}/naming.sh"

lib::require_cmds jq bash sha256sum sed tr date sort wc

# ---- inputs -----------------------------------------------------------------
INPUT_TOPOLOGIES="${INPUT_TOPOLOGIES:-ss,xs}"
INPUT_REGIONS="${INPUT_REGIONS:-eastus2euap,centraluseuap}"
INPUT_RELEASE="${INPUT_RELEASE:-false}"
INPUT_RELEASE_VERSION="${INPUT_RELEASE_VERSION:-}"
INPUT_RUN_FULL_VALIDATION="${INPUT_RUN_FULL_VALIDATION:-false}"
INPUT_MOVING_TAGS="${INPUT_MOVING_TAGS:-}"
INPUT_KEEP_RESOURCES_ON_FAILURE="${INPUT_KEEP_RESOURCES_ON_FAILURE:-${INPUT_KEEP_RESOURCES:-false}}"
PRIMARY_SUBSCRIPTION_ID="${PRIMARY_SUBSCRIPTION_ID:-}"
SECONDARY_SUBSCRIPTION_ID="${SECONDARY_SUBSCRIPTION_ID:-}"
EVENT_NAME="${EVENT_NAME:-${GITHUB_EVENT_NAME:-}}"
REF_TYPE="${REF_TYPE:-}"
REF_NAME="${REF_NAME:-}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"

# Capture DATE_UTC once and propagate it (stable across midnight, NFR-006).
export DATE_UTC="${DATE_UTC:-$(date -u +%Y%m%d)}"

for input_name in INPUT_RELEASE INPUT_RUN_FULL_VALIDATION INPUT_KEEP_RESOURCES_ON_FAILURE; do
  input_value="${!input_name}"
  [[ "$input_value" == "true" || "$input_value" == "false" ]] \
    || log::die "${input_name} must be true or false (got '${input_value}')"
done

# ---- parse + validate validation_topologies (CON-009) -----------------------
has_ss=false; has_xs=false
IFS=',' read -r -a _raw_topos <<< "$INPUT_TOPOLOGIES"
for t in "${_raw_topos[@]}"; do
  t="$(printf '%s' "$t" | tr -d '[:space:]' | tr '[:upper:]' '[:lower:]')"
  [[ -z "$t" ]] && continue
  case "$t" in
    ss|same-subscription)  has_ss=true ;;
    xs|cross-subscription) has_xs=true ;;
    *) log::die "invalid validation_topology '${t}' (allowed: ss, xs)" ;;
  esac
done
effective_topos=()
if $has_ss; then effective_topos+=("ss"); fi
if $has_xs; then effective_topos+=("xs"); fi
[[ ${#effective_topos[@]} -gt 0 ]] || log::die "validation_topologies is empty"

# ---- validate regions (canary-only, CON-001) --------------------------------
regions=()
IFS=',' read -r -a _raw_regions <<< "$INPUT_REGIONS"
for r in "${_raw_regions[@]}"; do
  r="$(printf '%s' "$r" | tr -d '[:space:]' | tr '[:upper:]' '[:lower:]')"
  [[ -z "$r" ]] && continue
  case "$r" in
    eastus2euap|centraluseuap) regions+=("$r") ;;
    *) log::die "region '${r}' is not a supported canary region (allowed: eastus2euap, centraluseuap)" ;;
  esac
done
[[ ${#regions[@]} -gt 0 ]] || log::die "regions is empty"
[[ "$(printf '%s\n' "${regions[@]}" | sort -u | wc -l | tr -d ' ')" == "${#regions[@]}" ]] \
  || log::die "regions contains duplicate entries"
[[ " ${regions[*]} " == *" eastus2euap "* && " ${regions[*]} " == *" centraluseuap "* ]] \
  || log::die "the two-cluster topology requires both canary regions: eastus2euap, centraluseuap"

# ---- determine release intent ----------------------------------------------
release_requested=false
if [[ "$INPUT_RELEASE" == "true" ]] \
   || [[ "$EVENT_NAME" == "release" ]] \
   || { [[ "$REF_TYPE" == "tag" ]] && [[ "$REF_NAME" == v* ]]; }; then
  release_requested=true
fi

# A published tag supplies the version when the input omits it.
if [[ -z "$INPUT_RELEASE_VERSION" && "$REF_TYPE" == "tag" && "$REF_NAME" == v* ]]; then
  INPUT_RELEASE_VERSION="$REF_NAME"
fi

# ---- release gating (CON-009 / FR-008 / FR-012) -----------------------------
if [[ "$release_requested" == "true" || "$INPUT_RUN_FULL_VALIDATION" == "true" ]]; then
  has_ss=true
  has_xs=true
  effective_topos=("ss" "xs")
fi
if [[ "$release_requested" == "true" ]]; then
  if [[ -z "$INPUT_RELEASE_VERSION" ]]; then
    log::die "release requires a release_version (semver, e.g. v1.2.3)"
  fi
  [[ "$INPUT_KEEP_RESOURCES_ON_FAILURE" != "true" ]] \
    || log::die "release forbids keep_resources_on_failure=true (cleanup is a mandatory release gate)"
fi
if [[ -n "$INPUT_RELEASE_VERSION" ]] \
   && ! [[ "$INPUT_RELEASE_VERSION" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  log::die "invalid release_version '${INPUT_RELEASE_VERSION}' (want semver, e.g. v1.2.3)"
fi

moving_tags=()
if [[ -n "$INPUT_MOVING_TAGS" ]]; then
  [[ "$release_requested" == "true" ]] \
    || log::die "moving_tags may be set only for a release run"
  version="${INPUT_RELEASE_VERSION#v}"
  IFS=. read -r major minor _ <<<"$version"
  allowed_tags=",latest,v${major},v${major}.${minor},"
  IFS=',' read -ra raw_moving_tags <<<"$INPUT_MOVING_TAGS"
  for tag in "${raw_moving_tags[@]}"; do
    tag="$(printf '%s' "$tag" | tr -d '[:space:]')"
    [[ -n "$tag" && "$allowed_tags" == *",$tag,"* ]] \
      || log::die "moving tag '${tag}' is not allowed; choose from latest,v${major},v${major}.${minor}"
    [[ " ${moving_tags[*]} " != *" ${tag} "* ]] \
      || log::die "moving_tags contains duplicate '${tag}'"
    moving_tags+=("$tag")
  done
fi
moving_tags_csv="$(IFS=,; printf '%s' "${moving_tags[*]}")"

# ---- cross-subscription ID checks (FR-025) ----------------------------------
if $has_xs; then
  if [[ -n "$PRIMARY_SUBSCRIPTION_ID" && -n "$SECONDARY_SUBSCRIPTION_ID" \
        && "${PRIMARY_SUBSCRIPTION_ID,,}" == "${SECONDARY_SUBSCRIPTION_ID,,}" ]]; then
    log::die "cross-subscription (xs) requires two DISTINCT subscription IDs; primary equals secondary"
  fi
  if [[ "$release_requested" == "true" && -z "$SECONDARY_SUBSCRIPTION_ID" ]]; then
    log::die "release with the xs topology requires secondary_subscription_id to be set"
  fi
fi

# ---- emit the run-manifest foundation + validate every name (CON-006) -------
log::info "generating run-manifest -> ${MANIFEST_PATH}"
"$NAMING_SH" emit-manifest > "$MANIFEST_PATH"
"$NAMING_SH" self-check

run_suffix="$(manifest::get "$MANIFEST_PATH" '.run.run_suffix')"
git_sha="$(manifest::get "$MANIFEST_PATH" '.run.git_sha')"
ctrl_repo="$(manifest::get "$MANIFEST_PATH" '.names.staging.controller_repo')"
ctrl_tag="$(manifest::get "$MANIFEST_PATH" '.names.staging.controller_tag')"
cni_repo="$(manifest::get "$MANIFEST_PATH" '.names.staging.cni_repo')"
cni_tag="$(manifest::get "$MANIFEST_PATH" '.names.staging.cni_tag')"

# ---- merge input-derived metadata into the manifest -------------------------
topos_json="$(printf '%s\n' "${effective_topos[@]}" | jq -R . | jq -s -c .)"
regions_json="$(printf '%s\n' "${regions[@]}" | jq -R . | jq -s -c .)"
manifest::put_json "$MANIFEST_PATH" run.validation_topologies "$topos_json"
manifest::put_json "$MANIFEST_PATH" run.regions "$regions_json"
manifest::put_json "$MANIFEST_PATH" release \
  "$(jq -n --argjson req "$release_requested" --arg ver "$INPUT_RELEASE_VERSION" \
       --arg tags "$moving_tags_csv" \
       '{requested: $req, version: $ver,
         moving_tags: ($tags | split(",") | map(select(length > 0)))}')"
manifest::put "$MANIFEST_PATH" run.full_validation "$INPUT_RUN_FULL_VALIDATION"
manifest::put "$MANIFEST_PATH" run.keep_resources_on_failure "$INPUT_KEEP_RESOURCES_ON_FAILURE"
# Subscription IDs are non-sensitive identifiers (PRD Section 8) recorded as
# manifest metadata to prove the xs topology uses two distinct subscriptions.
manifest::put_json "$MANIFEST_PATH" subscriptions \
  "$(jq -n --arg p "$PRIMARY_SUBSCRIPTION_ID" --arg s "$SECONDARY_SUBSCRIPTION_ID" \
       '{primary: {id: $p, configured: (($p|length) > 0)},
         secondary: {id: $s, configured: (($s|length) > 0)}}')"

# ---- step outputs (consumed by downstream jobs in later epics) --------------
topos_csv="$(IFS=,; printf '%s' "${effective_topos[*]}")"
regions_csv="$(IFS=,; printf '%s' "${regions[*]}")"
# JSON array form of the canary regions, consumed as a GitHub Actions matrix by
# the per-region `provision` job (EPIC-003 / ITEM-010, PAT-002).
regions_json="$(lib::csv_to_json_array "$regions_csv")"
gha::output date_utc "$DATE_UTC"
gha::output run_suffix "$run_suffix"
gha::output git_sha "$git_sha"
gha::output validation_topologies "$topos_csv"
gha::output regions "$regions_csv"
gha::output regions_json "$regions_json"
gha::output release_requested "$release_requested"
gha::output release_version "$INPUT_RELEASE_VERSION"
gha::output run_full_validation "$INPUT_RUN_FULL_VALIDATION"
gha::output moving_tags "$moving_tags_csv"
gha::output keep_resources_on_failure "$INPUT_KEEP_RESOURCES_ON_FAILURE"
gha::output controller_staging_repo "$ctrl_repo"
gha::output controller_staging_tag "$ctrl_tag"
gha::output cni_staging_repo "$cni_repo"
gha::output cni_staging_tag "$cni_tag"
gha::output manifest_path "$MANIFEST_PATH"
gha::export DATE_UTC "$DATE_UTC"

# ---- job summary (FR-015; no credentials/tokens) ----------------------------
role_rows="$(jq -r '
  ["ss","xs"][] as $tc
  | (.names[$tc] // {}) | to_entries[]
  | select(((.value|type) == "object") and (.value|has("subscription_role")))
  | "| \($tc) | \(.key) | \(.value.cluster_id) | \(.value.namespace) | \(.value.subscription_role) |"
' "$MANIFEST_PATH")"

gha::summary "## E2E run metadata (\`meta\`)"
gha::summary ""
gha::summary "| Field | Value |"
gha::summary "|---|---|"
gha::summary "| Purpose | pnc-e2e |"
gha::summary "| DATE_UTC | ${DATE_UTC} |"
gha::summary "| RUN_SUFFIX | ${run_suffix} |"
gha::summary "| GIT_SHA | ${git_sha} |"
gha::summary "| Validation topologies | ${topos_csv} |"
gha::summary "| Regions | ${regions_csv} |"
gha::summary "| Release requested | ${release_requested} |"
gha::summary "| Release version | ${INPUT_RELEASE_VERSION:-<none>} |"
gha::summary "| Full validation | ${INPUT_RUN_FULL_VALIDATION} |"
gha::summary "| Moving tags | ${moving_tags_csv:-<none>} |"
gha::summary "| Keep resources on upstream failure | ${INPUT_KEEP_RESOURCES_ON_FAILURE} |"
gha::summary "| Controller candidate | \`${ctrl_repo}:${ctrl_tag}\` |"
gha::summary "| CNI candidate | \`${cni_repo}:${cni_tag}\` |"
gha::summary ""
gha::summary "### Subscription-role map"
gha::summary "| Topology | Region | Cluster | Namespace | Role |"
gha::summary "|---|---|---|---|---|"
gha::summary "${role_rows}"

log::info "meta complete: topologies=[${topos_csv}] regions=[${regions_csv}] release=${release_requested}"
