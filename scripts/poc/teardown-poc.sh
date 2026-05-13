#!/usr/bin/env bash
# =============================================================================
# Teardown — Delete all POC resources
# =============================================================================
set -euo pipefail

PREFIX="asnStripe"

echo "============================================="
echo " Tearing down POC environment"
echo "============================================="
echo ""
echo "This will delete the following resource groups:"
echo "  - ${PREFIX}-westus2 (Runners subscription)"
echo "  - ${PREFIX}-eastus2 (Test subscription)"
echo "  - ${PREFIX}-eastus2euap (Runners subscription)"
echo ""
read -p "Are you sure? (y/N): " CONFIRM
[ "${CONFIRM:-n}" = "y" ] || { echo "Aborted."; exit 0; }

echo ""
echo ">>> Deleting ${PREFIX}-westus2..."
az account set --subscription "9bd7ff15-396a-4478-a89b-4d8ae7e302b6"
az group delete --name "${PREFIX}-westus2" --yes --no-wait
echo "  ✓ Deletion started (async)"

echo ">>> Deleting ${PREFIX}-eastus2..."
az account set --subscription "9b8218f9-902a-4d20-a65c-e98acec5362f"
az group delete --name "${PREFIX}-eastus2" --yes --no-wait
echo "  ✓ Deletion started (async)"

echo ">>> Deleting ${PREFIX}-eastus2euap..."
az account set --subscription "9bd7ff15-396a-4478-a89b-4d8ae7e302b6"
az group delete --name "${PREFIX}-eastus2euap" --yes --no-wait
echo "  ✓ Deletion started (async)"

# Clean up kubeconfigs
rm -f "${HOME}/.config/kube/${PREFIX}-westus2-kubeconfig.yaml"
rm -f "${HOME}/.config/kube/${PREFIX}-eastus2-kubeconfig.yaml"
rm -f "${HOME}/.config/kube/${PREFIX}-eastus2euap-kubeconfig.yaml"
echo "  ✓ Kubeconfigs removed"

echo ""
echo "============================================="
echo " Teardown initiated. RG deletions are async."
echo " Monitor with: az group show -n ${PREFIX}-<region>"
echo "============================================="
