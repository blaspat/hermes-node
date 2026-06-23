#!/usr/bin/env bash
#
# install.sh — install hermes-node on macOS or Linux and register it as a
# user-level background service.
#
# Downloads a release binary from GitHub Releases (https://github.com/blaspat/
# hermes-nodes/releases), drops it in ~/.local/bin/, and creates a per-user
# service unit so the node runs at login:
#
#   - macOS:   ~/Library/LaunchAgents/com.blaspat.hermes-node.plist
#   - Linux (systemd available):
#             ~/.config/systemd/user/hermes-node.service
#   - Linux (no systemd): warns the user, leaves the binary installed but
#             does not register a service. The user can run `hermes-node`
#             manually or wire it into their init of choice.
#
# Modes
# -----
#   --version <tag>   install a specific release (default: latest)
#   --dry-run         show what would happen, change nothing, exit 0
#   --print-layout    print the planned file paths and exit (no network call)
#   --no-service      install the binary but do not register a service
#   --uninstall       remove the binary and service registration
#   --yes             skip the "already installed" confirmation prompt
#   --from-source   [DEPRECATED — build from source is now the default]
#                   build from source (git clone + go build) instead of
#                   downloading a release. This is the default behavior.
#                   Requires: git, go (1.22+)
#   --binary        download a pre-built release binary instead of
#                   building from source (may trigger antivirus false
#                   positives on Windows)
#   -h | --help       show this message
#
# Environment overrides
# --------------------
#   HERMES_NODE_VERSION   default value of --version
#   HERMES_NODE_REPO      GitHub repo (default: blaspat/hermes-nodes)
#   HERMES_NODE_BIN_DIR   install dir for the binary
#                          (default: ~/.local/bin)
#   HERMES_NODE_CONFIG_DIR default: ~/.hermes-nodes
#   HERMES_NODE_NO_SERVICE  1 to skip service registration
#   HERMES_NODE_DRY_RUN     1 to dry-run
#   HERMES_NODE_ASSUME_YES  1 to skip confirmation prompts (like --yes)
#
# Exit codes
# ----------
#   0  success (or dry-run completed without errors)
#   1  generic error
#   2  invalid arguments
#   3  download/verification failed
#   4  unsupported OS/arch
#   5  service registration failed (binary was still installed)

set -euo pipefail

REPO="${HERMES_NODE_REPO:-blaspat/hermes-nodes}"
BIN_NAME="hermes-node"
SERVICE_LABEL="com.blaspat.hermes-node"

print_help() {
  sed -n '2,/^# Exit codes/p' "$0" | sed 's/^# \{0,1\}//'
}

log() { printf '==> %s\n' "$*" >&2; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------

DRY_RUN="${HERMES_NODE_DRY_RUN:-0}"
NO_SERVICE="${HERMES_NODE_NO_SERVICE:-0}"
PRINT_LAYOUT=0
UNINSTALL=0
ASSUME_YES="${HERMES_NODE_ASSUME_YES:-0}"
FROM_SOURCE=1
BINARY_DOWNLOAD=0
VERSION="${HERMES_NODE_VERSION:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --version)        VERSION="$2"; shift 2 ;;
    --version=*)      VERSION="${1#*=}"; shift ;;
    --dry-run)        DRY_RUN=1; shift ;;
    --print-layout)   PRINT_LAYOUT=1; shift ;;
    --no-service)     NO_SERVICE=1; shift ;;
    --uninstall)      UNINSTALL=1; shift ;;
    --binary)         BINARY_DOWNLOAD=1; FROM_SOURCE=0; shift ;;
    --from-source)    FROM_SOURCE=1; shift ;;  # now the default, kept for compat
    --yes|-y)         ASSUME_YES=1; shift ;;
    -h|--help)        print_help; exit 0 ;;
    *)                die "unknown argument: $1 (try --help)" ;;
  esac
done

# ---------------------------------------------------------------------------
# OS / arch detection
# ---------------------------------------------------------------------------

UNAME_S="$(uname -s)"
UNAME_M="$(uname -m)"

case "$UNAME_S" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  *)      die "unsupported OS: $UNAME_S (this script supports macOS and Linux)" ;;
esac

case "$UNAME_M" in
  x86_64|amd64)   ARCH=amd64 ;;
  arm64|aarch64)  ARCH=arm64 ;;
  *)              die "unsupported arch: $UNAME_M" ;;
esac

ASSET_NAME="${BIN_NAME}-${OS}-${ARCH}"

# ---------------------------------------------------------------------------
# Path resolution
# ---------------------------------------------------------------------------

# Resolve install + config dirs. The binary goes in a directory that is
# typically on the user's PATH; the config dir was separate so the user could
# wipe it without losing the binary. Now both share the same directory for
# simplicity.
BIN_DIR="${HERMES_NODE_BIN_DIR:-$HOME/.hermes-nodes}"
CONFIG_DIR="${HERMES_NODE_CONFIG_DIR:-$HOME/.hermes-nodes}"
BIN_PATH="$BIN_DIR/$BIN_NAME"

# Service files are placed under the user's own config root so we never need
# root / sudo. (systemd --user and launchd as the logged-in user both read
# from the per-user prefix.)
case "$OS" in
  linux)
    SYSTEMD_USER_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
    SERVICE_FILE="$SYSTEMD_USER_DIR/${BIN_NAME}.service"
    SERVICE_KIND="systemd-user"
    ;;
  darwin)
    LAUNCH_AGENTS_DIR="$HOME/Library/LaunchAgents"
    SERVICE_FILE="$LAUNCH_AGENTS_DIR/${SERVICE_LABEL}.plist"
    SERVICE_KIND="launchd"
    ;;
esac

# ---------------------------------------------------------------------------
# --print-layout: report the planned layout and exit before doing anything
# ---------------------------------------------------------------------------

# Exposed as a subshell-readable function so install_layout_test.sh can call
# it under controlled env (it sets BIN_DIR / CONFIG_DIR / HOME).
print_layout() {
  # Build the JSON via jq -n --arg so that any value containing quotes,
  # backslashes, or control characters is escaped correctly. The values are
  # env-controlled (HERMES_NODE_BIN_DIR, HERMES_NODE_VERSION, etc.), not
  # user-arbitrary, but a hardened path is the right default for an
  # installer that prints machine-readable output.
  jq -n \
    --arg os "$OS" \
    --arg arch "$ARCH" \
    --arg asset "$ASSET_NAME" \
    --arg binary "$BIN_PATH" \
    --arg config_dir "$CONFIG_DIR" \
    --arg service_kind "$SERVICE_KIND" \
    --arg service_file "$SERVICE_FILE" \
    --arg version_requested "${VERSION:-latest}" \
    --argjson dry_run "$DRY_RUN" \
    '{
      os: $os,
      arch: $arch,
      asset: $asset,
      binary: $binary,
      config_dir: $config_dir,
      service: {
        kind: $service_kind,
        file: $service_file
      },
      version_requested: $version_requested,
      dry_run: $dry_run
    }'
}

if [ "$PRINT_LAYOUT" = 1 ]; then
  print_layout
  exit 0
fi

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# confirm_installed <message> — ask the user to proceed when something is
# already in place. Respects --yes.
confirm_proceed() {
  if [ "$ASSUME_YES" = 1 ]; then
    return 0
  fi
  if [ ! -t 0 ]; then
    # No interactive stdin (CI, piped input): default to "no" so the user
    # must opt in with --yes.
    warn "stdin is not a TTY and --yes was not passed; refusing to overwrite"
    return 1
  fi
  local reply
  printf '%s [y/N] ' "$1" >&2
  read -r reply
  case "$reply" in
    y|Y|yes|YES) return 0 ;;
    *)           return 1 ;;
  esac
}

# run_actual <cmd...> — if DRY_RUN=1, echo the command and skip; otherwise run
# it. We never exec a destructive command while dry-running, so reviewers can
# see exactly what would happen.
run_actual() {
  if [ "$DRY_RUN" = 1 ]; then
    printf '  [dry-run] %s\n' "$*" >&2
    return 0
  fi
  "$@"
}

# detect_installed_version <path> — read the version string from an existing
# binary. Returns empty if the file is missing or not a hermes-node binary.
detect_installed_version() {
  local path="$1"
  if [ ! -x "$path" ]; then
    return 0
  fi
  # hermes-node --version prints "hermes-node <version>". We extract the
  # second whitespace-separated token. If the binary exists but is not
  # ours, we return empty so the caller can prompt for overwrite.
  local out
  if ! out="$("$path" --version 2>/dev/null)"; then
    return 0
  fi
  case "$out" in
    "hermes-node "*) printf '%s' "${out#hermes-node }" ;;
    *)               return 0 ;;
  esac
}

# Latest release tag, e.g. "v0.1.0" or empty on failure.
latest_release_tag() {
  if command -v gh >/dev/null 2>&1; then
    gh release view --repo "$REPO" --json tagName --jq '.tagName' 2>/dev/null \
      || return 1
  elif command -v curl >/dev/null 2>&1; then
    # The /releases/latest endpoint redirects to the actual release page; use
    # the API endpoint to get the tag directly. Anonymous requests are
    # rate-limited to 60/h per IP, which is fine for occasional use.
    curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null \
      | jq -r '.tagName // empty' 2>/dev/null \
      || return 1
  else
    warn "neither gh nor curl is on PATH; cannot look up the latest release"
    return 1
  fi
}

# ---------------------------------------------------------------------------
# Uninstall path
# ---------------------------------------------------------------------------

do_uninstall() {
  log "uninstalling $BIN_NAME"

  if [ -L "$BIN_PATH" ] || [ -f "$BIN_PATH" ]; then
    run_actual rm -f "$BIN_PATH"
    log "removed $BIN_PATH"
  else
    log "no binary at $BIN_PATH"
  fi

  case "$OS" in
    linux)
      if [ "$DRY_RUN" != 1 ] && [ -f "$SERVICE_FILE" ]; then
        if command -v systemctl >/dev/null 2>&1; then
          systemctl --user disable --now "${BIN_NAME}.service" 2>/dev/null || true
        fi
        run_actual rm -f "$SERVICE_FILE"
        log "removed systemd user service: $SERVICE_FILE"
      elif [ -f "$SERVICE_FILE" ]; then
        run_actual rm -f "$SERVICE_FILE"
        log "removed $SERVICE_FILE (service would also be disabled)"
      fi
      ;;
    darwin)
      if [ "$DRY_RUN" != 1 ] && [ -f "$SERVICE_FILE" ]; then
        launchctl unload "$SERVICE_FILE" 2>/dev/null || true
      fi
      run_actual rm -f "$SERVICE_FILE"
      log "removed launchd agent: $SERVICE_FILE"
      ;;
  esac

  log "config dir left in place: $CONFIG_DIR (remove manually if you want a full wipe)"
  log "uninstall complete"
}

# ---------------------------------------------------------------------------
# Install path
# ---------------------------------------------------------------------------

do_install() {
  log "installing $BIN_NAME for $OS/$ARCH"

  # --- resolve version (--version > env > latest) -----------------------
  if [ -z "$VERSION" ]; then
    log "looking up latest release of $REPO"
    if [ "$DRY_RUN" = 1 ]; then
      VERSION="v0.0.0-dryrun"
      log "  [dry-run] would call: gh release view --repo $REPO"
    else
      VERSION="$(latest_release_tag || true)"
      if [ -z "$VERSION" ]; then
        die "could not determine the latest release; pass --version <tag> explicitly"
      fi
    fi
  fi
  log "version: $VERSION"

  # --- check for existing install --------------------------------------
  if [ -e "$BIN_PATH" ]; then
    local_existing="$(detect_installed_version "$BIN_PATH" || true)"
    if [ -n "$local_existing" ]; then
      log "already installed: $local_existing at $BIN_PATH"
      if [ "$local_existing" = "$VERSION" ]; then
        log "installed version matches; nothing to do"
      else
        if ! confirm_proceed "upgrade $local_existing -> $VERSION?"; then
          die "aborted by user"
        fi
      fi
    else
      if ! confirm_proceed "replace existing file at $BIN_PATH?"; then
        die "aborted by user"
      fi
    fi
  fi

  # --- source of the binary -------------------------------------------
  # Default: build from source (avoids antivirus false positives on
  # pre-built binaries). Use --binary to download a release instead.
  if [ "$BINARY_DOWNLOAD" = 1 ]; then
    # --- download release -----------------------------------------------
    DOWNLOAD_URL="https://github.com/$REPO/releases/download/$VERSION/${BIN_NAME}-${OS}-${ARCH}"

    if [ "$DRY_RUN" = 1 ]; then
      log "  [dry-run] would download: $DOWNLOAD_URL"
    else
      TMP_DIR="$(mktemp -d -t hermes-node-install.XXXXXX)"
      # shellcheck disable=SC2064  # we want $TMP_DIR captured now
      trap "rm -rf '$TMP_DIR'" EXIT
      TMP_FILE="$TMP_DIR/$ASSET_NAME"

      if command -v curl >/dev/null 2>&1; then
        log "downloading with curl"
        if ! curl -fL --retry 3 --connect-timeout 15 \
            -o "$TMP_FILE" "$DOWNLOAD_URL"; then
          die "download failed: $DOWNLOAD_URL"
        fi
      elif command -v wget >/dev/null 2>&1; then
        log "downloading with wget"
        if ! wget -q --tries=3 --timeout=15 \
            -O "$TMP_FILE" "$DOWNLOAD_URL"; then
          die "download failed: $DOWNLOAD_URL"
        fi
      else
        die "neither curl nor wget is on PATH; cannot download"
      fi

      if [ ! -s "$TMP_FILE" ]; then
        die "downloaded file is empty: $TMP_FILE"
      fi
    fi
  else
    # --- build from source (default) ------------------------------------
    # If Go is not available, fall back to downloading the pre-built binary.
    if ! command -v go >/dev/null 2>&1; then
      log "Go not found on PATH — falling back to pre-built binary"
      BINARY_DOWNLOAD=1
    else
      go_version="$(go version 2>/dev/null | awk '{print $3}')"
      case "$go_version" in
        go1.2[2-9]*|go1.[3-9]*|go[2-9]*) ;;  # 1.22+
        *)
          log "Go $go_version is too old (need 1.22+) — falling back to pre-built binary"
          BINARY_DOWNLOAD=1
          ;;
      esac
    fi

    if [ "$BINARY_DOWNLOAD" = 1 ]; then
      # --- download release (fallback) ----------------------------------
      DOWNLOAD_URL="https://github.com/$REPO/releases/download/$VERSION/${BIN_NAME}-${OS}-${ARCH}"
      if [ "$DRY_RUN" = 1 ]; then
        log "  [dry-run] would download: $DOWNLOAD_URL"
      else
        TMP_DIR="$(mktemp -d -t hermes-node-install.XXXXXX)"
        trap "rm -rf '$TMP_DIR'" EXIT
        TMP_FILE="$TMP_DIR/$ASSET_NAME"
        if command -v curl >/dev/null 2>&1; then
          log "downloading with curl"
          curl -fL --retry 3 --connect-timeout 15 -o "$TMP_FILE" "$DOWNLOAD_URL" || die "download failed: $DOWNLOAD_URL"
        elif command -v wget >/dev/null 2>&1; then
          log "downloading with wget"
          wget -q --tries=3 --timeout=15 -O "$TMP_FILE" "$DOWNLOAD_URL" || die "download failed: $DOWNLOAD_URL"
        else
          die "neither curl nor wget is on PATH; cannot download"
        fi
        if [ ! -s "$TMP_FILE" ]; then
          die "downloaded file is empty: $TMP_FILE"
        fi
      fi
    elif [ "$DRY_RUN" = 1 ]; then
      log "  [dry-run] would: git clone --depth 1 --branch $VERSION https://github.com/$REPO.git \$TMP_DIR/src"
      log "  [dry-run] would: cd \$TMP_DIR/src && go build -o \$TMP_DIR/hermes-node ./cmd/hermes-node"
    else
      command -v git >/dev/null 2>&1 || die "requires git on PATH"
      command -v go >/dev/null 2>&1 || die "requires go (1.22+) on PATH"
      go_version="$(go version 2>/dev/null | awk '{print $3}')"
      case "$go_version" in
        go1.2[2-9]*|go1.[3-9]*|go[2-9]*) ;;  # 1.22+
        *) die "requires go 1.22 or later; found: $go_version" ;;
      esac
      TMP_DIR="$(mktemp -d -t hermes-node-install.XXXXXX)"
      trap "rm -rf '$TMP_DIR'" EXIT
      log "cloning $REPO at $VERSION"
      git clone --depth 1 --branch "$VERSION" "https://github.com/$REPO.git" "$TMP_DIR/src" \
        || die "git clone failed (tag $VERSION not found?)"
      log "building with $go_version"
      ( cd "$TMP_DIR/src" && go build -ldflags "-X main.version=$VERSION" -o "$TMP_DIR/hermes-node" ./cmd/hermes-node ) \
        || die "go build failed"
      TMP_FILE="$TMP_DIR/hermes-node"
    fi
  fi

  # --- place binary -----------------------------------------------------
  run_actual mkdir -p "$BIN_DIR"
  if [ "$DRY_RUN" = 1 ]; then
    log "  [dry-run] would install binary to $BIN_PATH (mode 0755)"
  else
    # TMP_FILE is set by the download or build branch above; the build
    # branch places the binary at $TMP_DIR/hermes-node rather than the
    # OS/ARCH-suffixed asset name.
    if [ ! -s "$TMP_FILE" ]; then
      die "source binary is empty or missing: $TMP_FILE"
    fi
    run_actual install -m 0755 "$TMP_FILE" "$BIN_PATH"
    log "installed binary: $BIN_PATH"
  fi

  # --- service registration -------------------------------------------
  if [ "$NO_SERVICE" = 1 ]; then
    log "--no-service set; skipping service registration"
  else
    register_service
  fi

  # --- user-facing summary --------------------------------------------
  cat <<EOF

$BIN_NAME $VERSION is installed.

  binary:  $BIN_PATH
  config:  $CONFIG_DIR  (created on first run via 'hermes-node pair')
  service: $SERVICE_FILE  (kind: $SERVICE_KIND)

Next step — pair this node with your Hermes Agent brain:

  $BIN_NAME pair --server <wss-url> --token <token>

If $BIN_DIR is not on your PATH, add this to your shell rc:

  export PATH="\$HOME/.hermes-nodes:\$PATH"
EOF
}

# ---------------------------------------------------------------------------
# Service registration
# ---------------------------------------------------------------------------

register_service() {
  case "$OS" in
    linux)
      if ! command -v systemctl >/dev/null 2>&1; then
        warn "systemctl not found; the binary is installed but no service was registered"
        warn "run '$BIN_NAME' manually, or wire it into your init of choice"
        return 0
      fi

      run_actual mkdir -p "$SYSTEMD_USER_DIR"
      if [ "$DRY_RUN" = 1 ]; then
        log "  [dry-run] would write $SERVICE_FILE"
        log "  [dry-run] would run: systemctl --user daemon-reload && systemctl --user enable --now $BIN_NAME.service"
        return 0
      fi

      cat >"$SERVICE_FILE" <<UNIT
[Unit]
Description=hermes-node (pairs with a Hermes Agent brain)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN_PATH
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
UNIT

      if ! systemctl --user daemon-reload \
          || ! systemctl --user enable --now "${BIN_NAME}.service"; then
        warn "service registration failed; check 'systemctl --user status $BIN_NAME'"
        return 1
      fi
      log "systemd user service enabled: $BIN_NAME.service"
      ;;

    darwin)
      run_actual mkdir -p "$LAUNCH_AGENTS_DIR"
      if [ "$DRY_RUN" = 1 ]; then
        log "  [dry-run] would write $SERVICE_FILE"
        log "  [dry-run] would run: launchctl load -w $SERVICE_FILE"
        return 0
      fi

      cat >"$SERVICE_FILE" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${SERVICE_LABEL}</string>
    <key>ProgramArguments</key>
    <array>
        <string>${BIN_PATH}</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
        <key>Crashed</key>
        <true/>
    </dict>
    <key>StandardOutPath</key>
    <string>${CONFIG_DIR}/stdout.log</string>
    <key>StandardErrorPath</key>
    <string>${CONFIG_DIR}/stderr.log</string>
</dict>
</plist>
PLIST

      if ! launchctl load -w "$SERVICE_FILE"; then
        warn "launchctl load failed; check 'launchctl list | grep $SERVICE_LABEL'"
        return 1
      fi
      log "launchd agent loaded: $SERVICE_LABEL"
      ;;
  esac
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

if [ "$UNINSTALL" = 1 ]; then
  do_uninstall
  exit 0
fi

do_install
exit 0
