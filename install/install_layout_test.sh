#!/usr/bin/env bash
#
# install_layout_test.sh — unit tests for install/install.sh
#
# Strategy
# --------
# The install script does two main things that are easy to verify without
# actually touching the system:
#
#   1. Resolves the planned file layout (binary path, config dir, service
#      file) based on the user's environment. We exercise this by setting
#      HOME / HERMES_NODE_BIN_DIR / HERMES_NODE_CONFIG_DIR explicitly and
#      running the script with --print-layout, then parsing the JSON it
#      emits. The script sources its layout logic at startup, so we can
#      trap on the early-exit path of --print-layout without ever running
#      the network or service code.
#
#   2. Detects the running OS/arch from uname. We test this by faking
#      uname via PATH manipulation — a tiny shim binary that prints the
#      values we want.
#
#   3. Refuses to overwrite an already-installed binary unless --yes is
#      passed. We stub the binary's --version output and confirm the
#      prompt + skip behavior.
#
# The script under test is never asked to download or touch the real
# system; we use --dry-run for any path that would otherwise side-effect.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_SH="$SCRIPT_DIR/install.sh"

if [ ! -f "$INSTALL_SH" ]; then
  echo "FAIL: install.sh not found at $INSTALL_SH" >&2
  exit 1
fi

PASS=0
FAIL=0

pass() { PASS=$((PASS + 1)); printf '  ok  %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n    %s\n' "$1" "$2"; }

# tmp dir auto-cleaned on exit
TMP_ROOT="$(mktemp -d -t hermes-node-install-test.XXXXXX)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# ----------------------------------------------------------------------
# Test 1: --help exits 0 and prints usage
# ----------------------------------------------------------------------
test_help() {
  local out
  if ! out="$("$INSTALL_SH" --help 2>&1)"; then
    fail "--help exits 0" "exit non-zero"
    return
  fi
  if ! grep -q -- '--dry-run' <<<"$out"; then
    fail "--help mentions --dry-run" "no match in output"
    return
  fi
  if ! grep -q -- '--print-layout' <<<"$out"; then
    fail "--help mentions --print-layout" "no match in output"
    return
  fi
  pass "--help exits 0 and lists key flags"
}

# ----------------------------------------------------------------------
# Test 2: --print-layout on linux/amd64 produces the expected JSON.
# We fake the arch via a uname shim because the host machine may be a
# different arch entirely (e.g. CI on arm64 testing the amd64 path).
# ----------------------------------------------------------------------
test_print_layout_linux_amd64() {
  local home="$TMP_ROOT/home1"
  mkdir -p "$home"
  local shim="$TMP_ROOT/shim-amd64"
  mkdir -p "$shim"
  cat >"$shim/uname" <<'SHIM'
#!/usr/bin/env bash
case "$1" in
  -s) echo "Linux" ;;
  -m) echo "x86_64" ;;
  *)  uname "$@" ;;
esac
SHIM
  chmod +x "$shim/uname"
  local out
  out="$(env -i HOME="$home" PATH="$shim:/usr/bin:/bin" "$INSTALL_SH" --print-layout 2>/dev/null)"

  local actual_arch actual_binary actual_config actual_service_kind actual_service_file
  actual_arch="$(jq -r .arch <<<"$out")"
  actual_binary="$(jq -r .binary <<<"$out")"
  actual_config="$(jq -r .config_dir <<<"$out")"
  actual_service_kind="$(jq -r .service.kind <<<"$out")"
  actual_service_file="$(jq -r .service.file <<<"$out")"

  if [ "$actual_arch" != "amd64" ]; then
    fail "linux layout: arch=amd64" "got $actual_arch"
    return
  fi
  if [ "$actual_binary" != "$home/.local/bin/hermes-node" ]; then
    fail "linux layout: binary path" "got $actual_binary"
    return
  fi
  if [ "$actual_config" != "$home/.hermes-node" ]; then
    fail "linux layout: config dir" "got $actual_config"
    return
  fi
  if [ "$actual_service_kind" != "systemd-user" ]; then
    fail "linux layout: service kind" "got $actual_service_kind"
    return
  fi
  if [ "$actual_service_file" != "$home/.config/systemd/user/hermes-node.service" ]; then
    fail "linux layout: service file" "got $actual_service_file"
    return
  fi
  pass "linux/amd64 --print-layout matches expected JSON"
}

# ----------------------------------------------------------------------
# Test 3: HERMES_NODE_BIN_DIR override is honored
# ----------------------------------------------------------------------
test_bin_dir_override() {
  local home="$TMP_ROOT/home2"
  local custom_bin="$TMP_ROOT/custom-bin"
  mkdir -p "$home" "$custom_bin"
  local out
  out="$(env -i HOME="$home" HERMES_NODE_BIN_DIR="$custom_bin" PATH="/usr/bin:/bin" \
        "$INSTALL_SH" --print-layout 2>/dev/null)"

  local actual
  actual="$(jq -r .binary <<<"$out")"
  if [ "$actual" != "$custom_bin/hermes-node" ]; then
    fail "HERMES_NODE_BIN_DIR override" "got $actual"
    return
  fi
  pass "HERMES_NODE_BIN_DIR override flows through to .binary"
}

# ----------------------------------------------------------------------
# Test 4: macOS path with a faked uname
# ----------------------------------------------------------------------
# Build a shim directory: a fake 'uname' that prints darwin/arm64.
test_print_layout_darwin_arm64() {
  local home="$TMP_ROOT/home_darwin"
  mkdir -p "$home"
  local shim="$TMP_ROOT/shim-darwin"
  mkdir -p "$shim"
  cat >"$shim/uname" <<'SHIM'
#!/usr/bin/env bash
case "$1" in
  -s) echo "Darwin" ;;
  -m) echo "arm64" ;;
  *)  uname "$@" ;;
esac
SHIM
  chmod +x "$shim/uname"
  # Build a reduced PATH that prefers our shim, then the system.
  local out
  out="$(env -i HOME="$home" PATH="$shim:/usr/bin:/bin" \
        "$INSTALL_SH" --print-layout 2>/dev/null)"

  local actual_os actual_arch actual_service_kind actual_service_file
  actual_os="$(jq -r .os <<<"$out")"
  actual_arch="$(jq -r .arch <<<"$out")"
  actual_service_kind="$(jq -r .service.kind <<<"$out")"
  actual_service_file="$(jq -r .service.file <<<"$out")"

  if [ "$actual_os" != "darwin" ]; then
    fail "darwin layout: os" "got $actual_os"
    return
  fi
  if [ "$actual_arch" != "arm64" ]; then
    fail "darwin layout: arch" "got $actual_arch"
    return
  fi
  if [ "$actual_service_kind" != "launchd" ]; then
    fail "darwin layout: service kind" "got $actual_service_kind"
    return
  fi
  if [ "$actual_service_file" != "$home/Library/LaunchAgents/com.blaspat.hermes-node.plist" ]; then
    fail "darwin layout: service file" "got $actual_service_file"
    return
  fi
  pass "darwin/arm64 --print-layout matches expected JSON"
}

# ----------------------------------------------------------------------
# Test 5: --dry-run install on Linux with HERMES_NODE_NO_SERVICE=1
#         leaves nothing behind in the home dir.
# ----------------------------------------------------------------------
test_dry_run_does_not_touch_home() {
  local home="$TMP_ROOT/home_dry"
  mkdir -p "$home"
  # Refuse to actually do a download: --dry-run + --version should skip
  # the network call entirely.
  if env -i HOME="$home" HERMES_NODE_DRY_RUN=1 HERMES_NODE_NO_SERVICE=1 \
        HERMES_NODE_VERSION=v9.9.9-test PATH="/usr/bin:/bin" \
        "$INSTALL_SH" 2>&1 | grep -q '^error'; then
    fail "dry-run install with explicit --version" "error message in output"
    return
  fi
  if [ -e "$home/.local/bin/hermes-node" ]; then
    fail "dry-run install" "binary was actually placed"
    return
  fi
  if [ -e "$home/.config/systemd" ]; then
    fail "dry-run install" "systemd user dir was created"
    return
  fi
  pass "dry-run install with --version + NO_SERVICE=1 touches nothing on disk"
}

# ----------------------------------------------------------------------
# Test 6: dry-run logs the actions it WOULD take
# ----------------------------------------------------------------------
test_dry_run_logs_actions() {
  local home="$TMP_ROOT/home_dry_log"
  mkdir -p "$home"
  local out
  out="$(env -i HOME="$home" HERMES_NODE_DRY_RUN=1 HERMES_NODE_NO_SERVICE=1 \
        HERMES_NODE_VERSION=v9.9.9-test PATH="/usr/bin:/bin" \
        "$INSTALL_SH" 2>&1)"

  if ! grep -q -- '\[dry-run\]' <<<"$out"; then
    fail "dry-run logs [dry-run] actions" "no [dry-run] marker in output"
    return
  fi
  if ! grep -q -- 'v9.9.9-test' <<<"$out"; then
    fail "dry-run echoes requested version" "no version in output"
    return
  fi
  if ! grep -q -- 'would install binary' <<<"$out"; then
    fail "dry-run mentions 'would install binary'" "no install message in output"
    return
  fi
  pass "dry-run logs [dry-run] actions and the requested version"
}

# ----------------------------------------------------------------------
# Test 7: unknown argument exits non-zero with an error
# ----------------------------------------------------------------------
test_unknown_argument() {
  local rc=0
  env -i HOME="$TMP_ROOT" PATH="/usr/bin:/bin" \
       "$INSTALL_SH" --no-such-flag >/dev/null 2>&1 || rc=$?
  if [ "$rc" -eq 0 ]; then
    fail "unknown argument exits non-zero" "got exit 0"
    return
  fi
  pass "unknown argument exits non-zero"
}

# ----------------------------------------------------------------------
# Test 8: detect_installed_version returns empty for a fake binary that
#         prints a non-hermes banner (so the installer will prompt to
#         replace it). We exercise this through --dry-run.
# ----------------------------------------------------------------------
test_existing_binary_detection() {
  local home="$TMP_ROOT/home_existing"
  local bin_dir="$home/.local/bin"
  mkdir -p "$bin_dir"
  # Fake hermes-node binary that just reports a different version.
  cat >"$bin_dir/hermes-node" <<'BIN'
#!/usr/bin/env bash
echo "hermes-node v0.0.1-fake"
BIN
  chmod +x "$bin_dir/hermes-node"

  # With HERMES_NODE_NO_SERVICE=1 and a non-TTY stdin, the script should
  # bail out with an error (refusing to overwrite without --yes).
  local rc=0
  env -i HOME="$home" HERMES_NODE_DRY_RUN=1 HERMES_NODE_NO_SERVICE=1 \
        HERMES_NODE_VERSION=v9.9.9-test PATH="/usr/bin:/bin" \
        "$INSTALL_SH" </dev/null >/dev/null 2>&1 || rc=$?
  if [ "$rc" -eq 0 ]; then
    fail "refuses to overwrite existing binary without --yes" "got exit 0"
    return
  fi
  # Existing binary must still be there (we did not touch it).
  if [ ! -f "$bin_dir/hermes-node" ]; then
    fail "refuses to overwrite" "binary was removed"
    return
  fi
  pass "refuses to overwrite existing hermes-node without --yes"
}

# ----------------------------------------------------------------------
# Test 9: with --yes and a fake existing binary, dry-run reports the
#         version and proceeds past the prompt.
# ----------------------------------------------------------------------
test_existing_binary_with_yes() {
  local home="$TMP_ROOT/home_existing_yes"
  local bin_dir="$home/.local/bin"
  mkdir -p "$bin_dir"
  cat >"$bin_dir/hermes-node" <<'BIN'
#!/usr/bin/env bash
echo "hermes-node v0.0.1-fake"
BIN
  chmod +x "$bin_dir/hermes-node"

  local out
  out="$(env -i HOME="$home" HERMES_NODE_DRY_RUN=1 HERMES_NODE_NO_SERVICE=1 \
        HERMES_NODE_VERSION=v9.9.9-test HERMES_NODE_ASSUME_YES=1 \
        PATH="/usr/bin:/bin" \
        "$INSTALL_SH" </dev/null 2>&1)"
  # Should not have bailed on the prompt.
  if grep -q 'aborted by user' <<<"$out"; then
    fail "with --yes: proceeds past prompt" "bailed: $(head -3 <<<"$out")"
    return
  fi
  if ! grep -q 'v9.9.9-test' <<<"$out"; then
    fail "with --yes: dry-run logs the target version" "no version in output"
    return
  fi
  pass "with --yes, dry-run proceeds past existing-binary prompt"
}

# ----------------------------------------------------------------------
# Test 10: when installed version equals requested, dry-run says
#          "nothing to do" and exits 0.
# ----------------------------------------------------------------------
test_installed_version_matches() {
  local home="$TMP_ROOT/home_match"
  local bin_dir="$home/.local/bin"
  mkdir -p "$bin_dir"
  cat >"$bin_dir/hermes-node" <<BIN
#!/usr/bin/env bash
echo "hermes-node v9.9.9-test"
BIN
  chmod +x "$bin_dir/hermes-node"

  local out rc=0
  out="$(env -i HOME="$home" HERMES_NODE_DRY_RUN=1 HERMES_NODE_NO_SERVICE=1 \
        HERMES_NODE_VERSION=v9.9.9-test PATH="/usr/bin:/bin" \
        "$INSTALL_SH" </dev/null 2>&1)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "matching version: clean exit" "got exit $rc: $(head -3 <<<"$out")"
    return
  fi
  if ! grep -q 'nothing to do' <<<"$out"; then
    fail "matching version: 'nothing to do'" "no match in output: $(head -3 <<<"$out")"
    return
  fi
  pass "matching installed version produces 'nothing to do' and exits 0"
}

# ----------------------------------------------------------------------
# Test 11: --uninstall in dry-run reports what it would remove and
#          touches nothing.
# ----------------------------------------------------------------------
test_uninstall_dry_run() {
  local home="$TMP_ROOT/home_uninst"
  local bin_dir="$home/.local/bin"
  local service_dir="$home/.config/systemd/user"
  mkdir -p "$bin_dir" "$service_dir"
  echo "fake" >"$bin_dir/hermes-node"
  echo "fake" >"$service_dir/hermes-node.service"

  local out
  out="$(env -i HOME="$home" HERMES_NODE_DRY_RUN=1 \
        PATH="/usr/bin:/bin" "$INSTALL_SH" --uninstall </dev/null 2>&1)"

  if ! grep -q -- '\[dry-run\]' <<<"$out"; then
    fail "uninstall dry-run logs [dry-run] actions" "no marker in output"
    return
  fi
  # Files still present (we did not actually remove them).
  if [ ! -f "$bin_dir/hermes-node" ] || [ ! -f "$service_dir/hermes-node.service" ]; then
    fail "uninstall dry-run touches nothing" "file disappeared"
    return
  fi
  pass "--uninstall --dry-run reports actions and removes nothing"
}

# ----------------------------------------------------------------------
# Run
# ----------------------------------------------------------------------

echo "running install.sh unit tests..."
echo

test_help
test_print_layout_linux_amd64
test_bin_dir_override
test_print_layout_darwin_arm64
test_dry_run_does_not_touch_home
test_dry_run_logs_actions
test_unknown_argument
test_existing_binary_detection
test_existing_binary_with_yes
test_installed_version_matches
test_uninstall_dry_run

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
