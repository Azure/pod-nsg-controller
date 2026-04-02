# Cross-Subscription ASG Access from a Self-Managed Kubernetes Pod

This document describes how to perform Azure Resource Manager (ARM) API calls from a pod running in a self-managed Kubernetes cluster to access Application Security Group (ASG) resources in a **different Azure subscription**.

## Table of Contents

- [1. Overview](#1-overview)
- [2. Prerequisites](#2-prerequisites)
- [3. Architecture](#3-architecture)
- [4. Step-by-Step Guide](#4-step-by-step-guide)
  - [4.1 Enable System-Assigned Managed Identity on the VM](#41-enable-system-assigned-managed-identity-on-the-vm)
  - [4.2 Grant RBAC on the Target Resource](#42-grant-rbac-on-the-target-resource)
  - [4.3 Configure IMDS Access for Pods (iptables MASQUERADE)](#43-configure-imds-access-for-pods-iptables-masquerade)
  - [4.4 Deploy a Test Pod](#44-deploy-a-test-pod)
  - [4.5 Acquire an Azure Token from the Pod](#45-acquire-an-azure-token-from-the-pod)
  - [4.6 Perform the Cross-Subscription ARM Call](#46-perform-the-cross-subscription-arm-call)
- [5. Applying This to pod-nsg-controller](#5-applying-this-to-pod-nsg-controller)
- [6. Validated Results](#6-validated-results)
- [7. Troubleshooting](#7-troubleshooting)

---

## 1. Overview

| Item | Value |
|---|---|
| **Source Cluster** | Self-managed K8s in `asn-rg2-k8s-selfmanaged` |
| **Source Subscription** | `Azure Network Agent - Runners` (`9bd7ff15-396a-4478-a89b-4d8ae7e302b6`) |
| **Target Subscription** | `Azure Network Agent - Test` (`9b8218f9-902a-4d20-a65c-e98acec5362f`) |
| **Target Resource** | `/subscriptions/9b8218f9-902a-4d20-a65c-e98acec5362f/resourceGroups/asnPOC/providers/Microsoft.Network/applicationSecurityGroups/asg-frontend` |
| **Authentication** | VM System-Assigned Managed Identity via IMDS |
| **K8s Version** | v1.31.14 |
| **CNI** | Azure CNI (transparent bridge mode) |

**What was proven**: A pod running on a K8s node in one subscription can authenticate via the VM's managed identity and successfully call the ARM API to read an ASG resource in a different subscription.

## 2. Prerequisites

- A self-managed Kubernetes cluster running on Azure VMs (see [self-managed-k8s-azure-cni-setup.md](self-managed-k8s-azure-cni-setup.md))
- `az` CLI authenticated with permissions to:
  - Manage identities on the source VMs
  - Create role assignments in the target subscription
- An existing ASG (or other ARM resource) in the target subscription
- The Azure CNI networking fix applied (bridge + L3 routing — see the cluster setup guide)

## 3. Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  Source Subscription: Azure Network Agent - Runners              │
│  (9bd7ff15-396a-4478-a89b-4d8ae7e302b6)                        │
│                                                                  │
│  ┌────────────────────────────────────────────────┐              │
│  │  Self-Managed K8s Cluster                      │              │
│  │  RG: asn-rg2-k8s-selfmanaged                   │              │
│  │                                                │              │
│  │  ┌──────────────────────────────┐              │              │
│  │  │  k8s-cp-01 (VM)             │              │              │
│  │  │  Managed Identity: ✅        │              │              │
│  │  │                              │              │              │
│  │  │  ┌────────────────────────┐  │              │              │
│  │  │  │  Pod: arm-test         │  │              │              │
│  │  │  │  IP: 10.1.1.10        │  │              │              │
│  │  │  │                        │  │              │              │
│  │  │  │  1. curl IMDS ─────────┤──┤──► MASQUERADE to 10.1.1.4  │
│  │  │  │     (169.254.169.254)  │  │     (node primary IP)      │
│  │  │  │                        │  │              │              │
│  │  │  │  2. curl ARM API ──────┤──┤──────────────┤──────────┐   │
│  │  │  │     (management.azure) │  │              │          │   │
│  │  │  └────────────────────────┘  │              │          │   │
│  │  └──────────────────────────────┘              │          │   │
│  └────────────────────────────────────────────────┘          │   │
└──────────────────────────────────────────────────────────────┤───┘
                                                               │
                              Bearer Token (from IMDS)         │
                                                               ▼
┌──────────────────────────────────────────────────────────────────┐
│  Target Subscription: Azure Network Agent - Test                 │
│  (9b8218f9-902a-4d20-a65c-e98acec5362f)                        │
│                                                                  │
│  ┌────────────────────────────────┐                              │
│  │  RG: asnPOC                    │                              │
│  │                                │                              │
│  │  ┌──────────────────────────┐  │                              │
│  │  │  ASG: asg-frontend       │  │  ◄── ARM GET (Reader RBAC)  │
│  │  │  Location: westus3       │  │                              │
│  │  └──────────────────────────┘  │                              │
│  └────────────────────────────────┘                              │
└──────────────────────────────────────────────────────────────────┘
```

**Authentication flow**:
1. Pod requests an OAuth2 token from Azure IMDS (169.254.169.254)
2. IMDS returns a token for the VM's managed identity
3. Pod uses the token as a Bearer token in the ARM API call
4. ARM validates the token and checks RBAC — the identity has `Reader` on the target ASG
5. ARM returns the ASG resource details

## 4. Step-by-Step Guide

### 4.1 Enable System-Assigned Managed Identity on the VM

If the VM doesn't already have a system-assigned managed identity, enable it:

```bash
az vm identity assign -g asn-rg2-k8s-selfmanaged -n k8s-cp-01
```

Retrieve the principal ID:

```bash
PRINCIPAL_ID=$(az vm identity show -g asn-rg2-k8s-selfmanaged -n k8s-cp-01 \
  --query principalId -o tsv)
echo "Principal ID: $PRINCIPAL_ID"
# Output: 46a8fe1c-9e15-4f76-a780-435ff1dbdb13
```

> **Note**: Every pod running on this VM will share the same managed identity. For finer-grained control, consider Azure AD Workload Identity (requires OIDC issuer setup on the cluster).

### 4.2 Grant RBAC on the Target Resource

Grant the VM's managed identity `Reader` access to the target ASG in the other subscription:

```bash
TARGET_RESOURCE="/subscriptions/9b8218f9-902a-4d20-a65c-e98acec5362f/resourceGroups/asnPOC/providers/Microsoft.Network/applicationSecurityGroups/asg-frontend"
PRINCIPAL_ID="46a8fe1c-9e15-4f76-a780-435ff1dbdb13"

az role assignment create \
  --assignee-object-id $PRINCIPAL_ID \
  --assignee-principal-type ServicePrincipal \
  --role "Reader" \
  --scope "$TARGET_RESOURCE"
```

**Role selection guide**:

| Operation | Minimum Role |
|---|---|
| GET/LIST ASGs | `Reader` |
| Create/Update ASGs | `Network Contributor` |
| Update NIC → ASG associations | `Network Contributor` (on the NIC's resource group) |
| Full pod-nsg-controller operations | `Network Contributor` on both source and target RGs |

> **Scope**: You can assign the role at the resource, resource group, or subscription level. Resource-level is the most restrictive (least privilege).

### 4.3 Configure IMDS Access for Pods (iptables MASQUERADE)

**This is the critical step.** Azure IMDS (Instance Metadata Service) only responds to requests from the VM's primary IP. Pods in Azure CNI use secondary IPs, so IMDS rejects their requests with HTTP 410.

The fix: add an iptables rule to MASQUERADE (SNAT) pod traffic to IMDS so it appears to come from the node's primary IP.

Run on **each node** where pods need IMDS access:

```bash
az vm run-command invoke -g asn-rg2-k8s-selfmanaged -n k8s-cp-01 \
  --command-id RunShellScript \
  --scripts 'iptables -t nat -A POSTROUTING -d 169.254.169.254/32 -j MASQUERADE'
```

Or directly on the node:

```bash
iptables -t nat -A POSTROUTING -d 169.254.169.254/32 -j MASQUERADE
```

**Verify the rule exists:**

```bash
iptables -t nat -L POSTROUTING -n | grep 169.254.169.254
# Expected: MASQUERADE  all  --  0.0.0.0/0  169.254.169.254
```

> **⚠️ Not persistent across reboots.** Add to a systemd unit or iptables-persistent for durability.

### 4.4 Deploy a Test Pod

Deploy a pod with `curl` on the target node:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: arm-test
spec:
  nodeName: k8s-cp-01          # Schedule on the node with the managed identity
  containers:
  - name: curl
    image: curlimages/curl:8.5.0
    command: ["sleep", "3600"]
  tolerations:
  - operator: Exists            # Tolerate control-plane taints if present
```

```bash
kubectl apply -f arm-test-pod.yaml
kubectl wait --for=condition=Ready pod/arm-test --timeout=120s
```

### 4.5 Acquire an Azure Token from the Pod

From inside the pod, request a token from IMDS for the Azure Resource Manager audience:

```bash
kubectl exec arm-test -- curl -s -H "Metadata: true" \
  "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fmanagement.azure.com%2F"
```

**Successful response:**

```json
{
  "access_token": "eyJ0eXAiOiJKV1QiLCJhbGciOi...<2046 chars>",
  "client_id": "...",
  "expires_in": "84073",
  "expires_on": "1743712234",
  "ext_expires_in": "86399",
  "not_before": "1743625534",
  "resource": "https://management.azure.com/",
  "token_type": "Bearer"
}
```

**Extract just the token:**

```bash
TOKEN=$(kubectl exec arm-test -- curl -s -H "Metadata: true" \
  "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fmanagement.azure.com%2F" \
  | jq -r '.access_token')
```

### 4.6 Perform the Cross-Subscription ARM Call

Use the token to call the ARM API for the ASG in the other subscription:

```bash
ASG_RESOURCE="/subscriptions/9b8218f9-902a-4d20-a65c-e98acec5362f/resourceGroups/asnPOC/providers/Microsoft.Network/applicationSecurityGroups/asg-frontend"

kubectl exec arm-test -- curl -s \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  "https://management.azure.com${ASG_RESOURCE}?api-version=2024-05-01"
```

**Successful response:**

```json
{
  "name": "asg-frontend",
  "id": "/subscriptions/9b8218f9-902a-4d20-a65c-e98acec5362f/resourceGroups/asnPOC/providers/Microsoft.Network/applicationSecurityGroups/asg-frontend",
  "etag": "W/\"afdd4689-83b9-4c8c-b4d0-f5991add40f8\"",
  "type": "Microsoft.Network/applicationSecurityGroups",
  "location": "westus3",
  "properties": {
    "provisioningState": "Succeeded"
  }
}
```

## 5. Applying This to pod-nsg-controller

The `pod-nsg-controller` uses `azidentity.NewDefaultAzureCredential(nil)` which automatically discovers credentials in this order:
1. Environment variables (Service Principal)
2. **Workload Identity** (if configured)
3. **Managed Identity** (IMDS — what we used above)
4. Azure CLI

For the controller to work cross-subscription:

1. **Set the target subscription** in the controller's environment:
   ```yaml
   env:
   - name: AZURE_SUBSCRIPTION_ID
     value: "9b8218f9-902a-4d20-a65c-e98acec5362f"    # Target subscription
   - name: AZURE_RESOURCE_GROUP
     value: "asnPOC"                                    # Target resource group
   ```

2. **Grant RBAC**: The VM's managed identity needs `Network Contributor` on the target resource group (for NIC + ASG operations):
   ```bash
   az role assignment create \
     --assignee-object-id $PRINCIPAL_ID \
     --assignee-principal-type ServicePrincipal \
     --role "Network Contributor" \
     --scope "/subscriptions/9b8218f9-902a-4d20-a65c-e98acec5362f/resourceGroups/asnPOC"
   ```

3. **Ensure IMDS MASQUERADE** is configured on all nodes where the controller pods may run.

4. **Deploy the controller** — `DefaultAzureCredential` will automatically use IMDS to get tokens.

> **Current limitation**: The controller takes a single `AZURE_SUBSCRIPTION_ID` and `AZURE_RESOURCE_GROUP`. Cross-subscription support (managing resources in multiple subscriptions simultaneously) is listed as a future consideration in [DESIGN.md](../docs/DESIGN.md). The current architecture requires one controller deployment per target subscription.

## 6. Validated Results

The following was validated on 2026-04-02 against the live cluster:

| Test | Result |
|---|---|
| Pod scheduled on CP node (`k8s-cp-01`) | ✅ `arm-test` running with IP `10.1.1.10` |
| IMDS reachable from pod | ✅ After MASQUERADE rule |
| Token acquisition via IMDS | ✅ 2046-char Bearer token obtained |
| ARM GET on cross-subscription ASG | ✅ Full ASG details returned |
| ASG `asg-frontend` in `westus3` readable | ✅ `provisioningState: Succeeded` |

**Identity chain**: Pod → IMDS (MASQUERADE to node IP) → Azure AD token → ARM API → RBAC check (Reader on ASG) → Success

## 7. Troubleshooting

### IMDS returns HTTP 410 "ResourceNotAvailable"

**Cause**: Pod is using a secondary IP (Azure CNI). IMDS only serves the VM's primary IP.

**Fix**: Add the MASQUERADE rule:
```bash
iptables -t nat -A POSTROUTING -d 169.254.169.254/32 -j MASQUERADE
```

### IMDS returns "curl: (7) Failed to connect"

**Cause**: Pod has no network path to 169.254.169.254.

**Fix**: Apply the Azure CNI bridge + L3 routing fix (see [cluster setup guide](self-managed-k8s-azure-cni-setup.md#5-networking-fix--azure-cni-transparent-bridge)).

### ARM returns HTTP 403 "AuthorizationFailed"

**Cause**: The managed identity lacks RBAC on the target resource.

**Fix**: Grant the appropriate role:
```bash
az role assignment create \
  --assignee-object-id <PRINCIPAL_ID> \
  --assignee-principal-type ServicePrincipal \
  --role "Reader" \
  --scope "<TARGET_RESOURCE_ID>"
```

Allow 1–5 minutes for RBAC propagation after creating the assignment.

### Token works for one subscription but not another

**Cause**: The token itself is subscription-agnostic (scoped to `https://management.azure.com/`). The issue is RBAC — the identity needs a role assignment in each target subscription.

### Pod can't resolve `management.azure.com`

**Cause**: CoreDNS or upstream DNS not working.

**Fix**: Ensure CoreDNS pods are running and the node's `systemd-resolved` is configured:
```bash
resolvectl dns azure0 168.63.129.16
resolvectl domain azure0 "~."
```
