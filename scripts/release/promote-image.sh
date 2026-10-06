#!/usr/bin/env bash
set -euo pipefail

SOURCE_IMAGE="${SOURCE_IMAGE:?SOURCE_IMAGE must be an immutable image reference}"
TARGET_IMAGE="${TARGET_IMAGE:?TARGET_IMAGE is required}"

if [[ ! "$SOURCE_IMAGE" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]]; then
  echo "SOURCE_IMAGE must be pinned by a complete sha256 digest: ${SOURCE_IMAGE}" >&2
  exit 1
fi

if [[ ! "$TARGET_IMAGE" =~ :v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "TARGET_IMAGE must end in :vMAJOR.MINOR.PATCH: ${TARGET_IMAGE}" >&2
  exit 1
fi

source_digest="${SOURCE_IMAGE##*@}"
target_repository="${TARGET_IMAGE%:*}"
target_tag="${TARGET_IMAGE##*:}"

target_tags="$(crane ls "$target_repository")" || {
  echo "failed to inspect existing tags in ${target_repository}" >&2
  exit 1
}
if grep -Fxq "$target_tag" <<<"$target_tags"; then
  existing_digest="$(crane digest "$TARGET_IMAGE")" || {
    echo "failed to resolve existing target digest: ${TARGET_IMAGE}" >&2
    exit 1
  }
  if [[ "$existing_digest" == "$source_digest" ]]; then
    echo "${TARGET_IMAGE} already points to ${source_digest}; promotion is complete"
    exit 0
  fi
  echo "refusing to overwrite ${TARGET_IMAGE}: existing=${existing_digest}, candidate=${source_digest}" >&2
  exit 1
fi

crane copy "$SOURCE_IMAGE" "$TARGET_IMAGE"

target_digest="$(crane digest "$TARGET_IMAGE")"
if [[ "$target_digest" != "$source_digest" ]]; then
  echo "promotion digest mismatch: source=${source_digest}, target=${target_digest}" >&2
  exit 1
fi

echo "Promoted ${TARGET_IMAGE} at ${target_digest}"
