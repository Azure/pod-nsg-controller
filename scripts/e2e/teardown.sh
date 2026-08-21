#!/usr/bin/env bash
# =============================================================================
# teardown.sh - topology-aware, explicit-subscription teardown for the E2E
# pipeline (EPIC-010 / ITEM-040 cross-subscription cleanup + dual-subscription
# reaper seam / FILE-021).
#
# SCOPE (EPIC-010): this file implements the CROSS-SUBSCRIPTION (`xs`) cleanup
# that the `cleanup_xs` job and the release gate depend on, and the
# dual-subscription `reap` seam that e2e-reaper.yml calls. It:
#   * removes every recorded run role assignment BY ID (delegating to
#     setup-cross-sub-rbac.sh) so neither subscription retains a run-VM grant;
#   * deletes BOTH run resource groups in their OWNING subscription with an
#     EXPLICIT --subscription (Cluster A/shared ASGs in primary, Cluster B in
#     secondary) and VERIFIES each is not-found (XSUB-003/AC-027/TEST-020);
#   * forbids debug resource retention on release runs (RD-007);
#   * `reap` deletes EXPIRED run-tagged resource groups in each configured
#     subscription while leaving live runs untouched.
# There is NO `az account set`; every Azure call is explicit-subscription
# (RD-020). The broader same-subscription teardown/debug-retention nuances and
# the full reaper hardening (dangling-assignment sweep, dry-run report) are
# EPIC-005 (ITEM-016/017); this file provides the xs cleanup + reaper seam.
#
# Usage: teardown.sh <cleanup|reap|names|help>
# Required env (cleanup): PRIMARY_SUBSCRIPTION_ID, SECONDARY_SUBSCRIPTION_ID (distinct).
# Naming context: REPO, RUN_ID, RUN_ATTEMPT, DATE_UTC. Tool seam: AZ_BIN (az).
#
# Traceability: ITEM-040, FR-007, FR-017, XSUB-003, AC-008, AC-027, TEST-008,
# TEST-020, RD-007, RD-020, NFR-003, NFR-012.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"

# ---- inputs / configuration -------------------------------------------------
TOPOLOGY="${TOPOLOGY:-xs}"                                 # xs (the EPIC-010 target)
PRIMARY_SUBSCRIPTION_ID="${PRIMARY_SUBSCRIPTION_ID:-}"
SECONDARY_SUBSCRIPTION_ID="${SECONDARY_SUBSCRIPTION_ID:-}"
REGIONS="${REGIONS:-eastus2euap,centraluseuap}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
AZ_BIN="${AZ_BIN:-az}"
KEEP_RESOURCES="${KEEP_RESOURCES:-false}"                 # debug retention (non-release only)
RELEASE_REQUESTED="${RELEASE_REQUESTED:-false}"
DELETE_ATTEMPTS="${DELETE_ATTEMPTS:-3}"
DELETE_DELAY="${DELETE_DELAY:-10}"
VERIFY_ATTEMPTS="${VERIFY_ATTEMPTS:-30}"
VERIFY_DELAY="${VERIFY_DELAY:-20}"
REAP_MAX_AGE_DAYS="${REAP_MAX_AGE_DAYS:-1}"

lib::require_cmds jq date

# ---- explicit-subscription az wrapper (RD-020) ------------------------------
teardown::az() { local sub="$1"; shift; "$AZ_BIN" "$@" --subscription "$sub"; }

teardown::_require_xs() {
  local tcode
  tcode="$(naming::topology_code "$TOPOLOGY")" || log::die "invalid TOPOLOGY '${TOPOLOGY}'"
  [[ "$tcode" == "xs" ]] || log::die "teardown.sh cleanup serves the xs topology (got '${TOPOLOGY}'); ss teardown is EPIC-005"
  [[ -n "$PRIMARY_SUBSCRIPTION_ID" ]]   || log::die "PRIMARY_SUBSCRIPTION_ID is required"
  [[ -n "$SECONDARY_SUBSCRIPTION_ID" ]] || log::die "SECONDARY_SUBSCRIPTION_ID is required for xs"
  [[ "${PRIMARY_SUBSCRIPTION_ID,,}" != "${SECONDARY_SUBSCRIPTION_ID,,}" ]] \
    || log::die "xs requires two DISTINCT subscription IDs; primary equals secondary (FR-025)"
}

# Build the run RG list: RG_NAMES[i] in RG_SUBS[i] (owning subscription).
teardown::_derive() {
  [[ "${TD_DERIVED:-0}" == "1" ]] && return 0
  teardown::_require_xs
  naming::_load_context
  TCODE="$(naming::topology_code "$TOPOLOGY")"
  local base region rnorm rg role sub
  base="$(naming::base "$NAMING_PURPOSE" "$TCODE" "$DATE_UTC" "$RUN_SUFFIX")"
  RG_NAMES=(); RG_SUBS=(); RG_REGIONS=()
  local IFS=','; local -a raw=(); read -r -a raw <<< "$REGIONS"; unset IFS
  for region in "${raw[@]}"; do
    [[ -z "${region// }" ]] && continue
    rnorm="$(lib::validate_canary_region "$region")" || log::die "REGION '${region}' is not a supported canary region"
    rg="$(naming::rbase "$base" "$rnorm")"
    role="$(naming::subscription_role "$TCODE" "$rnorm")"
    case "$role" in
      primary)   sub="$PRIMARY_SUBSCRIPTION_ID" ;;
      secondary) sub="$SECONDARY_SUBSCRIPTION_ID" ;;
      *) log::die "unexpected subscription role '${role}'" ;;
    esac
    RG_NAMES+=("$rg"); RG_SUBS+=("$sub"); RG_REGIONS+=("$rnorm")
  done
  [[ ${#RG_NAMES[@]} -gt 0 ]] || log::die "no run resource groups derived"
  TD_DERIVED=1
}

# teardown::_delete_and_verify <sub> <rg> : delete (bounded) then poll until the
# RG is not-found (bounded). Prints 'true' when absent, 'false' otherwise.
teardown::_delete_and_verify() {
  local sub="$1" rg="$2" attempt=1 exists
  lib::retry "$DELETE_ATTEMPTS" "$DELETE_DELAY" -- \
    teardown::az "$sub" group delete --name "$rg" --yes --no-wait \
    || log::warn "group delete returned non-zero for ${rg} (may already be gone); verifying"
  while true; do
    exists="$(teardown::az "$sub" group exists --name "$rg" 2>/dev/null | tr -d '[:space:]')"
    if [[ "$exists" == "false" ]]; then printf 'true'; return 0; fi
    if (( attempt >= VERIFY_ATTEMPTS )); then
      log::error "resource group ${rg} still present in subscription ${sub} after ${attempt} verify attempt(s)"
      printf 'false'; return 1
    fi
    log::warn "awaiting deletion of ${rg} (subscription ${sub}, ${attempt}/${VERIFY_ATTEMPTS})"
    sleep "$VERIFY_DELAY"; attempt=$(( attempt + 1 ))
  done
}

# ---- cross-subscription cleanup (cleanup_xs) --------------------------------
teardown::cleanup() {
  # Debug retention: allowed for non-release diagnostics only; FORBIDDEN for
  # release runs so the gate never publishes while cloud state lingers (RD-007).
  if [[ "$KEEP_RESOURCES" == "true" ]]; then
    if [[ "$RELEASE_REQUESTED" == "true" ]]; then
      log::die "debug resource retention (KEEP_RESOURCES=true) is FORBIDDEN for release runs (RD-007/ITEM-016)"
    fi
    teardown::_require_xs
    [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
    log::warn "KEEP_RESOURCES=true: skipping xs teardown (debug retention; non-release only)"
    manifest::put_json "$MANIFEST_PATH" cleanup.xs \
      "$(jq -n '{status:"skipped", reason:"keep_resources", resource_groups:[]}')"
    return 0
  fi

  teardown::_derive
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  local rc=0

  # 1) Remove recorded run role assignments (verified) so neither subscription
  # retains a run-VM grant. Reuse the setup-cross-sub-rbac remove lifecycle; it
  # records .cleanup.xs.rbac. No-op (success) when nothing was recorded.
  log::info "removing recorded cross-subscription run role assignments"
  PRIMARY_SUBSCRIPTION_ID="$PRIMARY_SUBSCRIPTION_ID" SECONDARY_SUBSCRIPTION_ID="$SECONDARY_SUBSCRIPTION_ID" \
    AZ_BIN="$AZ_BIN" MANIFEST_PATH="$MANIFEST_PATH" TOPOLOGY=xs \
    bash "${HERE}/setup-cross-sub-rbac.sh" remove \
    || { log::error "cross-subscription assignment removal could not be fully verified"; rc=1; }

  # 2) Delete BOTH run RGs in their owning subscription; verify not-found.
  local i rg sub region absent rgs_json='[]'
  for i in "${!RG_NAMES[@]}"; do
    rg="${RG_NAMES[$i]}"; sub="${RG_SUBS[$i]}"; region="${RG_REGIONS[$i]}"
    log::info "deleting run RG ${rg} in subscription ${sub} (region ${region})"
    if absent="$(teardown::_delete_and_verify "$sub" "$rg")"; then :; else rc=1; fi
    rgs_json="$(jq -c -n --argjson a "$rgs_json" --arg rg "$rg" --arg sub "$sub" \
      --arg region "$region" --argjson absent "$absent" \
      '$a + [{resource_group:$rg, subscription:$sub, region:$region, absent:$absent}]')"
  done

  manifest::put_json "$MANIFEST_PATH" cleanup.xs.resource_groups "$rgs_json"
  manifest::put "$MANIFEST_PATH" cleanup.xs.status "$([[ $rc -eq 0 ]] && echo pass || echo fail)"
  gha::output cleanup_xs_status "$([[ $rc -eq 0 ]] && echo pass || echo fail)"
  if (( rc == 0 )); then
    log::info "cleanup_xs: both run RGs absent in their owning subscriptions; assignments removed (AC-008/AC-027)"
  else
    log::error "cleanup_xs: teardown could NOT be fully verified"
  fi
  return "$rc"
}

# ---- dual-subscription reaper seam (e2e-reaper.yml) --------------------------
# Deletes EXPIRED run-tagged resource groups in each configured subscription.
# "Expired" = validation-purpose == pnc-e2e AND managed-by == github-actions AND
# validation-date-utc older than the cutoff (REAP_MAX_AGE_DAYS). Live runs are
# left untouched. Deleting the RG cascades its RG-scoped role assignments.
teardown::reap() {
  local subs=() cutoff sub json name deleted=0 listed=0
  [[ -n "$PRIMARY_SUBSCRIPTION_ID" ]] && subs+=("$PRIMARY_SUBSCRIPTION_ID")
  [[ -n "$SECONDARY_SUBSCRIPTION_ID" && "${SECONDARY_SUBSCRIPTION_ID,,}" != "${PRIMARY_SUBSCRIPTION_ID,,}" ]] \
    && subs+=("$SECONDARY_SUBSCRIPTION_ID")
  [[ ${#subs[@]} -gt 0 ]] || log::die "reap requires at least one subscription (PRIMARY_/SECONDARY_SUBSCRIPTION_ID)"
  cutoff="$(date -u -d "${REAP_MAX_AGE_DAYS} days ago" +%Y%m%d 2>/dev/null)" \
    || cutoff="$(date -u -v-"${REAP_MAX_AGE_DAYS}"d +%Y%m%d 2>/dev/null)" \
    || log::die "unable to compute reap cutoff date"
  local purpose="${NAMING_PURPOSE:-pnc-e2e}"
  for sub in "${subs[@]}"; do
    listed=$(( listed + 1 ))
    log::info "reaping expired run RGs (purpose=${purpose}, older than ${cutoff}) in subscription ${sub}"
    json="$(teardown::az "$sub" group list -o json 2>/dev/null)" || { log::warn "group list failed for ${sub}; skipping"; continue; }
    while IFS= read -r name; do
      [[ -z "$name" ]] && continue
      log::info "reaping expired RG ${name} in subscription ${sub}"
      teardown::az "$sub" group delete --name "$name" --yes --no-wait \
        || log::warn "reap: group delete returned non-zero for ${name} (subscription ${sub})"
      deleted=$(( deleted + 1 ))
    done < <(printf '%s' "$json" | jq -r --arg p "$purpose" --arg cut "$cutoff" '
      .[]? | select(
        (.tags["validation-purpose"] == $p) and
        (.tags["managed-by"] == "github-actions") and
        ((.tags["validation-date-utc"] // "99999999") < $cut)
      ) | .name')
  done
  log::info "reaper complete: swept ${listed} subscription(s), deleted ${deleted} expired run RG(s)"
  gha::output reaped_count "$deleted"
}

teardown::names() {
  teardown::_derive
  local i
  printf 'topology=%s\n' "$TCODE"
  for i in "${!RG_NAMES[@]}"; do
    printf 'rg=%s subscription=%s region=%s\n' "${RG_NAMES[$i]}" "${RG_SUBS[$i]}" "${RG_REGIONS[$i]}"
  done
}

teardown::usage() {
  cat <<'USAGE'
Usage: teardown.sh <command>

Cross-subscription (xs) teardown + dual-subscription reaper seam (EPIC-010).

Commands:
  cleanup  Remove recorded run assignments + delete BOTH run RGs in their owning
           subscription and verify not-found (cleanup_xs). Default is 'cleanup'.
  reap     Delete EXPIRED run-tagged RGs in each configured subscription.
  names    Print the derived run RGs + owning subscriptions and exit.

Required env (cleanup): PRIMARY_SUBSCRIPTION_ID, SECONDARY_SUBSCRIPTION_ID (distinct).
KEEP_RESOURCES=true retains resources for debugging (non-release runs ONLY).
Naming context: REPO, RUN_ID, RUN_ATTEMPT, DATE_UTC. Tool seam: AZ_BIN (az).
USAGE
}

teardown::main() {
  local cmd="${1:-cleanup}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    cleanup) teardown::cleanup ;;
    reap)    teardown::reap ;;
    names)   teardown::names ;;
    help|-h|--help) teardown::usage ;;
    *) log::error "unknown command: ${cmd}"; teardown::usage >&2; return 1 ;;
  esac
}

teardown::main "$@"
