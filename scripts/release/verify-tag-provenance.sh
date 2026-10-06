#!/usr/bin/env bash

set -euo pipefail

: "${TAG_NAME:?TAG_NAME is required}"
: "${TARGET_SHA:?TARGET_SHA is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_TOKEN:?GITHUB_TOKEN is required}"

[[ "$TAG_NAME" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "release tag must use vMAJOR.MINOR.PATCH" >&2
  exit 1
}
[[ "$TARGET_SHA" =~ ^[0-9a-fA-F]{40}$ ]] || {
  echo "TARGET_SHA must be a full commit SHA" >&2
  exit 1
}

TAG_OBJECT_SHA="$(git rev-parse "${TAG_NAME}^{tag}" 2>/dev/null)" || {
  echo "release tag must be annotated: ${TAG_NAME}" >&2
  exit 1
}
[[ "$TAG_OBJECT_SHA" =~ ^[0-9a-f]{40}$ ]] || {
  echo "invalid tag object SHA for ${TAG_NAME}: ${TAG_OBJECT_SHA}" >&2
  exit 1
}
TAG_TARGET_SHA="$(git rev-parse "${TAG_NAME}^{commit}")"
[[ "$TAG_TARGET_SHA" == "${TARGET_SHA,,}" ]] || {
  echo "release tag ${TAG_NAME} does not target ${TARGET_SHA}" >&2
  exit 1
}

API_URL="${GITHUB_API_URL:-https://api.github.com}"
STATUS_CONTEXT="release-tag/${TAG_NAME}/${TAG_OBJECT_SHA}"
EXPECTED_RUN_PREFIX="https://github.com/${GITHUB_REPOSITORY}/actions/runs/"
ATTEMPTS="${PROVENANCE_MAX_ATTEMPTS:-30}"
DELAY_SECONDS="${PROVENANCE_RETRY_DELAY_SECONDS:-10}"
RUN_ID=""

github_api() {
  curl --fail --silent --show-error \
    --header "Accept: application/vnd.github+json" \
    --header "Authorization: Bearer ${GITHUB_TOKEN}" \
    --header "X-GitHub-Api-Version: 2022-11-28" \
    "$1"
}

for ((attempt = 1; attempt <= ATTEMPTS; attempt++)); do
  statuses="$(github_api \
    "${API_URL}/repos/${GITHUB_REPOSITORY}/commits/${TARGET_SHA}/statuses?per_page=100")"
  target_url="$(
    jq -r \
      --arg context "$STATUS_CONTEXT" \
      --arg run_prefix "$EXPECTED_RUN_PREFIX" \
      '
        first(
          .[]
          | select(
              .context == $context
              and .state == "success"
              and .creator.login == "github-actions[bot]"
              and .creator.id == 41898282
              and (.target_url | startswith($run_prefix))
            )
          | .target_url
        ) // empty
      ' <<<"$statuses"
  )"
  if [[ -n "$target_url" ]]; then
    RUN_ID="${target_url#"$EXPECTED_RUN_PREFIX"}"
    [[ "$RUN_ID" =~ ^[0-9]+$ ]] || {
      echo "invalid workflow run URL in ${STATUS_CONTEXT}: ${target_url}" >&2
      exit 1
    }
    break
  fi

  if ((attempt < ATTEMPTS)); then
    sleep "$DELAY_SECONDS"
  fi
done

[[ -n "$RUN_ID" ]] || {
  echo "no approved GitHub Actions provenance found for ${TAG_NAME} object ${TAG_OBJECT_SHA}" >&2
  exit 1
}

for ((attempt = 1; attempt <= ATTEMPTS; attempt++)); do
  run="$(github_api \
    "${API_URL}/repos/${GITHUB_REPOSITORY}/actions/runs/${RUN_ID}")"
  provenance="$(
    jq -r \
      '
        [
          .event,
          .status,
          (.conclusion // ""),
          .path,
          .head_branch
        ]
        | @tsv
      ' <<<"$run"
  )"
  IFS=$'\t' read -r event status conclusion path head_branch <<<"$provenance"

  [[ "$event" == "workflow_dispatch" ]] || {
    echo "provenance run ${RUN_ID} was not manually dispatched" >&2
    exit 1
  }
  [[ "$path" == ".github/workflows/create-release-tag.yml" ||
    "$path" == ".github/workflows/create-release-tag.yml@"* ]] || {
    echo "provenance run ${RUN_ID} used unexpected workflow ${path}" >&2
    exit 1
  }
  [[ "$head_branch" == "main" ]] || {
    echo "provenance run ${RUN_ID} did not use the protected main branch" >&2
    exit 1
  }
  if [[ "$status" == "completed" ]]; then
    [[ "$conclusion" == "success" ]] || {
      echo "provenance run ${RUN_ID} completed with ${conclusion}" >&2
      exit 1
    }
    echo "verified ${TAG_NAME} provenance via GitHub Actions run ${RUN_ID}"
    exit 0
  fi

  if ((attempt < ATTEMPTS)); then
    sleep "$DELAY_SECONDS"
  fi
done

echo "provenance run ${RUN_ID} did not complete within the allowed time" >&2
exit 1
