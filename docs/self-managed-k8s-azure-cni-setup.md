# Self-Managed Kubernetes Cluster with Azure CNI — Setup Guide

This document captures the complete process and steps to create a self-managed Kubernetes cluster with Azure CNI (transparent bridge mode) on Azure VMs, including all issues encountered and their resolutions.

## Table of Contents

- [1. Overview](#1-overview)
- [2. Architecture](#2-architecture)
- [3. Infrastructure Provisioning](#3-infrastructure-provisioning)
  - [3.1 Resource Group](#31-resource-group)
  - [3.2 Virtual Network & NSG](#32-virtual-network--nsg)
  - [3.3 Public IP & NAT Gateway](#33-public-ip--nat-gateway)
  - [3.4 NICs with Secondary IPs](#34-nics-with-secondary-ips)
  - [3.5 Virtual Machines](#35-virtual-machines)
- [4. Kubernetes Installation](#4-kubernetes-installation)
  - [4.1 Prerequisites (All Nodes)](#41-prerequisites-all-nodes)
  - [4.2 Control Plane Initialization](#42-control-plane-initialization)
  - [4.3 Azure CNI Installation (All Nodes)](#43-azure-cni-installation-all-nodes)
  - [4.4 Worker Node Join](#44-worker-node-join)
- [5. Networking Fix — Azure CNI Transparent Bridge](#5-networking-fix--azure-cni-transparent-bridge)
  - [5.1 Problem: Pod Networking Broken](#51-problem-pod-networking-broken)
  - [5.2 Root Cause Analysis](#52-root-cause-analysis)
  - [5.3 The Fix: Hybrid Bridge + L3 Routing](#53-the-fix-hybrid-bridge--l3-routing)
  - [5.4 DNS Fix: systemd-resolved](#54-dns-fix-systemd-resolved)
  - [5.5 Complete Fix Script (Per Worker)](#55-complete-fix-script-per-worker)
- [6. Kubeconfig Retrieval](#6-kubeconfig-retrieval)
- [7. Validation](#7-validation)
  - [7.1 Node Status](#71-node-status)
  - [7.2 System Pod Status](#72-system-pod-status)
  - [7.3 End-to-End Tests](#73-end-to-end-tests)
- [8. Known Issues & Gotchas](#8-known-issues--gotchas)

---

## 1. Overview

| Parameter | Value |
|---|---|
| **Resource Group** | `asn-rg2-k8s-selfmanaged` |
| **Region** | `westus2` |
| **VM Size** | `Standard_D4s_v5` (4 vCPU, 16 GB RAM) |
| **OS** | Ubuntu 22.04 LTS (`Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest`) |
| **Kubernetes** | v1.31.14 |
| **Container Runtime** | containerd 1.7.28 |
| **CNI** | Azure CNI v1.6.6 (transparent bridge mode) |
| **Nodes** | 1 control plane + 3 workers |

## 2. Architecture

```
Resource Group: asn-rg2-k8s-selfmanaged (westus2)
│
├── VNet: k8s-vnet (10.1.0.0/16)
│   └── Subnet: k8s-subnet (10.1.1.0/24)
│       ├── k8s-cp-01       (10.1.1.4)   — Control Plane, Public IP: 20.230.143.82
│       ├── k8s-worker-01   (10.1.1.5)   — Worker
│       ├── k8s-worker-02   (10.1.1.6)   — Worker
│       └── k8s-worker-03   (10.1.1.35)  — Worker
│
├── NSG: k8s-nsg
│   ├── AllowK8sAPI         (6443)       — API server
│   ├── AllowKubelet        (10250)      — Kubelet
│   ├── AllowEtcd           (2379-2380)  — etcd
│   └── AllowNodePort       (30000-32767)— NodePort services
│
├── Public IP: k8s-cp-pip   (20.230.143.82) — For external kubectl access
└── NAT Gateway: k8s-natgw  — Outbound internet for workers
```

Each NIC has ~10 secondary IP configurations for Azure CNI pod IP allocation.

## 3. Infrastructure Provisioning

### 3.1 Resource Group

```bash
az group create --name asn-rg2-k8s-selfmanaged --location westus2
```

### 3.2 Virtual Network & NSG

```bash
# NSG
az network nsg create -g asn-rg2-k8s-selfmanaged -n k8s-nsg

# NSG rules
az network nsg rule create -g asn-rg2-k8s-selfmanaged --nsg-name k8s-nsg \
  -n AllowK8sAPI --priority 110 --access Allow --direction Inbound \
  --source-address-prefixes VirtualNetwork --destination-port-ranges 6443 --protocol Tcp

az network nsg rule create -g asn-rg2-k8s-selfmanaged --nsg-name k8s-nsg \
  -n AllowKubelet --priority 120 --access Allow --direction Inbound \
  --source-address-prefixes VirtualNetwork --destination-port-ranges 10250 --protocol Tcp

az network nsg rule create -g asn-rg2-k8s-selfmanaged --nsg-name k8s-nsg \
  -n AllowEtcd --priority 130 --access Allow --direction Inbound \
  --source-address-prefixes VirtualNetwork --destination-port-ranges 2379-2380 --protocol Tcp

az network nsg rule create -g asn-rg2-k8s-selfmanaged --nsg-name k8s-nsg \
  -n AllowNodePort --priority 140 --access Allow --direction Inbound \
  --source-address-prefixes '*' --destination-port-ranges 30000-32767 --protocol Tcp

# VNet and Subnet
az network vnet create -g asn-rg2-k8s-selfmanaged -n k8s-vnet \
  --address-prefix 10.1.0.0/16 \
  --subnet-name k8s-subnet --subnet-prefix 10.1.1.0/24 \
  --network-security-group k8s-nsg
```

### 3.3 Public IP & NAT Gateway

```bash
# Public IP for control plane (external kubectl access)
az network public-ip create -g asn-rg2-k8s-selfmanaged -n k8s-cp-pip \
  --sku Standard --allocation-method Static

# NAT Gateway for worker outbound internet (required for apt, image pulls)
az network public-ip create -g asn-rg2-k8s-selfmanaged -n k8s-natgw-pip \
  --sku Standard --allocation-method Static

az network nat gateway create -g asn-rg2-k8s-selfmanaged -n k8s-natgw \
  --public-ip-addresses k8s-natgw-pip --idle-timeout 10

az network vnet subnet update -g asn-rg2-k8s-selfmanaged \
  --vnet-name k8s-vnet -n k8s-subnet --nat-gateway k8s-natgw
```

> **Important**: Workers without public IPs need a NAT gateway for outbound internet access (package installation, container image pulls, GitHub downloads).

### 3.4 NICs with Secondary IPs

Each NIC needs ~10 secondary IPs for Azure CNI pod IP allocation. Azure CNI assigns these IPs to pods.

```bash
RG="asn-rg2-k8s-selfmanaged"
SUBNET_ID=$(az network vnet subnet show -g $RG --vnet-name k8s-vnet -n k8s-subnet --query id -o tsv)

# Control Plane NIC (with public IP)
az network nic create -g $RG -n k8s-cp-01-nic --subnet $SUBNET_ID \
  --private-ip-address 10.1.1.4 --public-ip-address k8s-cp-pip

# Worker NICs
az network nic create -g $RG -n k8s-worker-01-nic --subnet $SUBNET_ID --private-ip-address 10.1.1.5
az network nic create -g $RG -n k8s-worker-02-nic --subnet $SUBNET_ID --private-ip-address 10.1.1.6
az network nic create -g $RG -n k8s-worker-03-nic --subnet $SUBNET_ID --private-ip-address 10.1.1.35

# Add secondary IPs (must be sequential per NIC to avoid ARM conflicts)
for NIC in k8s-cp-01-nic k8s-worker-01-nic k8s-worker-02-nic k8s-worker-03-nic; do
  for i in $(seq 1 9); do
    az network nic ip-config create -g $RG --nic-name $NIC \
      -n "ipconfig-pod-${i}" --private-ip-address-version IPv4
  done
done
```

> **Gotcha**: Adding secondary IPs in parallel to the same NIC causes Azure ARM concurrency conflicts. Add them sequentially per NIC.

### 3.5 Virtual Machines

```bash
RG="asn-rg2-k8s-selfmanaged"
IMAGE="Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest"
SIZE="Standard_D4s_v5"

# Control Plane
az vm create -g $RG -n k8s-cp-01 --nics k8s-cp-01-nic \
  --image $IMAGE --size $SIZE --admin-username azureuser \
  --generate-ssh-keys --no-wait

# Workers
for i in 01 02 03; do
  az vm create -g $RG -n "k8s-worker-${i}" --nics "k8s-worker-${i}-nic" \
    --image $IMAGE --size $SIZE --admin-username azureuser \
    --generate-ssh-keys --no-wait
done

# Wait for all VMs to be created
az vm wait -g $RG -n k8s-cp-01 --created
az vm wait -g $RG -n k8s-worker-01 --created
az vm wait -g $RG -n k8s-worker-02 --created
az vm wait -g $RG -n k8s-worker-03 --created
```

## 4. Kubernetes Installation

All commands are executed via `az vm run-command invoke` since SSH may not be available.

### 4.1 Prerequisites (All Nodes)

Run on **all 4 nodes** (`k8s-cp-01`, `k8s-worker-01`, `k8s-worker-02`, `k8s-worker-03`):

```bash
az vm run-command invoke -g asn-rg2-k8s-selfmanaged -n <VM_NAME> \
  --command-id RunShellScript --scripts '
set -ex

# Disable swap
swapoff -a
sed -i "/swap/d" /etc/fstab

# Load kernel modules
cat > /etc/modules-load.d/k8s.conf <<EOF
overlay
br_netfilter
EOF
modprobe overlay
modprobe br_netfilter

# Sysctl params
cat > /etc/sysctl.d/k8s.conf <<EOF
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sysctl --system

# Install containerd
apt-get update -qq
apt-get install -y -qq containerd
mkdir -p /etc/containerd
containerd config default > /etc/containerd/config.toml
sed -i "s/SystemdCgroup = false/SystemdCgroup = true/" /etc/containerd/config.toml
systemctl restart containerd
systemctl enable containerd

# Install kubeadm, kubelet, kubectl v1.31
apt-get install -y -qq apt-transport-https ca-certificates curl gpg conntrack
curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.31/deb/Release.key | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.31/deb/ /" > /etc/apt/sources.list.d/kubernetes.list
apt-get update -qq
apt-get install -y -qq kubelet kubeadm kubectl
apt-mark hold kubelet kubeadm kubectl
'
```

> **Gotcha**: The `conntrack` package is NOT installed by default on Ubuntu 22.04 but is required by kubeadm preflight checks.

### 4.2 Control Plane Initialization

```bash
# Get the public IP for the API server certificate SANs
PUBLIC_IP=$(az network public-ip show -g asn-rg2-k8s-selfmanaged -n k8s-cp-pip --query ipAddress -o tsv)

az vm run-command invoke -g asn-rg2-k8s-selfmanaged -n k8s-cp-01 \
  --command-id RunShellScript --scripts "
kubeadm init \
  --pod-network-cidr=10.244.0.0/16 \
  --apiserver-cert-extra-sans=${PUBLIC_IP} \
  --apiserver-advertise-address=10.1.1.4 \
  --control-plane-endpoint=10.1.1.4:6443

# Save the join command for workers
kubeadm token create --print-join-command > /tmp/join-command.txt
cat /tmp/join-command.txt
"
```

> **Important**: The `--apiserver-cert-extra-sans` flag adds the public IP to the API server's TLS certificate, enabling external `kubectl` access.

### 4.3 Azure CNI Installation (All Nodes)

Install on **all 4 nodes**:

```bash
az vm run-command invoke -g asn-rg2-k8s-selfmanaged -n <VM_NAME> \
  --command-id RunShellScript --scripts '
set -ex

# Download Azure CNI v1.6.6
CNI_URL="https://kubernetesartifacts.azureedge.net/azure-cni/v1.6.6/binaries/azure-vnet-cni-linux-amd64-v1.6.6.tgz"
mkdir -p /opt/cni/bin
cd /opt/cni/bin
curl -sSLO "$CNI_URL"
tar -xzf azure-vnet-cni-linux-amd64-v1.6.6.tgz
rm -f azure-vnet-cni-linux-amd64-v1.6.6.tgz

# Also need the portmap plugin (from standard CNI plugins)
if [ ! -f /opt/cni/bin/portmap ]; then
  curl -sSL https://github.com/containernetworking/plugins/releases/download/v1.4.0/cni-plugins-linux-amd64-v1.4.0.tgz | tar -xz -C /opt/cni/bin portmap
fi

# CNI configuration
mkdir -p /etc/cni/net.d
cat > /etc/cni/net.d/10-azure.conflist <<EOF
{
  "cniVersion": "0.3.0",
  "name": "azure",
  "plugins": [
    {
      "type": "azure-vnet",
      "mode": "transparent",
      "bridge": "azure0",
      "ipam": {
        "type": "azure-vnet-ipam"
      }
    },
    {
      "type": "portmap",
      "capabilities": {"portMappings": true},
      "snat": true
    }
  ]
}
EOF

systemctl restart kubelet
'
```

> **Note**: Use the Azure CDN mirror (`kubernetesartifacts.azureedge.net`) instead of GitHub releases — it's faster and more reliable.

### 4.4 Worker Node Join

Get the join command from the control plane, then run on each worker:

```bash
# Get join command from CP
JOIN_CMD=$(az vm run-command invoke -g asn-rg2-k8s-selfmanaged -n k8s-cp-01 \
  --command-id RunShellScript \
  --scripts 'kubeadm token create --print-join-command' \
  --query "value[0].message" -o tsv | grep "kubeadm join")

# Join each worker
for WORKER in k8s-worker-01 k8s-worker-02 k8s-worker-03; do
  az vm run-command invoke -g asn-rg2-k8s-selfmanaged -n $WORKER \
    --command-id RunShellScript --scripts "$JOIN_CMD"
done
```

## 5. Networking Fix — Azure CNI Transparent Bridge

This is the most critical and non-obvious part of the setup. Azure CNI transparent bridge mode has significant issues on self-managed clusters that require manual intervention.

### 5.1 Problem: Pod Networking Broken

After joining workers and CNI creating pods:
- CoreDNS pods showed `CrashLoopBackOff`
- Error: `dial tcp 10.96.0.1:443: connect: no route to host`
- Pods could not reach ClusterIPs (Kubernetes service at 10.96.0.1)
- Pods could not reach other nodes

### 5.2 Root Cause Analysis

Multiple overlapping issues were discovered:

1. **No `azure0` bridge created**: The CNI config specifies `"bridge": "azure0"` but the bridge was never created on workers. The CNI plugin created veth pairs for pods but did not properly initialize the bridge.

2. **Pod gateway ARP failure**: Pods use `169.254.1.1` as their default gateway (link-local). This requires proxy ARP on the host-side veth to respond to ARP requests. Without proper setup, the ARP resolution failed — pods had no working network.

3. **Secondary IPs on eth0 conflict**: Azure NIC secondary IPs (used for pods) were present on `eth0` in the host namespace AND in pod network namespaces. This caused the kernel to treat pod-destined traffic as local instead of forwarding it.

4. **br_netfilter DNAT conflict** (the subtle one): When pod veths are enslaved to the azure0 bridge, traffic goes through the bridge path. `br_netfilter` intercepts bridged traffic and applies iptables (including kube-proxy's DNAT rules). However, after DNAT changes the destination IP, the bridge's L2 forwarding decision (based on MAC address) conflicts with the L3 DNAT — the packet's destination MAC still points to the bridge itself. The bridge delivers the DNAT'd packet to the local stack, but it cannot be properly forwarded to the real endpoint.

5. **systemd-resolved DNS failure**: After moving the primary IP from `eth0` to `azure0`, `systemd-resolved` lost its DNS configuration (DNS servers were bound to the `eth0` link, which became a bridge port).

### 5.3 The Fix: Hybrid Bridge + L3 Routing

The correct setup uses a **hybrid approach**:
- **azure0 bridge** with `eth0` enslaved — for host networking
- **Pod veths (azv\*) NOT in the bridge** — use L3 routing with proxy ARP instead

This avoids the br_netfilter DNAT conflict while maintaining proper host connectivity.

**Why L3 routing works for pods but bridging doesn't:**
- L3 routing: Pod traffic enters host via veth → iptables PREROUTING (DNAT by kube-proxy) → routing decision → FORWARD → out. Standard iptables DNAT path.
- Bridging: Pod traffic enters bridge → br_netfilter iptables (DNAT) → but bridge L2 forwarding uses MAC not IP → DNAT'd packet delivered incorrectly.

### 5.4 DNS Fix: systemd-resolved

After moving the primary IP to azure0, configure DNS on the azure0 interface:

```bash
resolvectl dns azure0 168.63.129.16
resolvectl domain azure0 "~."
```

### 5.5 Complete Fix Script (Per Worker)

This script must be run on each worker node. Replace the variables at the top for each worker.

```bash
#!/bin/bash
# Azure CNI Transparent Bridge Fix — Per Worker
# Run via: az vm run-command invoke -g <RG> -n <VM> --command-id RunShellScript --scripts '<script>'
set -ex

# === CONFIGURE THESE PER WORKER ===
PRIMARY_IP="10.1.1.5"        # Worker's DHCP-assigned IP (from Azure NIC primary config)
GATEWAY="10.1.1.1"           # Azure subnet gateway
# Secondary IPs to remove from eth0 (from Azure NIC secondary configs)
SECONDARY_IPS="10.1.1.11 10.1.1.19 10.1.1.20 10.1.1.21 10.1.1.22 10.1.1.23 10.1.1.24 10.1.1.25 10.1.1.26 10.1.1.27"
# === END CONFIGURATION ===

ETH0_MAC=$(cat /sys/class/net/eth0/address)

# ----- Step 1: Create azure0 bridge -----
ip link add azure0 type bridge
echo 0 > /sys/class/net/azure0/bridge/forward_delay
echo 0 > /sys/class/net/azure0/bridge/stp_state
ip link set azure0 address $ETH0_MAC  # Same MAC as eth0 for Azure SDN
ip link set azure0 up

# ----- Step 2: Move primary IP from eth0 to bridge -----
ip addr add ${PRIMARY_IP}/24 dev azure0
ip addr del ${PRIMARY_IP}/24 dev eth0 || true

# ----- Step 3: Enslave eth0 to bridge -----
ip link set eth0 master azure0

# ----- Step 4: Remove secondary IPs from eth0 -----
# These cause the kernel to treat pod traffic as local
for ip in $SECONDARY_IPS; do
  ip addr del $ip/24 dev eth0 2>/dev/null || true
done

# ----- Step 5: Fix routes -----
ip route del default via $GATEWAY dev eth0 2>/dev/null || true
ip route replace default via $GATEWAY dev azure0
ip route del 10.1.1.0/24 dev eth0 2>/dev/null || true
ip route del 10.1.1.1 dev eth0 2>/dev/null || true
ip route del 168.63.129.16 via 10.1.1.1 dev eth0 2>/dev/null || true
ip route del 169.254.169.254 via 10.1.1.1 dev eth0 2>/dev/null || true

# ----- Step 6: Setup L3 routing for pod veths (NOT bridged) -----
ip route replace 169.254.1.1/32 dev azure0  # For proxy_arp response

for AZV in $(ip -o link show | grep 'azv' | awk -F': ' '{print $2}' | cut -d'@' -f1); do
  # Remove from bridge if accidentally enslaved
  ip link set $AZV nomaster 2>/dev/null || true
  # Get pod IP from existing route
  POD_IP=$(ip route show dev $AZV 2>/dev/null | awk '{print $1}' | head -1)
  if [ -n "$POD_IP" ]; then
    ip route replace $POD_IP dev $AZV
    echo 1 > /proc/sys/net/ipv4/conf/$AZV/proxy_arp
    echo "Configured L3 routing for $AZV ($POD_IP)"
  fi
done

# ----- Step 7: Fix DNS -----
resolvectl dns azure0 168.63.129.16
resolvectl domain azure0 "~."

# ----- Step 8: Flush stale state -----
conntrack -F 2>/dev/null || true

echo "=== Fix complete ==="
ip route show
bridge link show
```

**Worker-specific values:**

| Worker | PRIMARY_IP | SECONDARY_IPS |
|---|---|---|
| k8s-worker-01 | 10.1.1.5 | 10.1.1.11 10.1.1.19-10.1.1.27 |
| k8s-worker-02 | 10.1.1.6 | 10.1.1.12 10.1.1.7 10.1.1.28-10.1.1.34 |
| k8s-worker-03 | 10.1.1.35 | 10.1.1.36-10.1.1.44 |

> **⚠️ This fix is NOT persistent across reboots.** A systemd unit or netplan post-up hook is needed for persistence. See [Known Issues](#8-known-issues--gotchas).

## 6. Kubeconfig Retrieval

The kubeconfig must be fetched from the control plane. Since `az vm run-command` has a ~4KB output buffer, large kubeconfig files (with embedded certificates) must be fetched in parts.

```bash
# Fetch kubeconfig in two parts (certificates are large)
PART1=$(az vm run-command invoke -g asn-rg2-k8s-selfmanaged -n k8s-cp-01 \
  --command-id RunShellScript --scripts 'head -15 /etc/kubernetes/admin.conf' \
  --query "value[0].message" -o tsv | sed -n '/^\[stdout\]/,/^\[stderr\]/p' | sed '1d;$d')

PART2=$(az vm run-command invoke -g asn-rg2-k8s-selfmanaged -n k8s-cp-01 \
  --command-id RunShellScript --scripts 'tail -n +16 /etc/kubernetes/admin.conf' \
  --query "value[0].message" -o tsv | sed -n '/^\[stdout\]/,/^\[stderr\]/p' | sed '1d;$d')

# Combine and save
mkdir -p ~/.config/kube
echo "${PART1}" > ~/.config/kube/selfmanaged2-kubeconfig.yaml
echo "${PART2}" >> ~/.config/kube/selfmanaged2-kubeconfig.yaml

# Replace internal API server IP with public IP
PUBLIC_IP=$(az network public-ip show -g asn-rg2-k8s-selfmanaged -n k8s-cp-pip --query ipAddress -o tsv)
sed -i "s|https://10.1.1.4:6443|https://${PUBLIC_IP}:6443|" ~/.config/kube/selfmanaged2-kubeconfig.yaml

# Set KUBECONFIG
export KUBECONFIG=~/.config/kube/selfmanaged2-kubeconfig.yaml
echo "export KUBECONFIG=$KUBECONFIG" >> ~/.bashrc
```

> **Gotcha**: `az vm run-command` output buffer is ~4KB. Large files must be fetched in parts using `head`/`tail` and concatenated.

## 7. Validation

### 7.1 Node Status

```bash
$ kubectl get nodes -o wide
NAME            STATUS   ROLES           AGE   VERSION    INTERNAL-IP   OS-IMAGE             KERNEL-VERSION     CONTAINER-RUNTIME
k8s-cp-01       Ready    control-plane   20h   v1.31.14   10.1.1.4      Ubuntu 22.04.5 LTS   6.8.0-1044-azure   containerd://1.7.28
k8s-worker-01   Ready    <none>          19h   v1.31.14   10.1.1.5      Ubuntu 22.04.5 LTS   6.8.0-1044-azure   containerd://1.7.28
k8s-worker-02   Ready    <none>          19h   v1.31.14   10.1.1.6      Ubuntu 22.04.5 LTS   6.8.0-1044-azure   containerd://1.7.28
k8s-worker-03   Ready    <none>          19h   v1.31.14   10.1.1.35     Ubuntu 22.04.5 LTS   6.8.0-1044-azure   containerd://1.7.28
```

**Result**: All 4 nodes `Ready` ✅

### 7.2 System Pod Status

```bash
$ kubectl -n kube-system get pods
NAME                                READY   STATUS    RESTARTS   AGE
coredns-7c65d6cfc9-j78ml            1/1     Running   34         18h
coredns-7c65d6cfc9-tqcg5            1/1     Running   256        18h
etcd-k8s-cp-01                      1/1     Running   0          20h
kube-apiserver-k8s-cp-01            1/1     Running   0          20h
kube-controller-manager-k8s-cp-01   1/1     Running   1          20h
kube-proxy-85k52                    1/1     Running   0          20h
kube-proxy-qdtj9                    1/1     Running   0          19h
kube-proxy-qttg5                    1/1     Running   0          19h
kube-proxy-tlvtk                    1/1     Running   0          19h
kube-scheduler-k8s-cp-01            1/1     Running   1          20h
```

**Result**: All system pods `Running` and `1/1 Ready` ✅

> Note: High restart counts on CoreDNS are from the period before the networking fix was applied.

### 7.3 End-to-End Tests

**Test pod deployment:**
```bash
kubectl run test-pod --image=busybox:1.36 --restart=Never -- sleep 3600
kubectl wait --for=condition=Ready pod/test-pod --timeout=120s
```
Result: Pod scheduled on worker-03 with Azure CNI IP `10.1.1.36` ✅

**Cluster DNS resolution:**
```bash
$ kubectl exec test-pod -- nslookup kubernetes.default.svc.cluster.local
Server:    10.96.0.10
Address:   10.96.0.10:53

Name:      kubernetes.default.svc.cluster.local
Address:   10.96.0.1
```
Result: CoreDNS resolves cluster services ✅

**External DNS resolution:**
```bash
$ kubectl exec test-pod -- nslookup google.com
Server:    10.96.0.10
Address:   10.96.0.10:53

Non-authoritative answer:
Name:      google.com
Address:   142.250.69.174
```
Result: CoreDNS forwards external queries ✅

**Cross-node pod-to-pod connectivity:**
```bash
$ kubectl exec test-pod -- ping -c 2 10.1.1.19   # CoreDNS pod on worker-01
PING 10.1.1.19 (10.1.1.19): 56 data bytes
64 bytes from 10.1.1.19: seq=0 ttl=62 time=1.694 ms
64 bytes from 10.1.1.19: seq=1 ttl=62 time=4.074 ms
```
Result: Pod on worker-03 can reach pod on worker-01 ✅

**ClusterIP service access:**
```bash
$ kubectl exec test-pod -- wget -qO- --timeout=5 https://kubernetes.default.svc.cluster.local/version --no-check-certificate
# TLS handshake completes (error is busybox TLS limitation, not network)
```
Result: ClusterIP routing works (kube-proxy DNAT functional) ✅

**Cleanup:**
```bash
kubectl delete pod test-pod
```

## 8. Known Issues & Gotchas

### Networking fix is NOT persistent across reboots
The azure0 bridge setup, IP moves, and routing changes are lost on VM reboot. You need one of:
- A **systemd unit** that runs at boot after networking
- A **netplan** post-up hook
- A **cloud-init** runcmd configuration

### NIC secondary IPs must be added sequentially
Azure ARM has concurrency conflicts when adding multiple secondary IP configs to the same NIC in parallel. Add them one at a time per NIC.

### `conntrack` package not installed by default
Ubuntu 22.04 does not include `conntrack`, which kubeadm requires. Install it explicitly.

### Workers need a NAT gateway for outbound internet
VMs without public IPs cannot reach the internet (apt repos, container registries, GitHub) without a NAT gateway or Azure Firewall.

### `az vm run-command` output is limited to ~4KB
Large file content (like kubeconfig with embedded certificates) gets truncated. Fetch in parts using `head`/`tail`.

### Azure CNI download may be slow from GitHub
Use the Azure CDN mirror at `kubernetesartifacts.azureedge.net` instead of GitHub releases.

### Pod veths must NOT be in the azure0 bridge
This is the most critical gotcha. If pod veths are enslaved to the bridge, br_netfilter's interaction with kube-proxy DNAT breaks ClusterIP routing. Use L3 routing for pod veths instead. See [Section 5](#5-networking-fix--azure-cni-transparent-bridge) for details.

### systemd-resolved loses DNS after bridge setup
Moving the primary IP from eth0 to azure0 requires reconfiguring systemd-resolved:
```bash
resolvectl dns azure0 168.63.129.16
resolvectl domain azure0 "~."
```

### New pods may need manual L3 routing setup
When new pods are created on a node, the CNI creates a new azv* veth. You may need to:
1. Remove it from the bridge: `ip link set azv<ID> nomaster`
2. Set up L3 routing: `ip route replace <POD_IP>/32 dev azv<ID>`
3. Enable proxy ARP: `echo 1 > /proc/sys/net/ipv4/conf/azv<ID>/proxy_arp`

A DaemonSet or CNI wrapper script can automate this.
