#!/usr/bin/env bash
set -euo pipefail

SOURCE_IMAGE="${SOURCE_IMAGE:?SOURCE_IMAGE must be an immutable image reference}"
TARGET_IMAGE="${TARGET_IMAGE:?TARGET_IMAGE is required}"

if [[ "$SOURCE_IMAGE" != *@sha256:* ]]; then
  echo "SOURCE_IMAGE must be pinned by sha256 digest: ${SOURCE_IMAGE}" >&2
  exit 1
fi

if [[ ! "$TARGET_IMAGE" =~ :v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "TARGET_IMAGE must end in :vMAJOR.MINOR.PATCH: ${TARGET_IMAGE}" >&2
  exit 1
fi

crane copy "$SOURCE_IMAGE" "$TARGET_IMAGE"

source_digest="${SOURCE_IMAGE##*@}"
target_digest="$(crane digest "$TARGET_IMAGE")"
if [[ "$target_digest" != "$source_digest" ]]; then
  echo "promotion digest mismatch: source=${source_digest}, target=${target_digest}" >&2
  exit 1
fi

echo "Promoted ${TARGET_IMAGE} at ${target_digest}"
