#!/usr/bin/env bash
# =============================================================================
# provision-tt-fixtures.sh - provision the transparent-tunnel same-node
# enforcement fixtures on the centraluseuap cluster (test doc §3):
#   * NSG rule 190 `DenyBackendToFrontend` (Deny; src ASG asg-backend -> dst ASG
#     asg-frontend; Any) and rule 200 `AllowFrontendToBackend` (Allow; src ASG
#     asg-frontend -> dst ASG asg-backend; TCP 8080) on the centraluseuap subnet
#     NSG (${RBASE}-nsg).
#   * the `app=backend` / `app=frontend` PodASGMappings in namespace `default`
#     (CON-003), applied on the control-plane node via `az vm run-command`.
#
# ASG resolution mirrors deploy-controller.sh / run-validation.sh: the shared
# asg-backend/asg-frontend live in the topology's PRIMARY region RG under the
# PRIMARY subscription, and BOTH clusters' controllers target them (FR-028). The
# ASG scope is overridable via TT_ASG_RG / TT_ASG_SUB (seam).
#
# NOTE (real-Azure caveat, surfaced in the run report): the centraluseuap subnet
# NSG referencing the shared ASGs — which the multi-cluster design places in the
# eastus2euap primary RG — is a cross-region reference. Where an environment
# requires the ASGs co-located with the NSG, set TT_ASG_RG/TT_ASG_SUB to the
# centraluseuap cluster RG/subscription; this script only encodes the intended
# rules and does not re-architect EPIC-003 ASG placement.
#
# Inputs (environment):
#   TOPOLOGY                 ss|xs (TT runs on the centraluseuap cluster)   [req]
#   REGION                   default: centraluseuap (secondary)
#   PRIMARY_SUBSCRIPTION_ID  cluster + ASG owner (primary)                  [req]
#   SECONDARY_SUBSCRIPTION_ID cluster owner for xs (default: primary)
#   TT_ASG_RG / TT_ASG_SUB   override the shared-ASG scope (default: primary RG/sub)
#   MANIFEST_PATH            run manifest to augment (default ./run-manifest.json)
#   AZ_BIN                   tool seam (default az)
#   KUBECONFIG_ON_NODE       node kubeconfig (default /etc/kubernetes/admin.conf)
#
# Commands: all(default) | nsg-rules | mappings | names | help
#
# Traceability: ITEM-031, FR-021, REQ-004, CON-003, CON-010, RD-020, RISK-013,
# PRD Section 3.6 / docs/transparent-tunnel-same-node-enforcement-test.md 3.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"

lib::require_cmds jq base64 sed

TOPOLOGY="${TOPOLOGY:-}"
REGION="${REGION:-$NAMING_SECONDARY_REGION}"     # centraluseuap-only (CON-010)
PRIMARY_SUBSCRIPTION_ID="${PRIMARY_SUBSCRIPTION_ID:-}"
SECONDARY_SUBSCRIPTION_ID="${SECONDARY_SUBSCRIPTION_ID:-}"
TT_ASG_RG="${TT_ASG_RG:-}"
TT_ASG_SUB="${TT_ASG_SUB:-}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
AZ_BIN="${AZ_BIN:-az}"
KUBECONFIG_ON_NODE="${KUBECONFIG_ON_NODE:-/etc/kubernetes/admin.conf}"
# Bounded retry/backoff for the NSG rule creates (NFR-003); tunable for tests.
TTFX_RETRY_ATTEMPTS="${TTFX_RETRY_ATTEMPTS:-3}"
TTFX_RETRY_DELAY="${TTFX_RETRY_DELAY:-5}"

# Documented enforcement rules (test doc §3); fixed names/priorities/ports.
DENY_RULE="DenyBackendToFrontend"; DENY_PRIO=190
ALLOW_RULE="AllowFrontendToBackend"; ALLOW_PRIO=200; ALLOW_PORT=8080

declare -gA N=()

ttfx::_require_inputs() {
  [[ -n "$TOPOLOGY" ]]                || log::die "TOPOLOGY is required (ss|xs)"
  [[ -n "$PRIMARY_SUBSCRIPTION_ID" ]] || log::die "PRIMARY_SUBSCRIPTION_ID is required (explicit subscription; RD-020)"
}

ttfx::_derive() {
  [[ "${TTFX_DERIVED:-0}" == "1" ]] && return 0
  ttfx::_require_inputs
  naming::_load_context
  TCODE="$(naming::topology_code "$TOPOLOGY")" || log::die "invalid TOPOLOGY '${TOPOLOGY}' (want ss|xs)"
  : "${SECONDARY_SUBSCRIPTION_ID:=$PRIMARY_SUBSCRIPTION_ID}"

  local pairs k v
  pairs="$(naming::_region_pairs "$TCODE" "$REGION")" || log::die "name generation failed for ${TCODE}/${REGION}"
  N=()
  while IFS='=' read -r k v; do [[ -n "$k" ]] && N["$k"]="$v"; done <<< "$pairs"
  case "${N[subscription_role]}" in
    primary)   CLUSTER_SUB="$PRIMARY_SUBSCRIPTION_ID" ;;
    secondary) CLUSTER_SUB="$SECONDARY_SUBSCRIPTION_ID" ;;
    *) log::die "unexpected subscription role '${N[subscription_role]}'" ;;
  esac
  [[ -n "$CLUSTER_SUB" ]] || log::die "no subscription resolved for role '${N[subscription_role]}'"

  # Shared ASG scope: PRIMARY region RG under the PRIMARY subscription (FR-028),
  # overridable for co-located ASGs.
  local base primary_rg
  base="$(naming::base "$NAMING_PURPOSE" "$TCODE" "$DATE_UTC" "$RUN_SUFFIX")"
  primary_rg="$(naming::rbase "$base" "$NAMING_PRIMARY_REGION")"
  ASG_SUB="${TT_ASG_SUB:-$PRIMARY_SUBSCRIPTION_ID}"
  ASG_RG="${TT_ASG_RG:-$primary_rg}"
  ASG_BACKEND_ID="/subscriptions/${ASG_SUB}/resourceGroups/${ASG_RG}/providers/Microsoft.Network/applicationSecurityGroups/asg-backend"
  ASG_FRONTEND_ID="/subscriptions/${ASG_SUB}/resourceGroups/${ASG_RG}/providers/Microsoft.Network/applicationSecurityGroups/asg-frontend"
  TTFX_DERIVED=1
}

# ---- NSG enforcement rules (test doc §3) ------------------------------------
ttfx::nsg_rules() {
  ttfx::_derive
  local rg="${N[resource_group]}" nsg="${N[nsg]}"
  log::info "creating NSG enforcement rule ${DENY_RULE} (prio ${DENY_PRIO}) on ${nsg} (subscription ${CLUSTER_SUB})"
  lib::retry "$TTFX_RETRY_ATTEMPTS" "$TTFX_RETRY_DELAY" -- "$AZ_BIN" network nsg rule create \
    -g "$rg" --nsg-name "$nsg" -n "$DENY_RULE" --subscription "$CLUSTER_SUB" \
    --priority "$DENY_PRIO" --access Deny --direction Inbound --protocol '*' \
    --source-asgs "$ASG_BACKEND_ID" --destination-asgs "$ASG_FRONTEND_ID" \
    --source-port-ranges '*' --destination-port-ranges '*' --output none \
    || log::die "failed to create ${DENY_RULE}"

  log::info "creating NSG enforcement rule ${ALLOW_RULE} (prio ${ALLOW_PRIO}, TCP ${ALLOW_PORT}) on ${nsg}"
  lib::retry "$TTFX_RETRY_ATTEMPTS" "$TTFX_RETRY_DELAY" -- "$AZ_BIN" network nsg rule create \
    -g "$rg" --nsg-name "$nsg" -n "$ALLOW_RULE" --subscription "$CLUSTER_SUB" \
    --priority "$ALLOW_PRIO" --access Allow --direction Inbound --protocol Tcp \
    --source-asgs "$ASG_FRONTEND_ID" --destination-asgs "$ASG_BACKEND_ID" \
    --source-port-ranges '*' --destination-port-ranges "$ALLOW_PORT" --output none \
    || log::die "failed to create ${ALLOW_RULE}"

  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  manifest::put "$MANIFEST_PATH" validate.tt.fixtures.nsg "$nsg"
  manifest::put_json "$MANIFEST_PATH" validate.tt.fixtures.nsg_rules "$(jq -n \
    --arg dn "$DENY_RULE" --argjson dp "$DENY_PRIO" \
    --arg an "$ALLOW_RULE" --argjson ap "$ALLOW_PRIO" --argjson port "$ALLOW_PORT" '
    [ {name:$dn, priority:$dp, access:"Deny",  source:"asg-backend",  destination:"asg-frontend", protocol:"*"},
      {name:$an, priority:$ap, access:"Allow", source:"asg-frontend", destination:"asg-backend", protocol:"Tcp", port:$port} ]')"
  log::info "NSG enforcement rules ${DENY_RULE}/${ALLOW_RULE} present on ${nsg}"
}

# ---- PodASGMappings (namespace default, app selectors; CON-003) --------------
ttfx::_mapping_yaml() {
  local name="$1" selector_value="$2" asg_id="$3"
  cat <<MAP
---
apiVersion: networking.azure.com/v1alpha1
kind: PodASGMapping
metadata:
  name: ${name}
  namespace: ${N[namespace]}
  labels:
    validation.networking.azure.com/purpose: "${NAMING_PURPOSE}"
    validation.networking.azure.com/run-suffix: "${RUN_SUFFIX}"
    validation.networking.azure.com/topology: "${TCODE}"
    app.kubernetes.io/managed-by: github-actions
spec:
  mappings:
    - podSelector:
        matchLabels:
          ${N[pod_label]}: ${selector_value}
      applicationSecurityGroups:
        - resourceId: ${asg_id}
MAP
}

ttfx::mappings() {
  ttfx::_derive
  local yaml b64 script
  yaml="$(ttfx::_mapping_yaml backend-asg-mapping  backend  "$ASG_BACKEND_ID"
          ttfx::_mapping_yaml frontend-asg-mapping frontend "$ASG_FRONTEND_ID")"
  b64="$(printf '%s' "$yaml" | base64 -w0)"
  # Pipe the decoded manifest straight to kubectl apply (idempotent) on the node.
  script="$(printf 'export KUBECONFIG=%s\nprintf %s %s | base64 -d | kubectl apply -f -' \
    "$KUBECONFIG_ON_NODE" "'%s'" "'${b64}'")"
  log::info "applying transparent-tunnel PodASGMappings in namespace ${N[namespace]} on ${N[cp_vm]}"
  azrun::exec "$AZ_BIN" "$CLUSTER_SUB" "${N[resource_group]}" "${N[cp_vm]}" "$script" \
    || log::die "failed to apply transparent-tunnel PodASGMappings on ${N[cp_vm]}"

  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  manifest::put_json "$MANIFEST_PATH" validate.tt.fixtures.mappings \
    "$(jq -n '["backend-asg-mapping","frontend-asg-mapping"]')"
  manifest::put "$MANIFEST_PATH" validate.tt.fixtures.namespace "${N[namespace]}"
  log::info "transparent-tunnel PodASGMappings applied (backend-asg-mapping/frontend-asg-mapping)"
}

ttfx::all() {
  ttfx::_derive
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  ttfx::nsg_rules
  ttfx::mappings
  gha::output nsg "${N[nsg]}"
  gha::output namespace "${N[namespace]}"
  gha::summary "## Transparent-tunnel enforcement fixtures (\`validate_tt\`)"
  gha::summary ""
  gha::summary "| Fixture | Value |"
  gha::summary "|---|---|"
  gha::summary "| NSG | \`${N[nsg]}\` |"
  gha::summary "| Rule 190 | \`${DENY_RULE}\` (Deny asg-backend -> asg-frontend, Any) |"
  gha::summary "| Rule 200 | \`${ALLOW_RULE}\` (Allow asg-frontend -> asg-backend, TCP ${ALLOW_PORT}) |"
  gha::summary "| Mappings | \`backend-asg-mapping\`/\`frontend-asg-mapping\` in ns \`${N[namespace]}\` |"
  log::info "transparent-tunnel fixtures provisioned on ${N[resource_group]}"
}

ttfx::names() {
  ttfx::_derive
  printf 'topology=%s\nregion=%s\nresource_group=%s\nnsg=%s\ncp_vm=%s\nnamespace=%s\nasg_backend_id=%s\nasg_frontend_id=%s\n' \
    "$TCODE" "$REGION" "${N[resource_group]}" "${N[nsg]}" "${N[cp_vm]}" "${N[namespace]}" \
    "$ASG_BACKEND_ID" "$ASG_FRONTEND_ID"
}

ttfx::usage() {
  cat <<USAGE
Usage: provision-tt-fixtures.sh <command>

Commands:
  all         nsg-rules + mappings (default).
  nsg-rules   Create rule 190 DenyBackendToFrontend and rule 200 AllowFrontendToBackend.
  mappings    Apply app=backend/app=frontend PodASGMappings in namespace default.
  names       Print resolved fixture names.
  help        Show this help.
USAGE
}

ttfx::main() {
  local cmd="${1:-all}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    all)        ttfx::all ;;
    nsg-rules)  ttfx::nsg_rules ;;
    mappings)   ttfx::mappings ;;
    names)      ttfx::names ;;
    help|-h|--help) ttfx::usage ;;
    *) log::error "unknown command: ${cmd}"; ttfx::usage >&2; return 1 ;;
  esac
}

ttfx::main "$@"
