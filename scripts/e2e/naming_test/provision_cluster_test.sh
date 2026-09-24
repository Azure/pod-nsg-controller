#!/usr/bin/env bash
# =============================================================================
# provision_cluster_test.sh - behavioural tests for ../provision-cluster.sh
# (EPIC-003 / ITEM-007 parameterized explicit-subscription provisioning,
# ITEM-008 shared ASGs + topology/subscription-role tags).
#
# Fully hermetic: `az` is a deterministic mock (no real cloud, no VMs, no SSH).
# Node access is exclusively via `az vm run-command` (the only node path), so
# the mock also models node-side success/failure, chunked kubeconfig retrieval,
# and control-plane node-readiness reporting. The suite proves:
#   * mandatory explicit (subscription, topology, region) inputs (fail fast);
#   * EVERY Azure operation carries an explicit --subscription; NEVER `az account
#     set` (RD-020 / RISK-013);
#   * a node-side script failure is DETECTED (az vm run-command returns 0 even
#     when the node script fails) and aborts provisioning (B2);
#   * shared asg-backend/asg-frontend are created ONLY in the primary region RG
#     under the explicit subscription (ITEM-008);
#   * every RG carries topology + subscription-role + correlation tags (3.4.6);
#   * kubeconfig is retrieved in chunks (run-command ~4 KB limit) and is intact;
#   * node readiness (4 Ready) is verified via the control plane (ITEM-007);
#   * resource IDs are recorded into the manifest even when a later phase fails
#     (verified-teardown inventory, B4/RISK-011).
#
# Traceability: ITEM-007, ITEM-008, FR-003, FR-011, NFR-001/003/012, RD-012/020.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROVISION_SH="${TEST_DIR}/../provision-cluster.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.provision_test_work"
MOCKBIN="${WORK}/bin"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() {
  if [[ "$2" == "$3" ]]; then pass "$1"; else
    fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2
  fi
}
assert_match() {
  if [[ "$3" =~ $2 ]]; then pass "$1"; else
    fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2
  fi
}

if [[ ! -f "$PROVISION_SH" ]]; then
  printf 'FATAL provision-cluster.sh not found at %s\n' "$PROVISION_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

# ---- mock az ---------------------------------------------------------------
# One collapsed log line per invocation (multi-line --scripts are flattened so
# the explicit-subscription invariant holds). `az vm run-command invoke` returns
# 0 for the extension regardless of the node script, so the mock reports node
# success/failure only via the message channel that provision-cluster.sh checks.
cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
scripts=""; query=""; prev=""
for a in "$@"; do
  case "$prev" in
    --scripts) scripts="$a" ;;
    --query)   query="$a" ;;
  esac
  prev="$a"
done
case "$1 $2" in
  "network public-ip")
    [[ "$3" == "show" ]] && echo "20.10.20.30"; exit 0 ;;
  "network nic")
    [[ "$3 $4" == "ip-config list" ]] && printf '10.3.1.20\n10.3.1.21\n'; exit 0 ;;
  "vm run-command")
    [[ "$query" == "value[0].message" ]] || exit 0
    if [[ "$scripts" == *__PNC_OK__* ]]; then
      # A wrapped provisioning script: succeed unless the test forces a failure
      # matching a substring of the node script (B2).
      if [[ -n "${MOCK_NODE_FAIL_MATCH:-}" && "$scripts" == *"${MOCK_NODE_FAIL_MATCH}"* ]]; then
        printf '[stdout]\n__PNC_FAIL__ rc=1\nsimulated node failure\n[stderr]\n'
      else
        printf '[stdout]\n__PNC_OK__\n[stderr]\n'
      fi
    elif [[ "$scripts" == *"get nodes"* ]]; then
      { printf '[stdout]\n'
        r="${MOCK_NODES_READY:-4}"; nr="${MOCK_NODES_NOTREADY:-0}"; i=0
        while (( i < r ));  do printf 'node-r%s   Ready      control-plane   1d   v1.31.0\n' "$i"; i=$((i+1)); done
        i=0
        while (( i < nr )); do printf 'node-n%s   NotReady   worker          1d   v1.31.0\n' "$i"; i=$((i+1)); done
        printf '[stderr]\n'; }
    elif [[ "$scripts" == *"head -15"* && "$scripts" == *admin.conf* ]]; then
      printf '[stdout]\napiVersion: v1\nclusters:\n- cluster:\n    certificate-authority-data: QUFB\n    server: https://%s:6443\n  name: kubernetes\ncontexts:\n- context:\n    cluster: kubernetes\n    user: kubernetes-admin\n  name: admin@kubernetes\ncurrent-context: admin@kubernetes\nkind: Config\npreferences: {}\n[stderr]\n' "${MOCK_CP_IP:-10.3.1.4}"
    elif [[ "$scripts" == *"tail -n +16"* && "$scripts" == *admin.conf* ]]; then
      printf '[stdout]\nusers:\n- name: kubernetes-admin\n  user:\n    client-certificate-data: QkJC\n    client-key-data: Q0ND\n[stderr]\n'
    elif [[ "$scripts" == *print-join-command* ]]; then
      printf '[stdout]\nkubeadm join %s:6443 --token abc.def --discovery-token-ca-cert-hash sha256:deadbeef\n[stderr]\n' "${MOCK_CP_IP:-10.3.1.4}"
    else
      printf '[stdout]\nok\n[stderr]\n'
    fi
    exit 0 ;;
  "account set") exit 0 ;;
  *) exit 0 ;;
esac
AZ
chmod +x "${MOCKBIN}/az"

# run_provision <case> <subcommand> <KEY=VAL...>; sets RC, AZLOG, MANIFEST, KUBEDIR, LOG.
run_provision() {
  local name="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; MANIFEST="${casedir}/run-manifest.json"
  KUBEDIR="${casedir}/kube"; LOG="${casedir}/log"
  mkdir -p "$casedir" "$KUBEDIR"; : > "$AZLOG"
  env \
    MOCK_AZ_LOG="$AZLOG" AZ_BIN="${MOCKBIN}/az" \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" \
    DATE_UTC="20260820" GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
    MANIFEST_PATH="$MANIFEST" KUBECONFIG_DIR="$KUBEDIR" \
    NODE_READY_ATTEMPTS="2" NODE_READY_DELAY="0" \
    "$@" bash "$PROVISION_SH" "$cmd" >"$LOG" 2>&1
  RC=$?
}
az_total()       { grep -c . "$AZLOG"; }
az_with_sub()    { grep -c -- '--subscription' "$AZLOG"; }
az_account_set() { grep -c '^account set' "$AZLOG"; }

echo "== fail fast on missing mandatory inputs (ITEM-007) =="
run_provision miss_sub infra TOPOLOGY=ss REGION=eastus2euap
assert_eq "missing SUBSCRIPTION_ID fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_provision miss_topo infra SUBSCRIPTION_ID=sub-a REGION=eastus2euap
assert_eq "missing TOPOLOGY fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_provision miss_region infra SUBSCRIPTION_ID=sub-a TOPOLOGY=ss
assert_eq "missing REGION fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_provision bad_region infra SUBSCRIPTION_ID=sub-a TOPOLOGY=ss REGION=westus2
assert_eq "non-canary REGION fails fast (CON-001)" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== names subcommand derives deterministic names (naming.sh wiring) =="
run_provision names_ss names SUBSCRIPTION_ID=sub-a TOPOLOGY=ss REGION=eastus2euap
assert_eq "names succeeds" "0" "$RC"
assert_match "names emits the deterministic RG" \
  'resource_group=pnc-e2e-ss-20260820-vahdkc-eastus2euap' "$(cat "$LOG")"

echo "== infra: explicit --subscription everywhere; no 'az account set' (RD-020) =="
run_provision infra_ss infra SUBSCRIPTION_ID=sub-primary TOPOLOGY=ss REGION=eastus2euap
assert_eq "infra succeeds" "0" "$RC"
assert_eq "no 'az account set' is ever called" "0" "$(az_account_set)"
assert_eq "EVERY az call carries an explicit --subscription" "$(az_total)" "$(az_with_sub)"
assert_match "az group create targets the explicit subscription" \
  'group create.*--subscription sub-primary' "$(cat "$AZLOG")"
assert_match "resource group is created with tags" 'group create.*--tags' "$(cat "$AZLOG")"

echo "== infra: resource-group tags include topology/role/correlation (ITEM-008 / 3.4.6) =="
tagline="$(grep 'group create' "$AZLOG" | head -1)"
assert_match "tag validation-topology=ss"               'validation-topology=ss' "$tagline"
assert_match "tag validation-subscription-role=primary" 'validation-subscription-role=primary' "$tagline"
assert_match "tag validation-run-id"                    'validation-run-id=10293847561' "$tagline"
assert_match "tag validation-purpose=pnc-e2e"           'validation-purpose=pnc-e2e' "$tagline"
assert_match "tag managed-by=github-actions"            'managed-by=github-actions' "$tagline"
assert_match "tag ttl-hours"                            'ttl-hours=' "$tagline"

echo "== node-script failure is detected and aborts provisioning (B2 / az returns 0) =="
run_provision nodefail kubernetes SUBSCRIPTION_ID=sub-primary TOPOLOGY=ss REGION=eastus2euap MOCK_NODE_FAIL_MATCH="kubeadm init"
assert_eq "a failing node script aborts (az vm run-command exit 0 is not trusted)" "1" \
  "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== asgs: shared ASGs ONLY in the primary region RG (ITEM-008) =="
run_provision asg_primary asgs SUBSCRIPTION_ID=sub-primary TOPOLOGY=ss REGION=eastus2euap
assert_eq "asgs (primary region) succeeds" "0" "$RC"
assert_eq "creates asg-backend once"  "1" "$(grep -c 'network asg create .*-n asg-backend' "$AZLOG")"
assert_eq "creates asg-frontend once" "1" "$(grep -c 'network asg create .*-n asg-frontend' "$AZLOG")"
assert_match "asg-backend created under explicit subscription" \
  'network asg create.*-n asg-backend.*--subscription sub-primary' "$(cat "$AZLOG")"
assert_match "asg created in the primary region RG" \
  'network asg create.*-g pnc-e2e-ss-20260820-vahdkc-eastus2euap' "$(cat "$AZLOG")"
run_provision asg_secondary asgs SUBSCRIPTION_ID=sub-primary TOPOLOGY=ss REGION=centraluseuap
assert_eq "asgs (non-primary region) succeeds (no-op)" "0" "$RC"
assert_eq "no shared ASG is created outside the primary region" "0" \
  "$(grep -c 'network asg create' "$AZLOG")"

echo "== verify: four Ready nodes via the control plane (ITEM-007 / run-command only) =="
run_provision verify_ok verify SUBSCRIPTION_ID=sub-a TOPOLOGY=ss REGION=eastus2euap MOCK_NODES_READY=4
assert_eq "verify passes with 4 Ready nodes" "0" "$RC"
assert_match "readiness is checked on the control plane via run-command" \
  'vm run-command invoke.*pnc-e2e-ss-20260820-vahdkc-eastus2euap-cp-01.*get nodes' \
  "$(grep 'get nodes' "$AZLOG" | tr '\n' '|')"
run_provision verify_bad verify SUBSCRIPTION_ID=sub-a TOPOLOGY=ss REGION=eastus2euap MOCK_NODES_READY=3 MOCK_NODES_NOTREADY=1
assert_eq "verify fails when fewer than 4 nodes are Ready" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== all (ss / primary region): full orchestration + manifest resource IDs =="
run_provision all_ss_primary all SUBSCRIPTION_ID=sub-primary TOPOLOGY=ss REGION=eastus2euap MOCK_CP_IP=10.3.1.4
assert_eq "all succeeds" "0" "$RC"
assert_eq "no 'az account set' across the full run (RD-020)" "0" "$(az_account_set)"
assert_eq "explicit --subscription invariant holds across the full run" "$(az_total)" "$(az_with_sub)"
assert_eq "manifest records the explicit subscription" "sub-primary" \
  "$(manifest::get "$MANIFEST" '.provision.ss.eastus2euap.subscription_id')"
assert_eq "manifest records subscription role" "primary" \
  "$(manifest::get "$MANIFEST" '.provision.ss.eastus2euap.subscription_role')"
assert_eq "manifest records the resource group" "pnc-e2e-ss-20260820-vahdkc-eastus2euap" \
  "$(manifest::get "$MANIFEST" '.provision.ss.eastus2euap.resource_group')"
assert_match "manifest records a subscription-qualified resource-group ID" \
  '^/subscriptions/sub-primary/resourceGroups/pnc-e2e-ss-20260820-vahdkc-eastus2euap$' \
  "$(manifest::get "$MANIFEST" '.provision.ss.eastus2euap.resource_group_id')"
assert_eq "manifest records four Ready nodes" "4" \
  "$(manifest::get "$MANIFEST" '.provision.ss.eastus2euap.nodes_ready')"
assert_eq "manifest records the two shared ASGs" "asg-backend asg-frontend" \
  "$(manifest::get "$MANIFEST" '.provision.ss.eastus2euap.asgs | join(" ")')"
KCFG="${KUBEDIR}/pnc-e2e-ss-20260820-vahdkc-eastus2euap-kubeconfig.yaml"
assert_eq "kubeconfig file was written" "yes" "$([[ -s "$KCFG" ]] && echo yes || echo no)"
assert_match "kubeconfig server rewritten to the public IP" 'server: https://20.10.20.30:6443' "$(cat "$KCFG" 2>/dev/null)"
assert_match "kubeconfig is intact (chunked fetch reassembled credentials)" 'client-key-data:' "$(cat "$KCFG" 2>/dev/null)"

echo "== all (ss / non-primary region): no shared ASGs recorded =="
run_provision all_ss_secondary all SUBSCRIPTION_ID=sub-primary TOPOLOGY=ss REGION=centraluseuap MOCK_CP_IP=10.4.1.4
assert_eq "all (centraluseuap) succeeds" "0" "$RC"
assert_eq "no shared ASGs recorded for the non-primary region" "0" \
  "$(manifest::get "$MANIFEST" '.provision.ss.centraluseuap.asgs | length')"
assert_eq "manifest still records four Ready nodes" "4" \
  "$(manifest::get "$MANIFEST" '.provision.ss.centraluseuap.nodes_ready')"

echo "== record-on-failure: RG inventory is captured even if a phase fails (B4/RISK-011) =="
run_provision all_fail all SUBSCRIPTION_ID=sub-primary TOPOLOGY=ss REGION=eastus2euap MOCK_CP_IP=10.3.1.4 MOCK_NODE_FAIL_MATCH="kubeadm init"
assert_eq "all fails when the control plane fails to init" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
assert_eq "manifest still inventories the resource group for teardown" "pnc-e2e-ss-20260820-vahdkc-eastus2euap" \
  "$(manifest::get "$MANIFEST" '.provision.ss.eastus2euap.resource_group')"

echo "== xs / secondary region uses the explicit SECONDARY subscription =="
run_provision all_xs_secondary all SUBSCRIPTION_ID=sub-secondary TOPOLOGY=xs REGION=centraluseuap MOCK_CP_IP=10.4.1.4
assert_eq "xs/centraluseuap succeeds" "0" "$RC"
assert_eq "manifest records role=secondary for xs/centraluseuap" "secondary" \
  "$(manifest::get "$MANIFEST" '.provision.xs.centraluseuap.subscription_role')"
assert_eq "every az call targets the secondary subscription explicitly" "$(az_total)" \
  "$(grep -c -- '--subscription sub-secondary' "$AZLOG")"

echo
printf 'provision_cluster_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
