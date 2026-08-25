#!/usr/bin/env bash
# Hermetic tests for final run-manifest aggregation (ITEM-025).
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${TEST_DIR}/../finalize-manifest.sh"
WORK="${TEST_DIR}/.finalize_manifest_test_work"
OUT="${WORK}/run-manifest.json"
PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1" >&2; }
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$WORK/fragments/a" "$WORK/fragments/b" "$WORK/fragments/c"

cat >"$WORK/fragments/a/run-manifest.json" <<'JSON'
{"run":{"validation_topologies":["ss","xs"],"regions":["eastus2euap","centraluseuap"]},"names":{"ss":{"eastus2euap":{"subscription_role":"primary"},"centraluseuap":{"subscription_role":"primary"}},"xs":{"eastus2euap":{"subscription_role":"primary"},"centraluseuap":{"subscription_role":"secondary"}}},"subscriptions":{"primary":{"id":"sub-a"},"secondary":{"id":"sub-b"}},"artifacts":{"controller":{"digest":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}},"validate":{"ss":{"status":"pass","tests":{"test1":{"status":"pass"},"test2":{"status":"pass"},"test3":{"status":"pass"},"test4":{"status":"pass"}}}}}
JSON
cat >"$WORK/fragments/b/run-manifest.json" <<'JSON'
{"artifacts":{"cni":{"digest":"sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}},"validate":{"tt":{"status":"pass","scenarios":{"tts1":{"status":"pass"},"tts2":{"status":"pass"},"tts3":{"status":"pass"},"tts4":{"status":"pass"},"tts5":{"status":"pass"},"tts6":{"status":"pass"},"tts7":{"status":"pass"}}},"xs":{"status":"pass","cross_subscription":"pass","tests":{"test1":{"status":"pass"},"test2":{"status":"pass"},"test3":{"status":"pass"},"test4":{"status":"pass"}},"xsub":{"xsub1":{"status":"pass"},"xsub2":{"status":"pass"},"xsub3":{"status":"pass"}}}},"rbac":{"ss":{"assignments":[{"assignment_id":"ss-a"}]},"xs":{"assignments":[{"assignment_id":"xs-a"}]}}}
JSON
cat >"$WORK/fragments/c/run-manifest.json" <<'JSON'
{"cleanup":{"ss":{"status":"pass","resource_groups":[{"subscription":"sub-a","absent":true}]},"xs":{"status":"pass","resource_groups":[{"subscription":"sub-a","absent":true},{"subscription":"sub-b","absent":true}]}}}
JSON

echo "== complete manifest =="
MANIFEST_FRAGMENTS_DIR="$WORK/fragments" MANIFEST_PATH="$OUT" \
  GITHUB_STEP_SUMMARY="$WORK/summary.md" bash "$SCRIPT"
RC=$?
if (( RC == 0 )); then pass "complete fragments finalize"; else fail "finalizer failed (rc=$RC)"; fi
if jq -e '
  .artifacts.controller.digest == "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd" and
  .artifacts.cni.digest == "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" and
  ([.validate.ss.tests.test1,.validate.ss.tests.test2,.validate.ss.tests.test3,.validate.ss.tests.test4] | all(.status == "pass")) and
  ([.validate.xs.tests.test1,.validate.xs.tests.test2,.validate.xs.tests.test3,.validate.xs.tests.test4] | all(.status == "pass")) and
  .validate.tt.status == "pass" and
  ([.validate.xs.xsub.xsub1,.validate.xs.xsub.xsub2,.validate.xs.xsub.xsub3] | all(.status == "pass")) and
  .rbac.ss.assignments[0].assignment_id == "ss-a" and
  .rbac.xs.assignments[0].assignment_id == "xs-a" and
  ([.cleanup.ss.resource_groups[],.cleanup.xs.resource_groups[]] | all(.absent == true)) and
  .audit.manifest_complete == true
' "$OUT" >/dev/null; then pass "all required evidence is retained"; else fail "final manifest evidence incomplete"; fi
if grep -q 'Final run manifest' "$WORK/summary.md"; then pass "auditable summary emitted"; else fail "summary missing"; fi

echo "== incomplete manifest is auditable without hiding failure =="
rm -f "$WORK/fragments/c/run-manifest.json"
MANIFEST_FRAGMENTS_DIR="$WORK/fragments" MANIFEST_PATH="$OUT" \
  GITHUB_STEP_SUMMARY="$WORK/summary-incomplete.md" bash "$SCRIPT"
RC=$?
if (( RC == 0 )); then pass "always-on finalizer tolerates failed upstream evidence"; else fail "finalizer should remain always-on"; fi
if [[ "$(jq -r '.audit.manifest_complete' "$OUT")" == "false" ]]; then
  pass "incomplete evidence is marked false"
else
  fail "incomplete evidence must not look complete"
fi

echo
printf 'finalize_manifest_test: %s passed, %s failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
