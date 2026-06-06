#!/usr/bin/env bash
# macOS path test: same as smoke_test.sh but with a darwin/arm64 uname shim
# and a launchctl shim.
set -euo pipefail

INSTALL_SH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/install.sh"
TMP="$(mktemp -d -t hermes-node-mac.XXXXXX)"
# shellcheck disable=SC2317  # invoked via trap below
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

mkdir -p "$TMP/release"
cat >"$TMP/release/hermes-node-darwin-arm64" <<'BIN'
#!/usr/bin/env bash
echo "hermes-node v9.9.9-darwin"
BIN
chmod +x "$TMP/release/hermes-node-darwin-arm64"

SHIM="$TMP/shim"
mkdir -p "$SHIM"
cat >"$SHIM/curl" <<SHIM
#!/usr/bin/env bash
out=""
prev=""
for arg in "\$@"; do
  if [ "\$prev" = "-o" ]; then
    out="\$arg"
  fi
  prev="\$arg"
done
cp "$TMP/release/hermes-node-darwin-arm64" "\$out"
exit 0
SHIM
chmod +x "$SHIM/curl"

cat >"$SHIM/uname" <<'SHIM'
#!/usr/bin/env bash
case "$1" in
  -s) echo "Darwin" ;;
  -m) echo "arm64" ;;
  *)  uname "$@" ;;
esac
SHIM
chmod +x "$SHIM/uname"

# launchctl shim: just exit 0; the install script will believe it loaded.
cat >"$SHIM/launchctl" <<'SHIM'
#!/usr/bin/env bash
exit 0
SHIM
chmod +x "$SHIM/launchctl"

HOME="$TMP/home"
mkdir -p "$HOME"

env -i HOME="$HOME" PATH="$SHIM:/usr/bin:/bin" \
    "$INSTALL_SH" --version v9.9.9-darwin 2>&1 | tail -20

echo
echo "--- file layout ---"
ls -la "$HOME/.local/bin/" 2>/dev/null
ls -la "$HOME/Library/LaunchAgents/" 2>/dev/null

echo
echo "--- launchd plist ---"
cat "$HOME/Library/LaunchAgents/com.blaspat.hermes-node.plist" 2>/dev/null

echo
echo "--- checks ---"
fail=0
[ -x "$HOME/.local/bin/hermes-node" ] || { echo "FAIL: binary not installed"; fail=1; }
[ -f "$HOME/Library/LaunchAgents/com.blaspat.hermes-node.plist" ] || { echo "FAIL: plist missing"; fail=1; }
grep -q 'com.blaspat.hermes-node' "$HOME/Library/LaunchAgents/com.blaspat.hermes-node.plist" || { echo "FAIL: plist label wrong"; fail=1; }
grep -q 'RunAtLoad' "$HOME/Library/LaunchAgents/com.blaspat.hermes-node.plist" || { echo "FAIL: plist missing RunAtLoad"; fail=1; }

if [ "$fail" -eq 0 ]; then
  echo "MAC SMOKE PASSED"
else
  echo "MAC SMOKE FAILED"
  exit 1
fi
exit 0
