#!/usr/bin/env bash
# Delete/verify per-run staging candidate tags and ACR pull tokens after every
# topology and release terminal path. Also provides the scheduled orphan sweep.
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"

STAGING_ACR="${STAGING_ACR:-}"
STAGING_ACR_SUBSCRIPTION_ID="${STAGING_ACR_SUBSCRIPTION_ID:-}"
CONTROLLER_STAGING_REPO="${CONTROLLER_STAGING_REPO:-candidate/pod-nsg-controller}"
CNI_STAGING_REPO="${CNI_STAGING_REPO:-candidate/pod-nsg-cni-transparent-tunnel}"
CONTROLLER_STAGING_TAG="${CONTROLLER_STAGING_TAG:-}"
CNI_STAGING_TAG="${CNI_STAGING_TAG:-}"
VALIDATION_TOPOLOGIES="${VALIDATION_TOPOLOGIES:-ss,xs}"
REGIONS="${REGIONS:-eastus2euap,centraluseuap}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
REAP_MAX_AGE_DAYS="${REAP_MAX_AGE_DAYS:-1}"
VERIFY_ATTEMPTS="${VERIFY_ATTEMPTS:-5}"
VERIFY_DELAY="${VERIFY_DELAY:-2}"
AZ_BIN="${AZ_BIN:-az}"

lib::require_cmds jq date

artifact_cleanup::_require_registry() {
  [[ -n "$STAGING_ACR" ]] || log::die "STAGING_ACR is required"
  [[ -n "$STAGING_ACR_SUBSCRIPTION_ID" ]] \
    || log::die "STAGING_ACR_SUBSCRIPTION_ID is required for explicit registry context"
  ACR_NAME="${STAGING_ACR%%.*}"
}

artifact_cleanup::az() {
  "$AZ_BIN" "$@" --subscription "$STAGING_ACR_SUBSCRIPTION_ID"
}

artifact_cleanup::_delete_tag() {
  local repo="$1" tag="$2" attempt=1 out
  artifact_cleanup::az acr repository delete --name "$ACR_NAME" \
    --image "${repo}:${tag}" --yes >/dev/null 2>&1 \
    || log::warn "candidate tag ${repo}:${tag} was already absent or delete returned non-zero"
  while true; do
    if out="$(artifact_cleanup::az acr repository show --name "$ACR_NAME" \
      --image "${repo}:${tag}" --query digest -o tsv 2>&1)"; then
      (( attempt >= VERIFY_ATTEMPTS )) \
        && { log::error "candidate tag still exists after deletion: ${repo}:${tag}"; return 1; }
      sleep "$VERIFY_DELAY"; attempt=$(( attempt + 1 )); continue
    fi
    grep -Eqi 'manifest.?not.?found|manifest unknown|not found|does not exist' <<<"$out" \
      && return 0
    log::error "could not verify candidate tag absence for ${repo}:${tag}: ${out:-unknown az error}"
    return 1
  done
}

artifact_cleanup::_delete_token() {
  local token="$1" attempt=1 out
  artifact_cleanup::az acr token delete --registry "$ACR_NAME" \
    --name "$token" --yes >/dev/null 2>&1 \
    || log::warn "ACR token ${token} was already absent or delete returned non-zero"
  while true; do
    if out="$(artifact_cleanup::az acr token show --registry "$ACR_NAME" \
      --name "$token" -o none 2>&1)"; then
      (( attempt >= VERIFY_ATTEMPTS )) \
        && { log::error "ACR token still exists after deletion: ${token}"; return 1; }
      sleep "$VERIFY_DELAY"; attempt=$(( attempt + 1 )); continue
    fi
    grep -Eqi 'resource.?not.?found|not found|does not exist' <<<"$out" && return 0
    log::error "could not verify ACR token absence for ${token}: ${out:-unknown az error}"
    return 1
  done
}

artifact_cleanup::_run_tokens() {
  naming::_load_context
  local topology tcode region pairs token
  local -A seen=()
  IFS=',' read -ra topologies <<<"$VALIDATION_TOPOLOGIES"
  IFS=',' read -ra regions <<<"$REGIONS"
  for topology in "${topologies[@]}"; do
    topology="${topology//[[:space:]]/}"
    [[ -n "$topology" ]] || continue
    tcode="$(naming::topology_code "$topology")" \
      || log::die "invalid topology '${topology}' in VALIDATION_TOPOLOGIES"
    for region in "${regions[@]}"; do
      region="${region//[[:space:]]/}"
      [[ -n "$region" ]] || continue
      pairs="$(naming::_region_pairs "$tcode" "$region")" \
        || log::die "could not derive token name for ${tcode}/${region}"
      token="$(printf '%s\n' "$pairs" | sed -n 's/^resource_group=//p')-pull"
      [[ -n "${seen[$token]:-}" ]] && continue
      seen["$token"]=1
      printf '%s\n' "$token"
    done
  done
}

artifact_cleanup::cleanup() {
  artifact_cleanup::_require_registry
  [[ -n "$CONTROLLER_STAGING_TAG" ]] || log::die "CONTROLLER_STAGING_TAG is required"
  [[ -n "$CNI_STAGING_TAG" ]] || log::die "CNI_STAGING_TAG is required"
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"

  local rc=0 token token_list absent tags='[]' tokens='[]'
  if artifact_cleanup::_delete_tag "$CONTROLLER_STAGING_REPO" "$CONTROLLER_STAGING_TAG"; then
    absent=true
  else
    absent=false; rc=1
  fi
  tags="$(jq -nc --arg r "$CONTROLLER_STAGING_REPO" --arg t "$CONTROLLER_STAGING_TAG" \
    --argjson absent "$absent" '[{repository:$r,tag:$t,absent:$absent}]')"
  if artifact_cleanup::_delete_tag "$CNI_STAGING_REPO" "$CNI_STAGING_TAG"; then
    absent=true
  else
    absent=false; rc=1
  fi
  tags="$(jq -nc --argjson a "$tags" --arg r "$CNI_STAGING_REPO" --arg t "$CNI_STAGING_TAG" \
    --argjson absent "$absent" '$a + [{repository:$r,tag:$t,absent:$absent}]')"

  token_list="$(artifact_cleanup::_run_tokens)" \
    || log::die "could not derive the complete per-run token inventory"
  while IFS= read -r token; do
    [[ -n "$token" ]] || continue
    if artifact_cleanup::_delete_token "$token"; then absent=true; else absent=false; rc=1; fi
    tokens="$(jq -nc --argjson a "$tokens" --arg n "$token" --argjson absent "$absent" \
      '$a + [{name:$n,absent:$absent}]')"
  done <<<"$token_list"

  manifest::put_json "$MANIFEST_PATH" cleanup.artifacts "$(jq -n \
    --arg status "$([[ $rc -eq 0 ]] && echo pass || echo fail)" \
    --arg registry "$STAGING_ACR" --argjson tags "$tags" --argjson tokens "$tokens" \
    '{status:$status,registry:$registry,candidate_tags:$tags,tokens:$tokens}')"
  gha::output artifact_cleanup_status "$([[ $rc -eq 0 ]] && echo pass || echo fail)"
  return "$rc"
}

artifact_cleanup::reap() {
  artifact_cleanup::_require_registry
  local cutoff repo json tag token_json token deleted_tags=0 deleted_tokens=0
  cutoff="$(date -u -d "${REAP_MAX_AGE_DAYS} days ago" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)" \
    || cutoff="$(date -u -v-"${REAP_MAX_AGE_DAYS}"d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)" \
    || log::die "unable to compute artifact reap cutoff"
  for repo in "$CONTROLLER_STAGING_REPO" "$CNI_STAGING_REPO"; do
    json="$(artifact_cleanup::az acr repository show-tags --name "$ACR_NAME" \
      --repository "$repo" --detail -o json 2>/dev/null)" \
      || { log::warn "could not list candidate tags for ${repo}"; continue; }
    while IFS= read -r tag; do
      [[ -n "$tag" ]] || continue
      artifact_cleanup::_delete_tag "$repo" "$tag"
      deleted_tags=$(( deleted_tags + 1 ))
    done < <(printf '%s' "$json" | jq -r --arg cutoff "$cutoff" '
      .[]? | select(
        (.name // "" | test("^run-[0-9]{8}-[a-z0-9]{6}$")) and
        ((.lastUpdateTime // .createdTime // "9999") < $cutoff)
      ) | .name')
  done

  token_json="$(artifact_cleanup::az acr token list --registry "$ACR_NAME" -o json 2>/dev/null)" \
    || log::die "could not list staging ACR tokens for orphan sweep"
  while IFS= read -r token; do
    [[ -n "$token" ]] || continue
    artifact_cleanup::_delete_token "$token"
    deleted_tokens=$(( deleted_tokens + 1 ))
  done < <(printf '%s' "$token_json" | jq -r --arg cutoff "$cutoff" '
    .[]? | select(
      (.name // "" | test("^pnc-e2e-(ss|xs)-[0-9]{8}-[a-z0-9]{6}-[a-z0-9-]+-pull$")) and
      ((.creationDate // .createdTime // "9999") < $cutoff)
    ) | .name')
  gha::output reaped_candidate_count "$deleted_tags"
  gha::output reaped_token_count "$deleted_tokens"
}

case "${1:-cleanup}" in
  cleanup) artifact_cleanup::cleanup ;;
  reap) artifact_cleanup::reap ;;
  *) log::die "usage: cleanup-artifacts.sh <cleanup|reap>" ;;
esac
