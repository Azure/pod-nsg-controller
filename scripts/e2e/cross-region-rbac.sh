#!/usr/bin/env bash
# =============================================================================
# cross-region-rbac.sh - run-scoped, explicit-subscription RBAC that grants both
# clusters' node identities the minimal role on the topology's PRIMARY run
# resource group scope ONLY, generalizing scripts/poc/setup-cross-sub-rbac.sh
# (RD-012 / FILE-017).
#
# EPIC-003 / ITEM-009:
#   * discovers every cluster's node VM managed-identity principals using each
#     cluster's OWNING subscription explicitly;
#   * grants `Network Contributor` (minimal, RG-scoped) on the primary run RG in
#     the primary subscription - never a subscription-scope grant (SEC-002);
#   * waits for propagation with a bounded retry that fails closed (NFR-003);
#   * records assignment IDs into the run manifest for verified teardown
#     (XSUB-003 inventory / RISK-011);
#   * asserts no subscription-scope grant references a run principal.
#
# In the `ss` topology both clusters live in the primary subscription; the same
# functions accept explicit subscription context for the `xs` topology, so
# EPIC-010's setup-cross-sub-rbac.sh reuses this core (the reusable interface
# required by EPIC-003). Cross-subscription-specific bootstrap (separate OIDC
# identities, IMDS/ARM preflight) stays in EPIC-010.
#
# Tool seam (override for hermetic tests): AZ_BIN (az).
#
# Usage: cross-region-rbac.sh <all|discover|grant|wait|assert-scope>
#
# Traceability: ITEM-009, FR-026, NFR-003, NFR-012, SEC-002, RD-020, XSUB-003,
# RISK-011, RISK-013, PRD Sections 3.4 / 4.5.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"

# ---- inputs / configuration -------------------------------------------------
TOPOLOGY="${TOPOLOGY:-}"                                  # ss|xs (REQUIRED)
REGIONS="${REGIONS:-eastus2euap,centraluseuap}"           # clusters to grant
PRIMARY_SUBSCRIPTION_ID="${PRIMARY_SUBSCRIPTION_ID:-}"    # primary sub (REQUIRED)
SECONDARY_SUBSCRIPTION_ID="${SECONDARY_SUBSCRIPTION_ID:-}" # required for xs
ROLE="${ROLE:-Network Contributor}"                      # minimal role (SEC-002)
PRINCIPAL_TYPE="${PRINCIPAL_TYPE:-ServicePrincipal}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
AZ_BIN="${AZ_BIN:-az}"
PROP_ATTEMPTS="${PROP_ATTEMPTS:-30}"
PROP_DELAY="${PROP_DELAY:-10}"
PROP_DELAY_MAX="${PROP_DELAY_MAX:-30}"   # cap per-attempt backoff (bounded total wait)

lib::require_cmds jq

# ---- explicit-subscription az wrapper (RD-020) ------------------------------
# rbac::az <subscription> <az-args...> : every call carries --subscription so
# nothing depends on mutable `az account` context.
rbac::az() { local sub="$1"; shift; "$AZ_BIN" "$@" --subscription "$sub"; }

rbac::_sub_for_role() {
  case "$1" in
    primary)   printf '%s' "$PRIMARY_SUBSCRIPTION_ID" ;;
    secondary) printf '%s' "$SECONDARY_SUBSCRIPTION_ID" ;;
    *) return 1 ;;
  esac
}

rbac::_require_inputs() {
  [[ -n "$PRIMARY_SUBSCRIPTION_ID" ]] || log::die "PRIMARY_SUBSCRIPTION_ID is required (explicit primary subscription; RD-020)"
  [[ -n "$TOPOLOGY" ]]                || log::die "TOPOLOGY is required (ss|xs)"
}

rbac::_derive() {
  [[ "${RBAC_DERIVED:-0}" == "1" ]] && return 0
  rbac::_require_inputs
  naming::_load_context
  TCODE="$(naming::topology_code "$TOPOLOGY")" || log::die "invalid TOPOLOGY '${TOPOLOGY}' (want ss|xs)"
  REGIONS_ARR=()
  local r rnorm
  IFS=',' read -r -a _raw_regions <<< "$REGIONS"
  for r in "${_raw_regions[@]}"; do
    [[ -z "${r// }" ]] && continue
    rnorm="$(lib::validate_canary_region "$r")" || log::die "REGION '${r}' is not a supported canary region"
    REGIONS_ARR+=("$rnorm")
  done
  [[ ${#REGIONS_ARR[@]} -gt 0 ]] || log::die "REGIONS is empty"
  # The single target scope is the PRIMARY region RG; it must be provisioned,
  # so the primary region must be part of this run (else grants target a
  # non-existent scope, N4/RISK-011).
  local has_primary=0
  for r in "${REGIONS_ARR[@]}"; do [[ "$r" == "$NAMING_PRIMARY_REGION" ]] && has_primary=1; done
  (( has_primary == 1 )) || log::die "REGIONS must include the primary region '${NAMING_PRIMARY_REGION}' (it owns the shared-ASG RG that every grant is scoped to)"
  # Fail fast when a cluster needs the secondary subscription but it is unset.
  for r in "${REGIONS_ARR[@]}"; do
    if [[ "$(naming::subscription_role "$TCODE" "$r")" == "secondary" && -z "$SECONDARY_SUBSCRIPTION_ID" ]]; then
      log::die "topology xs region ${r} requires SECONDARY_SUBSCRIPTION_ID"
    fi
  done
  # The single target scope: the topology's PRIMARY region RG, in the primary
  # subscription. This is the ONLY scope any runtime grant is created at.
  local base primary_rg
  base="$(naming::base "$NAMING_PURPOSE" "$TCODE" "$DATE_UTC" "$RUN_SUFFIX")"
  primary_rg="$(naming::rbase "$base" "$NAMING_PRIMARY_REGION")"
  TARGET_SCOPE="/subscriptions/${PRIMARY_SUBSCRIPTION_ID}/resourceGroups/${primary_rg}"
  if [[ ! "$TARGET_SCOPE" =~ ^/subscriptions/[^/]+/resourceGroups/[^/]+$ ]]; then
    log::die "refusing to proceed: computed target scope is not RG-scoped: ${TARGET_SCOPE}"
  fi
  RBAC_DERIVED=1
}

# ---- principal discovery (explicit per-cluster subscription) ----------------
rbac::discover() {
  [[ "${RBAC_DISCOVERED:-0}" == "1" ]] && return 0
  rbac::_derive
  PRINCIPALS=(); P_VMS=(); P_REGIONS=(); P_SUBS=()
  local region role sub vm pid k v pairs
  for region in "${REGIONS_ARR[@]}"; do
    role="$(naming::subscription_role "$TCODE" "$region")"
    sub="$(rbac::_sub_for_role "$role")" || log::die "no subscription for role '${role}'"
    [[ -n "$sub" ]] || log::die "no subscription configured for role '${role}' (region ${region})"
    # Capture first so a name-validation failure is not swallowed by the
    # process substitution (N5); mirrors provision-cluster.sh's derive.
    pairs="$(naming::_region_pairs "$TCODE" "$region")" || log::die "name generation failed for ${TCODE}/${region}"
    declare -A M=()
    while IFS='=' read -r k v; do [[ -n "$k" ]] && M["$k"]="$v"; done <<< "$pairs"
    for vm in "${M[cp_vm]}" "${M[worker1_vm]}" "${M[worker2_vm]}" "${M[worker3_vm]}"; do
      pid="$(rbac::az "$sub" vm identity show -g "${M[resource_group]}" -n "$vm" --query principalId -o tsv 2>/dev/null)" \
        || log::die "could not read managed-identity principal for ${vm} (subscription ${sub})"
      [[ -n "$pid" ]] || log::die "empty managed-identity principal for ${vm} (subscription ${sub})"
      PRINCIPALS+=("$pid"); P_VMS+=("$vm"); P_REGIONS+=("$region"); P_SUBS+=("$sub")
    done
  done
  RBAC_DISCOVERED=1
  log::info "discovered ${#PRINCIPALS[@]} node principal(s) across ${#REGIONS_ARR[@]} region(s)"
}

rbac::print_principals() {
  local i
  for i in "${!PRINCIPALS[@]}"; do
    printf 'principal_id=%s vm=%s region=%s subscription=%s scope=%s\n' \
      "${PRINCIPALS[$i]}" "${P_VMS[$i]}" "${P_REGIONS[$i]}" "${P_SUBS[$i]}" "$TARGET_SCOPE"
  done
}

# ---- grant (RG-scoped, recorded, idempotent) --------------------------------
rbac::grant() {
  rbac::discover
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  local i pid vm region csub out aid aname assignments='[]'
  for i in "${!PRINCIPALS[@]}"; do
    pid="${PRINCIPALS[$i]}"; vm="${P_VMS[$i]}"; region="${P_REGIONS[$i]}"; csub="${P_SUBS[$i]}"
    log::info "granting '${ROLE}' to ${vm} identity on ${TARGET_SCOPE}"
    # Attempt once; a non-zero result is treated as "already exists" and the
    # existing assignment is looked up (idempotent re-runs, RISK-004). Transient
    # failures fall through to the lookup and fail closed if nothing is found;
    # whole-job retry (workflow) plus the propagation wait cover NFR-003.
    if out="$(rbac::az "$PRIMARY_SUBSCRIPTION_ID" role assignment create \
                --assignee-object-id "$pid" --assignee-principal-type "$PRINCIPAL_TYPE" \
                --role "$ROLE" --scope "$TARGET_SCOPE" -o json 2>/dev/null)"; then
      :
    else
      log::warn "assignment create failed for ${pid} (may already exist); querying existing (idempotent)"
      # `|| true` so a transient list failure does not abort the loop before the
      # emptiness gate below produces a clear error (N6).
      out="$(rbac::az "$PRIMARY_SUBSCRIPTION_ID" role assignment list --assignee "$pid" --scope "$TARGET_SCOPE" -o json 2>/dev/null \
             | jq -c --arg r "$ROLE" 'map(select(.roleDefinitionName == $r)) | (.[0] // {})' || true)"
    fi
    aid="$(printf '%s' "$out" | jq -r '.id // ""')"
    aname="$(printf '%s' "$out" | jq -r '.name // ""')"
    [[ -n "$aid" ]] || log::die "failed to create or find a role assignment for ${pid} on ${TARGET_SCOPE}"
    # Defensive: never record anything that is not RG-scoped (ITEM-009).
    [[ "$TARGET_SCOPE" =~ ^/subscriptions/[^/]+/resourceGroups/[^/]+$ ]] \
      || log::die "refusing to record a non-RG-scoped assignment: ${TARGET_SCOPE}"
    assignments="$(jq -c -n --argjson a "$assignments" \
      --arg pid "$pid" --arg vm "$vm" --arg region "$region" --arg csub "$csub" \
      --arg aid "$aid" --arg aname "$aname" --arg scope "$TARGET_SCOPE" \
      '$a + [{principal_id:$pid, vm:$vm, region:$region, cluster_subscription:$csub,
              assignment_id:$aid, assignment_name:$aname, scope:$scope}]')"
  done
  local rec
  rec="$(jq -n --arg role "$ROLE" --arg scope "$TARGET_SCOPE" --arg sub "$PRIMARY_SUBSCRIPTION_ID" \
    --argjson a "$assignments" '{role:$role, scope:$scope, subscription:$sub, assignments:$a}')"
  manifest::put_json "$MANIFEST_PATH" "rbac.${TCODE}" "$rec"
  gha::output rbac_assignment_count "${#PRINCIPALS[@]}"
  log::info "recorded ${#PRINCIPALS[@]} run-scoped assignment(s) -> ${MANIFEST_PATH}"
}

# ---- bounded propagation wait (NFR-003) -------------------------------------
rbac::wait() {
  rbac::discover
  local pid attempt delay n
  for pid in "${PRINCIPALS[@]}"; do
    attempt=1; delay="$PROP_DELAY"
    while true; do
      n="$(rbac::az "$PRIMARY_SUBSCRIPTION_ID" role assignment list --assignee "$pid" --scope "$TARGET_SCOPE" -o json 2>/dev/null \
           | jq -r --arg r "$ROLE" '[.[] | select(.roleDefinitionName == $r)] | length' 2>/dev/null)"
      n="${n//[^0-9]/}"; n="${n:-0}"
      (( n >= 1 )) && break
      if (( attempt >= PROP_ATTEMPTS )); then
        log::error "RBAC propagation timed out for principal ${pid} on ${TARGET_SCOPE} after ${attempt} attempt(s)"
        return 1
      fi
      log::warn "awaiting RBAC propagation (principal ${pid}, ${attempt}/${PROP_ATTEMPTS})"
      # Cap the exponential backoff so the total bounded wait stays within the
      # job timeout instead of ballooning to 10+ minute sleeps (N2/NFR-003).
      sleep "$delay"; attempt=$(( attempt + 1 ))
      delay=$(( delay * 2 )); (( delay > PROP_DELAY_MAX )) && delay="$PROP_DELAY_MAX"
    done
  done
  log::info "RBAC propagated for ${#PRINCIPALS[@]} principal(s) on ${TARGET_SCOPE}"
}

# ---- scope assertions (ITEM-009: no subscription-scope grant) ---------------
# Manifest-based: every recorded scope must be run-RG-scoped.
rbac::assert_scope() {
  local tcode; tcode="$(naming::topology_code "$TOPOLOGY")" || log::die "invalid TOPOLOGY '${TOPOLOGY}'"
  [[ -f "$MANIFEST_PATH" ]] || log::die "manifest not found: ${MANIFEST_PATH}"
  local bad
  bad="$(jq -r --arg tc "$tcode" '
    [ (.rbac[$tc].scope // empty), (.rbac[$tc].assignments[]?.scope // empty) ]
    | map(select((test("^/subscriptions/[^/]+/resourceGroups/[^/]+$")) | not)) | length' "$MANIFEST_PATH")"
  bad="${bad//[^0-9]/}"; bad="${bad:-0}"
  (( bad == 0 )) || { log::error "rbac.${tcode}: ${bad} non-RG-scoped grant(s) found (subscription-scope forbidden, ITEM-009)"; return 1; }
  log::info "rbac.${tcode}: all recorded grants are run-RG-scoped"
}

# Live: no run principal may hold a subscription-scope assignment.
rbac::assert_no_subscription_scope() {
  rbac::discover
  local pid rc=0 bad
  for pid in "${PRINCIPALS[@]}"; do
    bad="$(rbac::az "$PRIMARY_SUBSCRIPTION_ID" role assignment list --assignee "$pid" -o json 2>/dev/null \
           | jq -r '[.[] | select(.scope | test("^/subscriptions/[^/]+$"))] | length')"
    bad="${bad//[^0-9]/}"; bad="${bad:-0}"
    if (( bad > 0 )); then
      log::error "principal ${pid} holds ${bad} subscription-scope grant(s) (forbidden, SEC-002/ITEM-009)"
      rc=1
    fi
  done
  (( rc == 0 )) && log::info "no run principal holds a subscription-scope grant"
  return "$rc"
}

rbac::all() {
  rbac::discover
  rbac::grant
  rbac::wait
  rbac::assert_scope
  rbac::assert_no_subscription_scope
}

rbac::usage() {
  cat <<'USAGE'
Usage: cross-region-rbac.sh <command>

Run-scoped, explicit-subscription cross-region RBAC (same subscription in ss).
Commands:
  all           discover -> grant -> wait -> assert-scope -> assert no subscription-scope grant
  discover      Print both clusters' node-identity principals
  grant         Create RG-scoped role assignments on the primary run RG; record IDs
  wait          Bounded wait for RBAC propagation
  assert-scope  Fail if any recorded grant is not run-RG-scoped

Required env: TOPOLOGY (ss|xs), PRIMARY_SUBSCRIPTION_ID (+ SECONDARY_SUBSCRIPTION_ID for xs).
Naming context: REPO, RUN_ID, RUN_ATTEMPT, DATE_UTC. Tool seam: AZ_BIN (az).
USAGE
}

rbac::main() {
  local cmd="${1:-all}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    all)          rbac::all ;;
    discover)     rbac::discover; rbac::print_principals ;;
    grant)        rbac::grant ;;
    wait)         rbac::wait ;;
    assert-scope) rbac::assert_scope ;;
    help|-h|--help) rbac::usage ;;
    *) log::error "unknown command: ${cmd}"; rbac::usage >&2; return 1 ;;
  esac
}

rbac::main "$@"
