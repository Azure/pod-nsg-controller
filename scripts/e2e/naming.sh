#!/usr/bin/env bash
# =============================================================================
# naming.sh - deterministic, topology-aware resource-naming library.
#
# Reference implementation of the naming algorithm normatively specified in the
# Infrastructure E2E Validation & Release Pipeline PRD, Section 3.4. Every Azure
# and Kubernetes resource name used by the pipeline is a pure function of the
# GitHub Actions run context, so names are reproducible, auditable, and
# collision-free across concurrent runs and across the same-subscription (ss)
# and cross-subscription (xs) topologies.
#
# The file is dual-purpose:
#   * When SOURCED it exposes pure `naming::*` helper functions and performs no
#     side effects (no `set -e`, no output).
#   * When EXECUTED it dispatches sub-commands (see `naming::usage`).
#
# Traceability: FR-011, NFR-007, CON-005, CON-006, AC-012, PRD Section 3.4.
# =============================================================================

# ---- Fixed configuration (Section 3.4.1) -----------------------------------
# These may be overridden via the environment for testing, but default to the
# values fixed by the PRD (PURPOSE constant, documented canary regions).
: "${NAMING_PURPOSE:=pnc-e2e}"                     # fixed PURPOSE constant
: "${NAMING_PRIMARY_REGION:=eastus2euap}"          # Cluster A region (primary sub)
: "${NAMING_SECONDARY_REGION:=centraluseuap}"      # Cluster B region (secondary in xs)
: "${NAMING_TTL_HOURS_DEFAULT:=6}"                 # default resource TTL tag (3.4.6)

# base36 alphabet for RUN_SUFFIX (charset [0-9a-z]).
NAMING_B36="0123456789abcdefghijklmnopqrstuvwxyz"
# 36^6 = 2176782336: the RUN_SUFFIX modulus (Section 3.4.2).
NAMING_B36_MOD=2176782336

naming::_err() { printf 'naming: %s\n' "$*" >&2; }

# ---- Primitive functions (Section 3.4.2) -----------------------------------

# naming::sha256_hex <string> -> 64-char lowercase hex digest (no trailing NL).
naming::sha256_hex() {
  printf '%s' "$1" | sha256sum | cut -c1-64
}

# naming::run_suffix <repo> <run_id> <run_attempt> -> 6-char base36 token.
#   lowercase base36 of int(hex(sha256("{REPO}:{RUN_ID}:{RUN_ATTEMPT}"))[0:8])
#   mod 36^6, left-padded to 6 chars. Deterministic; charset [0-9a-z].
naming::run_suffix() {
  local repo="$1" run_id="$2" run_attempt="$3"
  local hex n x out=""
  hex="$(naming::sha256_hex "${repo}:${run_id}:${run_attempt}")"
  hex="${hex:0:8}"
  n=$((16#$hex))
  x=$(( n % NAMING_B36_MOD ))
  if (( x == 0 )); then out="0"; fi
  while (( x > 0 )); do
    out="${NAMING_B36:$((x % 36)):1}${out}"
    x=$(( x / 36 ))
  done
  while (( ${#out} < 6 )); do out="0${out}"; done
  printf '%s' "$out"
}

# naming::normalize <s>: lowercase; non [a-z0-9-] -> '-'; collapse '-'; strip ends.
naming::normalize() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]' \
    | sed -E 's/[^a-z0-9-]+/-/g; s/-{2,}/-/g; s/^-+//; s/-+$//'
}

# naming::alnum <s>: lowercase; delete every char not in [a-z0-9].
naming::alnum() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9'
}

# naming::fit <name> <maxLen>: identity when short enough; otherwise a
# deterministic, uniqueness-preserving truncation to exactly maxLen chars:
#   name[0:maxLen-5] + "-" + hex(sha256(name))[0:4].
naming::fit() {
  local name="$1" maxlen="$2" h
  if (( ${#name} <= maxlen )); then
    printf '%s' "$name"
  else
    h="$(naming::sha256_hex "$name")"
    printf '%s-%s' "${name:0:$((maxlen - 5))}" "${h:0:4}"
  fi
}

# naming::topology_code <topology>: same-subscription|ss -> ss; cross-*|xs -> xs.
naming::topology_code() {
  case "$1" in
    ss|same-subscription)  printf 'ss' ;;
    xs|cross-subscription) printf 'xs' ;;
    *) naming::_err "unknown topology: $1"; return 1 ;;
  esac
}

# ---- Base tokens (Section 3.4.3) -------------------------------------------

# naming::base <purpose> <topology_code> <date_utc> <run_suffix>
naming::base() { printf '%s-%s-%s-%s' "$1" "$2" "$3" "$4"; }

# naming::rbase <base> <region>: fit(BASE-normalize(region), 44). The 44-char
# cap guarantees the address-prefix-set invariant (Section 3.4.4 / RD-006).
naming::rbase() {
  local base="$1" region="$2"
  naming::fit "${base}-$(naming::normalize "$region")" 44
}

# naming::prefix_set_name <cluster_rg> <namespace> <mapping>: the controller-
# computed address-prefix-set name = normalize(RG)-<ns>-<mapping> (Section 3.4.4).
naming::prefix_set_name() {
  printf '%s-%s-%s' "$(naming::normalize "$1")" "$2" "$3"
}

# naming::storage_account <purpose> <topology_code> <date_utc> <run_suffix>:
# alnum(PURPOSE+CODE+DATE+SUFFIX)[0:24] (3-24 lowercase alnum, globally unique).
naming::storage_account() {
  local s
  s="$(naming::alnum "${1}${2}${3}${4}")"
  printf '%s' "${s:0:24}"
}

# naming::staging_run_tag <date_utc> <run_suffix>: topology-neutral candidate tag
# (one candidate serves both ss and xs) -> run-<DATE_UTC>-<RUN_SUFFIX>.
naming::staging_run_tag() { printf 'run-%s-%s' "$1" "$2"; }

# naming::subscription_role <topology_code> <region> [primary_region]:
#   primary region  -> primary (Cluster A, shared ASGs, always primary sub)
#   other region    -> primary in ss, secondary in xs (Cluster B placement)
naming::subscription_role() {
  local tcode="$1" region="$2" primary_region="${3:-$NAMING_PRIMARY_REGION}"
  if [[ "$region" == "$primary_region" ]]; then
    printf 'primary'
  elif [[ "$tcode" == "ss" ]]; then
    printf 'primary'
  else
    printf 'secondary'
  fi
}

# ---- Validators (Section 3.4.5 / CON-005 / CON-006, fail fast) --------------

naming::validate_length() {
  local name="$1" min="$2" max="$3" label="$4" len=${#1}
  if (( len < min || len > max )); then
    naming::_err "invalid ${label} length ${len} (allowed ${min}..${max}): '${name}'"
    return 1
  fi
}

naming::validate_charset() {
  local name="$1" regex="$2" label="$3"
  if [[ ! "$name" =~ $regex ]]; then
    naming::_err "invalid ${label} charset (want /${regex}/): '${name}'"
    return 1
  fi
}

# naming::validate_name <name> <min> <max> <charset_ere> <label> [no_trailing_dot]
naming::validate_name() {
  local name="$1" min="$2" max="$3" regex="$4" label="$5" flag="${6:-}"
  naming::validate_length "$name" "$min" "$max" "$label" || return 1
  naming::validate_charset "$name" "$regex" "$label" || return 1
  if [[ "$flag" == "no_trailing_dot" && "$name" == *. ]]; then
    naming::_err "${label} must not end with '.': '${name}'"
    return 1
  fi
}

# naming::validate_prefix_set_invariant <cluster_rg> <namespace> <mapping>
# Enforces len(RG)+1+len(ns)+1+len(mapping) <= 80 (Section 3.4.4).
naming::validate_prefix_set_invariant() {
  local name
  name="$(naming::prefix_set_name "$1" "$2" "$3")"
  if (( ${#name} > 80 )); then
    naming::_err "address-prefix-set name exceeds 80 chars (${#name}): '${name}'"
    return 1
  fi
}

# ---- Run context ------------------------------------------------------------
# Populates the deterministic run globals from the environment (with GitHub
# Actions fallbacks). RUN_SUFFIX and GIT_SHA are always derived, never trusted
# from the environment, to keep the chain reproducible.
naming::_load_context() {
  REPO="${REPO:-${GITHUB_REPOSITORY:-Azure/pod-nsg-controller}}"
  RUN_ID="${RUN_ID:-${GITHUB_RUN_ID:-0}}"
  RUN_ATTEMPT="${RUN_ATTEMPT:-${GITHUB_RUN_ATTEMPT:-1}}"
  DATE_UTC="${DATE_UTC:-$(date -u +%Y%m%d)}"
  GIT_REF="${GIT_REF:-${GITHUB_REF:-}}"
  TTL_HOURS="${TTL_HOURS:-$NAMING_TTL_HOURS_DEFAULT}"
  local git_sha_raw="${GIT_SHA:-${GITHUB_SHA:-}}"
  GIT_SHA="${git_sha_raw:0:12}"                    # first 12 used in tags/labels
  RUN_SUFFIX="$(naming::run_suffix "$REPO" "$RUN_ID" "$RUN_ATTEMPT")"
}

# naming::_region_pairs <topology_code> <region>
# Emits validated `key=value` lines (relative keys) for one cluster/region and
# returns non-zero if ANY generated name violates a provider constraint.
# Requires naming::_load_context to have populated DATE_UTC/RUN_SUFFIX.
naming::_region_pairs() {
  local tcode="$1" region="$2"
  local base rbase role ns pod_label cluster_id
  base="$(naming::base "$NAMING_PURPOSE" "$tcode" "$DATE_UTC" "$RUN_SUFFIX")"
  rbase="$(naming::rbase "$base" "$region")"
  role="$(naming::subscription_role "$tcode" "$region")"
  if [[ "$region" == "$NAMING_PRIMARY_REGION" ]]; then
    ns="test-apps"; pod_label="role"; cluster_id="A"     # CON-003
  else
    ns="default"; pod_label="app"; cluster_id="B"
  fi

  local vnet="${rbase}-vnet" subnet="${rbase}-subnet" nsg="${rbase}-nsg"
  local natgw="${rbase}-natgw" natgw_pip="${rbase}-natgw-pip" cp_pip="${rbase}-cp-pip"
  local cp_vm worker1 worker2 worker3
  cp_vm="$(naming::fit "${rbase}-cp-01" 64)"
  worker1="$(naming::fit "${rbase}-worker-01" 64)"
  worker2="$(naming::fit "${rbase}-worker-02" 64)"
  worker3="$(naming::fit "${rbase}-worker-03" 64)"

  # Fail-fast validation against Section 3.4.5 limits (CON-005/CON-006).
  naming::validate_name "$rbase"     1 90 '^[a-z0-9-]+$' "resource-group(${region})" no_trailing_dot || return 1
  naming::validate_name "$vnet"      2 64 '^[a-z0-9-]+$' "vnet(${region})"      || return 1
  naming::validate_name "$subnet"    1 80 '^[a-z0-9-]+$' "subnet(${region})"    || return 1
  naming::validate_name "$nsg"       1 80 '^[a-z0-9-]+$' "nsg(${region})"       || return 1
  naming::validate_name "$natgw"     1 80 '^[a-z0-9-]+$' "natgw(${region})"     || return 1
  naming::validate_name "$natgw_pip" 1 80 '^[a-z0-9-]+$' "natgw-pip(${region})" || return 1
  naming::validate_name "$cp_pip"    1 80 '^[a-z0-9-]+$' "cp-pip(${region})"    || return 1
  # VM names double as Kubernetes node names (RFC1123 label <= 63, tighter than
  # Azure's 64-char VM limit), so validate against the binding 63-char bound.
  naming::validate_name "$cp_vm"     1 63 '^[a-z0-9-]+$' "cp-vm(${region})"     || return 1
  naming::validate_name "$worker1"   1 63 '^[a-z0-9-]+$' "worker-01(${region})" || return 1
  naming::validate_name "$worker2"   1 63 '^[a-z0-9-]+$' "worker-02(${region})" || return 1
  naming::validate_name "$worker3"   1 63 '^[a-z0-9-]+$' "worker-03(${region})" || return 1
  local mapping
  for mapping in backend-asg-mapping frontend-asg-mapping; do
    naming::validate_prefix_set_invariant "$rbase" "$ns" "$mapping" || return 1
  done

  cat <<PAIRS
subscription_role=${role}
cluster_id=${cluster_id}
namespace=${ns}
pod_label=${pod_label}
resource_group=${rbase}
vnet=${vnet}
subnet=${subnet}
nsg=${nsg}
natgw=${natgw}
natgw_pip=${natgw_pip}
cp_pip=${cp_pip}
cp_vm=${cp_vm}
worker1_vm=${worker1}
worker2_vm=${worker2}
worker3_vm=${worker3}
prefix_set_backend=$(naming::prefix_set_name "$rbase" "$ns" backend-asg-mapping)
prefix_set_frontend=$(naming::prefix_set_name "$rbase" "$ns" frontend-asg-mapping)
PAIRS
}

# ---- Emitters ---------------------------------------------------------------

# naming::dump_flat: sorted-friendly `dotted.key=value` lines for every run,
# topology (ss, xs) and region (primary, secondary). Deterministic; used by the
# golden test and as the source for emit_manifest.
naming::dump_flat() {
  naming::_load_context
  local tcode region pairs line tag
  tag="$(naming::staging_run_tag "$DATE_UTC" "$RUN_SUFFIX")"

  printf 'run.purpose=%s\n'        "$NAMING_PURPOSE"
  printf 'run.date_utc=%s\n'       "$DATE_UTC"
  printf 'run.run_suffix=%s\n'     "$RUN_SUFFIX"
  printf 'run.run_id=%s\n'         "$RUN_ID"
  printf 'run.run_attempt=%s\n'    "$RUN_ATTEMPT"
  printf 'staging.controller_repo=%s\n' 'candidate/pod-nsg-controller'
  printf 'staging.controller_tag=%s\n'  "$tag"
  printf 'staging.cni_repo=%s\n'        'candidate/pod-nsg-cni-transparent-tunnel'
  printf 'staging.cni_tag=%s\n'         "$tag"

  for tcode in ss xs; do
    printf '%s.base=%s\n' "$tcode" "$(naming::base "$NAMING_PURPOSE" "$tcode" "$DATE_UTC" "$RUN_SUFFIX")"
    printf '%s.storage_account=%s\n' "$tcode" "$(naming::storage_account "$NAMING_PURPOSE" "$tcode" "$DATE_UTC" "$RUN_SUFFIX")"
    for region in "$NAMING_PRIMARY_REGION" "$NAMING_SECONDARY_REGION"; do
      pairs="$(naming::_region_pairs "$tcode" "$region")" || return 1
      while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        printf '%s.%s.%s\n' "$tcode" "$region" "$line"
      done <<< "$pairs"
    done
  done
}

# naming::emit_manifest: the run-manifest foundation as JSON. Combines run
# metadata, the common correlation labels (Section 3.4.6), and the full nested
# name tree. Downstream jobs augment this with subscription IDs, digests, and
# per-test results.
naming::emit_manifest() {
  naming::_load_context
  local names_json
  # Exclude the run.* lines: run metadata is represented at the top level; the
  # names tree carries only resource names (ss, xs, staging).
  names_json="$(naming::dump_flat | grep -v '^run\.' | jq -R -s '
    split("\n") | map(select(length > 0))
    | map(split("=") | {k: .[0], v: (.[1:] | join("="))})
    | reduce .[] as $kv ({}; setpath($kv.k | split("."); $kv.v))
  ')" || return 1

  jq -n \
    --arg schema      "pnc-e2e/run-manifest/v1" \
    --arg purpose     "$NAMING_PURPOSE" \
    --arg date_utc    "$DATE_UTC" \
    --arg run_suffix  "$RUN_SUFFIX" \
    --arg repo        "$REPO" \
    --arg run_id      "$RUN_ID" \
    --arg run_attempt "$RUN_ATTEMPT" \
    --arg git_sha     "$GIT_SHA" \
    --arg git_ref     "$GIT_REF" \
    --arg ttl_hours   "$TTL_HOURS" \
    --argjson names   "$names_json" \
    '{
      schema: $schema,
      generated_by: "scripts/e2e/naming.sh",
      run: {
        purpose: $purpose, date_utc: $date_utc, run_suffix: $run_suffix,
        repo: $repo, run_id: $run_id, run_attempt: $run_attempt,
        git_sha: $git_sha, git_ref: $git_ref, ttl_hours: $ttl_hours
      },
      labels: {
        "validation-purpose": $purpose,
        "validation-run-id": $run_id,
        "validation-run-attempt": $run_attempt,
        "validation-date-utc": $date_utc,
        "validation-run-suffix": $run_suffix,
        "git-sha": $git_sha,
        "git-ref": $git_ref,
        "managed-by": "github-actions",
        "ttl-hours": $ttl_hours
      },
      names: $names
    }'
}

# naming::self_check: generate and validate every name for both topologies and
# both regions, assert ss/xs topology separation, and validate the run-level
# derived names. Exits non-zero on ANY violation (CON-006 / AC-012).
naming::self_check() {
  naming::_load_context
  local tcode region pairs rc=0
  local ss_rg xs_rg

  for region in "$NAMING_PRIMARY_REGION" "$NAMING_SECONDARY_REGION"; do
    for tcode in ss xs; do
      if ! pairs="$(naming::_region_pairs "$tcode" "$region")"; then
        naming::_err "self-check: name generation failed for ${tcode}/${region}"
        rc=1; continue
      fi
      : "$pairs"
    done
    ss_rg="$(naming::rbase "$(naming::base "$NAMING_PURPOSE" ss "$DATE_UTC" "$RUN_SUFFIX")" "$region")"
    xs_rg="$(naming::rbase "$(naming::base "$NAMING_PURPOSE" xs "$DATE_UTC" "$RUN_SUFFIX")" "$region")"
    if [[ "$ss_rg" == "$xs_rg" ]]; then
      naming::_err "self-check: ss and xs names collide for ${region}: ${ss_rg}"
      rc=1
    fi
  done

  for tcode in ss xs; do
    local sa
    sa="$(naming::storage_account "$NAMING_PURPOSE" "$tcode" "$DATE_UTC" "$RUN_SUFFIX")"
    naming::validate_name "$sa" 3 24 '^[a-z0-9]+$' "storage-account(${tcode})" || rc=1
  done
  naming::validate_name "$(naming::staging_run_tag "$DATE_UTC" "$RUN_SUFFIX")" \
    1 128 '^[A-Za-z0-9_][A-Za-z0-9_.-]*$' 'staging-run-tag' || rc=1

  if (( rc == 0 )); then
    printf 'naming self-check OK: purpose=%s date=%s suffix=%s regions=[%s,%s]\n' \
      "$NAMING_PURPOSE" "$DATE_UTC" "$RUN_SUFFIX" \
      "$NAMING_PRIMARY_REGION" "$NAMING_SECONDARY_REGION" >&2
  fi
  return "$rc"
}

naming::usage() {
  cat <<'USAGE'
Usage: naming.sh <command> [args]

Deterministic naming library (PRD Section 3.4). Commands:
  run-suffix <repo> <run_id> <run_attempt>     Print the 6-char base36 run suffix
  normalize <string>                           Normalize to [a-z0-9-]
  alnum <string>                               Reduce to [a-z0-9]
  fit <name> <maxLen>                          Length-fit with deterministic hash
  base <purpose> <code> <date> <suffix>        Print BASE token
  rbase <base> <region>                        Print region base (RBASE, <=44)
  prefix-set-name <rg> <namespace> <mapping>   Controller-computed prefix-set name
  storage-account <purpose> <code> <date> <suffix>
  dump-flat                                    Emit every name as dotted key=value
  emit-manifest                                Emit the run-manifest foundation JSON
  self-check                                   Validate all names; exit non-zero on error

Context is read from the environment (with GitHub Actions fallbacks):
  REPO, RUN_ID, RUN_ATTEMPT, DATE_UTC, GIT_SHA, GIT_REF, TTL_HOURS
  NAMING_PRIMARY_REGION (default eastus2euap), NAMING_SECONDARY_REGION (centraluseuap)
USAGE
}

naming::main() {
  set -euo pipefail
  export LC_ALL=C
  local cmd="${1:-help}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    run-suffix)        naming::run_suffix "$@" ;;
    normalize)         naming::normalize "$@" ;;
    alnum)             naming::alnum "$@" ;;
    fit)               naming::fit "$@" ;;
    base)              naming::base "$@" ;;
    rbase)             naming::rbase "$@" ;;
    prefix-set-name)   naming::prefix_set_name "$@" ;;
    storage-account)   naming::storage_account "$@" ;;
    subscription-role) naming::subscription_role "$@" ;;
    dump-flat)         naming::dump_flat ;;
    emit-manifest)     naming::emit_manifest ;;
    self-check)        naming::self_check ;;
    help|-h|--help)    naming::usage ;;
    *) naming::_err "unknown command: ${cmd}"; naming::usage >&2; return 1 ;;
  esac
}

# Execute only when run directly; do nothing when sourced.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  naming::main "$@"
fi
