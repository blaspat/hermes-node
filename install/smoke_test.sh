#!/usr/bin/env bash
# Real end-to-end smoke test: stub curl, run install.sh, verify file layout.
#
# Lives in the repo's install/ dir so it travels with the source. Run with
#   bash install/smoke_test.sh
#
# Exits 0 on success, non-zero on failure.
set -euo pipefail

INSTALL_SH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/install.sh"
TMP="$(mktemp -d -t hermes-node-smoke.XXXXXX)"
# shellcheck disable=SC2317  # invoked via trap below
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

# Build the "release binary" — a real shell script that prints
# "hermes-node v9.9.9-smoke".
mkdir -p "$TMP/release"
cat >"$TMP/release/hermes-node-linux-arm64" <<'BIN'
#!/usr/bin/env bash
echo "hermes-node v9.9.9-smoke"
BIN
chmod +x "$TMP/release/hermes-node-linux-arm64"

# Build a curl shim that returns the local file regardless of URL.
SHIM="$TMP/shim"
mkdir -p "$SHIM"
cat >"$SHIM/curl" <<SHIM
#!/usr/bin/env bash
# Stub curl for the install test: any -o file gets the local release.
out=""
prev=""
for arg in "\$@"; do
  if [ "\$prev" = "-o" ]; then
    out="\$arg"
  fi
  prev="\$arg"
done
cp "$TMP/release/hermes-node-linux-arm64" "\$out"
exit 0
SHIM
chmod +x "$SHIM/curl"

# Build an uname shim for Linux/arm64.
cat >"$SHIM/uname" <<'SHIM'
#!/usr/bin/env bash
case "$1" in
  -s) echo "Linux" ;;
  -m) echo "aarch64" ;;
  *)  uname "$@" ;;
esac
SHIM
chmod +x "$SHIM/uname"

# Build a systemctl shim that no-ops. We don't want to actually start
# systemd --user; the test just needs the install script to believe it
# succeeded.
cat >"$SHIM/systemctl" <<'SHIM'
#!/usr/bin/env bash
exit 0
SHIM
chmod +x "$SHIM/systemctl"

HOME="$TMP/home"
mkdir -p "$HOME"
mkdir -p "$HOME/.local/bin"

# Run the installer. NO_SERVICE=0 to exercise the service path; the systemctl
# shim makes the call succeed.
env -i HOME="$HOME" PATH="$SHIM:/usr/bin:/bin" \
    "$INSTALL_SH" --version v9.9.9-smoke 2>&1 | tail -25

echo
echo "--- file layout after install ---"
ls -la "$HOME/.local/bin/" 2>/dev/null || echo "no .local/bin"
ls -la "$HOME/.config/systemd/user/" 2>/dev/null || echo "no systemd user dir"

echo
echo "--- binary --version ---"
"$HOME/.local/bin/hermes-node" --version 2>&1 || true

echo
echo "--- service unit ---"
cat "$HOME/.config/systemd/user/hermes-node.service" 2>/dev/null || echo "no service file"

echo
echo "--- checks ---"
fail=0
[ -x "$HOME/.local/bin/hermes-node" ] || { echo "FAIL: binary not executable"; fail=1; }
[ -f "$HOME/.config/systemd/user/hermes-node.service" ] || { echo "FAIL: service file missing"; fail=1; }
grep -q 'ExecStart=' "$HOME/.config/systemd/user/hermes-node.service" || { echo "FAIL: service unit missing ExecStart"; fail=1; }

if [ "$fail" -eq 0 ]; then
  echo "SMOKE TEST PASSED"
else
  echo "SMOKE TEST FAILED"
  exit 1
fi
exit 0
