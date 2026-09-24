#!/usr/bin/env bash
# Merge all per-job run-manifest fragments and emit an auditable final summary.
# Missing upstream evidence is recorded in .audit instead of making this
# always-on aggregation step fail.
set -euo pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/lib.sh
source "${HERE}/lib.sh"

MANIFEST_FRAGMENTS_DIR="${MANIFEST_FRAGMENTS_DIR:-manifest-fragments}"
MANIFEST_PATH="${MANIFEST_PATH:-run-manifest.json}"
lib::require_cmds jq find sort

mapfile -d '' fragments < <(
  find "$MANIFEST_FRAGMENTS_DIR" -type f -name 'run-manifest.json' -print0 2>/dev/null |
    sort -z
)

if (( ${#fragments[@]} == 0 )); then
  manifest::init "$MANIFEST_PATH"
else
  jq -s 'reduce .[] as $fragment ({}; . * $fragment)' "${fragments[@]}" >"$MANIFEST_PATH"
fi

missing_json="$(jq -c '
  def need($condition; $name): if $condition then [] else [$name] end;
  def selected($topology): (.run.validation_topologies // []) | index($topology) != null;
  def tests_complete($topology):
    [range(1;5) as $n | .validate[$topology].tests["test\($n)"].status == "pass"] | all;
  def role_map_complete($topology):
    [(.run.regions // [])[] as $region
      | (.names[$topology][$region].subscription_role // "") as $role
      | ($role == "primary" or $role == "secondary")] | all;
  def cleanup_complete($topology):
    .subscriptions.primary.id as $primary
    | .subscriptions.secondary.id as $secondary
    | (.cleanup[$topology].resource_groups // []) as $groups
    | .cleanup[$topology].status == "pass"
      and ($groups | length > 0)
      and ([$groups[]? | .absent == true] | all)
      and (if $topology == "xs" then
        ([$groups[]?.subscription] | index($primary) != null)
        and ([$groups[]?.subscription] | index($secondary) != null)
      else
        ([$groups[]? | .subscription == $primary] | all)
      end);
  [
    need((.artifacts.controller.digest // "") | test("^sha256:[0-9a-fA-F]{64}$"); "artifacts.controller.digest"),
    need((.artifacts.cni.digest // "") | test("^sha256:[0-9a-fA-F]{64}$"); "artifacts.cni.digest"),
    need((.subscriptions.primary.id // "") | length > 0; "subscriptions.primary.id"),
    (if selected("xs") then need((.subscriptions.secondary.id // "") | length > 0; "subscriptions.secondary.id") else [] end),
    (if selected("ss") then need(role_map_complete("ss"); "names.ss.subscription_role_map") else [] end),
    (if selected("xs") then need(role_map_complete("xs"); "names.xs.subscription_role_map") else [] end),
    (if selected("ss") then need(tests_complete("ss"); "validate.ss.tests1-4") else [] end),
    (if selected("xs") then need(tests_complete("xs"); "validate.xs.tests1-4") else [] end),
    (if selected("ss") then need(
      .validate.tt.status == "pass"
      and ([range(1;8) as $n | .validate.tt.scenarios["tts\($n)"].status == "pass"] | all);
      "validate.tt.tts1-7") else [] end),
    (if selected("xs") then need(
      .validate.xs.cross_subscription == "pass"
      and ([range(1;4) as $n | .validate.xs.xsub["xsub\($n)"].status == "pass"] | all);
      "validate.xs.xsub1-3") else [] end),
    (if selected("ss") then need((.rbac.ss.assignments // []) | length > 0; "rbac.ss.assignment_ids") else [] end),
    (if selected("xs") then need((.rbac.xs.assignments // []) | length > 0; "rbac.xs.assignment_ids") else [] end),
    (if selected("ss") then need(cleanup_complete("ss"); "cleanup.ss.per_subscription_evidence") else [] end),
    (if selected("xs") then need(cleanup_complete("xs"); "cleanup.xs.per_subscription_evidence") else [] end)
  ] | add
' "$MANIFEST_PATH")"

manifest::put_json "$MANIFEST_PATH" audit \
  "$(jq -n --argjson count "${#fragments[@]}" --argjson missing "$missing_json" \
    '{fragment_count:$count, missing_evidence:$missing, manifest_complete:($missing|length == 0)}')"

complete="$(manifest::get "$MANIFEST_PATH" '.audit.manifest_complete')"
topologies="$(manifest::get "$MANIFEST_PATH" '(.run.validation_topologies // []) | join(",")')"
controller_digest="$(manifest::get "$MANIFEST_PATH" '.artifacts.controller.digest // "<missing>"')"
cni_digest="$(manifest::get "$MANIFEST_PATH" '.artifacts.cni.digest // "<missing>"')"
ss_status="$(manifest::get "$MANIFEST_PATH" '.validate.ss.status // "not-selected-or-missing"')"
tt_status="$(manifest::get "$MANIFEST_PATH" '.validate.tt.status // "not-selected-or-missing"')"
xs_status="$(manifest::get "$MANIFEST_PATH" '.validate.xs.status // "not-selected-or-missing"')"
cleanup_ss="$(manifest::get "$MANIFEST_PATH" '.cleanup.ss.status // "not-selected-or-missing"')"
cleanup_xs="$(manifest::get "$MANIFEST_PATH" '.cleanup.xs.status // "not-selected-or-missing"')"
missing_summary="$(manifest::get "$MANIFEST_PATH" '(.audit.missing_evidence // []) | join(", ")')"

gha::summary "## Final run manifest"
gha::summary ""
gha::summary "| Evidence | Result |"
gha::summary "|---|---|"
gha::summary "| Selected topologies | ${topologies:-<missing>} |"
gha::summary "| Controller digest | \`${controller_digest}\` |"
gha::summary "| CNI digest | \`${cni_digest}\` |"
gha::summary "| ss Tests 1-4 | ${ss_status} |"
gha::summary "| TTS-001..007 | ${tt_status} |"
gha::summary "| xs Tests 1-4 / XSUB | ${xs_status} |"
gha::summary "| ss cleanup | ${cleanup_ss} |"
gha::summary "| xs cleanup | ${cleanup_xs} |"
gha::summary "| Manifest complete | ${complete} |"
gha::summary "| Missing evidence | ${missing_summary:-none} |"

log::info "final manifest aggregated from ${#fragments[@]} fragment(s); complete=${complete}"
