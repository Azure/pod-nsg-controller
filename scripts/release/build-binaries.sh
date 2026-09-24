#!/usr/bin/env bash
set -euo pipefail

VERSION="${VERSION:-}"
OUTPUT_DIR="${OUTPUT_DIR:-bin/release}"

if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "VERSION must use vMAJOR.MINOR.PATCH format, got: ${VERSION:-<empty>}" >&2
  exit 1
fi

mkdir -p "$OUTPUT_DIR"

for arch in amd64 arm64; do
  output="${OUTPUT_DIR}/pod-nsg-controller-linux-${arch}"
  echo "Building ${output} (${VERSION})"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build \
    -trimpath \
    -buildvcs=true \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o "$output" \
    ./cmd
  chmod 0555 "$output"
done

printf '%s\n' "$VERSION" >"${OUTPUT_DIR}/VERSION"
git rev-parse HEAD >"${OUTPUT_DIR}/COMMIT"
sha256sum "${OUTPUT_DIR}"/pod-nsg-controller-linux-* >"${OUTPUT_DIR}/SHA256SUMS"
