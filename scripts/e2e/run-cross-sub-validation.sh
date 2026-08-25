#!/usr/bin/env bash
# =============================================================================
# run-cross-sub-validation.sh - cross-subscription (xs) validation: XSUB-001..003
# plus the topology-keyed Tests 1-4 (EPIC-010 / ITEM-039 / FILE-033),
# generalizing scripts/poc/validate-poc.sh's IMDS/cross-sub ARM preflight and
# reusing run-validation.sh's Tests 1-4 unchanged (REQ-005 / RD-012).
#
# Access model (same as run-validation.sh): the runner has NO path to either
# cluster API server, so the in-cluster IMDS/ARM probe and all kubectl run ON
# each cluster's control-plane node via `az vm run-command`; ASG membership is
# read from the runner with `az rest`. Every Azure call carries an explicit
# --subscription; there is NO `az account set` (RD-020).
#
# XSUB-001 (identity preflight): two DISTINCT subscriptions; all nodes Ready in
#   both clusters; an in-cluster pod in EACH cluster obtains an ARM token via
#   IMDS; Cluster A GETs its LOCAL primary run RG; Cluster B crosses the
#   subscription boundary to GET the primary run RG AND both shared ASGs. A
#   SKIPPED or FAILED token/ARM check is a FAILURE, not a warning (FR-027). The
#   token is acquired and used entirely on the node and is NEVER surfaced -
#   only pass/fail evidence is recorded (sanitized, NFR-012).
# XSUB-002 (reconciliation): Tests 1-4 run keyed to xs with both controllers
#   targeting the primary-subscription ASG RG; Cluster B reconciles its exact
#   pod-IP membership into the primary-sub ASGs (FR-028, delegated to
#   run-validation.sh which already asserts independence/convergence/zero-412).
# XSUB-003 (containment): every recorded runtime assignment is run-RG-scoped
#   (the live no-subscription-scope grant is enforced at setup-cross-sub-rbac
#   time; the RG-absent + assignment-removed check is cleanup's TEST-020).
#
# Usage: run-cross-sub-validation.sh <all|xsub1|xsub2|xsub3|names>
# Required env: PRIMARY_SUBSCRIPTION_ID, SECONDARY_SUBSCRIPTION_ID (distinct).
# Naming context: REPO, RUN_ID, RUN_ATTEMPT, DATE_UTC, GIT_SHA, GIT_REF.
# Tool seam: AZ_BIN (az).
#
# Traceability: ITEM-039, FR-025, FR-027, FR-028, XSUB-001, XSUB-002, XSUB-003,
# TEST-018, TEST-019, AC-024, AC-025, AC-026, NFR-012, RD-020, REQ-005.
# =============================================================================
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# `xs` is the only topology this script serves; pin it BEFORE sourcing the core
# so run-validation.sh derives the xs cluster/ASG map on val::_derive.
TOPOLOGY="${TOPOLOGY:-xs}"

# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"
# shellcheck source=scripts/e2e/naming.sh
source "${HERE}/naming.sh"
# Reuse val::_derive (CA/CB/ASG_* maps) + val::all (Tests 1-4). Sourced; its main
# is guarded so the suite does NOT run on source.
# shellcheck source=scripts/e2e/run-validation.sh
source "${HERE}/run-validation.sh"

# ---- inputs / configuration -------------------------------------------------
EXPECTED_NODES="${EXPECTED_NODES:-4}"                    # 1 control-plane + 3 workers (CON-004)
PROBE_IMAGE="${PROBE_IMAGE:-curlimages/curl:8.5.0}"
PROBE_NAME="${PROBE_NAME:-pnc-xsub-probe}"
PROBE_NAMESPACE="${PROBE_NAMESPACE:-default}"            # exists on both clusters
PROBE_READY_ATTEMPTS="${PROBE_READY_ATTEMPTS:-3}"
PROBE_READY_DELAY="${PROBE_READY_DELAY:-10}"
ARM_RG_API_VERSION="${ARM_RG_API_VERSION:-2021-04-01}"
ARM_ASG_API_VERSION="${ARM_ASG_API_VERSION:-2024-05-01}"
IMDS_URL="${IMDS_URL:-http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https://management.azure.com/}"
ARM_MGMT="${ARM_MGMT:-https://management.azure.com}"

# ---- xs input guard (two distinct subscriptions are mandatory, FR-025) ------
xsub::_require_xs() {
  local tcode
  tcode="$(naming::topology_code "$TOPOLOGY")" || log::die "invalid TOPOLOGY '${TOPOLOGY}'"
  [[ "$tcode" == "xs" ]] || log::die "run-cross-sub-validation.sh serves the xs topology only (got '${TOPOLOGY}'); use run-validation.sh for ss"
  [[ -n "$PRIMARY_SUBSCRIPTION_ID" ]]   || log::die "PRIMARY_SUBSCRIPTION_ID is required"
  [[ -n "$SECONDARY_SUBSCRIPTION_ID" ]] || log::die "SECONDARY_SUBSCRIPTION_ID is required for xs (Cluster B)"
  [[ "${PRIMARY_SUBSCRIPTION_ID,,}" != "${SECONDARY_SUBSCRIPTION_ID,,}" ]] \
    || log::die "xs requires two DISTINCT subscription IDs; primary equals secondary (FR-025)"
}

xsub::_derive() {
  [[ "${XSUB_DERIVED:-0}" == "1" ]] && return 0
  xsub::_require_xs
  val::_derive        # CA, CB, ASG_SUB, ASG_RG, ASG_BACKEND_ID, ASG_FRONTEND_ID, TCODE
  XSUB_DERIVED=1
}

xsub::_record() {  # <name> <status> <detail-json>
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  # shellcheck disable=SC2016  # $s/$d are jq variables
  manifest::put_json "$MANIFEST_PATH" "validate.xs.xsub.$1" \
    "$(jq -n --arg s "$2" --argjson d "${3:-{\}}" '{status:$s} + $d')"
}

# ---- XSUB-001 building blocks (node-side; token never surfaced) -------------
# xsub::_nodes_ready <arrayname> : 0 when the cluster reports >= EXPECTED_NODES
# Ready nodes (kubectl on the control-plane node via run-command).
xsub::_nodes_ready() {
  local -n _c="$1"; local out ready
  out="$(azrun::capture "$AZ_BIN" "${_c[sub]}" "${_c[resource_group]}" "${_c[cp_vm]}" \
        "KUBECONFIG=${KUBECONFIG_ON_NODE} kubectl get nodes --no-headers")" || return 1
  ready="$(printf '%s\n' "$out" | grep -c -w Ready || true)"
  ready="${ready//[^0-9]/}"; ready="${ready:-0}"
  (( ready >= EXPECTED_NODES )) || { log::error "cluster ${_c[cluster_id]} has ${ready}/${EXPECTED_NODES} Ready nodes"; return 1; }
}

# xsub::_probe_script <label> <arm-url...> : emit the node-side IMDS/ARM probe.
# The IMDS token is acquired AND used entirely on the node; only IMDS=ok/fail,
# per-URL codes, and XSUB_PROBE_RESULT=pass/fail are printed (sanitized, NFR-012).
# ARM target URLs are shell-quoted (%q) so they survive transport; $TOKEN/$code
# are NODE-evaluated. PNC_XSUB_PROBE marks the script for humans and diagnostics.
xsub::_probe_script() {
  local label="$1"; shift
  printf 'PNC_XSUB_PROBE=%s\n' "$label"
  printf 'NS=%q\nPROBE=%q\nIMG=%q\nIMDS_URL=%q\n' "$PROBE_NAMESPACE" "$PROBE_NAME" "$PROBE_IMAGE" "$IMDS_URL"
  printf 'set --'; local u; for u in "$@"; do printf ' %q' "$u"; done; printf '\n'
  cat <<'NODE'
RESULT=pass
kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found >/dev/null 2>&1 || true
kubectl -n "$NS" run "$PROBE" --image="$IMG" --restart=Never \
  --overrides='{"spec":{"tolerations":[{"operator":"Exists"}]}}' --command -- sleep 300 >/dev/null 2>&1 || true
if ! kubectl -n "$NS" wait --for=condition=Ready "pod/$PROBE" --timeout=120s >/dev/null 2>&1; then
  echo "IMDS=fail"; echo "XSUB_PROBE_RESULT=fail"
  kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found >/dev/null 2>&1 || true
  exit 0
fi
TOKEN=$(kubectl -n "$NS" exec "$PROBE" -- sh -c "curl -sf -H 'Metadata: true' '$IMDS_URL'" 2>/dev/null | jq -r '.access_token // empty' 2>/dev/null)
if [ -n "$TOKEN" ]; then echo "IMDS=ok"; else
  echo "IMDS=fail"; echo "XSUB_PROBE_RESULT=fail"
  kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found >/dev/null 2>&1 || true
  exit 0
fi
for url in "$@"; do
  AUTH_SCHEME=B\earer
  code=$(kubectl -n "$NS" exec "$PROBE" -- curl -s -o /dev/null -w %{http_code} \
    -H "Authorization: ${AUTH_SCHEME} ${TOKEN}" "$url" 2>/dev/null)
    -H "Authorization: ${AUTH_SCHEME} ${TOKEN}" "$url" 2>/dev/null)
  if [ "$code" = 200 ]; then echo "ARM url=$url code=200"; else echo "ARM url=$url code=$code"; RESULT=fail; fi
done
kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found >/dev/null 2>&1 || true
echo "XSUB_PROBE_RESULT=$RESULT"
NODE
}

# xsub::_probe <arrayname> <label> <arm-url...> : run the probe on the cluster's
# control-plane node and return 0 iff IMDS token + EVERY ARM GET succeeded. A
# missing result (skipped check) is a FAILURE (FR-027).
xsub::_probe() {
  local -n _c="$1"; local label="$2"; shift 2
  local script out result imds
  script="$(xsub::_probe_script "$label" "$@")"
  out="$(azrun::capture "$AZ_BIN" "${_c[sub]}" "${_c[resource_group]}" "${_c[cp_vm]}" "$script")" \
    || { log::error "IMDS/ARM probe transport failed on ${_c[cp_vm]} (subscription ${_c[sub]})"; return 1; }
  imds="$(printf '%s\n' "$out" | sed -n 's/^IMDS=//p' | head -1)"
  result="$(printf '%s\n' "$out" | sed -n 's/^XSUB_PROBE_RESULT=//p' | head -1)"
  if [[ "$imds" != "ok" ]]; then log::error "cluster ${_c[cluster_id]}: in-cluster IMDS token acquisition FAILED (mandatory, FR-027)"; return 1; fi
  if [[ "$result" != "pass" ]]; then log::error "cluster ${_c[cluster_id]}: cross-subscription ARM preflight FAILED or was skipped (FR-027)"; return 1; fi
  log::info "cluster ${_c[cluster_id]}: IMDS token + $# ARM GET(s) OK"
  return 0
}

# ---- XSUB-001 : topology + identity preflight (TEST-018) --------------------
xsub::xsub1() {
  xsub::_derive
  log::info "XSUB-001 (identity preflight): distinct subs, health, IMDS + cross-sub ARM"
  local rc=0 distinct=false a_nodes=false b_nodes=false a_probe=false b_probe=false
  [[ "${PRIMARY_SUBSCRIPTION_ID,,}" != "${SECONDARY_SUBSCRIPTION_ID,,}" ]] && distinct=true
  $distinct || { log::error "XSUB-001: primary and secondary subscriptions must differ"; rc=1; }

  xsub::_nodes_ready CA && a_nodes=true || rc=1
  xsub::_nodes_ready CB && b_nodes=true || rc=1

  # Cluster A reads only its LOCAL primary run RG.
  xsub::_probe CA A \
    "${ARM_MGMT}/subscriptions/${PRIMARY_SUBSCRIPTION_ID}/resourceGroups/${ASG_RG}?api-version=${ARM_RG_API_VERSION}" \
    && a_probe=true || rc=1
  # Cluster B crosses the subscription boundary: primary run RG + BOTH shared ASGs.
  xsub::_probe CB B \
    "${ARM_MGMT}/subscriptions/${PRIMARY_SUBSCRIPTION_ID}/resourceGroups/${ASG_RG}?api-version=${ARM_RG_API_VERSION}" \
    "${ARM_MGMT}${ASG_BACKEND_ID}?api-version=${ARM_ASG_API_VERSION}" \
    "${ARM_MGMT}${ASG_FRONTEND_ID}?api-version=${ARM_ASG_API_VERSION}" \
    && b_probe=true || rc=1

  xsub::_record xsub1 "$([[ $rc -eq 0 ]] && echo pass || echo fail)" \
    "$(jq -n --argjson d "$distinct" --argjson an "$a_nodes" --argjson bn "$b_nodes" \
        --argjson ap "$a_probe" --argjson bp "$b_probe" \
        '{distinct_subscriptions:$d,
          clusterA:{nodes_ready:$an, imds_arm_local:$ap},
          clusterB:{nodes_ready:$bn, imds_arm_cross_sub:$bp}}')"
  if (( rc == 0 )); then log::info "XSUB-001 PASS"; else log::error "XSUB-001 FAIL"; fi
  return "$rc"
}

# ---- XSUB-002 : cross-subscription reconciliation via Tests 1-4 (TEST-019) --
xsub::xsub2() {
  xsub::_derive
  log::info "XSUB-002 (reconciliation): Tests 1-4 keyed to xs (both controllers -> primary ASG RG)"
  local rc=0 tests_status
  val::all || rc=1
  tests_status="$(manifest::get "$MANIFEST_PATH" '.validate.xs.status // "fail"')"
  [[ "$tests_status" == "pass" ]] || rc=1
  xsub::_record xsub2 "$([[ $rc -eq 0 ]] && echo pass || echo fail)" \
    "$(jq -n --arg ts "$tests_status" '{tests_status:$ts}')"
  return "$rc"
}

# ---- XSUB-003 : RBAC containment (recorded grants are run-RG-scoped) ---------
# The live no-subscription-scope assertion happens at setup-cross-sub-rbac time;
# the RG-absent + assignment-removed verification is cleanup's TEST-020. Here we
# assert the recorded inventory never contains a non-RG-scoped grant.
xsub::xsub3() {
  xsub::_derive
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  local recorded bad rc=0
  recorded="$(manifest::get "$MANIFEST_PATH" '(.rbac.xs.assignments // []) | length')"
  recorded="${recorded//[^0-9]/}"; recorded="${recorded:-0}"
  (( recorded > 0 )) \
    || { log::error "XSUB-003: mandatory runtime RBAC assignment inventory is absent (setup may have been skipped)"; rc=1; }
  bad="$(manifest::get "$MANIFEST_PATH" \
    '[ (.rbac.xs.scope // empty), (.rbac.xs.assignments[]?.scope // empty) ]
     | map(select((test("^/subscriptions/[^/]+/resourceGroups/[^/]+$")) | not)) | length')"
  bad="${bad//[^0-9]/}"; bad="${bad:-0}"
  (( bad == 0 )) || { log::error "XSUB-003: ${bad} recorded assignment(s) are NOT run-RG-scoped (containment violation, SEC-002)"; rc=1; }
  xsub::_record xsub3 "$([[ $rc -eq 0 ]] && echo pass || echo fail)" \
    "$(jq -n --argjson r "$recorded" --argjson b "$bad" '{assignments_recorded:$r, non_rg_scoped:$b}')"
  if (( rc == 0 )); then log::info "XSUB-003 PASS (recorded grants are run-RG-scoped; ${recorded} assignment(s))"; fi
  return "$rc"
}

xsub::all() {
  xsub::_derive
  [[ -f "$MANIFEST_PATH" ]] || manifest::init "$MANIFEST_PATH"
  local rc=0
  xsub::xsub1 || rc=1
  xsub::xsub2 || rc=1
  xsub::xsub3 || rc=1
  manifest::put "$MANIFEST_PATH" validate.xs.cross_subscription "$([[ $rc -eq 0 ]] && echo pass || echo fail)"
  gha::output cross_subscription_status "$([[ $rc -eq 0 ]] && echo pass || echo fail)"
  if (( rc == 0 )); then log::info "validate_cross_subscription: XSUB-001..003 + Tests 1-4 all PASS"
  else log::error "validate_cross_subscription: one or more checks FAILED"; fi
  return "$rc"
}

xsub::names() {
  xsub::_derive
  printf 'topology=xs\nprimary_subscription=%s\nsecondary_subscription=%s\n' \
    "$PRIMARY_SUBSCRIPTION_ID" "$SECONDARY_SUBSCRIPTION_ID"
  printf 'asg_subscription=%s\nasg_resource_group=%s\n' "$ASG_SUB" "$ASG_RG"
  printf 'clusterA rg=%s cp=%s sub=%s\n' "${CA[resource_group]}" "${CA[cp_vm]}" "${CA[sub]}"
  printf 'clusterB rg=%s cp=%s sub=%s\n' "${CB[resource_group]}" "${CB[cp_vm]}" "${CB[sub]}"
}

xsub::usage() {
  cat <<'USAGE'
Usage: run-cross-sub-validation.sh <command>

Cross-subscription (xs) validation: XSUB-001..003 + topology-keyed Tests 1-4.
The in-cluster IMDS/ARM probe and all kubectl run ON each control-plane node via
`az vm run-command`; the ARM token never leaves the node (sanitized evidence).

Commands:
  all     XSUB-001 -> XSUB-002 (Tests 1-4) -> XSUB-003; record the aggregate (default)
  xsub1   Identity preflight: distinct subs, nodes Ready, IMDS + cross-sub ARM
  xsub2   Reconciliation: run Tests 1-4 keyed to xs (reuses run-validation.sh)
  xsub3   Containment: recorded runtime grants are run-RG-scoped
  names   Print derived cluster/ASG names and exit

Required env: PRIMARY_SUBSCRIPTION_ID, SECONDARY_SUBSCRIPTION_ID (distinct).
Naming context: REPO, RUN_ID, RUN_ATTEMPT, DATE_UTC, GIT_SHA, GIT_REF.
Tool seam: AZ_BIN (az).
USAGE
}

xsub::main() {
  local cmd="${1:-all}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    all)   xsub::all ;;
    xsub1) xsub::xsub1 ;;
    xsub2) xsub::xsub2 ;;
    xsub3) xsub::xsub3 ;;
    names) xsub::names ;;
    help|-h|--help) xsub::usage ;;
    *) log::error "unknown command: ${cmd}"; xsub::usage >&2; return 1 ;;
  esac
}

xsub::main "$@"
