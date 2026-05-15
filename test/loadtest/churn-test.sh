#!/usr/bin/env bash
# Pod Churn Load Test for pod-nsg-controller (Parallelized)
#
# Achieves 300 pod ops/min using parallel kubectl workers and pre-generated
# manifests. Focuses on measuring controller reconciliation latency — the time
# from a pod event to the ASG prefix set being updated in ARM.
#
# Architecture:
#   - Pre-generates manifest batches on disk (zero kubectl overhead at fire time)
#   - Dispatches create/delete ops via GNU parallel or background workers
#   - Polls PodASGMapping status every 2s for fine-grained latency measurement
#   - Records pod Running+IP timestamps via kubectl wait + jsonpath
set -euo pipefail

export KUBECONFIG="${KUBECONFIG:-/home/asn/DevOps/asnStripe-eastus2euap-kubeconfig.yaml}"
NAMESPACE="test-apps"
DURATION_MINUTES=10
OPS_PER_MINUTE=300
PARALLEL_WORKERS=20        # concurrent kubectl processes
MAX_ACTIVE_PODS=350        # cap to stay within cluster capacity (~425 allocatable)
POD_IMAGE="registry.k8s.io/pause:3.9"
STATUS_POLL_INTERVAL=2     # seconds between PodASGMapping status polls

LOG_DIR="/tmp/churn-test-$(date +%Y%m%d-%H%M%S)"
MANIFEST_DIR="$LOG_DIR/manifests"
mkdir -p "$LOG_DIR" "$MANIFEST_DIR"

ROLES=("frontend" "backend")

echo "================================================================"
echo "  Pod Churn Load Test (Parallelized)"
echo "  Target: $OPS_PER_MINUTE ops/min for $DURATION_MINUTES minutes"
echo "  Workers: $PARALLEL_WORKERS  Max active: $MAX_ACTIVE_PODS"
echo "  Namespace: $NAMESPACE"
echo "  Log dir: $LOG_DIR"
echo "================================================================"

# ── Manifest generator ─────────────────────────────────────────────
generate_manifest() {
    local name="$1" role="$2"
    cat > "$MANIFEST_DIR/${name}.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    app: churn-test
    role: ${role}
spec:
  containers:
  - name: pause
    image: ${POD_IMAGE}
    resources:
      requests:
        cpu: 10m
        memory: 16Mi
      limits:
        cpu: 50m
        memory: 32Mi
  terminationGracePeriodSeconds: 0
EOF
}

# ── Parallel worker functions (called via xargs -P) ────────────────
do_create() {
    local manifest="$1"
    local name role ts
    name=$(basename "$manifest" .yaml)
    role=$(grep 'role:' "$manifest" | awk '{print $2}')
    ts=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
    if kubectl apply -f "$manifest" > /dev/null 2>&1; then
        echo "${ts},CREATE,${name},${role}" >> "$LOG_DIR/events.csv"
    else
        echo "${ts},CREATE_FAIL,${name},${role}" >> "$LOG_DIR/events.csv"
    fi
}
export -f do_create
export LOG_DIR NAMESPACE

do_delete() {
    local name="$1"
    local ts
    ts=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
    kubectl delete pod -n "$NAMESPACE" "$name" --grace-period=0 --force > /dev/null 2>&1 || true
    echo "${ts},DELETE,${name}," >> "$LOG_DIR/events.csv"
}
export -f do_delete

# ── Capture pre-test controller logs ───────────────────────────────
kubectl logs -n pod-nsg-controller-system deploy/pod-nsg-controller --timestamps \
    > "$LOG_DIR/controller_logs_before.txt" 2>&1 || true

TEST_START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
TEST_START_EPOCH=$(date +%s)
echo "$TEST_START" > "$LOG_DIR/test_start.txt"

# ── CSV headers ────────────────────────────────────────────────────
echo "timestamp,operation,pod_name,role" > "$LOG_DIR/events.csv"

# ── Background: poll PodASGMapping status every 2s ─────────────────
(
    echo "timestamp,mapping,reconciled,matchedPods,syncState" > "$LOG_DIR/mapping_status.csv"
    while true; do
        ts=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
        for mapping in frontend-asg-mapping backend-asg-mapping; do
            json=$(kubectl get podasgmapping -n "$NAMESPACE" "$mapping" \
                -o jsonpath='{.status.conditions[?(@.type=="Reconciled")].status},{.status.mappingStatuses[0].matchedPods},{.status.mappingStatuses[0].asgSyncState}' 2>/dev/null || echo "Unknown,0,Unknown")
            echo "${ts},${mapping},${json}" >> "$LOG_DIR/mapping_status.csv"
        done
        sleep "$STATUS_POLL_INTERVAL"
    done
) &
STATUS_PID=$!

# ── Background: track pod IP assignments every 3s ──────────────────
(
    echo "timestamp,pod_name,pod_ip,phase" > "$LOG_DIR/pod_ips.csv"
    declare -A SEEN_IPS=()
    while true; do
        ts=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
        while IFS= read -r line; do
            # line format: "pod_name ip phase"
            pod_name=$(echo "$line" | awk '{print $1}')
            pod_ip=$(echo "$line" | awk '{print $2}')
            phase=$(echo "$line" | awk '{print $3}')
            if [ -n "$pod_ip" ] && [ "$pod_ip" != "<none>" ] && [ -z "${SEEN_IPS[$pod_name]:-}" ]; then
                echo "${ts},${pod_name},${pod_ip},${phase}" >> "$LOG_DIR/pod_ips.csv"
                SEEN_IPS["$pod_name"]=1
            fi
        done < <(kubectl get pods -n "$NAMESPACE" -l app=churn-test \
            -o custom-columns='NAME:.metadata.name,IP:.status.podIP,PHASE:.status.phase' \
            --no-headers 2>/dev/null || true)
        sleep 3
    done
) &
IPTRACK_PID=$!

echo ""
echo "Starting churn test at $(date -u +%H:%M:%S)..."
echo ""

# ── Main churn loop ────────────────────────────────────────────────
declare -a ACTIVE_PODS=()
ACTIVE_FILE="$LOG_DIR/active_pods.txt"
: > "$ACTIVE_FILE"

TOTAL_CREATED=0
TOTAL_DELETED=0
END_EPOCH=$((TEST_START_EPOCH + DURATION_MINUTES * 60))
LAST_PRINT_EPOCH=$TEST_START_EPOCH

# Each tick fires a batch of ops; target OPS_PER_MINUTE / 6 = 50 ops every 10s
BATCH_INTERVAL=10
OPS_PER_BATCH=$(( OPS_PER_MINUTE * BATCH_INTERVAL / 60 ))

while [ "$(date +%s)" -lt "$END_EPOCH" ]; do
    NOW_EPOCH=$(date +%s)
    ELAPSED=$((NOW_EPOCH - TEST_START_EPOCH))
    ACTIVE_COUNT=${#ACTIVE_PODS[@]}

    # ── Progress report every 30s ──
    if [ $((NOW_EPOCH - LAST_PRINT_EPOCH)) -ge 30 ]; then
        LAST_PRINT_EPOCH=$NOW_EPOCH
        TOTAL_OPS=$((TOTAL_CREATED + TOTAL_DELETED))
        RATE=0
        [ $ELAPSED -gt 0 ] && RATE=$(( TOTAL_OPS * 60 / ELAPSED ))
        echo "[${ELAPSED}s] Created=$TOTAL_CREATED Deleted=$TOTAL_DELETED Active=$ACTIVE_COUNT Rate=${RATE} ops/min"
    fi

    # ── Decide create vs delete ratio for this batch ──
    # If near capacity, bias toward deletes; otherwise ~50/50
    if [ $ACTIVE_COUNT -ge $MAX_ACTIVE_PODS ]; then
        NUM_CREATES=0
        NUM_DELETES=$OPS_PER_BATCH
    elif [ $ACTIVE_COUNT -lt 30 ]; then
        # Ramp-up phase: mostly creates
        NUM_CREATES=$OPS_PER_BATCH
        NUM_DELETES=0
    else
        NUM_CREATES=$(( OPS_PER_BATCH / 2 ))
        NUM_DELETES=$(( OPS_PER_BATCH - NUM_CREATES ))
        # Clamp deletes to available pods
        [ $NUM_DELETES -gt $ACTIVE_COUNT ] && NUM_DELETES=$ACTIVE_COUNT
    fi

    # ── Generate create manifests ──
    CREATE_LIST="$LOG_DIR/create_batch.txt"
    : > "$CREATE_LIST"
    for _ in $(seq 1 $NUM_CREATES); do
        role="${ROLES[$((RANDOM % 2))]}"
        pod_name="churn-${role}-${ELAPSED}-${RANDOM}"
        generate_manifest "$pod_name" "$role"
        echo "$MANIFEST_DIR/${pod_name}.yaml" >> "$CREATE_LIST"
        ACTIVE_PODS+=("$pod_name")
    done

    # ── Generate delete list ──
    DELETE_LIST="$LOG_DIR/delete_batch.txt"
    : > "$DELETE_LIST"
    for _ in $(seq 1 $NUM_DELETES); do
        if [ ${#ACTIVE_PODS[@]} -eq 0 ]; then break; fi
        idx=$((RANDOM % ${#ACTIVE_PODS[@]}))
        echo "${ACTIVE_PODS[$idx]}" >> "$DELETE_LIST"
        # Swap-remove
        ACTIVE_PODS[$idx]="${ACTIVE_PODS[-1]}"
        unset 'ACTIVE_PODS[-1]'
    done

    # ── Fire creates in parallel ──
    if [ -s "$CREATE_LIST" ]; then
        cat "$CREATE_LIST" | xargs -P "$PARALLEL_WORKERS" -I{} bash -c 'do_create "{}"' &
        CREATE_BG=$!
    fi

    # ── Fire deletes in parallel ──
    if [ -s "$DELETE_LIST" ]; then
        cat "$DELETE_LIST" | xargs -P "$PARALLEL_WORKERS" -I{} bash -c 'do_delete "{}"' &
        DELETE_BG=$!
    fi

    # Wait for this batch to finish
    [ -n "${CREATE_BG:-}" ] && wait "$CREATE_BG" 2>/dev/null || true
    [ -n "${DELETE_BG:-}" ] && wait "$DELETE_BG" 2>/dev/null || true
    unset CREATE_BG DELETE_BG

    TOTAL_CREATED=$((TOTAL_CREATED + NUM_CREATES))
    TOTAL_DELETED=$((TOTAL_DELETED + NUM_DELETES))

    # ── Pace to batch interval ──
    BATCH_END=$(date +%s)
    BATCH_ELAPSED=$((BATCH_END - NOW_EPOCH))
    if [ $BATCH_ELAPSED -lt $BATCH_INTERVAL ]; then
        sleep $((BATCH_INTERVAL - BATCH_ELAPSED))
    fi
done

echo ""
echo "Churn phase complete. Created=$TOTAL_CREATED Deleted=$TOTAL_DELETED Active=${#ACTIVE_PODS[@]}"
echo ""

TEST_END=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "$TEST_END" > "$LOG_DIR/test_end.txt"

# ── Wait for final reconciliation ──────────────────────────────────
echo "Waiting up to 120s for final reconciliation..."
for i in $(seq 1 24); do
    sleep 5
    fe_status=$(kubectl get podasgmapping -n "$NAMESPACE" frontend-asg-mapping \
        -o jsonpath='{.status.conditions[?(@.type=="Reconciled")].status}' 2>/dev/null || echo "Unknown")
    be_status=$(kubectl get podasgmapping -n "$NAMESPACE" backend-asg-mapping \
        -o jsonpath='{.status.conditions[?(@.type=="Reconciled")].status}' 2>/dev/null || echo "Unknown")
    fe_matched=$(kubectl get podasgmapping -n "$NAMESPACE" frontend-asg-mapping \
        -o jsonpath='{.status.mappingStatuses[0].matchedPods}' 2>/dev/null || echo "?")
    be_matched=$(kubectl get podasgmapping -n "$NAMESPACE" backend-asg-mapping \
        -o jsonpath='{.status.mappingStatuses[0].matchedPods}' 2>/dev/null || echo "?")
    echo "  [${i}] frontend: ${fe_status} (${fe_matched} pods)  backend: ${be_status} (${be_matched} pods)"
    if [ "$fe_status" = "True" ] && [ "$be_status" = "True" ]; then
        echo "  Both mappings reconciled."
        break
    fi
done

# ── Cleanup background monitors ───────────────────────────────────
kill $STATUS_PID 2>/dev/null || true
kill $IPTRACK_PID 2>/dev/null || true
wait $STATUS_PID 2>/dev/null || true
wait $IPTRACK_PID 2>/dev/null || true

# ── Capture controller logs for the test window ───────────────────
kubectl logs -n pod-nsg-controller-system deploy/pod-nsg-controller --timestamps \
    --since-time="$TEST_START" > "$LOG_DIR/controller_logs_during.txt" 2>&1 || true

# ── Final counts ──────────────────────────────────────────────────
REMAINING_FE=$(kubectl get pods -n "$NAMESPACE" -l app=churn-test,role=frontend --no-headers 2>/dev/null | wc -l)
REMAINING_BE=$(kubectl get pods -n "$NAMESPACE" -l app=churn-test,role=backend --no-headers 2>/dev/null | wc -l)
TOTAL_OPS=$((TOTAL_CREATED + TOTAL_DELETED))
EFFECTIVE_RATE=$(( TOTAL_OPS * 60 / (DURATION_MINUTES * 60) ))

echo ""
echo "================================================================"
echo "  Churn Test Complete"
echo "================================================================"
echo "  Duration:       $DURATION_MINUTES minutes"
echo "  Total Created:  $TOTAL_CREATED"
echo "  Total Deleted:  $TOTAL_DELETED"
echo "  Total Ops:      $TOTAL_OPS"
echo "  Effective Rate: ${EFFECTIVE_RATE} ops/min"
echo "  Remaining Pods: frontend=$REMAINING_FE backend=$REMAINING_BE"
echo "  Log dir:        $LOG_DIR"
echo "================================================================"
echo ""
echo "Run analysis:  bash test/loadtest/analyze-churn.sh $LOG_DIR"
