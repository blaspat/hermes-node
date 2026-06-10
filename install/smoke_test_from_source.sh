#!/usr/bin/env bash
# Smoke test for --from-source: stub git + go, run install.sh, verify the
# built binary is placed at ~/.local/bin/hermes-node and is executable.
#
# The "go" shim simulates `go build` by writing a fake hermes-node
# binary into the requested -o path. The "git" shim populates the
# clone target with a fake ./cmd/hermes-node directory and a fake
# go.mod so the real `go build` invocation would succeed in a real
# environment.
#
# Run with:   bash install/smoke_test_from_source.sh
set -euo pipefail

INSTALL_SH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/install.sh"
TMP="$(mktemp -d -t hermes-node-src-smoke.XXXXXX)"
# shellcheck disable=SC2317  # invoked via trap below
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

SHIM="$TMP/shim"
mkdir -p "$SHIM"

# uname shim: Linux/arm64
cat >"$SHIM/uname" <<'SHIM'
#!/usr/bin/env bash
case "$1" in
  -s) echo "Linux" ;;
  -m) echo "aarch64" ;;
  *)  uname "$@" ;;
esac
SHIM
chmod +x "$SHIM/uname"

# systemctl shim
cat >"$SHIM/systemctl" <<'SHIM'
#!/usr/bin/env bash
exit 0
SHIM
chmod +x "$SHIM/systemctl"

# git shim: simulate a clone. The install script calls
#   git clone --depth 1 <url> <dest>
# We just need to create <dest>/cmd/hermes-node/ and a go.mod so a
# real `go build` would succeed in a real environment.
cat >"$SHIM/git" <<'SHIM'
#!/usr/bin/env bash
# Last positional arg is the dest dir.
dest="${@: -1}"
# The arg before that is the URL — skip.
url="${@:(-2):1}"
[ -n "$dest" ] || { echo "git shim: missing dest" >&2; exit 1; }
mkdir -p "$dest/cmd/hermes-node"
cat >"$dest/go.mod" <<MOD
module github.com/blaspat/hermes-nodes
go 1.22
MOD
cat >"$dest/cmd/hermes-node/main.go" <<'GO'
package main
import "fmt"
func main() { fmt.Println("hermes-node v0.0.0-fromsource-smoke") }
GO
exit 0
SHIM
chmod +x "$SHIM/git"

# go shim: simulate `go build -o <out> <package>` and `go version`.
# Writes a stub binary that prints our version string. The `go version`
# call comes first (the install script uses it to verify the toolchain
# is new enough), so we have to handle it without trying to parse
# `-o` from its argv.
cat >"$SHIM/go" <<'SHIM'
#!/usr/bin/env bash
case "$1" in
  version) echo "go version go1.22.0 linux/arm64"; exit 0 ;;
  build)
    out=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "-o" ]; then
        out="$arg"
      fi
      prev="$arg"
    done
    [ -n "$out" ] || { echo "go shim: missing -o" >&2; exit 1; }
    cat >"$out" <<'BIN'
#!/usr/bin/env bash
echo "hermes-node v0.0.0-fromsource-smoke"
BIN
    chmod +x "$out"
    exit 0
    ;;
  *) echo "go shim: unknown subcommand: $1" >&2; exit 1 ;;
esac
SHIM
chmod +x "$SHIM/go"

HOME="$TMP/home"
mkdir -p "$HOME/.local/bin"

echo "--- running install.sh --from-source ---"
env -i HOME="$HOME" PATH="$SHIM:/usr/bin:/bin" \
    "$INSTALL_SH" --from-source --no-service 2>&1 | tail -20

echo
echo "--- binary --version ---"
"$HOME/.local/bin/hermes-node" --version 2>&1 || true

echo
echo "--- checks ---"
fail=0
[ -x "$HOME/.local/bin/hermes-node" ] || { echo "FAIL: binary not executable"; fail=1; }
[ ! -f "$HOME/.config/systemd/user/hermes-node.service" ] || { echo "FAIL: service file should not exist (--no-service)"; fail=1; }

if [ "$fail" -eq 0 ]; then
  echo "FROM-SOURCE SMOKE TEST PASSED"
else
  echo "FROM-SOURCE SMOKE TEST FAILED"
  exit 1
fi
exit 0
