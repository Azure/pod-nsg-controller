#!/usr/bin/env bash
# =============================================================================
# POC Validation Script
# Validates both clusters are healthy and cross-subscription access works.
#
# NOTE (EPIC-010 / ITEM-039): this hand-run POC is the SOURCE that
# scripts/e2e/run-cross-sub-validation.sh generalizes for the automated
# pipeline. The pipeline variant (XSUB-001..003 + Tests 1-4) is parameterized,
# runs the IMDS/ARM probe ON the control-plane node via `az vm run-command`
# (no runner->API path), treats a skipped IMDS/token/ARM check as a FAILURE
# rather than a warning (FR-027), and NEVER surfaces the acquired token
# (sanitized evidence, NFR-012). Prefer the e2e script in CI; keep this POC for
# interactive bring-up.
# =============================================================================
set -euo pipefail

PREFIX="asnStripe"

CLUSTER1_SUB="9bd7ff15-396a-4478-a89b-4d8ae7e302b6"
CLUSTER1_RG="${PREFIX}-westus2"
CLUSTER1_KUBECONFIG="${HOME}/.config/kube/${CLUSTER1_RG}-kubeconfig.yaml"

CLUSTER2_SUB="9b8218f9-902a-4d20-a65c-e98acec5362f"
CLUSTER2_RG="${PREFIX}-eastus2"
CLUSTER2_KUBECONFIG="${HOME}/.config/kube/${CLUSTER2_RG}-kubeconfig.yaml"

CLUSTER3_SUB="9bd7ff15-396a-4478-a89b-4d8ae7e302b6"
CLUSTER3_RG="${PREFIX}-eastus2euap"
CLUSTER3_KUBECONFIG="${HOME}/.config/kube/${CLUSTER3_RG}-kubeconfig.yaml"

PASS=0
FAIL=0

check() {
  local desc="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    echo "  ✅ ${desc}"
    ((PASS++))
  else
    echo "  ❌ ${desc}"
    ((FAIL++))
  fi
}

echo "============================================="
echo " POC Environment Validation"
echo "============================================="

# ─────────────────────────────────────────────
# Cluster 1: asnStripe-westus2
# ─────────────────────────────────────────────
echo ""
echo ">>> Cluster 1: ${CLUSTER1_RG}"

if [ -f "$CLUSTER1_KUBECONFIG" ]; then
  export KUBECONFIG="$CLUSTER1_KUBECONFIG"

  echo "  --- Node Status ---"
  kubectl get nodes -o wide 2>/dev/null || echo "  (could not reach cluster)"

  check "All nodes Ready" bash -c "[ \$(kubectl get nodes --no-headers 2>/dev/null | grep -c 'Ready') -eq 4 ]"
  check "CoreDNS running" kubectl -n kube-system get pods -l k8s-app=kube-dns --no-headers 2>/dev/null
  check "kube-proxy running" kubectl -n kube-system get pods -l k8s-app=kube-proxy --no-headers 2>/dev/null

  # Deploy test pod for IMDS and cross-sub validation
  echo ""
  echo "  --- IMDS & Cross-Subscription Test ---"

  kubectl delete pod arm-test-validate 2>/dev/null || true

  cat <<EOF | kubectl apply -f - 2>/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: arm-test-validate
spec:
  containers:
  - name: curl
    image: curlimages/curl:8.5.0
    command: ["sleep", "300"]
  tolerations:
  - operator: Exists
EOF

  kubectl wait --for=condition=Ready pod/arm-test-validate --timeout=120s 2>/dev/null

  check "IMDS reachable from pod" kubectl exec arm-test-validate -- \
    curl -sf -H "Metadata: true" \
    "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fmanagement.azure.com%2F"

  # Test cross-subscription access: Cluster 1 pod → Cluster 2 RG
  TOKEN=$(kubectl exec arm-test-validate -- \
    curl -sf -H "Metadata: true" \
    "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fmanagement.azure.com%2F" \
    2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])" 2>/dev/null) || true

  if [ -n "${TOKEN:-}" ]; then
    check "Cross-sub ARM call (westus2 → eastus2 RG)" kubectl exec arm-test-validate -- \
      curl -sf \
      -H "Authorization: Bearer ${TOKEN}" \
      -H "Content-Type: application/json" \
      "https://management.azure.com/subscriptions/${CLUSTER2_SUB}/resourceGroups/${CLUSTER2_RG}?api-version=2021-04-01"
  else
    echo "  ⚠️  Could not acquire token — skipping cross-sub test"
  fi

  kubectl delete pod arm-test-validate 2>/dev/null || true

else
  echo "  ⚠️  Kubeconfig not found: ${CLUSTER1_KUBECONFIG}"
  ((FAIL++))
fi

# ─────────────────────────────────────────────
# Cluster 2: asnStripe-eastus2
# ─────────────────────────────────────────────
echo ""
echo ">>> Cluster 2: ${CLUSTER2_RG}"

if [ -f "$CLUSTER2_KUBECONFIG" ]; then
  export KUBECONFIG="$CLUSTER2_KUBECONFIG"

  echo "  --- Node Status ---"
  kubectl get nodes -o wide 2>/dev/null || echo "  (could not reach cluster)"

  check "All nodes Ready" bash -c "[ \$(kubectl get nodes --no-headers 2>/dev/null | grep -c 'Ready') -eq 4 ]"
  check "CoreDNS running" kubectl -n kube-system get pods -l k8s-app=kube-dns --no-headers 2>/dev/null
  check "kube-proxy running" kubectl -n kube-system get pods -l k8s-app=kube-proxy --no-headers 2>/dev/null

  echo ""
  echo "  --- IMDS & Cross-Subscription Test ---"

  kubectl delete pod arm-test-validate 2>/dev/null || true

  cat <<EOF | kubectl apply -f - 2>/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: arm-test-validate
spec:
  containers:
  - name: curl
    image: curlimages/curl:8.5.0
    command: ["sleep", "300"]
  tolerations:
  - operator: Exists
EOF

  kubectl wait --for=condition=Ready pod/arm-test-validate --timeout=120s 2>/dev/null

  check "IMDS reachable from pod" kubectl exec arm-test-validate -- \
    curl -sf -H "Metadata: true" \
    "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fmanagement.azure.com%2F"

  # Test cross-subscription access: Cluster 2 pod → Cluster 1 RG
  TOKEN=$(kubectl exec arm-test-validate -- \
    curl -sf -H "Metadata: true" \
    "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fmanagement.azure.com%2F" \
    2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])" 2>/dev/null) || true

  if [ -n "${TOKEN:-}" ]; then
    check "Cross-sub ARM call (eastus2 → westus2 RG)" kubectl exec arm-test-validate -- \
      curl -sf \
      -H "Authorization: Bearer ${TOKEN}" \
      -H "Content-Type: application/json" \
      "https://management.azure.com/subscriptions/${CLUSTER1_SUB}/resourceGroups/${CLUSTER1_RG}?api-version=2021-04-01"
  else
    echo "  ⚠️  Could not acquire token — skipping cross-sub test"
  fi

  kubectl delete pod arm-test-validate 2>/dev/null || true

else
  echo "  ⚠️  Kubeconfig not found: ${CLUSTER2_KUBECONFIG}"
  ((FAIL++))
fi

# ─────────────────────────────────────────────
# Cluster 3: asnStripe-eastus2euap
# ─────────────────────────────────────────────
echo ""
echo ">>> Cluster 3: ${CLUSTER3_RG}"

if [ -f "$CLUSTER3_KUBECONFIG" ]; then
  export KUBECONFIG="$CLUSTER3_KUBECONFIG"

  echo "  --- Node Status ---"
  kubectl get nodes -o wide 2>/dev/null || echo "  (could not reach cluster)"

  check "All nodes Ready" bash -c "[ \$(kubectl get nodes --no-headers 2>/dev/null | grep -c 'Ready') -eq 4 ]"
  check "CoreDNS running" kubectl -n kube-system get pods -l k8s-app=kube-dns --no-headers 2>/dev/null
  check "kube-proxy running" kubectl -n kube-system get pods -l k8s-app=kube-proxy --no-headers 2>/dev/null

  echo ""
  echo "  --- IMDS & Cross-Subscription Test ---"

  kubectl delete pod arm-test-validate 2>/dev/null || true

  cat <<EOF | kubectl apply -f - 2>/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: arm-test-validate
spec:
  containers:
  - name: curl
    image: curlimages/curl:8.5.0
    command: ["sleep", "300"]
  tolerations:
  - operator: Exists
EOF

  kubectl wait --for=condition=Ready pod/arm-test-validate --timeout=120s 2>/dev/null

  check "IMDS reachable from pod" kubectl exec arm-test-validate -- \
    curl -sf -H "Metadata: true" \
    "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fmanagement.azure.com%2F"

  # Test cross-subscription access: Cluster 3 pod → Cluster 2 RG (different subscription)
  TOKEN=$(kubectl exec arm-test-validate -- \
    curl -sf -H "Metadata: true" \
    "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fmanagement.azure.com%2F" \
    2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])" 2>/dev/null) || true

  if [ -n "${TOKEN:-}" ]; then
    check "Cross-sub ARM call (eastus2euap → eastus2 RG)" kubectl exec arm-test-validate -- \
      curl -sf \
      -H "Authorization: Bearer ${TOKEN}" \
      -H "Content-Type: application/json" \
      "https://management.azure.com/subscriptions/${CLUSTER2_SUB}/resourceGroups/${CLUSTER2_RG}?api-version=2021-04-01"
  else
    echo "  ⚠️  Could not acquire token — skipping cross-sub test"
  fi

  kubectl delete pod arm-test-validate 2>/dev/null || true

else
  echo "  ⚠️  Kubeconfig not found: ${CLUSTER3_KUBECONFIG}"
  ((FAIL++))
fi

# ─────────────────────────────────────────────
# Summary
# ─────────────────────────────────────────────
echo ""
echo "============================================="
echo " Results: ${PASS} passed, ${FAIL} failed"
echo "============================================="

[ "$FAIL" -eq 0 ] && exit 0 || exit 1
