# Transparent-Tunnel Same-Node NSG/ASG Enforcement — Test Guide

This document describes how to validate that Azure NSG rules backed by **Application
Security Groups (ASGs)** — whose membership is maintained by the **pod-nsg-controller**
— are enforced for **same-node** pod-to-pod traffic when the data plane uses the
Azure CNI **`transparent-tunnel`** mode. It is written as a step-by-step runbook: an
automation agent (or engineer) should be able to read it top to bottom and execute
every step without additional context.

## Table of Contents

- [1. Overview](#1-overview)
- [2. Prerequisites](#2-prerequisites)
- [3. Architecture](#3-architecture)
- [4. Step-by-Step Guide](#4-step-by-step-guide)
  - [4.1 Get the transparent-tunnel CNI binary onto all worker nodes](#41-get-the-transparent-tunnel-cni-binary-onto-all-worker-nodes)
  - [4.2 Check ASG membership](#42-check-asg-membership)
  - [4.3 Create two pods with ASG membership](#43-create-two-pods-with-asg-membership)
  - [4.4 Ping test](#44-ping-test)
  - [4.5 (Optional) TCP/8080 and packet-capture evidence](#45-optional-tcp8080-and-packet-capture-evidence)
- [5. Expected Results](#5-expected-results)
- [6. Cleanup](#6-cleanup)
- [7. Troubleshooting](#7-troubleshooting)

---

## 1. Overview

| Item | Value |
|---|---|
| **Resource Group / cluster** | `<rg_name>` (self-managed K8s on Azure VMs) |
| **Subscription** | `<sub>` |
| **Region** | `centraluseuap` (Central US EUAP) |
| **Nodes** | `<rg_name>-cp-01` (control plane, stock CNI by design) + `<rg_name>-worker-01..03` (workers, get transparent-tunnel) |
| **Subnet (NSG attached)** | `k8s-subnet` |
| **NSG** | `<rg_name>-nsg` |
| **CNI under test** | Azure CNI `mode=transparent-tunnel` (azure-container-networking, transparent-tunnel build) |
| **Pod namespace** | `default` |
| **Controller** | pod-nsg-controller (control plane, ns `pod-nsg-controller-system`) |

**What this validates**: With stock Azure CNI (`transparent` bridge mode), two pods
on the **same node** talk over the local `azure0` bridge — that traffic never reaches
the physical NIC, so the host **VFP (Virtual Filtering Platform)** never evaluates the
NSG/ASG rule and the segmentation silently does not apply. The `transparent-tunnel`
mode **closes that gap**: it is an **addition on top of the existing `transparent`
mode** that forces same-node pod-to-pod traffic out the host's primary NIC so VFP
evaluates the rules, then hairpins it back. This guide proves that, with
`transparent-tunnel` active, the controller-maintained ASG membership **is** enforced
on the same node.

## 2. Prerequisites

- A self-managed Kubernetes cluster on Azure VMs with the Azure CNI networking fix
  applied (see [self-managed-k8s-azure-cni-setup.md](self-managed-k8s-azure-cni-setup.md)).
- `az` CLI authenticated to the subscription (`az account set --subscription <sub>`).
- pod-nsg-controller deployed and `Synced` (see [multi-cluster-test-setup.md](multi-cluster-test-setup.md)).
- An NSG on `k8s-subnet` with the two rules in [§3](#3-architecture), and the ASGs
  `asg-backend` / `asg-frontend` with PodASGMappings selecting `app: backend` /
  `app: frontend` in namespace `default`.
- The transparent-tunnel CNI binary + conflist hosted at a URL the nodes can `curl`
  (see [4.1](#41-get-the-transparent-tunnel-cni-binary-onto-all-worker-nodes) — keep
  the URL a placeholder; do **not** commit a real SAS token to the repo).
- Node shell access. For this self-managed cluster the only path is
  `az vm run-command invoke` (no direct SSH). Define these helpers once and reuse them:

  ```bash
  RG=<rg_name>          # resource group (also the VM name prefix / cluster name)
  SUB=<sub>             # subscription id
  CP=${RG}-cp-01        # control-plane VM
  WORKERS="worker-01 worker-02 worker-03"

  run_on() {  # run_on <vm-name> <local-script-file>
    az vm run-command invoke -g "$RG" -n "$1" --subscription "$SUB" \
      --command-id RunShellScript --scripts @"$2" \
      --query 'value[0].message' -o tsv | sed 's/\\n/\n/g'
  }
  kubectl_cp() {  # run kubectl on the control plane
    az vm run-command invoke -g "$RG" -n "$CP" --subscription "$SUB" \
      --command-id RunShellScript \
      --scripts "KUBECONFIG=/etc/kubernetes/admin.conf kubectl $*" \
      --query 'value[0].message' -o tsv | sed 's/\\n/\n/g'
  }
  ```

## 3. Architecture

```
 STOCK CNI (transparent bridge)            TRANSPARENT-TUNNEL CNI
 ─────────────────────────────            ───────────────────────────────────────
 podA ─▶ azure0 (local bridge) ─▶ podB     podA ─▶ veth ─▶ eth0 ─▶ ┌─────────┐
        (never hits eth0/VFP)                                      │  Azure  │  NSG/ASG
        => NSG/ASG NOT enforced                                    │   VFP   │  rule eval
                                           podB ◀─ hairpin ◀───────┤(NSG/ASG)│  allow/deny
                                                                   └─────────┘
                                           => NSG/ASG ENFORCED on same node
```

`transparent-tunnel` is **policy routing only** — it keeps the normal transparent
setup and adds kernel rules (fwmark → `ip rule` → a dedicated route table → host
primary NIC → VFP → hairpin re-entry). There is **no encapsulation and no encryption**;
"tunnel" refers to steering the packet through the host NIC, not a VXLAN/IPsec tunnel.

**NSG rules under test:**

| Prio | Name | Action | Source | Dest | Proto/Port |
|---|---|---|---|---|---|
| 190 | `DenyBackendToFrontend` | **Deny** | ASG `asg-backend` | ASG `asg-frontend` | Any |
| 200 | `AllowFrontendToBackend` | Allow | ASG `asg-frontend` | ASG `asg-backend` | TCP 8080 |

> The deny is **directional**: `backend → frontend` is denied; the reverse is allowed.

**ASGs and controller mapping** (namespace `default`):

| ASG | PodASGMapping selector |
|---|---|
| `asg-backend` | `app: backend` |
| `asg-frontend` | `app: frontend` |

The controller publishes one `addressPrefixSet` per cluster into each ASG, named
`<cluster-rg-lowercase>-<namespace>-<mapping-name>`, e.g.
`<rg_name>-default-backend-asg-mapping`.

## 4. Step-by-Step Guide

### 4.1 Get the transparent-tunnel CNI binary onto all worker nodes

Replace the stock Azure CNI conflist with the `transparent-tunnel` conflist and drop
in the transparent-tunnel `azure-vnet` binary on **every worker node**, then restart
kubelet so new pods come up in `transparent-tunnel` mode. Leave the control-plane node
(`${RG}-cp-01`) on stock CNI — it hosts the controller and is intentionally excluded.

Point the nodes at the hosted artifacts (keep these as placeholders):

```bash
# PLACEHOLDER — replace with the real artifact location at run time.
# Do NOT commit a real SAS token / credential to this repo.
CNI_BINARY_URL="<CNI_BINARY_URL>"        # azure-vnet (transparent-tunnel build)
CNI_CONFLIST_URL="<CNI_CONFLIST_URL>"    # azure-linux-transparent-tunnel.conflist (mode=transparent-tunnel)
```

> The artifacts come from the azure-container-networking `transparent-tunnel` build:
> the `azure-vnet` CNI binary and `cni/azure-linux-transparent-tunnel.conflist`, which
> sets `"mode": "transparent-tunnel"`.

Save the per-node installer as `tt_install.sh`:

```bash
cat > tt_install.sh <<'EOF'
set -euo pipefail
CNI_BINARY_URL="${CNI_BINARY_URL:?set CNI_BINARY_URL}"
CNI_CONFLIST_URL="${CNI_CONFLIST_URL:?set CNI_CONFLIST_URL}"
TS="$(date +%Y%m%d-%H%M%S)"
BK="/opt/cni/tt-backup-${TS}"; mkdir -p "$BK"

# 1) Back up the current CNI binary + conflists (rollback path)
cp -a /opt/cni/bin/azure-vnet "$BK"/ 2>/dev/null || true
cp -a /etc/cni/net.d/*.conflist "$BK"/ 2>/dev/null || true
echo "backup at $BK"

# 2) Install the transparent-tunnel azure-vnet binary
curl -fsSL "$CNI_BINARY_URL" -o /opt/cni/bin/azure-vnet.new
chmod +x /opt/cni/bin/azure-vnet.new
mv /opt/cni/bin/azure-vnet.new /opt/cni/bin/azure-vnet

# 3) Swap the active conflist to the transparent-tunnel one
curl -fsSL "$CNI_CONFLIST_URL" -o /etc/cni/net.d/10-azure.conflist
grep -q '"mode": *"transparent-tunnel"' /etc/cni/net.d/10-azure.conflist \
  || { echo "ERROR: conflist is not transparent-tunnel"; exit 1; }

# 4) Restart kubelet so new sandboxes use the new mode
systemctl restart kubelet
echo "TT_INSTALL_OK mode=transparent-tunnel"
EOF
```

Run it on each worker:

```bash
for n in $WORKERS; do
  echo "=== installing TT on ${RG}-$n ==="
  # az vm run-command invoke's --scripts is single-valued (a repeated flag would
  # override, not append), so prepend the env exports into one combined payload.
  { echo "export CNI_BINARY_URL='$CNI_BINARY_URL' CNI_CONFLIST_URL='$CNI_CONFLIST_URL'"; cat tt_install.sh; } > tt_run.sh
  az vm run-command invoke -g "$RG" -n "${RG}-$n" --subscription "$SUB" \
    --command-id RunShellScript --scripts @tt_run.sh \
    --query 'value[0].message' -o tsv | sed 's/\\n/\n/g'
done
```

Verify the mode is active on every worker:

```bash
for n in $WORKERS; do
  echo "=== $n ==="
  az vm run-command invoke -g "$RG" -n "${RG}-$n" --subscription "$SUB" \
    --command-id RunShellScript \
    --scripts 'grep -o "\"mode\": *\"[a-z-]*\"" /etc/cni/net.d/*.conflist; systemctl is-active kubelet' \
    --query 'value[0].message' -o tsv | sed 's/\\n/\n/g'
# Expected per worker: "mode": "transparent-tunnel"   and   active
done
```

**Pass criteria:** every worker prints `"mode": "transparent-tunnel"`, `kubelet` is
`active`, and a `/opt/cni/tt-backup-<ts>` backup directory exists on each.

### 4.2 Check ASG membership

Confirm the controller has populated `asg-backend` and `asg-frontend` with pod IPs so
the NSG rule has something to match.

```bash
for asg in asg-backend asg-frontend; do
  echo "=== $asg ==="
  az rest --method get \
    --url "https://management.azure.com/subscriptions/$SUB/resourceGroups/$RG/providers/Microsoft.Network/applicationSecurityGroups/$asg/addressPrefixSets?api-version=2025-07-01" \
    --query 'value[].{name:name, prefixes:properties.addressPrefixes}' -o jsonc
done
```

Cross-check against the running pods:

```bash
kubectl_cp get pods -n default \
  -o custom-columns='NAME:.metadata.name,IP:.status.podIP,APP:.metadata.labels.app,NODE:.spec.nodeName'
```

**Pass criteria:** every `app=backend` pod IP appears as a `/32` in `asg-backend`'s
prefix set (e.g. `${RG}-default-backend-asg-mapping`), and every `app=frontend` pod IP
appears in `asg-frontend`'s.

> ⚠️ Do **not** use `az network nic list-effective-nsg` to verify membership — it
> renders ASG rules with raw ASG resource IDs, **not** the expanded member IPs (the
> `addressPrefixSets` membership is not rendered there). VFP still enforces correctly;
> effective-nsg is simply not a reliable view for APS-backed ASG membership.

### 4.3 Create two pods with ASG membership

Create one `backend` pod and one `frontend` pod **on the same worker node** so the
flow under test is genuinely same-node.

```bash
NODE=${RG}-worker-01   # host both pods on one TT worker

kubectl_cp run backend-tt  --image=busybox --labels=app=backend \
  --overrides='{"spec":{"nodeName":"'"$NODE"'"}}' --command -- sleep infinity
kubectl_cp run frontend-tt --image=busybox --labels=app=frontend \
  --overrides='{"spec":{"nodeName":"'"$NODE"'"}}' --command -- sleep infinity
```

> The `app=backend` / `app=frontend` labels are what the PodASGMappings select, so the
> controller will place each pod's IP into the matching ASG.

Wait for `Running` and capture IPs:

```bash
kubectl_cp get pods backend-tt frontend-tt -n default \
  -o custom-columns='NAME:.metadata.name,IP:.status.podIP,APP:.metadata.labels.app,NODE:.spec.nodeName,STATUS:.status.phase'
```

**Pass criteria:** both pods `Running` and on the **same `NODE`**. Record the two IPs
as `BACKEND_IP` and `FRONTEND_IP`, then re-run the REST check from
[4.2](#42-check-asg-membership) and confirm both new IPs landed in their ASGs (allow
~60s for the controller to reconcile).

### 4.4 Ping test

Prove the deny is enforced on the same node: `backend → frontend` is blocked, the
reverse is allowed, and a no-ASG control pod is allowed.

```bash
# A) BLOCKED path — backend -> frontend
kubectl_cp exec backend-tt -n default -- ping -c 4 -W 2 <FRONTEND_IP>
# Expected: 100% packet loss  -> DENIED by rule 190 DenyBackendToFrontend

# B) ALLOWED path — frontend -> backend
kubectl_cp exec frontend-tt -n default -- ping -c 4 -W 2 <BACKEND_IP>
# Expected: 0% packet loss  -> ALLOWED (no deny applies in this direction)
```

(Recommended) Control pod in **no** ASG — isolates the cause to ASG membership rather
than a transparent-tunnel artifact:

```bash
kubectl_cp run tt-canary --image=busybox \
  --overrides='{"spec":{"nodeName":"'"$NODE"'"}}' --command -- sleep infinity
# wait for Running, then:
kubectl_cp exec tt-canary -n default -- ping -c 4 -W 2 <FRONTEND_IP>
# Expected: 0% packet loss  -> ALLOWED (a non-member reaches the frontend)
```

### 4.5 (Optional) TCP/8080 and packet-capture evidence

For airtight, go-live-grade evidence, capture on the host's physical NIC (`eth0`)
while driving the flows. With `transparent-tunnel`, same-node traffic appears on
`eth0`; with stock CNI it would not.

```bash
# On the node hosting both pods, find each pod's host veth:
ip route get <BACKEND_IP>    # -> dev azv...  (backend veth)
ip route get <FRONTEND_IP>   # -> dev azv...  (frontend veth)

# Detached capture that survives the run-command session (90s window):
setsid bash -c 'timeout 90 tcpdump -i eth0 -w /tmp/tt-eth0.pcap "icmp or tcp port 8080"' \
  </dev/null >/dev/null 2>&1 &
```

Then drive TCP from the control plane (busybox has no listener, so a reachable pod
replies with RST):

```bash
kubectl_cp exec backend-tt  -n default -- nc -w 4 <FRONTEND_IP> 8080   # expect timeout  -> BLOCKED
kubectl_cp exec frontend-tt -n default -- nc -w 4 <BACKEND_IP>  8080   # expect fast RST -> ALLOWED
```

Read back with `tcpdump -tttt -nr /tmp/tt-eth0.pcap`. Expected signatures:

- **Blocked** `backend → frontend`: only the outbound `echo request` / TCP `[S]` SYN —
  **no reply of any kind** (no echo reply, no SYN-ACK, no RST). VFP dropped it.
- **Allowed** `frontend → backend`: full `echo request`+`reply`, or TCP `[S]` → `[R.]`
  (the RST returned *from the pod* proves the SYN was delivered).

## 5. Expected Results

A passing run looks like the following. (`<BACKEND_IP>` / `<FRONTEND_IP>` are the
same-node pod IPs from [4.3](#43-create-two-pods-with-asg-membership); the control pod
is in no ASG.)

| Flow (same node) | Source ASG | ICMP | TCP :8080 | Verdict |
|---|---|---|---|---|
| `backend-tt → frontend-tt` | `asg-backend` | 100% loss | SYN, no reply | **DENIED** (rule 190) |
| `frontend-tt → backend-tt` | `asg-frontend` | 0% loss | SYN → RST | ALLOWED |
| `tt-canary → frontend-tt` | none (control) | 0% loss | SYN → RST | ALLOWED (control) |

Expected `eth0` (physical NIC) capture — what each verdict looks like:

```
# DENIED  backend -> frontend  — request leaves the node, NOTHING comes back
<BACKEND_IP> > <FRONTEND_IP>  ICMP echo request        (x4, no echo reply)
<BACKEND_IP>.* > <FRONTEND_IP>.8080  Flags [S]         (SYN, no SYN-ACK, no RST)
        *** packet dropped at VFP ***

# ALLOWED  frontend -> backend  — request is delivered and answered
<FRONTEND_IP> > <BACKEND_IP>  ICMP echo request  +  echo reply
<FRONTEND_IP>.* > <BACKEND_IP>.8080  Flags [S]
<BACKEND_IP>.8080 > <FRONTEND_IP>.*  Flags [R.]        (RST proves SYN reached the pod)
```

**Why this is conclusive:** the denied path shows the request leaving on the physical
NIC but receiving no reply of any kind (a pure VFP drop), while the allowed paths
complete (echo reply, or a RST returned from the pod proving delivery). The **only**
difference between the denied flow and the allowed control flow is **ASG membership** —
confirming genuine VFP/ASG enforcement on the same node, made possible by
`transparent-tunnel`. With stock CNI, none of this same-node traffic would ever appear
on `eth0`, and the deny would not apply.

## 6. Cleanup

```bash
kubectl_cp delete pod backend-tt frontend-tt tt-canary -n default --ignore-not-found

# To revert a node to stock CNI, restore from the backup created in 4.1:
#   cp -a /opt/cni/tt-backup-<ts>/azure-vnet /opt/cni/bin/azure-vnet
#   cp -a /opt/cni/tt-backup-<ts>/*.conflist /etc/cni/net.d/
#   systemctl restart kubelet
```

## 7. Troubleshooting

### Same-node traffic is not blocked (deny does not apply)

**Cause**: The node is still on stock CNI, so same-node traffic stays on the `azure0`
bridge and never reaches VFP.

**Fix**: Re-run [4.1](#41-get-the-transparent-tunnel-cni-binary-onto-all-worker-nodes)
and confirm the conflist reports `"mode": "transparent-tunnel"`. New pods must be
created *after* the kubelet restart to pick up the new mode.

### `list-effective-nsg` shows the ASG but no member IPs

**Cause**: `az network nic list-effective-nsg` renders ASG rules with raw resource IDs
and does not expand `addressPrefixSets` membership.

**Fix**: Verify membership with the REST `addressPrefixSets` GET in
[4.2](#42-check-asg-membership). VFP enforces regardless of what effective-nsg shows.

### Pod IPs do not appear in the ASG prefix set

**Cause**: The controller has not reconciled yet, or the pod labels don't match the
PodASGMapping selector.

**Fix**: Confirm the pod labels (`app=backend` / `app=frontend`), wait ~60s, and check
the controller logs:
```bash
kubectl_cp logs -n pod-nsg-controller-system -l app.kubernetes.io/name=pod-nsg-controller --tail=50
```

### Manual ASG prefix-set edits keep reverting

**Cause**: The controller reconciles continuously and overwrites manual edits within
~60s.

**Fix**: Only edit prefix sets manually if you scale the controller to 0 first; revert
the scale afterward. For this test you should not need manual edits — let the
controller populate membership from the pod labels.

### Both pods landed on different nodes

**Cause**: `nodeName` was not set, so the scheduler placed the pods on different nodes —
this no longer tests *same-node* enforcement.

**Fix**: Recreate both pods pinned to the same `NODE` (see
[4.3](#43-create-two-pods-with-asg-membership)) and confirm the `NODE` column matches.
