#!/usr/bin/env bash
# =============================================================================
# Cross-Subscription RBAC Setup
# Grants managed identities from each cluster access to the other cluster's
# resource group for pod-nsg-controller cross-subscription operations.
#
# NOTE (EPIC-010 / ITEM-038): this hand-run POC is the SOURCE that
# scripts/e2e/setup-cross-sub-rbac.sh generalizes for the automated pipeline.
# The pipeline variant is parameterized (no hard-coded subscriptions/prefixes),
# scopes every grant to the run RG only (never a mesh of subscription-wide
# grants), passes an explicit --subscription on every call (never `az account
# set`), inventories assignment IDs, and adds a verified `remove` for cleanup.
# Prefer the e2e script in CI; keep this for interactive POC bring-up.
# =============================================================================
set -euo pipefail

PREFIX="asnStripe"

# Cluster 1: westus2 in Runners subscription
CLUSTER1_SUB="9bd7ff15-396a-4478-a89b-4d8ae7e302b6"
CLUSTER1_RG="${PREFIX}-westus2"
CLUSTER1_CP="${PREFIX}-westus2-cp-01"

# Cluster 2: eastus2 in Test subscription
CLUSTER2_SUB="9b8218f9-902a-4d20-a65c-e98acec5362f"
CLUSTER2_RG="${PREFIX}-eastus2"
CLUSTER2_CP="${PREFIX}-eastus2-cp-01"

# Cluster 3: eastus2euap in Runners subscription
CLUSTER3_SUB="9bd7ff15-396a-4478-a89b-4d8ae7e302b6"
CLUSTER3_RG="${PREFIX}-eastus2euap"
CLUSTER3_CP="${PREFIX}-eastus2euap-cp-01"

ROLE="Network Contributor"

echo "============================================="
echo " Cross-Subscription RBAC Setup"
echo "============================================="

# ─────────────────────────────────────────────
# Collect principal IDs from all VMs in all clusters
# ─────────────────────────────────────────────
echo ""
echo ">>> Collecting managed identity principal IDs..."

collect_principals() {
  local sub="$1" rg="$2" region="$3"
  local -n principals_ref="$4"
  az account set --subscription "$sub"
  for VM in "${PREFIX}-${region}-cp-01" "${PREFIX}-${region}-worker-01" "${PREFIX}-${region}-worker-02" "${PREFIX}-${region}-worker-03"; do
    PID=$(az vm identity show -g "$rg" -n "$VM" --query principalId -o tsv)
    principals_ref+=("$PID")
    echo "  ${VM}: ${PID}"
  done
}

CLUSTER1_PRINCIPALS=()
collect_principals "$CLUSTER1_SUB" "$CLUSTER1_RG" "westus2" CLUSTER1_PRINCIPALS

CLUSTER2_PRINCIPALS=()
collect_principals "$CLUSTER2_SUB" "$CLUSTER2_RG" "eastus2" CLUSTER2_PRINCIPALS

CLUSTER3_PRINCIPALS=()
collect_principals "$CLUSTER3_SUB" "$CLUSTER3_RG" "eastus2euap" CLUSTER3_PRINCIPALS

ALL_PRINCIPALS=("${CLUSTER1_PRINCIPALS[@]}" "${CLUSTER2_PRINCIPALS[@]}" "${CLUSTER3_PRINCIPALS[@]}")

# ─────────────────────────────────────────────
# Grant every VM access to every cluster's RG (mesh RBAC)
# ─────────────────────────────────────────────

grant_role() {
  local desc="$1" scope="$2"
  shift 2
  local pids=("$@")
  echo ""
  echo ">>> Granting '${ROLE}' on ${desc} (${scope})..."
  for PID in "${pids[@]}"; do
    az role assignment create \
      --assignee-object-id "$PID" \
      --assignee-principal-type ServicePrincipal \
      --role "$ROLE" \
      --scope "$scope" --output none 2>/dev/null || echo "    (assignment may already exist)"
    echo "  ✓ ${PID} → ${scope}"
  done
}

# All VMs → Cluster 1 RG
grant_role "Cluster 1 (westus2)" \
  "/subscriptions/${CLUSTER1_SUB}/resourceGroups/${CLUSTER1_RG}" \
  "${ALL_PRINCIPALS[@]}"

# All VMs → Cluster 2 RG
grant_role "Cluster 2 (eastus2)" \
  "/subscriptions/${CLUSTER2_SUB}/resourceGroups/${CLUSTER2_RG}" \
  "${ALL_PRINCIPALS[@]}"

# All VMs → Cluster 3 RG
grant_role "Cluster 3 (eastus2euap)" \
  "/subscriptions/${CLUSTER3_SUB}/resourceGroups/${CLUSTER3_RG}" \
  "${ALL_PRINCIPALS[@]}"

echo ""
echo "============================================="
echo " Cross-subscription RBAC setup complete!"
echo " Allow 1-5 minutes for RBAC propagation."
echo "============================================="
