#!/usr/bin/env bash
# =============================================================================
# deploy_controller_test.sh - behavioural tests for ../deploy-controller.sh
# (EPIC-004 / ITEM-011 candidate deploy + imagePullSecret; ITEM-012 mappings,
# workloads, and baseline).
#
# Fully hermetic: `az` is a deterministic mock (no real cloud, no ACR, no VMs).
# The GitHub runner has NO path to the cluster API server, so kubectl is
# executed on the control-plane node via `az vm run-command`; the mock models
# that seam (message envelope + "extension always exits 0"), the repo-scoped ACR
# token minting, and node-side pod/mapping readbacks. The suite proves:
#   * mandatory explicit inputs (subscription, topology, region, image, ACR);
#   * EVERY Azure call carries an explicit --subscription; NEVER `az account set`;
#   * the rendered bundle overrides the image to the candidate digest, disables
#     imagePullPolicy: Never, and carries CRD + RBAC + manager + the documented
#     PodASGMappings and Cluster-A/Cluster-B workload asymmetry (CON-003);
#   * a short-lived, repository-scoped ACR token becomes the imagePullSecret;
#   * kubectl runs on the control-plane node (deploy + baseline reads);
#   * CLUSTER_NAME is set to the cluster RG so the controller's ownership key
#     matches the documented address-prefix-set naming;
#   * a node-side failure aborts the deploy (az run-command exit 0 not trusted);
#   * the run manifest records the deploy coordinates + baseline (A=4, B=0).
#
# Traceability: ITEM-011, ITEM-012, FR-004, FR-011, SEC-004, RD-004, RD-020,
# CON-003, PRD Sections 3.4/3.5.
# =============================================================================
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_SH="${TEST_DIR}/../deploy-controller.sh"
LIB_SH="${TEST_DIR}/../lib.sh"
WORK="${TEST_DIR}/.deploy_test_work"
MOCKBIN="${WORK}/bin"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1"; printf '        expected [%s] got [%s]\n' "$2" "$3" >&2; fi; }
assert_match() { if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1"; printf '        value [%s] did not match /%s/\n' "$3" "$2" >&2; fi; }
assert_contains() { if [[ "$3" == *"$2"* ]]; then pass "$1"; else fail "$1"; printf '        [%s] did not contain [%s]\n' "$3" "$2" >&2; fi; }

if [[ ! -f "$DEPLOY_SH" ]]; then
  printf 'FATAL deploy-controller.sh not found at %s\n' "$DEPLOY_SH" >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$LIB_SH"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$MOCKBIN"

IMG_REF="pncstg.azurecr.io/candidate/pod-nsg-controller@sha256:$(printf 'a%.0s' $(seq 1 64))"

# ---- mock az ----------------------------------------------------------------
cat > "${MOCKBIN}/az" <<'AZ'
#!/usr/bin/env bash
{ printf '%s' "$*" | tr '\n\t' '  '; printf '\n'; } >> "${MOCK_AZ_LOG}"
scripts=""; query=""; prev=""
for a in "$@"; do
  case "$prev" in --scripts) scripts="$a" ;; --query) query="$a" ;; esac
  prev="$a"
done
case "$1 $2" in
  "acr token")
    # `acr token credential generate` emits the (fake) password; create is a no-op.
    [[ "$3" == "credential" ]] && echo "s3cr3t-pass-value"
    exit 0 ;;
  "vm run-command")
    [[ "$query" == "value[0].message" ]] || exit 0
    if [[ "$scripts" == *__AZRUN_OK__* ]]; then
      if [[ -n "${MOCK_NODE_FAIL_MATCH:-}" && "$scripts" == *"${MOCK_NODE_FAIL_MATCH}"* ]]; then
        printf '[stdout]\n__AZRUN_FAIL__ rc=1\nsimulated node failure\n[stderr]\n'
      else
        printf '[stdout]\n__AZRUN_OK__\n[stderr]\n'
      fi
    elif [[ "$scripts" == *"get podasgmappings"* ]]; then
      printf '[stdout]\npodasgmapping.networking.azure.com/backend-asg-mapping\npodasgmapping.networking.azure.com/frontend-asg-mapping\n[stderr]\n'
    elif [[ "$scripts" == *"get pods"* ]]; then
      { printf '[stdout]\n'; n="${MOCK_POD_COUNT:-2}"; i=0
        while (( i < n )); do printf 'pod-%s Running\n' "$i"; i=$((i+1)); done
        printf '[stderr]\n'; }
    else
      printf '[stdout]\nok\n[stderr]\n'
    fi
    exit 0 ;;
  "account set") exit 0 ;;
  *) exit 0 ;;
esac
AZ
chmod +x "${MOCKBIN}/az"

# run_deploy <case> <subcommand> <KEY=VAL...>; sets RC, AZLOG, MANIFEST, RENDER, LOG.
run_deploy() {
  local name="$1" cmd="$2"; shift 2
  local casedir="${WORK}/${name}"
  AZLOG="${casedir}/az.log"; MANIFEST="${casedir}/run-manifest.json"
  RENDER="${casedir}/render"; LOG="${casedir}/log"
  mkdir -p "$casedir" "$RENDER"; : > "$AZLOG"
  env \
    MOCK_AZ_LOG="$AZLOG" AZ_BIN="${MOCKBIN}/az" \
    REPO="Azure/pod-nsg-controller" RUN_ID="10293847561" RUN_ATTEMPT="1" \
    DATE_UTC="20260820" GIT_SHA="8504b2e1c3a9" GIT_REF="refs/heads/test" \
    MANIFEST_PATH="$MANIFEST" RENDER_DIR="$RENDER" \
    ROLLOUT_TIMEOUT="5s" BASELINE_ATTEMPTS="2" BASELINE_DELAY="0" \
    AZRUN_ATTEMPTS="2" AZRUN_DELAY="0" \
    "$@" bash "$DEPLOY_SH" "$cmd" >"$LOG" 2>&1
  RC=$?
}
az_total()       { grep -c . "$AZLOG"; }
az_with_sub()    { grep -c -- '--subscription' "$AZLOG"; }
az_account_set() { grep -c '^account set' "$AZLOG"; }

echo "== fail fast on missing mandatory inputs (ITEM-011) =="
run_deploy miss_topo render REGION=eastus2euap PRIMARY_SUBSCRIPTION_ID=sub-a IMAGE_REFERENCE="$IMG_REF"
assert_eq "missing TOPOLOGY fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_deploy miss_region render TOPOLOGY=ss PRIMARY_SUBSCRIPTION_ID=sub-a IMAGE_REFERENCE="$IMG_REF"
assert_eq "missing REGION fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_deploy miss_sub render TOPOLOGY=ss REGION=eastus2euap IMAGE_REFERENCE="$IMG_REF"
assert_eq "missing PRIMARY_SUBSCRIPTION_ID fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_deploy miss_img render TOPOLOGY=ss REGION=eastus2euap PRIMARY_SUBSCRIPTION_ID=sub-a
assert_eq "missing IMAGE_REFERENCE fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_deploy bad_region render TOPOLOGY=ss REGION=westus2 PRIMARY_SUBSCRIPTION_ID=sub-a IMAGE_REFERENCE="$IMG_REF"
assert_eq "non-canary REGION fails fast (CON-001)" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"
run_deploy miss_acr deploy TOPOLOGY=ss REGION=eastus2euap PRIMARY_SUBSCRIPTION_ID=sub-a IMAGE_REFERENCE="$IMG_REF"
assert_eq "deploy without STAGING_ACR fails" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== render (Cluster A / eastus2euap): image digest override + Never disabled =="
run_deploy render_a render TOPOLOGY=ss REGION=eastus2euap PRIMARY_SUBSCRIPTION_ID=sub-primary IMAGE_REFERENCE="$IMG_REF"
assert_eq "render succeeds" "0" "$RC"
CORE="$(cat "${RENDER}"/deploy-core-ss-eastus2euap.yaml 2>/dev/null)"
WL="$(cat "${RENDER}"/deploy-workload-ss-eastus2euap.yaml 2>/dev/null)"
assert_contains "core carries the CRD" "kind: CustomResourceDefinition" "$CORE"
assert_contains "core carries the controller ServiceAccount" "name: pod-nsg-controller" "$CORE"
assert_contains "core overrides image to the candidate digest" "image: ${IMG_REF}" "$CORE"
assert_eq "core does NOT keep imagePullPolicy: Never" "0" "$(printf '%s' "$CORE" | grep -c 'imagePullPolicy: Never')"
assert_contains "core sets imagePullPolicy: IfNotPresent" "imagePullPolicy: IfNotPresent" "$CORE"
assert_contains "workload has the backend PodASGMapping" "name: backend-asg-mapping" "$WL"
assert_contains "workload has the frontend PodASGMapping" "name: frontend-asg-mapping" "$WL"
assert_contains "Cluster A mappings live in test-apps" "namespace: test-apps" "$WL"
assert_contains "Cluster A selector uses the 'role' label (CON-003)" "role: backend" "$WL"
assert_contains "backend mapping targets the shared asg-backend by resourceId" \
  "applicationSecurityGroups/asg-backend" "$WL"
assert_match "asg resourceId is under the PRIMARY subscription + primary RG" \
  '/subscriptions/sub-primary/resourceGroups/pnc-e2e-ss-20260820-vahdkc-eastus2euap/providers/Microsoft.Network/applicationSecurityGroups/asg-frontend' "$WL"
assert_contains "Cluster A defines a backend Deployment" "kind: Deployment" "$WL"
assert_match "Cluster A backend Deployment has baseline replicas 2" 'replicas: 2' "$WL"

echo "== render (Cluster B / centraluseuap): default ns, app label, NO workloads =="
run_deploy render_b render TOPOLOGY=ss REGION=centraluseuap PRIMARY_SUBSCRIPTION_ID=sub-primary IMAGE_REFERENCE="$IMG_REF"
assert_eq "render (B) succeeds" "0" "$RC"
WLB="$(cat "${RENDER}"/deploy-workload-ss-centraluseuap.yaml 2>/dev/null)"
assert_contains "Cluster B mappings live in default" "namespace: default" "$WLB"
assert_contains "Cluster B selector uses the 'app' label (CON-003)" "app: backend" "$WLB"
assert_eq "Cluster B has NO Deployment (standalone pods; baseline 0)" "0" \
  "$(printf '%s' "$WLB" | grep -c 'kind: Deployment')"

echo "== deploy (Cluster A): explicit --subscription, repo-scoped token, node kubectl =="
run_deploy deploy_a deploy TOPOLOGY=ss REGION=eastus2euap PRIMARY_SUBSCRIPTION_ID=sub-primary \
  IMAGE_REFERENCE="$IMG_REF" STAGING_ACR=pncstg.azurecr.io
assert_eq "deploy succeeds" "0" "$RC"
assert_eq "no 'az account set' is ever called (RD-020)" "0" "$(az_account_set)"
assert_eq "EVERY az call carries an explicit --subscription" "$(az_total)" "$(az_with_sub)"
assert_match "mints a REPOSITORY-scoped ACR token (SEC-004/RD-004)" \
  'acr token create.*--repository candidate/pod-nsg-controller content/read' "$(cat "$AZLOG")"
assert_match "generates a short-lived token credential" \
  'acr token credential generate.*--expiration' "$(cat "$AZLOG")"
NODE_APPLY="$(grep 'run-command invoke' "$AZLOG" | grep 'kubectl apply' | head -1)"
assert_match "kubectl apply runs on the control-plane node via run-command" \
  'run-command invoke -g pnc-e2e-ss-20260820-vahdkc-eastus2euap -n pnc-e2e-ss-20260820-vahdkc-eastus2euap-cp-01' \
  "$NODE_APPLY"
assert_contains "creates a docker-registry imagePullSecret from the ACR token" \
  "create secret docker-registry" "$(cat "$AZLOG")"
assert_contains "docker-server points at the staging ACR" \
  "--docker-server=pncstg.azurecr.io" "$(cat "$AZLOG")"
assert_contains "sets CLUSTER_NAME to the cluster RG (ownership-key match)" \
  "CLUSTER_NAME=pnc-e2e-ss-20260820-vahdkc-eastus2euap" "$(cat "$AZLOG")"
assert_contains "injects imagePullSecrets into the manager Deployment" \
  "imagePullSecrets" "$(cat "$AZLOG")"

echo "== deploy: node-script failure aborts (az run-command exit 0 not trusted) =="
run_deploy deploy_fail deploy TOPOLOGY=ss REGION=eastus2euap PRIMARY_SUBSCRIPTION_ID=sub-primary \
  IMAGE_REFERENCE="$IMG_REF" STAGING_ACR=pncstg.azurecr.io MOCK_NODE_FAIL_MATCH="rollout status"
assert_eq "a failing node deploy aborts" "1" "$([[ $RC -ne 0 ]] && echo 1 || echo 0)"

echo "== all (Cluster A): baseline (A=4) + manifest record =="
run_deploy all_a all TOPOLOGY=ss REGION=eastus2euap PRIMARY_SUBSCRIPTION_ID=sub-primary \
  IMAGE_REFERENCE="$IMG_REF" STAGING_ACR=pncstg.azurecr.io MOCK_POD_COUNT=2
assert_eq "all (A) succeeds" "0" "$RC"
assert_eq "manifest records the deploy image reference" "$IMG_REF" \
  "$(manifest::get "$MANIFEST" '.deploy.ss.eastus2euap.image_reference')"
assert_eq "manifest records CLUSTER_NAME=cluster RG" "pnc-e2e-ss-20260820-vahdkc-eastus2euap" \
  "$(manifest::get "$MANIFEST" '.deploy.ss.eastus2euap.cluster_name')"
assert_eq "manifest records the controller namespace" "pod-nsg-controller-system" \
  "$(manifest::get "$MANIFEST" '.deploy.ss.eastus2euap.controller_namespace')"
assert_eq "manifest records baseline expected pods (A=4)" "4" \
  "$(manifest::get "$MANIFEST" '.deploy.ss.eastus2euap.baseline.expected_pods')"
assert_eq "manifest records the workload namespace" "test-apps" \
  "$(manifest::get "$MANIFEST" '.deploy.ss.eastus2euap.namespace')"
assert_match "manifest records the backend prefix-set name" \
  '^pnc-e2e-ss-20260820-vahdkc-eastus2euap-test-apps-backend-asg-mapping$' \
  "$(manifest::get "$MANIFEST" '.deploy.ss.eastus2euap.prefix_sets.backend')"

echo "== all (Cluster B): baseline (B=0) =="
run_deploy all_b all TOPOLOGY=ss REGION=centraluseuap PRIMARY_SUBSCRIPTION_ID=sub-primary \
  IMAGE_REFERENCE="$IMG_REF" STAGING_ACR=pncstg.azurecr.io MOCK_POD_COUNT=0
assert_eq "all (B) succeeds" "0" "$RC"
assert_eq "manifest records baseline expected pods (B=0)" "0" \
  "$(manifest::get "$MANIFEST" '.deploy.ss.centraluseuap.baseline.expected_pods')"
assert_eq "Cluster B workload namespace is default" "default" \
  "$(manifest::get "$MANIFEST" '.deploy.ss.centraluseuap.namespace')"

echo "== xs / Cluster B uses the SECONDARY subscription for node ops, PRIMARY for ASGs =="
run_deploy xs_b all TOPOLOGY=xs REGION=centraluseuap PRIMARY_SUBSCRIPTION_ID=sub-primary \
  SECONDARY_SUBSCRIPTION_ID=sub-secondary IMAGE_REFERENCE="$IMG_REF" STAGING_ACR=pncstg.azurecr.io MOCK_POD_COUNT=0
assert_eq "xs/centraluseuap succeeds" "0" "$RC"
assert_match "node kubectl targets the SECONDARY subscription (Cluster B owner)" \
  'run-command invoke.*--subscription sub-secondary' "$(grep 'run-command invoke' "$AZLOG" | tr '\n' '|')"
assert_eq "manifest records role=secondary for xs Cluster B" "secondary" \
  "$(manifest::get "$MANIFEST" '.deploy.xs.centraluseuap.subscription_role')"
assert_match "ASG resourceId stays under the PRIMARY subscription for xs" \
  '/subscriptions/sub-primary/resourceGroups/pnc-e2e-xs-20260820-vahdkc-eastus2euap/' \
  "$(manifest::get "$MANIFEST" '.deploy.xs.centraluseuap.asgs.backend')"

echo
printf 'deploy_controller_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
