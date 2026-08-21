#!/usr/bin/env bash
# =============================================================================
# setup-cross-sub-rbac.sh - explicit-subscription, run-RG-scoped cross-subscription
# RBAC for the `xs` topology, generalizing scripts/poc/setup-cross-sub-rbac.sh
# into the pipeline (EPIC-010 / ITEM-038 / FILE-032).
#
# This is the cross-subscription specialization of cross-region-rbac.sh (EPIC-003):
# it REUSES that file's discover/grant/wait/assert core (sourced, not re-run) and
# adds only the two things unique to `xs`:
#   1. two DISTINCT, validated subscriptions are MANDATORY - Cluster A/shared ASGs
#      in the primary subscription, Cluster B in the secondary (FR-025/CON-002);
#   2. a `remove` lifecycle that deletes every recorded run assignment BY ID with
#      an explicit --subscription and verifies each is gone, so cleanup_xs can
#      guarantee no subscription retains a run-VM role assignment (ITEM-040,
#      XSUB-003, AC-027).
#
# The grants themselves are IDENTICAL to the same-subscription case: every run
# VM identity (both clusters) receives the minimal `Network Contributor` role on
# the PRIMARY run RG scope ONLY - never a subscription-scope grant (SEC-002).
# Cluster B's grant crosses the subscription boundary; Cluster A's is local. Every
# Azure call carries an explicit --subscription and `az account set` is NEVER used
# (RD-020).
#
# Tool seam (hermetic tests): AZ_BIN (az).
#
# Usage: setup-cross-sub-rbac.sh <setup|discover|grant|wait|assert-scope|remove|names>
# Required env: PRIMARY_SUBSCRIPTION_ID, SECONDARY_SUBSCRIPTION_ID (distinct).
# Naming context: REPO, RUN_ID, RUN_ATTEMPT, DATE_UTC.
#
# Traceability: ITEM-038, ITEM-040, FR-025, FR-026, NFR-003, NFR-012, SEC-002,
# RD-020, XSUB-003, AC-027, PRD Sections 4.5 / 8.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# `xs` is the only topology this script serves; pin it BEFORE sourcing the core
# so cross-region-rbac.sh derives the xs scope/subscription map. A caller may
# still override to prove the guard rejects a non-xs value.
TOPOLOGY="${TOPOLOGY:-xs}"

# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"
# Reuse the EPIC-003 discover/grant/wait/assert core (sourced; its main is
# guarded so it does NOT run here). This is the reusable interface promised in
# cross-region-rbac.sh's header.
# shellcheck source=scripts/e2e/cross-region-rbac.sh
source "${HERE}/cross-region-rbac.sh"

# ---- cross-subscription cleanup tuning (bounded, NFR-003) -------------------
REMOVE_ATTEMPTS="${REMOVE_ATTEMPTS:-30}"
REMOVE_DELAY="${REMOVE_DELAY:-10}"

# ---- xs input guard (two distinct subscriptions are mandatory, FR-025) ------
xsub::_require_xs() {
  local tcode
  tcode="$(naming::topology_code "$TOPOLOGY")" || log::die "invalid TOPOLOGY '${TOPOLOGY}'"
  [[ "$tcode" == "xs" ]] || log::die "setup-cross-sub-rbac.sh serves the xs topology only (got TOPOLOGY='${TOPOLOGY}'); use cross-region-rbac.sh for ss"
  [[ -n "$PRIMARY_SUBSCRIPTION_ID" ]]   || log::die "PRIMARY_SUBSCRIPTION_ID is required (Cluster A / shared ASGs)"
  [[ -n "$SECONDARY_SUBSCRIPTION_ID" ]] || log::die "SECONDARY_SUBSCRIPTION_ID is required for xs (Cluster B)"
  [[ "${PRIMARY_SUBSCRIPTION_ID,,}" != "${SECONDARY_SUBSCRIPTION_ID,,}" ]] \
    || log::die "xs requires two DISTINCT subscription IDs; primary equals secondary (FR-025)"
}

# ---- setup: discover -> grant -> wait -> assert (reuse the core) -------------
xsub::setup() {
  xsub::_require_xs
  # rbac::all == discover -> grant -> wait -> assert_scope -> assert_no_subscription_scope.
  rbac::all
  log::info "cross-subscription RBAC established (xs): run-RG-scoped grants only"
}

# ---- remove: delete every recorded assignment BY ID + verify gone -----------
# cleanup_xs assignment removal (ITEM-040 / XSUB-003 / AC-027). Reads the
# inventory recorded by grant (.rbac.xs.assignments[]), deletes each assignment
# by its ARM id with an explicit --subscription (the assignments all live in the
# PRIMARY subscription), then bounded-polls until each is gone. Fails CLOSED if
# any recorded assignment still resolves. Resilient to an empty inventory so the
# always() cleanup job never aborts when rbac never ran.
xsub::remove() {
  xsub::_require_xs
  [[ -f "$MANIFEST_PATH" ]] || { log::warn "manifest ${MANIFEST_PATH} absent; no recorded xs assignments to remove"; manifest::init "$MANIFEST_PATH"; }

  local n
  n="$(manifest::get "$MANIFEST_PATH" '(.rbac.xs.assignments // []) | length')"
  n="${n//[^0-9]/}"; n="${n:-0}"
  if (( n == 0 )); then
    log::info "no recorded xs run assignments to remove (nothing to clean up)"
    manifest::put_json "$MANIFEST_PATH" cleanup.xs.rbac \
      "$(jq -n '{removed:0, verified:true, assignment_ids:[]}')"
    return 0
  fi

  local -a AIDS PIDS SCOPES
  mapfile -t AIDS   < <(manifest::get "$MANIFEST_PATH" '.rbac.xs.assignments[].assignment_id')
  mapfile -t PIDS   < <(manifest::get "$MANIFEST_PATH" '.rbac.xs.assignments[].principal_id')
  mapfile -t SCOPES < <(manifest::get "$MANIFEST_PATH" '.rbac.xs.assignments[].scope')

  local i aid removed=0
  for i in "${!AIDS[@]}"; do
    aid="${AIDS[$i]}"
    [[ -n "$aid" && "$aid" != "null" ]] || { log::warn "skipping empty assignment id at index ${i}"; continue; }
    log::info "deleting run assignment ${aid} (subscription ${PRIMARY_SUBSCRIPTION_ID})"
    # Idempotent: a NotFound delete is success (already gone). Bounded retry
    # covers the flaky control plane (NFR-003).
    lib::retry "$REMOVE_ATTEMPTS" "$REMOVE_DELAY" -- \
      rbac::az "$PRIMARY_SUBSCRIPTION_ID" role assignment delete --ids "$aid" \
      || log::warn "delete returned non-zero for ${aid} (treating as already-removed; verified below)"
    removed=$(( removed + 1 ))
  done

  # Verify each recorded (principal, scope) no longer carries the role. Fail
  # CLOSED so a stuck assignment blocks the release gate (XSUB-003 / AC-027).
  local rc=0 pid scope attempt delay left
  for i in "${!AIDS[@]}"; do
    pid="${PIDS[$i]}"; scope="${SCOPES[$i]}"
    [[ -n "$pid" && -n "$scope" ]] || continue
    attempt=1; delay="$REMOVE_DELAY"
    while true; do
      left="$(rbac::az "$PRIMARY_SUBSCRIPTION_ID" role assignment list --assignee "$pid" --scope "$scope" -o json 2>/dev/null \
              | jq -r --arg r "$ROLE" '[.[] | select(.roleDefinitionName == $r)] | length' 2>/dev/null)"
      left="${left//[^0-9]/}"; left="${left:-0}"
      (( left == 0 )) && break
      if (( attempt >= REMOVE_ATTEMPTS )); then
        log::error "assignment for principal ${pid} on ${scope} still present after ${attempt} attempt(s) (cleanup NOT verified)"
        rc=1; break
      fi
      log::warn "awaiting assignment removal (principal ${pid}, ${attempt}/${REMOVE_ATTEMPTS})"
      sleep "$delay"; attempt=$(( attempt + 1 ))
    done
  done

  manifest::put_json "$MANIFEST_PATH" cleanup.xs.rbac \
    "$(jq -n --argjson removed "$removed" --argjson verified "$([[ $rc -eq 0 ]] && echo true || echo false)" \
        --argjson ids "$(printf '%s\n' "${AIDS[@]}" | jq -R . | jq -s -c 'map(select(length>0 and . != "null"))')" \
        '{removed:$removed, verified:$verified, assignment_ids:$ids}')"
  gha::output rbac_removed "$removed"
  if (( rc == 0 )); then
    log::info "removed and verified ${removed} run assignment(s); no subscription retains a run-VM grant (AC-027)"
  else
    log::error "cross-subscription assignment removal could NOT be fully verified"
  fi
  return "$rc"
}

xsub::names() {
  xsub::_require_xs
  rbac::_derive
  printf 'topology=xs\nprimary_subscription=%s\nsecondary_subscription=%s\ntarget_scope=%s\n' \
    "$PRIMARY_SUBSCRIPTION_ID" "$SECONDARY_SUBSCRIPTION_ID" "$TARGET_SCOPE"
}

xsub::usage() {
  cat <<'USAGE'
Usage: setup-cross-sub-rbac.sh <command>

Explicit-subscription, run-RG-scoped cross-subscription (xs) RBAC. Reuses the
cross-region-rbac.sh discover/grant/wait/assert core and adds distinct-subscription
enforcement + a verified removal lifecycle for cleanup_xs.

Commands:
  setup         discover -> grant -> wait -> assert-scope -> assert no subscription-scope (default)
  discover      Print both clusters' node-identity principals (A primary, B secondary)
  grant         Create RG-scoped Network Contributor grants on the primary run RG; record IDs
  wait          Bounded wait for RBAC propagation
  assert-scope  Fail if any recorded grant is not run-RG-scoped
  remove        Delete every recorded run assignment BY ID and verify it is gone (cleanup_xs)
  names         Print derived subscriptions + target scope and exit

Required env: PRIMARY_SUBSCRIPTION_ID, SECONDARY_SUBSCRIPTION_ID (distinct).
Naming context: REPO, RUN_ID, RUN_ATTEMPT, DATE_UTC. Tool seam: AZ_BIN (az).
USAGE
}

xsub::main() {
  local cmd="${1:-setup}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    setup)        xsub::setup ;;
    discover)     xsub::_require_xs; rbac::discover; rbac::print_principals ;;
    grant)        xsub::_require_xs; rbac::grant ;;
    wait)         xsub::_require_xs; rbac::wait ;;
    assert-scope) xsub::_require_xs; rbac::assert_scope ;;
    remove)       xsub::remove ;;
    names)        xsub::names ;;
    help|-h|--help) xsub::usage ;;
    *) log::error "unknown command: ${cmd}"; xsub::usage >&2; return 1 ;;
  esac
}

xsub::main "$@"
