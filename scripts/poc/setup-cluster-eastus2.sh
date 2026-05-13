#!/usr/bin/env bash
# =============================================================================
# POC Cluster Setup — asnStripe-eastus2
# Subscription: Azure Network Agent - Test (9b8218f9-902a-4d20-a65c-e98acec5362f)
# Region: eastus2
# Based on: docs/self-managed-k8s-azure-cni-setup.md
# =============================================================================
set -euo pipefail

# === Configuration ===
SUBSCRIPTION="9b8218f9-902a-4d20-a65c-e98acec5362f"
REGION="eastus2"
PREFIX="asnStripe"
RG="${PREFIX}-${REGION}"
VNET="${PREFIX}-${REGION}-vnet"
SUBNET="${PREFIX}-${REGION}-subnet"
NSG="${PREFIX}-${REGION}-nsg"
NAT_GW="${PREFIX}-${REGION}-natgw"
NAT_PIP="${PREFIX}-${REGION}-natgw-pip"
CP_PIP="${PREFIX}-${REGION}-cp-pip"
CP_NAME="${PREFIX}-${REGION}-cp-01"
VNET_PREFIX="10.2.0.0/16"
SUBNET_PREFIX="10.2.1.0/24"
SUBNET_GW="10.2.1.1"
CP_IP="10.2.1.4"
WORKER_IPS=("10.2.1.5" "10.2.1.6" "10.2.1.7")
IMAGE="Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest"
SIZE="Standard_D4s_v5"
ADMIN_USER="azureuser"

echo "============================================="
echo " Setting up cluster: ${RG}"
echo " Subscription: ${SUBSCRIPTION}"
echo " Region: ${REGION}"
echo "============================================="

# Set subscription context
az account set --subscription "$SUBSCRIPTION"

# ─────────────────────────────────────────────
# Phase 1: Infrastructure
# ─────────────────────────────────────────────
echo ">>> Phase 1: Infrastructure Provisioning"

# Resource Group
az group create --name "$RG" --location "$REGION" --output none
echo "  ✓ Resource group: $RG"

# NSG
az network nsg create -g "$RG" -n "$NSG" --output none

az network nsg rule create -g "$RG" --nsg-name "$NSG" \
  -n AllowSSH --priority 100 --access Allow --direction Inbound \
  --source-address-prefixes '*' --destination-port-ranges 22 --protocol Tcp --output none

az network nsg rule create -g "$RG" --nsg-name "$NSG" \
  -n AllowK8sAPI --priority 110 --access Allow --direction Inbound \
  --source-address-prefixes VirtualNetwork --destination-port-ranges 6443 --protocol Tcp --output none

az network nsg rule create -g "$RG" --nsg-name "$NSG" \
  -n AllowKubelet --priority 120 --access Allow --direction Inbound \
  --source-address-prefixes VirtualNetwork --destination-port-ranges 10250 --protocol Tcp --output none

az network nsg rule create -g "$RG" --nsg-name "$NSG" \
  -n AllowEtcd --priority 130 --access Allow --direction Inbound \
  --source-address-prefixes VirtualNetwork --destination-port-ranges 2379-2380 --protocol Tcp --output none

az network nsg rule create -g "$RG" --nsg-name "$NSG" \
  -n AllowNodePort --priority 140 --access Allow --direction Inbound \
  --source-address-prefixes '*' --destination-port-ranges 30000-32767 --protocol Tcp --output none

echo "  ✓ NSG: $NSG"

# VNet and Subnet
az network vnet create -g "$RG" -n "$VNET" \
  --address-prefix "$VNET_PREFIX" \
  --subnet-name "$SUBNET" --subnet-prefix "$SUBNET_PREFIX" \
  --network-security-group "$NSG" --output none
echo "  ✓ VNet: $VNET ($VNET_PREFIX)"

# Public IP for control plane
az network public-ip create -g "$RG" -n "$CP_PIP" \
  --sku Standard --allocation-method Static --output none
echo "  ✓ Public IP: $CP_PIP"

# NAT Gateway
az network public-ip create -g "$RG" -n "$NAT_PIP" \
  --sku Standard --allocation-method Static --output none

az network nat gateway create -g "$RG" -n "$NAT_GW" \
  --public-ip-addresses "$NAT_PIP" --idle-timeout 10 --output none

az network vnet subnet update -g "$RG" \
  --vnet-name "$VNET" -n "$SUBNET" --nat-gateway "$NAT_GW" --output none
echo "  ✓ NAT Gateway: $NAT_GW"

# NICs
# Control Plane NIC
az network nic create -g "$RG" -n "${CP_NAME}-nic" \
  --vnet-name "$VNET" --subnet "$SUBNET" \
  --private-ip-address "$CP_IP" --public-ip-address "$CP_PIP" --output none
echo "  ✓ NIC: ${CP_NAME}-nic (${CP_IP})"

# Worker NICs
for i in 0 1 2; do
  WORKER_NUM=$(printf "%02d" $((i + 1)))
  WORKER_NAME="${PREFIX}-${REGION}-worker-${WORKER_NUM}"
  az network nic create -g "$RG" -n "${WORKER_NAME}-nic" \
    --vnet-name "$VNET" --subnet "$SUBNET" \
    --private-ip-address "${WORKER_IPS[$i]}" --output none
  echo "  ✓ NIC: ${WORKER_NAME}-nic (${WORKER_IPS[$i]})"
done

# Add secondary IPs for Azure CNI pod allocation
echo "  Adding secondary IPs to NICs (this takes a few minutes)..."
for NIC in "${CP_NAME}-nic" "${PREFIX}-${REGION}-worker-01-nic" "${PREFIX}-${REGION}-worker-02-nic" "${PREFIX}-${REGION}-worker-03-nic"; do
  for j in $(seq 1 9); do
    az network nic ip-config create -g "$RG" --nic-name "$NIC" \
      -n "ipconfig-pod-${j}" --private-ip-address-version IPv4 --output none
  done
  echo "    ✓ $NIC: 9 secondary IPs added"
done

# VMs
echo "  Creating VMs..."
az vm create -g "$RG" -n "$CP_NAME" --nics "${CP_NAME}-nic" \
  --image "$IMAGE" --size "$SIZE" --admin-username "$ADMIN_USER" \
  --generate-ssh-keys --no-wait --output none

for i in 0 1 2; do
  WORKER_NUM=$(printf "%02d" $((i + 1)))
  WORKER_NAME="${PREFIX}-${REGION}-worker-${WORKER_NUM}"
  az vm create -g "$RG" -n "$WORKER_NAME" --nics "${WORKER_NAME}-nic" \
    --image "$IMAGE" --size "$SIZE" --admin-username "$ADMIN_USER" \
    --generate-ssh-keys --no-wait --output none
done

# Wait for all VMs
echo "  Waiting for VMs to be created..."
az vm wait -g "$RG" -n "$CP_NAME" --created
for i in 01 02 03; do
  az vm wait -g "$RG" -n "${PREFIX}-${REGION}-worker-${i}" --created
done
echo "  ✓ All VMs created"

# Enable managed identity on all VMs
echo "  Enabling managed identity on VMs..."
az vm identity assign -g "$RG" -n "$CP_NAME" --output none
for i in 01 02 03; do
  az vm identity assign -g "$RG" -n "${PREFIX}-${REGION}-worker-${i}" --output none
done
echo "  ✓ Managed identities enabled"

echo ">>> Phase 1 complete!"

# ─────────────────────────────────────────────
# Phase 2: Kubernetes Installation
# ─────────────────────────────────────────────
echo ">>> Phase 2: Kubernetes Installation"

K8S_PREREQ_SCRIPT='
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

ALL_VMS=("$CP_NAME" "${PREFIX}-${REGION}-worker-01" "${PREFIX}-${REGION}-worker-02" "${PREFIX}-${REGION}-worker-03")

for VM in "${ALL_VMS[@]}"; do
  echo "  Installing K8s prerequisites on ${VM}..."
  az vm run-command invoke -g "$RG" -n "$VM" \
    --command-id RunShellScript --scripts "$K8S_PREREQ_SCRIPT" --output none
  echo "    ✓ ${VM} prerequisites installed"
done

# ─────────────────────────────────────────────
# Phase 3: Azure CNI Installation
# ─────────────────────────────────────────────
echo ">>> Phase 3: Azure CNI Installation"

CNI_INSTALL_SCRIPT='
set -ex

# Download Azure CNI v1.6.6
CNI_URL="https://kubernetesartifacts.azureedge.net/azure-cni/v1.6.6/binaries/azure-vnet-cni-linux-amd64-v1.6.6.tgz"
mkdir -p /opt/cni/bin
cd /opt/cni/bin
curl -sSLO "$CNI_URL"
tar -xzf azure-vnet-cni-linux-amd64-v1.6.6.tgz
rm -f azure-vnet-cni-linux-amd64-v1.6.6.tgz

# Also need the portmap plugin
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

for VM in "${ALL_VMS[@]}"; do
  echo "  Installing Azure CNI on ${VM}..."
  az vm run-command invoke -g "$RG" -n "$VM" \
    --command-id RunShellScript --scripts "$CNI_INSTALL_SCRIPT" --output none
  echo "    ✓ ${VM} CNI installed"
done

echo ">>> Phase 3 complete!"

# ─────────────────────────────────────────────
# Phase 4: Control Plane Init
# ─────────────────────────────────────────────
echo ">>> Phase 4: Control Plane Initialization"

PUBLIC_IP=$(az network public-ip show -g "$RG" -n "$CP_PIP" --query ipAddress -o tsv)

az vm run-command invoke -g "$RG" -n "$CP_NAME" \
  --command-id RunShellScript --scripts "
kubeadm init \
  --pod-network-cidr=10.244.0.0/16 \
  --apiserver-cert-extra-sans=${PUBLIC_IP} \
  --apiserver-advertise-address=${CP_IP} \
  --control-plane-endpoint=${CP_IP}:6443

kubeadm token create --print-join-command > /tmp/join-command.txt
cat /tmp/join-command.txt
" --output none

echo "  ✓ Control plane initialized"

# ─────────────────────────────────────────────
# Phase 5: Worker Join
# ─────────────────────────────────────────────
echo ">>> Phase 5: Worker Node Join"

JOIN_CMD=$(az vm run-command invoke -g "$RG" -n "$CP_NAME" \
  --command-id RunShellScript \
  --scripts 'kubeadm token create --print-join-command' \
  --query "value[0].message" -o tsv | grep "kubeadm join")

echo "  Join command: ${JOIN_CMD}"

for i in 01 02 03; do
  WORKER="${PREFIX}-${REGION}-worker-${i}"
  echo "  Joining ${WORKER}..."
  az vm run-command invoke -g "$RG" -n "$WORKER" \
    --command-id RunShellScript --scripts "$JOIN_CMD" --output none
  echo "    ✓ ${WORKER} joined"
done

echo ">>> Phase 5 complete!"

# ─────────────────────────────────────────────
# Phase 6: Networking Fix (Bridge + L3 Routing)
# ─────────────────────────────────────────────
echo ">>> Phase 6: Networking Fix"

for i in 0 1 2; do
  WORKER_NUM=$(printf "%02d" $((i + 1)))
  WORKER="${PREFIX}-${REGION}-worker-${WORKER_NUM}"
  PRIMARY="${WORKER_IPS[$i]}"

  echo "  Applying networking fix on ${WORKER} (${PRIMARY})..."

  SECONDARY_IPS=$(az network nic ip-config list -g "$RG" --nic-name "${WORKER}-nic" \
    --query "[?name!='ipconfig1'].privateIpAddress" -o tsv | tr '\n' ' ')

  az vm run-command invoke -g "$RG" -n "$WORKER" \
    --command-id RunShellScript --scripts "
set -ex

PRIMARY_IP=\"${PRIMARY}\"
GATEWAY=\"${SUBNET_GW}\"
SECONDARY_IPS=\"${SECONDARY_IPS}\"

ETH0_MAC=\$(cat /sys/class/net/eth0/address)

# Step 1: Create azure0 bridge
ip link add azure0 type bridge 2>/dev/null || true
echo 0 > /sys/class/net/azure0/bridge/forward_delay
echo 0 > /sys/class/net/azure0/bridge/stp_state
ip link set azure0 address \$ETH0_MAC
ip link set azure0 up

# Step 2: Move primary IP from eth0 to bridge
ip addr add \${PRIMARY_IP}/24 dev azure0 2>/dev/null || true
ip addr del \${PRIMARY_IP}/24 dev eth0 2>/dev/null || true

# Step 3: Enslave eth0 to bridge
ip link set eth0 master azure0

# Step 4: Remove secondary IPs from eth0
for ip in \$SECONDARY_IPS; do
  ip addr del \$ip/24 dev eth0 2>/dev/null || true
done

# Step 5: Fix routes
ip route del default via \$GATEWAY dev eth0 2>/dev/null || true
ip route replace default via \$GATEWAY dev azure0
ip route del ${SUBNET_PREFIX} dev eth0 2>/dev/null || true

# Step 6: Setup L3 routing for pod veths
ip route replace 169.254.1.1/32 dev azure0

for AZV in \$(ip -o link show | grep 'azv' | awk -F': ' '{print \$2}' | cut -d'@' -f1); do
  ip link set \$AZV nomaster 2>/dev/null || true
  POD_IP=\$(ip route show dev \$AZV 2>/dev/null | awk '{print \$1}' | head -1)
  if [ -n \"\$POD_IP\" ]; then
    ip route replace \$POD_IP dev \$AZV
    echo 1 > /proc/sys/net/ipv4/conf/\$AZV/proxy_arp
  fi
done

# Step 7: Fix DNS
resolvectl dns azure0 168.63.129.16
resolvectl domain azure0 '~.'

# Step 8: Flush stale state
conntrack -F 2>/dev/null || true

echo '=== Fix complete ==='
" --output none

  echo "    ✓ ${WORKER} networking fixed"
done

# ─────────────────────────────────────────────
# Phase 7: IMDS MASQUERADE
# ─────────────────────────────────────────────
echo ">>> Phase 7: IMDS MASQUERADE for Managed Identity"

for VM in "${ALL_VMS[@]}"; do
  az vm run-command invoke -g "$RG" -n "$VM" \
    --command-id RunShellScript \
    --scripts 'iptables -t nat -A POSTROUTING -d 169.254.169.254/32 -j MASQUERADE' --output none
  echo "  ✓ ${VM}: IMDS MASQUERADE configured"
done

echo ">>> Phase 7 complete!"

# ─────────────────────────────────────────────
# Phase 8: Kubeconfig Retrieval
# ─────────────────────────────────────────────
echo ">>> Phase 8: Kubeconfig Retrieval"

KUBECONFIG_DIR="${HOME}/.config/kube"
KUBECONFIG_FILE="${KUBECONFIG_DIR}/${RG}-kubeconfig.yaml"
mkdir -p "$KUBECONFIG_DIR"

PART1=$(az vm run-command invoke -g "$RG" -n "$CP_NAME" \
  --command-id RunShellScript --scripts 'head -15 /etc/kubernetes/admin.conf' \
  --query "value[0].message" -o tsv | sed -n '/^\[stdout\]/,/^\[stderr\]/p' | sed '1d;$d')

PART2=$(az vm run-command invoke -g "$RG" -n "$CP_NAME" \
  --command-id RunShellScript --scripts 'tail -n +16 /etc/kubernetes/admin.conf' \
  --query "value[0].message" -o tsv | sed -n '/^\[stdout\]/,/^\[stderr\]/p' | sed '1d;$d')

echo "${PART1}" > "$KUBECONFIG_FILE"
echo "${PART2}" >> "$KUBECONFIG_FILE"

# Fix carriage returns from az vm run-command output and remove blank lines
sed -i 's/\r//g' "$KUBECONFIG_FILE"
sed -i '/^$/d' "$KUBECONFIG_FILE"

# Replace internal IP with public IP
sed -i "s|https://${CP_IP}:6443|https://${PUBLIC_IP}:6443|" "$KUBECONFIG_FILE"

echo "  ✓ Kubeconfig saved to: ${KUBECONFIG_FILE}"
echo "  Run: export KUBECONFIG=${KUBECONFIG_FILE}"

echo ""
echo "============================================="
echo " Cluster ${RG} setup complete!"
echo " Public IP: ${PUBLIC_IP}"
echo " Kubeconfig: ${KUBECONFIG_FILE}"
echo "============================================="
