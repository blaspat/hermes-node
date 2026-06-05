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
#   ./scripts/build.sh
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

# Targets: space-separated "GOOS/GOARCH" pairs.
TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
  "windows/arm64"
)

# Strip the Windows .exe when verifying arches with `file` later (it's the same
# arch; the suffix is just convention so Windows can launch it).
EXT=""
if [[ "${TARGETS[0]%%/*}" == "windows" ]]; then
  EXT=".exe"
fi

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
    -ldflags "-s -w" \
    -o "$out" \
    ./cmd/hermes-node
done

echo
echo "==> built artifacts:"
ls -lh "$DIST"
