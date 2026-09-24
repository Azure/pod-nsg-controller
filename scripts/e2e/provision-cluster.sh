#!/usr/bin/env bash
# =============================================================================
# provision-cluster.sh - parameterized, explicit-subscription provisioning of
# one self-managed Kubernetes cluster on Azure VMs, generalizing the proven
# POC in scripts/poc/setup-cluster-eastus2euap.sh (RD-012 / FILE-016).
#
# EPIC-003:
#   * ITEM-007 - mandatory explicit (subscription, topology, region) inputs;
#     EVERY Azure operation carries an explicit --subscription and the script
#     NEVER runs `az account set` (RD-020 / RISK-013). Works for both `ss` and
#     `xs` topologies. Verifies four Ready nodes per cluster.
#   * ITEM-008 - provisions the shared `asg-backend`/`asg-frontend` ONLY in the
#     topology's primary region resource group, and tags every resource group
#     with the topology, subscription role, and correlation labels (Section
#     3.4.6).
#
# All resource names come from the deterministic naming library (naming.sh);
# subscription IDs are NOT name inputs (Section 3.4.1) and are supplied
# explicitly. Node access uses `az vm run-command invoke` exclusively (the only
# node-access path in the POC), so the "SSH/VM seam" is the `az` seam below.
#
# Node access (install, init, join, readiness check, kubeconfig retrieval) is via
# `az vm run-command invoke` ONLY; there is no runner->API-server path (the NSG
# admits 6443 from VirtualNetwork only). Tool seam (hermetic tests): AZ_BIN (az).
#
# Usage: provision-cluster.sh <all|infra|asgs|kubernetes|kubeconfig|verify|record|names>
#
# Traceability: ITEM-007, ITEM-008, FR-003, FR-011, NFR-001, NFR-003, NFR-012,
# REQ-002, RD-012, RD-020, RISK-013, PRD Sections 3.4 / 3.4.6.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"

# ---- inputs / configuration (all overridable; explicit context required) ----
SUBSCRIPTION_ID="${SUBSCRIPTION_ID:-}"      # explicit subscription (REQUIRED)
TOPOLOGY="${TOPOLOGY:-}"                     # ss|xs                 (REQUIRED)
REGION="${REGION:-}"                         # canary region        (REQUIRED)
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
KUBECONFIG_DIR="${KUBECONFIG_DIR:-${HOME}/.config/kube}"
AZ_BIN="${AZ_BIN:-az}"
VM_SIZE="${VM_SIZE:-Standard_D4s_v5}"        # smallest viable SKU (NFR-004)
VM_IMAGE="${VM_IMAGE:-Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest}"
ADMIN_USER="${ADMIN_USER:-azureuser}"
POD_CIDR="${POD_CIDR:-10.244.0.0/16}"
SHARED_ASGS="${SHARED_ASGS:-asg-backend asg-frontend}"
EXPECTED_NODES="${EXPECTED_NODES:-4}"        # 1 control-plane + 3 workers (CON-004)
SECONDARY_IPS_PER_NIC="${SECONDARY_IPS_PER_NIC:-9}"
NODE_READY_ATTEMPTS="${NODE_READY_ATTEMPTS:-20}"
NODE_READY_DELAY="${NODE_READY_DELAY:-15}"
RUNCMD_ATTEMPTS="${RUNCMD_ATTEMPTS:-3}"
RUNCMD_DELAY="${RUNCMD_DELAY:-10}"
AZ_WAIT_ATTEMPTS="${AZ_WAIT_ATTEMPTS:-3}"
AZ_WAIT_DELAY="${AZ_WAIT_DELAY:-15}"

lib::require_cmds jq

# ---- explicit-subscription az wrapper (RD-020) ------------------------------
# Appends --subscription to EVERY Azure call so nothing depends on mutable
# `az account` context. There is intentionally no `az account set` anywhere.
provision::az() { "$AZ_BIN" "$@" --subscription "$SUBSCRIPTION_ID"; }

# ---- node-side scripts (opaque to shellcheck: single-quoted heredocs) --------
# Host values are injected via @TOKEN@ replacement after the fact, so the node
# scripts contain no host-side shell expansions to escape.
read -r -d '' K8S_PREREQ_SCRIPT <<'EOS' || true
set -ex
swapoff -a
sed -i "/swap/d" /etc/fstab
cat > /etc/modules-load.d/k8s.conf <<EOF
overlay
br_netfilter
EOF
modprobe overlay
modprobe br_netfilter
cat > /etc/sysctl.d/k8s.conf <<EOF
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sysctl --system
apt-get update -qq
apt-get install -y -qq containerd
mkdir -p /etc/containerd
containerd config default > /etc/containerd/config.toml
sed -i "s/SystemdCgroup = false/SystemdCgroup = true/" /etc/containerd/config.toml
systemctl restart containerd
systemctl enable containerd
apt-get install -y -qq apt-transport-https ca-certificates curl gpg conntrack
mkdir -p /etc/apt/keyrings
curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.31/deb/Release.key | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.31/deb/ /" > /etc/apt/sources.list.d/kubernetes.list
apt-get update -qq
apt-get install -y -qq kubelet kubeadm kubectl
apt-mark hold kubelet kubeadm kubectl
EOS

read -r -d '' CNI_INSTALL_SCRIPT <<'EOS' || true
set -ex
CNI_URL="https://kubernetesartifacts.azureedge.net/azure-cni/v1.6.6/binaries/azure-vnet-cni-linux-amd64-v1.6.6.tgz"
mkdir -p /opt/cni/bin
cd /opt/cni/bin
curl -sSLO "$CNI_URL"
tar -xzf azure-vnet-cni-linux-amd64-v1.6.6.tgz
rm -f azure-vnet-cni-linux-amd64-v1.6.6.tgz
if [ ! -f /opt/cni/bin/portmap ]; then
  curl -sSL https://github.com/containernetworking/plugins/releases/download/v1.4.0/cni-plugins-linux-amd64-v1.4.0.tgz | tar -xz -C /opt/cni/bin portmap
fi
mkdir -p /etc/cni/net.d
cat > /etc/cni/net.d/10-azure.conflist <<EOF
{
  "cniVersion": "0.3.0",
  "name": "azure",
  "plugins": [
    { "type": "azure-vnet", "mode": "transparent", "bridge": "azure0", "ipam": { "type": "azure-vnet-ipam" } },
    { "type": "portmap", "capabilities": {"portMappings": true}, "snat": true }
  ]
}
EOF
systemctl restart kubelet
EOS

read -r -d '' IMDS_MASQ_SCRIPT <<'EOS' || true
iptables -t nat -C POSTROUTING -d 169.254.169.254/32 -j MASQUERADE 2>/dev/null \
  || iptables -t nat -A POSTROUTING -d 169.254.169.254/32 -j MASQUERADE
EOS

# Networking fix (bridge + L3 routing) - node-side vars are literal; host values
# arrive via @PRIMARY_IP@/@GATEWAY@/@SECONDARY_IPS@/@SUBNET_PREFIX@.
read -r -d '' NETFIX_TMPL <<'EOS' || true
set -ex
PRIMARY_IP="@PRIMARY_IP@"
GATEWAY="@GATEWAY@"
SECONDARY_IPS="@SECONDARY_IPS@"
ETH0_MAC=$(cat /sys/class/net/eth0/address)
ip link add azure0 type bridge 2>/dev/null || true
echo 0 > /sys/class/net/azure0/bridge/forward_delay
echo 0 > /sys/class/net/azure0/bridge/stp_state
ip link set azure0 address $ETH0_MAC
ip link set azure0 up
ip addr add ${PRIMARY_IP}/24 dev azure0 2>/dev/null || true
ip addr del ${PRIMARY_IP}/24 dev eth0 2>/dev/null || true
ip link set eth0 master azure0
for ip in $SECONDARY_IPS; do ip addr del $ip/24 dev eth0 2>/dev/null || true; done
ip route del default via $GATEWAY dev eth0 2>/dev/null || true
ip route replace default via $GATEWAY dev azure0
ip route del @SUBNET_PREFIX@ dev eth0 2>/dev/null || true
ip route replace 169.254.1.1/32 dev azure0
for AZV in $(ip -o link show | grep 'azv' | awk -F': ' '{print $2}' | cut -d'@' -f1); do
  ip link set $AZV nomaster 2>/dev/null || true
  POD_IP=$(ip route show dev $AZV 2>/dev/null | awk '{print $1}' | head -1)
  if [ -n "$POD_IP" ]; then
    ip route replace $POD_IP dev $AZV
    echo 1 > /proc/sys/net/ipv4/conf/$AZV/proxy_arp
  fi
done
resolvectl dns azure0 168.63.129.16
resolvectl domain azure0 '~.'
conntrack -F 2>/dev/null || true
echo '=== Fix complete ==='
EOS

# ---- name / network / tag derivation ---------------------------------------
provision::_require_inputs() {
  [[ -n "$SUBSCRIPTION_ID" ]] || log::die "SUBSCRIPTION_ID is required (explicit subscription context; no implicit az account, RD-020)"
  [[ -n "$TOPOLOGY" ]]        || log::die "TOPOLOGY is required (ss|xs)"
  [[ -n "$REGION" ]]          || log::die "REGION is required (canary region)"
}

# Deterministic, isolated per-region address plan (clusters are not peered).
provision::_netplan() {
  case "$1" in
    eastus2euap)   VNET_PREFIX="10.3.0.0/16"; SUBNET_PREFIX="10.3.1.0/24"; SUBNET_GW="10.3.1.1"; CP_IP="10.3.1.4"; WORKER_IPS=("10.3.1.5" "10.3.1.6" "10.3.1.7") ;;
    centraluseuap) VNET_PREFIX="10.4.0.0/16"; SUBNET_PREFIX="10.4.1.0/24"; SUBNET_GW="10.4.1.1"; CP_IP="10.4.1.4"; WORKER_IPS=("10.4.1.5" "10.4.1.6" "10.4.1.7") ;;
    *) log::die "no network plan for region '${1}' (supported canary regions: eastus2euap, centraluseuap)" ;;
  esac
  VNET_PREFIX="${VNET_PREFIX_OVERRIDE:-$VNET_PREFIX}"
  SUBNET_PREFIX="${SUBNET_PREFIX_OVERRIDE:-$SUBNET_PREFIX}"
  SUBNET_GW="${SUBNET_GW_OVERRIDE:-$SUBNET_GW}"
  CP_IP="${CP_IP_OVERRIDE:-$CP_IP}"
}

# Correlation tags (Section 3.4.6): as an az `--tags k=v ...` array and JSON.
provision::_compute_tags() {
  TAGS_ARGS=(
    "validation-purpose=${NAMING_PURPOSE}"
    "validation-run-id=${RUN_ID}"
    "validation-run-attempt=${RUN_ATTEMPT}"
    "validation-date-utc=${DATE_UTC}"
    "validation-run-suffix=${RUN_SUFFIX}"
    "validation-topology=${TCODE}"
    "validation-subscription-role=${ROLE}"
    "git-sha=${GIT_SHA}"
    "git-ref=${GIT_REF}"
    "managed-by=github-actions"
    "ttl-hours=${TTL_HOURS}"
  )
  TAGS_JSON="$(printf '%s\n' "${TAGS_ARGS[@]}" | jq -R -s -c '
    split("\n") | map(select(length > 0))
    | map(split("=") | {key: .[0], value: (.[1:] | join("="))}) | from_entries')"
}

provision::_derive() {
  [[ "${PROVISION_DERIVED:-0}" == "1" ]] && return 0
  provision::_require_inputs
  naming::_load_context
  TCODE="$(naming::topology_code "$TOPOLOGY")" || log::die "invalid TOPOLOGY '${TOPOLOGY}' (want ss|xs)"
  local region_norm
  region_norm="$(lib::validate_canary_region "$REGION")" || log::die "REGION '${REGION}' is not a supported canary region"
  REGION="$region_norm"
  local pairs k v
  pairs="$(naming::_region_pairs "$TCODE" "$REGION")" || log::die "name generation failed for ${TCODE}/${REGION}"
  declare -gA N=()
  while IFS='=' read -r k v; do [[ -n "$k" ]] && N["$k"]="$v"; done <<< "$pairs"
  ROLE="${N[subscription_role]}"
  RG="${N[resource_group]}"; VNET="${N[vnet]}"; SUBNET="${N[subnet]}"; NSG="${N[nsg]}"
  NATGW="${N[natgw]}"; NATGW_PIP="${N[natgw_pip]}"; CP_PIP="${N[cp_pip]}"; CP_VM="${N[cp_vm]}"
  WORKERS=("${N[worker1_vm]}" "${N[worker2_vm]}" "${N[worker3_vm]}")
  if [[ "$REGION" == "$NAMING_PRIMARY_REGION" ]]; then IS_PRIMARY_REGION=1; else IS_PRIMARY_REGION=0; fi
  provision::_netplan "$REGION"
  provision::_compute_tags
  PROVISION_DERIVED=1
}

# ---- node run-command helpers (NFR-003 bounded retries) ---------------------
# `az vm run-command invoke` exits 0 for the extension REGARDLESS of the node
# script's exit code, so a bare invoke cannot detect node failures. Run the
# script under `set -e` with output captured to a log, and emit a small,
# truncation-resistant sentinel; a missing sentinel means the node script failed
# (B2). This also keeps raw node scripts/tokens out of retry logs (SEC hygiene).
provision::_runcmd() {
  local vm="$1" script="$2" wrapped msg stdout
  # shellcheck disable=SC2016  # $?, $__pnc_rc are evaluated on the NODE, not here
  wrapped="$(printf '( set -e\n%s\n) >/tmp/pnc-node.log 2>&1; __pnc_rc=$?; if [ "$__pnc_rc" -eq 0 ]; then echo __PNC_OK__; else echo "__PNC_FAIL__ rc=$__pnc_rc"; tail -c 3000 /tmp/pnc-node.log; fi' "$script")"
  msg="$(provision::_runcmd_msg "$vm" "$wrapped")" \
    || log::die "run-command transport failed on ${vm} (subscription ${SUBSCRIPTION_ID})"
  stdout="$(provision::_extract_stdout "$msg")"
  if [[ "$stdout" != *__PNC_OK__* ]]; then
    log::error "node script failed on ${vm}:"
    printf '%s\n' "$stdout" >&2
    log::die "run-command script failed on ${vm} (subscription ${SUBSCRIPTION_ID})"
  fi
}

provision::_runcmd_msg() {
  local vm="$1" script="$2" out attempt=1 max="$RUNCMD_ATTEMPTS" delay="$RUNCMD_DELAY"
  while true; do
    if out="$(provision::az vm run-command invoke -g "$RG" -n "$vm" \
                --command-id RunShellScript --scripts "$script" \
                --query "value[0].message" -o tsv 2>/dev/null)"; then
      printf '%s' "$out"; return 0
    fi
    if (( attempt >= max )); then log::error "run-command message fetch failed on ${vm}"; return 1; fi
    log::warn "run-command message fetch attempt ${attempt}/${max} failed on ${vm}; retrying in ${delay}s"
    sleep "$delay"; attempt=$(( attempt + 1 )); delay=$(( delay * 2 ))
  done
}

# Extract the [stdout] section of an `az vm run-command` message (POC parsing).
provision::_extract_stdout() {
  if printf '%s' "$1" | grep -q '^\[stdout\]'; then
    printf '%s\n' "$1" | sed -n '/^\[stdout\]/,/^\[stderr\]/p' | sed '1d;$d'
  else
    printf '%s' "$1"
  fi
}

provision::_public_ip() {
  provision::az network public-ip show -g "$RG" -n "$CP_PIP" --query ipAddress -o tsv
}

provision::_secondary_ips() {
  provision::az network nic ip-config list -g "$RG" --nic-name "$1" \
    --query "[?name!='ipconfig1'].privateIpAddress" -o tsv | tr '\n' ' '
}

provision::_cp_init_script() {
  local public_ip="$1"
  local s="kubeadm init --pod-network-cidr=@POD_CIDR@ --apiserver-cert-extra-sans=@PUBLIC_IP@ --apiserver-advertise-address=@CP_IP@ --control-plane-endpoint=@CP_IP@:6443
kubeadm token create --print-join-command > /tmp/join-command.txt
cat /tmp/join-command.txt"
  s="${s//@POD_CIDR@/$POD_CIDR}"; s="${s//@PUBLIC_IP@/$public_ip}"; s="${s//@CP_IP@/$CP_IP}"
  printf '%s' "$s"
}

provision::_netfix_script() {
  local s="$NETFIX_TMPL"
  s="${s//@PRIMARY_IP@/$1}"; s="${s//@GATEWAY@/$SUBNET_GW}"
  s="${s//@SECONDARY_IPS@/$2}"; s="${s//@SUBNET_PREFIX@/$SUBNET_PREFIX}"
  printf '%s' "$s"
}

# ---- phases -----------------------------------------------------------------

# Phase 1: infrastructure (RG + tags, NSG, VNet/subnet, PIPs, NAT, NICs +
# secondary pod IPs, VMs, managed identities). ITEM-007 / ITEM-008 (RG tags).
provision::infra() {
  provision::_derive
  log::info "provisioning infra ${TCODE}/${REGION} (role=${ROLE}) in subscription ${SUBSCRIPTION_ID} -> RG ${RG}"

  provision::az group create --name "$RG" --location "$REGION" --tags "${TAGS_ARGS[@]}" --output none

  provision::az network nsg create -g "$RG" -n "$NSG" --output none
  provision::az network nsg rule create -g "$RG" --nsg-name "$NSG" -n AllowSSH \
    --priority 100 --access Allow --direction Inbound --protocol Tcp \
    --source-address-prefixes '*' --destination-port-ranges 22 --output none
  provision::az network nsg rule create -g "$RG" --nsg-name "$NSG" -n AllowK8sAPI \
    --priority 110 --access Allow --direction Inbound --protocol Tcp \
    --source-address-prefixes VirtualNetwork --destination-port-ranges 6443 --output none
  provision::az network nsg rule create -g "$RG" --nsg-name "$NSG" -n AllowKubelet \
    --priority 120 --access Allow --direction Inbound --protocol Tcp \
    --source-address-prefixes VirtualNetwork --destination-port-ranges 10250 --output none
  provision::az network nsg rule create -g "$RG" --nsg-name "$NSG" -n AllowEtcd \
    --priority 130 --access Allow --direction Inbound --protocol Tcp \
    --source-address-prefixes VirtualNetwork --destination-port-ranges 2379-2380 --output none
  provision::az network nsg rule create -g "$RG" --nsg-name "$NSG" -n AllowNodePort \
    --priority 140 --access Allow --direction Inbound --protocol Tcp \
    --source-address-prefixes '*' --destination-port-ranges 30000-32767 --output none

  provision::az network vnet create -g "$RG" -n "$VNET" \
    --address-prefix "$VNET_PREFIX" --subnet-name "$SUBNET" --subnet-prefix "$SUBNET_PREFIX" \
    --network-security-group "$NSG" --output none

  provision::az network public-ip create -g "$RG" -n "$CP_PIP" --sku Standard --allocation-method Static --output none
  provision::az network public-ip create -g "$RG" -n "$NATGW_PIP" --sku Standard --allocation-method Static --output none
  provision::az network nat gateway create -g "$RG" -n "$NATGW" --public-ip-addresses "$NATGW_PIP" --idle-timeout 10 --output none
  provision::az network vnet subnet update -g "$RG" --vnet-name "$VNET" -n "$SUBNET" --nat-gateway "$NATGW" --output none

  provision::az network nic create -g "$RG" -n "${CP_VM}-nic" \
    --vnet-name "$VNET" --subnet "$SUBNET" --private-ip-address "$CP_IP" --public-ip-address "$CP_PIP" --output none
  local i
  for i in 0 1 2; do
    provision::az network nic create -g "$RG" -n "${WORKERS[$i]}-nic" \
      --vnet-name "$VNET" --subnet "$SUBNET" --private-ip-address "${WORKER_IPS[$i]}" --output none
  done

  log::info "adding ${SECONDARY_IPS_PER_NIC} secondary pod IPs per NIC (Azure CNI capacity, CON-004)"
  local nic j
  for nic in "${CP_VM}-nic" "${WORKERS[0]}-nic" "${WORKERS[1]}-nic" "${WORKERS[2]}-nic"; do
    for (( j = 1; j <= SECONDARY_IPS_PER_NIC; j++ )); do
      provision::az network nic ip-config create -g "$RG" --nic-name "$nic" \
        -n "ipconfig-pod-${j}" --private-ip-address-version IPv4 --output none
    done
  done

  provision::az vm create -g "$RG" -n "$CP_VM" --nics "${CP_VM}-nic" \
    --image "$VM_IMAGE" --size "$VM_SIZE" --admin-username "$ADMIN_USER" --generate-ssh-keys --no-wait --output none
  for i in 0 1 2; do
    provision::az vm create -g "$RG" -n "${WORKERS[$i]}" --nics "${WORKERS[$i]}-nic" \
      --image "$VM_IMAGE" --size "$VM_SIZE" --admin-username "$ADMIN_USER" --generate-ssh-keys --no-wait --output none
  done

  local vm
  for vm in "$CP_VM" "${WORKERS[@]}"; do
    lib::retry "$AZ_WAIT_ATTEMPTS" "$AZ_WAIT_DELAY" -- provision::az vm wait -g "$RG" -n "$vm" --created \
      || log::die "vm wait failed for ${vm}"
  done
  for vm in "$CP_VM" "${WORKERS[@]}"; do
    provision::az vm identity assign -g "$RG" -n "$vm" --output none
  done
  log::info "infra ready: ${RG} (4 VMs, managed identities enabled)"
}

# Phase 1b: shared ASGs (ITEM-008) - primary region RG only.
provision::asgs() {
  provision::_derive
  if (( IS_PRIMARY_REGION == 0 )); then
    log::info "region ${REGION} is not the primary region (${NAMING_PRIMARY_REGION}); shared ASGs live in the primary RG only - skipping"
    return 0
  fi
  local asgs=() asg
  read -r -a asgs <<< "$SHARED_ASGS"
  for asg in "${asgs[@]}"; do
    log::info "creating shared ASG ${asg} in ${RG} (subscription ${SUBSCRIPTION_ID})"
    lib::retry "$RUNCMD_ATTEMPTS" "$RUNCMD_DELAY" -- \
      provision::az network asg create -g "$RG" -n "$asg" --location "$REGION" --tags "${TAGS_ARGS[@]}" --output none \
      || log::die "failed to create shared ASG ${asg}"
  done
}

# Phase 2-7: Kubernetes install, control-plane init, worker join, networking
# fix, IMDS masquerade (faithful generalization of the POC).
provision::kubernetes() {
  provision::_derive
  local all_vms=("$CP_VM" "${WORKERS[@]}") vm i

  for vm in "${all_vms[@]}"; do log::info "k8s prerequisites on ${vm}"; provision::_runcmd "$vm" "$K8S_PREREQ_SCRIPT"; done
  for vm in "${all_vms[@]}"; do log::info "Azure CNI on ${vm}";          provision::_runcmd "$vm" "$CNI_INSTALL_SCRIPT"; done

  local public_ip; public_ip="$(provision::_public_ip)"
  [[ -n "$public_ip" ]] || log::die "could not read control-plane public IP for ${CP_VM}"
  log::info "initializing control plane on ${CP_VM} (public IP ${public_ip})"
  provision::_runcmd "$CP_VM" "$(provision::_cp_init_script "$public_ip")"

  local raw join_cmd
  raw="$(provision::_runcmd_msg "$CP_VM" 'kubeadm token create --print-join-command')" \
    || log::die "failed to fetch kubeadm join command"
  # `|| true` guards against SIGPIPE from `head` closing the pipe early under
  # `set -o pipefail`; the emptiness check below is the real gate.
  join_cmd="$(provision::_extract_stdout "$raw" | grep 'kubeadm join' | head -1 || true)"
  [[ -n "$join_cmd" ]] || log::die "could not parse a kubeadm join command from the control plane"

  for vm in "${WORKERS[@]}"; do log::info "joining ${vm}"; provision::_runcmd "$vm" "$join_cmd"; done

  for i in 0 1 2; do
    local worker="${WORKERS[$i]}" secondary
    secondary="$(provision::_secondary_ips "${worker}-nic")"
    log::info "applying networking fix on ${worker}"
    provision::_runcmd "$worker" "$(provision::_netfix_script "${WORKER_IPS[$i]}" "$secondary")"
  done

  for vm in "${all_vms[@]}"; do log::info "IMDS masquerade on ${vm}"; provision::_runcmd "$vm" "$IMDS_MASQ_SCRIPT"; done
  log::info "kubernetes install complete for ${RG}"
}

# Phase 8: kubeconfig retrieval (server rewritten to the CP public IP). The
# admin.conf (with embedded certs) exceeds the ~4 KB `az vm run-command` output
# limit, so it is fetched in two chunks and reassembled (matches the POC).
provision::kubeconfig() {
  provision::_derive
  local public_ip raw1 raw2 file
  public_ip="$(provision::_public_ip)"
  [[ -n "$public_ip" ]] || log::die "could not read control-plane public IP for ${CP_VM}"
  raw1="$(provision::_runcmd_msg "$CP_VM" 'head -15 /etc/kubernetes/admin.conf')" \
    || log::die "failed to retrieve kubeconfig (head) from ${CP_VM}"
  raw2="$(provision::_runcmd_msg "$CP_VM" 'tail -n +16 /etc/kubernetes/admin.conf')" \
    || log::die "failed to retrieve kubeconfig (tail) from ${CP_VM}"
  mkdir -p "$KUBECONFIG_DIR"
  file="${KUBECONFIG_DIR}/${RG}-kubeconfig.yaml"
  { printf '%s\n' "$(provision::_extract_stdout "$raw1")"
    printf '%s\n' "$(provision::_extract_stdout "$raw2")"; } > "$file"
  sed -i 's/\r//g' "$file"
  sed -i '/^$/d' "$file"
  sed -i "s|https://${CP_IP}:6443|https://${public_ip}:6443|" "$file"
  # Fail closed if the run-command output was truncated (missing credentials).
  grep -q 'client-key-data' "$file" \
    || log::die "retrieved kubeconfig appears truncated (missing client-key-data): ${file}"
  gha::output kubeconfig "$file"
  log::info "kubeconfig written to ${file} (server https://${public_ip}:6443)"
}

# Phase 9: node-ready verification (ITEM-007: four Ready nodes). Checked ON the
# control plane via run-command (the runner has no path to the API server).
provision::verify() {
  provision::_derive
  local attempt=1 max="$NODE_READY_ATTEMPTS" delay="$NODE_READY_DELAY" ready raw
  while true; do
    raw="$(provision::_runcmd_msg "$CP_VM" 'KUBECONFIG=/etc/kubernetes/admin.conf kubectl get nodes --no-headers' 2>/dev/null || true)"
    ready="$(provision::_extract_stdout "$raw" | grep -cw Ready || true)"
    ready="${ready//[^0-9]/}"; ready="${ready:-0}"
    if (( ready >= EXPECTED_NODES )); then
      log::info "cluster ${RG}: ${ready}/${EXPECTED_NODES} nodes Ready"
      PROVISION_NODES_READY="$ready"; return 0
    fi
    if (( attempt >= max )); then
      log::error "cluster ${RG}: only ${ready}/${EXPECTED_NODES} nodes Ready after ${attempt} attempt(s)"
      PROVISION_NODES_READY="$ready"; return 1
    fi
    log::warn "cluster ${RG}: ${ready}/${EXPECTED_NODES} Ready; retrying in ${delay}s (${attempt}/${max})"
    sleep "$delay"; attempt=$(( attempt + 1 ))
  done
}

# Record provisioned resource IDs, ASGs, role, and node count into the manifest
# (ITEM-007 resource IDs under expected subscription; ITEM-008 ASGs/tags).
provision::record() {
  provision::_derive
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  local asgs_json='[]'
  (( IS_PRIMARY_REGION == 1 )) && asgs_json="$(jq -n --arg s "$SHARED_ASGS" '$s | split(" ") | map(select(length > 0))')"
  local rg_id="/subscriptions/${SUBSCRIPTION_ID}/resourceGroups/${RG}"
  local nodes="${PROVISION_NODES_READY:-0}"
  local rec
  rec="$(jq -n \
    --arg sub "$SUBSCRIPTION_ID" --arg role "$ROLE" --arg region "$REGION" \
    --arg rg "$RG" --arg rgid "$rg_id" --arg cid "${N[cluster_id]}" --arg ns "${N[namespace]}" \
    --argjson nodes "$nodes" --argjson expected "$EXPECTED_NODES" \
    --argjson asgs "$asgs_json" --argjson tags "$TAGS_JSON" \
    '{subscription_id: $sub, subscription_role: $role, region: $region, cluster_id: $cid,
      namespace: $ns, resource_group: $rg, resource_group_id: $rgid,
      nodes_ready: $nodes, expected_nodes: $expected, asgs: $asgs, tags: $tags}')"
  manifest::put_json "$MANIFEST_PATH" "provision.${TCODE}.${REGION}" "$rec"
  gha::output resource_group "$RG"
  gha::output resource_group_id "$rg_id"
  gha::output subscription_role "$ROLE"
  log::info "recorded provision.${TCODE}.${REGION} -> ${MANIFEST_PATH}"
}

provision::names() {
  provision::_derive
  local k
  for k in $(printf '%s\n' "${!N[@]}" | sort); do printf '%s=%s\n' "$k" "${N[$k]}"; done
  printf 'region=%s\nsubscription_id=%s\nsubscription_role=%s\nvnet_prefix=%s\ncp_ip=%s\nis_primary_region=%s\n' \
    "$REGION" "$SUBSCRIPTION_ID" "$ROLE" "$VNET_PREFIX" "$CP_IP" "$IS_PRIMARY_REGION"
}

provision::all() {
  provision::_derive
  # Inventory the RG/resource IDs even if a later phase fails, so verified
  # teardown always has them (B4 / RISK-011). The EXIT trap fires on a set -e
  # abort as well as on normal completion.
  trap 'provision::record || log::warn "manifest record failed"' EXIT
  provision::infra
  provision::asgs
  provision::kubernetes
  provision::kubeconfig
  provision::verify
}

provision::usage() {
  cat <<'USAGE'
Usage: provision-cluster.sh <command>

Parameterized, explicit-subscription self-managed cluster provisioning.
Commands:
  all          Full provisioning: infra -> asgs -> kubernetes -> kubeconfig -> verify -> record
  infra        Resource group (+tags), NSG, VNet/subnet, PIPs, NAT, NICs (+secondary IPs), VMs, MIs
  asgs         Shared asg-backend/asg-frontend (primary region RG only)
  kubernetes   K8s install, control-plane init, worker join, networking fix, IMDS masquerade
  kubeconfig   Retrieve the admin kubeconfig (server rewritten to the CP public IP)
  verify       Assert EXPECTED_NODES (default 4) nodes are Ready
  record       Record resource IDs / ASGs / role / node count into the run manifest
  names        Print the derived names for (TOPOLOGY, REGION) and exit

Required env: SUBSCRIPTION_ID, TOPOLOGY (ss|xs), REGION (canary).
Naming context: REPO, RUN_ID, RUN_ATTEMPT, DATE_UTC, GIT_SHA, GIT_REF, TTL_HOURS.
Tool seams: AZ_BIN (az), KUBECTL_BIN (kubectl).
USAGE
}

provision::main() {
  local cmd="${1:-all}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    all)        provision::all ;;
    infra)      provision::infra ;;
    asgs)       provision::asgs ;;
    kubernetes) provision::kubernetes ;;
    kubeconfig) provision::kubeconfig ;;
    verify)     provision::verify ;;
    record)     provision::record ;;
    names)      provision::names ;;
    help|-h|--help) provision::usage ;;
    *) log::error "unknown command: ${cmd}"; provision::usage >&2; return 1 ;;
  esac
}

provision::main "$@"
