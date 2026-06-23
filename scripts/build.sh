#!/usr/bin/env bash
#
# Cross-compile hermes-node for all supported targets.
#
# Outputs static binaries to dist/ named:
#   hermes-node-<os>-<arch>[.exe]
#
# Targets (6):
#   linux/amd64, linux/arm64
#   darwin/amd64, darwin/arm64
#   windows/amd64, windows/arm64
#
# Usage:
#   ./scripts/build.sh [<version>]
#
# When <version> is provided, it is injected via -ldflags -X main.version.
# When omitted, the binary defaults to "dev" (see cmd/hermes-node/main.go).
#
# Requirements: Go toolchain on PATH (cross-compile is automatic — no CGO needed).

set -euo pipefail

# Resolve repo root from this script's location so the script works from anywhere.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

DIST="$REPO_ROOT/dist"
BINARY="hermes-node"

# Disable CGO so the output is a static binary (no glibc/Musl linkage surprises
# across distros). CGO_ENABLED=0 is the standard Go way to get a portable build.
export CGO_ENABLED=0

# Version string. When a tag is passed as the first argument, inject it.
# Otherwise the binary falls back to the "dev" default in main.go.
VERSION="${1:-}"
LDFLAGS="-s -w"
if [ -n "$VERSION" ]; then
  LDFLAGS="$LDFLAGS -X main.version=$VERSION"
fi

# Targets: space-separated "GOOS/GOARCH" pairs.
TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
  "windows/arm64"
)

echo "==> cleaning $DIST"
rm -rf "$DIST"
mkdir -p "$DIST"

for target in "${TARGETS[@]}"; do
  GOOS="${target%/*}"
  GOARCH="${target#*/}"
  suffix=""
  if [[ "$GOOS" == "windows" ]]; then
    suffix=".exe"
  fi
  out="$DIST/${BINARY}-${GOOS}-${GOARCH}${suffix}"

  echo "==> building $target -> ${out#$REPO_ROOT/}"
  GOOS="$GOOS" GOARCH="$GOARCH" go build \
    -trimpath \
    -ldflags "$LDFLAGS" \
    -o "$out" \
    ./cmd/hermes-node
done

echo
echo "==> built artifacts:"
ls -lh "$DIST"
