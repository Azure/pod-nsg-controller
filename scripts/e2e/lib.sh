#!/usr/bin/env bash
# =============================================================================
# lib.sh - shared helpers for the E2E validation & release pipeline scripts.
#
# Provides the foundational, side-effect-free helpers reused across the
# scripts/e2e/* family and the GitHub Actions jobs:
#   * logging            - timestamped, level-tagged, stderr-only
#   * retry/backoff      - bounded retries with exponential backoff (NFR-003)
#   * run-manifest emit  - jq-based JSON manifest init/set/merge/get (FILE-023)
#   * GitHub Actions glue - $GITHUB_OUTPUT / _ENV / _STEP_SUMMARY helpers
#
# Sourcing this file has no side effects. Do not enable `set -e` here; callers
# own their shell options.
#
# Traceability: FILE-023, NFR-003, NFR-006, PRD Section 3 (run manifest).
# =============================================================================

# ---- logging ----------------------------------------------------------------
lib::_ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }

log::info()  { printf '%s [INFO]  %s\n'  "$(lib::_ts)" "$*" >&2; }
log::warn()  { printf '%s [WARN]  %s\n'  "$(lib::_ts)" "$*" >&2; }
log::error() { printf '%s [ERROR] %s\n'  "$(lib::_ts)" "$*" >&2; }
log::debug() { [[ -n "${DEBUG:-}" ]] && printf '%s [DEBUG] %s\n' "$(lib::_ts)" "$*" >&2; return 0; }
log::die()   { log::error "$*"; exit 1; }

# ---- dependency guard -------------------------------------------------------
# lib::require_cmds <cmd...> : die if any required command is missing.
lib::require_cmds() {
  local missing=() c
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || missing+=("$c")
  done
  if (( ${#missing[@]} > 0 )); then
    log::die "missing required command(s): ${missing[*]}"
  fi
}

# ---- retry / backoff (NFR-003) ---------------------------------------------
# lib::retry <max_attempts> <base_delay_seconds> -- <cmd...>
# Runs <cmd...> until it succeeds or <max_attempts> is reached, doubling the
# delay between attempts. Returns the last non-zero exit code on exhaustion.
lib::retry() {
  local max="$1" delay="$2"; shift 2
  [[ "${1:-}" == "--" ]] && shift
  local attempt=1 rc=0
  while true; do
    "$@" && return 0
    rc=$?   # captured directly from "$@" (the non-final command in the && list)
    if (( attempt >= max )); then
      log::error "command failed after ${attempt} attempt(s) (rc=${rc}): $*"
      return "$rc"
    fi
    log::warn "attempt ${attempt}/${max} failed (rc=${rc}); retrying in ${delay}s: $*"
    sleep "$delay"
    attempt=$(( attempt + 1 ))
    delay=$(( delay * 2 ))
  done
}

# ---- run-manifest helpers (jq-based JSON, FILE-023) -------------------------
# The manifest is a plain JSON file updated via atomic sibling-temp writes (no
# /tmp, no mktemp) so it works in restricted CI sandboxes.

manifest::_write() {
  # manifest::_write <path> <jq-program> [jq-args...]
  local path="$1"; shift
  local tmp="${path}.tmp.$$"
  if jq "$@" "$path" > "$tmp"; then
    mv "$tmp" "$path"
  else
    local rc=$?
    rm -f "$tmp"
    log::error "manifest update failed for ${path}"
    return "$rc"
  fi
}

# manifest::init <path> [initial-json]  (defaults to '{}')
manifest::init() {
  local path="$1" initial="${2:-}"
  [[ -n "$initial" ]] || initial='{}'
  printf '%s' "$initial" | jq '.' > "$path"
}

# manifest::put <path> <dotted.key> <string-value>
manifest::put() {
  # shellcheck disable=SC2016  # $k/$v are jq variables, not shell expansions
  manifest::_write "$1" 'setpath($k | split("."); $v)' --arg k "$2" --arg v "$3"
}

# manifest::put_json <path> <dotted.key> <json-value>
manifest::put_json() {
  # shellcheck disable=SC2016  # $k/$v are jq variables, not shell expansions
  manifest::_write "$1" 'setpath($k | split("."); $v)' --arg k "$2" --argjson v "$3"
}

# manifest::merge <path> <json-object> : deep-merge object into the manifest root.
manifest::merge() {
  # shellcheck disable=SC2016  # $add is a jq variable, not a shell expansion
  manifest::_write "$1" '. * $add' --argjson add "$2"
}

# manifest::get <path> <jq-filter> : print a raw value from the manifest.
manifest::get() { jq -r "$2" "$1"; }

# ---- GitHub Actions glue ----------------------------------------------------
# All helpers degrade gracefully to stdout when the corresponding GitHub file
# environment variable is unset (e.g. local runs), so scripts stay testable.

# gha::output <name> <value> : set a step output (multiline-safe).
gha::output() {
  local name="$1" value="$2"
  if [[ -z "${GITHUB_OUTPUT:-}" ]]; then
    printf '%s=%s\n' "$name" "$value"
    return 0
  fi
  if [[ "$value" == *$'\n'* ]]; then
    local delim="ghadelim_${RANDOM}${RANDOM}"
    { printf '%s<<%s\n' "$name" "$delim"
      printf '%s\n' "$value"
      printf '%s\n' "$delim"
    } >> "$GITHUB_OUTPUT"
  else
    printf '%s=%s\n' "$name" "$value" >> "$GITHUB_OUTPUT"
  fi
}

# gha::export <name> <value> : export an env var to later steps.
gha::export() {
  if [[ -n "${GITHUB_ENV:-}" ]]; then
    printf '%s=%s\n' "$1" "$2" >> "$GITHUB_ENV"
  else
    printf '%s=%s\n' "$1" "$2"
  fi
}

# gha::summary <markdown> : append to the job step summary.
gha::summary() {
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    printf '%s\n' "$1" >> "$GITHUB_STEP_SUMMARY"
  else
    printf '%s\n' "$1"
  fi
}
