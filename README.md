# hermes-node

> Standalone Go binary that pairs a remote laptop with a Hermes Agent brain over WSS and exposes the laptop's shell + filesystem to the agent over an authenticated, encrypted WebSocket connection. The node is the *arm* in a brain-and-arm architecture — it connects outbound, so no inbound ports required on the laptop.

**Status:** v0.2.0. Protocol and architecture are stable.

## Table of Contents
- [Quick Start](#quick-start)
- [Installation](#installation)
- [Subcommands](#subcommands)
- [Configuration](#configuration)
- [Architecture](#architecture)
- [Security](#security)
- [Troubleshooting](#troubleshooting)
- [Contributing](#contributing)
- [FAQ](#faq)

## Quick Start

```bash
# Install the binary
curl -sSL https://raw.githubusercontent.com/blaspat/hermes-nodes/main/install/install.sh | sh

# Pair with your Hermes brain
hermes-node pair --server wss://vps.yourdomain.com:7000 --token <TOKEN> --name work-laptop

# Edit config to set allowed paths
#   ~/.hermes-nodes/config.toml

# Start the daemon
hermes-node run
```

## Installation

### Install the binary

```bash
# macOS / Linux
curl -sSL https://raw.githubusercontent.com/blaspat/hermes-nodes/main/install/install.sh | sh

# Windows (PowerShell)
irm https://raw.githubusercontent.com/blaspat/hermes-nodes/main/install/install.ps1 | iex
```

Both installers:
- Download the latest release binary for your OS/arch
- Drop it in `~/.local/bin/hermes-node` (or `%LOCALAPPDATA%\Programs\hermes-node\` on Windows)
- Register it as a background service (launchd / systemd --user / Task Scheduler)
- Print next-step instructions for pairing with your Hermes brain

**No admin rights required. One static binary.**

### Build from source

```bash
git clone https://github.com/blaspat/hermes-nodes.git
cd hermes-nodes
go build -o hermes-node ./cmd/hermes-node
```

Requires Go 1.22+.

### Binary matrix

| OS      | Arch   | Binary name                     | Status         |
|---------|--------|---------------------------------|----------------|
| Linux   | amd64  | `hermes-node-linux-amd64`       | ✅ Confirmed   |
| Linux   | arm64  | `hermes-node-linux-arm64`       | 🏗️ Build only |
| macOS   | amd64  | `hermes-node-darwin-amd64`      | ✅ Confirmed   |
| macOS   | arm64  | `hermes-node-darwin-arm64`      | ✅ Confirmed   |
| Windows | amd64  | `hermes-node-windows-amd64.exe` | 🏗️ Build only |
| Windows | arm64  | `hermes-node-windows-arm64.exe` | 🏗️ Build only |

> **🏗️ Build only** — cross-compiles successfully but not end-to-end tested. Linux amd64 and macOS amd64 are the confirmed platforms. Windows requires WSL or Git Bash for the shell executor.

## Subcommands

### `hermes-node pair`

Writes a fresh `config.toml` with the server URL, node name, and pairing token. The file is created with mode 0600. The operator edits it after pairing to add `allowed_paths`.

```bash
hermes-node pair --server wss://vps.example.com:7000 --token <TOKEN> --name <name> [--config <path>]
```

### `hermes-node run`

Long-lived background service. Loads the config, opens the audit log, connects to the server, and stays connected across network drops via exponential backoff.

```bash
hermes-node run [--config <path>]
```

**SIGHUP reload:** Send `SIGHUP` to the daemon process to reload `log_level` at runtime without restarting:
```bash
kill -HUP <pid>
```

The daemon re-reads `config.toml` and applies the new `log_level`. Other changes (`allowed_paths`, `log_path`, `server_url`) still require a full restart.

**Exit codes:** The daemon exits 0 on clean shutdown, 1 on fatal error.

### `hermes-node status`

Reads the daemon's status file and displays connection state, session ID, uptime, and last error. No server connection required.

```bash
hermes-node status
```

Output example:
```
hermes-node dev go1.26.3 a1b2c3d4 2026-06-22
  PID:       12345
  State:     connected
  Name:      work-laptop
  Server:    wss://vps.example.com:7000
  Session:   abc-def-123
  Started:   2026-06-22T21:00:00Z
  Connected: 2026-06-22T21:05:00Z
```

If the daemon has never been started:
```
hermes-node: daemon status file not found — node has never been started.
```

### `hermes-node update`

Self-updates the binary from GitHub Releases. Downloads the latest release for your OS/arch, verifies it, and replaces the running binary (Unix only — Windows prints manual instructions).

```bash
hermes-node update                          # latest release with confirmation
hermes-node update --yes                    # skip confirmation prompt
hermes-node update --version v0.2.0         # pin to a specific version
hermes-node update --restart-service        # also restart systemd/launchd
```

The downloaded binary is verified by running `--version` before replacement. On Linux, `os.Rename` replaces the binary atomically. If the temp and target directories are on different filesystems, you'll get instructions to copy manually.

### `hermes-node uninstall`

Removes the binary, stops and deregisters the background service, and optionally removes the config directory.

```bash
hermes-node uninstall                        # remove binary + service, keep config
hermes-node uninstall --purge               # also remove ~/.hermes-nodes/
hermes-node uninstall --dry-run             # preview without making changes
```

**Per-platform service removal:**
- **Linux:** `systemctl --user disable --now hermes-node.service`, then deletes `~/.config/systemd/user/hermes-node.service`
- **macOS:** `launchctl unload`, then deletes `~/Library/LaunchAgents/com.blaspat.hermes-node.plist`
- **Windows:** Use `install.ps1 --Uninstall` (Task Scheduler removal)

The config directory (`~/.hermes-nodes/`) is left in place by default. Pass `--purge` to also remove all config, tokens, and audit logs.

### `hermes-node validate`

Validates `config.toml` without connecting to the server.

```bash
hermes-node validate [--config <path>]
```

Checks performed:
- TOML syntax and required fields (`server_url`, `name`, `token`)
- `allowed_paths` exist and are directories
- Log path directory is writable
- TLS `ca_cert` file exists (if set) and `pinned_cert_sha256` is valid hex

**Exit codes:**
- `0` — all checks passed
- `1` — one or more validation failures
- `2` — flag/argument error or config file not found

### `hermes-node --version`

Displays version and build metadata:

```
$ hermes-node --version
hermes-node dev go1.26.3 a1b2c3d4 2026-06-22
```

Format: `hermes-node <version> <go-version> <commit-sha>[-dirty] <date>`. When built from a git repo (the normal case), the commit SHA and date are embedded automatically by `debug.ReadBuildInfo()`. A bare `hermes-node dev` indicates the binary was built without VCS info (e.g., `go run`).

## Configuration

The config file is written by `hermes-node pair` and edited manually by the operator. Default location: `~/.hermes-nodes/config.toml`.

### `[node]` section

```toml
[node]
# Required — set by `hermes-node pair`
server_url = "wss://vps.example.com:7000"
name = "work-laptop"
token = "abc123..."

# Filesystem roots the agent can touch (deny-by-default)
# Empty list rejects all paths. Each entry must be an
# existing directory.
allowed_paths = ["/home/user", "/tmp"]

# Audit log location (default: ~/.hermes-nodes/audit.log)
log_path = "/home/user/.hermes-nodes/audit.log"

# Log level (default: "info")
# One of: debug, info, warn, error
log_level = "debug"

# Reconnect backoff (defaults shown)
backoff_initial = "1s"      # default; Go duration, e.g. "500ms", "5s"
backoff_max = "60s"         # default; maximum delay between retries
backoff_factor = 2.0        # default; multiplier per retry
```

### `[server]` section

```toml
[server]
# Custom CA bundle for self-signed certs (optional)
ca_cert = "/home/user/.hermes-nodes/my-ca.pem"

# Leaf certificate SHA-256 pin (optional, hex-encoded 64 chars)
pinned_cert_sha256 = "a1b2c3d4e5f6..."
```

### Hot-reloadable fields (SIGHUP)

| Field | Reloadable? | Since |
|-------|------------|-------|
| `log_level` | ✅ Yes | v0.2.0 |
| `allowed_paths` | ❌ Requires restart | — |
| `log_path` | ❌ Requires restart | — |
| `server_url` / `name` / `token` | ❌ Requires restart | — |
| Backoff settings | ❌ Requires restart | — |

## Architecture

```
┌───────────────────────┐  outbound WSS  ┌───────────────────────┐
│ Laptop                │ ──────────────►│ VPS (Hermes brain)    │
│ hermes-node (Go)      │ ◄────────────── │ hermes-nodes-plugin   │
│  • shell exec         │   commands     │  • Python server      │
│  • file read/write    │   + results    │  • token auth         │
│  • audit log          │                │  • registers as env   │
│  • auto-reconnect     │                │                       │
└───────────────────────┘                └───────────────────────┘
```

Same protocol on both sides — see [`PROTOCOL.md`](./PROTOCOL.md).

### Core features
- **Remote shell execution** — persistent `bash` session with preserved cwd and env across calls. Stderr is captured alongside stdout.
- **Remote file read/write** — read and write files through a WSS tunnel, with per-path allowlisting enforced on the laptop.
- **Real-time streaming** — stdout and stderr stream back to the brain in real time with a 10 MB per-stream cap.
- **Auto-reconnect** — exponential backoff (configurable via `config.toml`). Survives reboots as a background service.
- **Full audit log** — every call is recorded in append-only JSONL with automatic rotation at 50 MB (keeps 5 files).
- **TLS 1.3 required** — public CAs work out of the box; custom CA and cert pinning supported for self-signed deployments.
- **Deny-by-default security** — empty `allowed_paths` rejects all paths. The allowlist is enforced on the laptop — the server cannot bypass it.

### What it cannot do (v0.2, by design)
- No camera, screen, browser, mic, push notifications, or location
- No live file watcher / auto-sync
- No interactive REPLs (`vim`, `python`, etc.)
- No multi-server pairing (one node → one brain)
- No GUI pairing flow (text token only)
- No cross-platform state sync (cwd/env is per-laptop)

## Security

- **Token** is stored in plaintext in `config.toml` (mode 0600). Revoke it on the server with `hermes node revoke --name <name>` — revocation is immediate.
- **Path allowlist** is enforced on the laptop. Each path is symlink-resolved before the check, so symlinks escaping the allowlist are rejected.
- **TLS 1.3** required for all connections. Custom CA and cert pinning are supported.
- **Audit log** every call with action, target, duration, exit code, and status.

## Troubleshooting

**TLS / cert errors** — Ensure the server uses a public CA (Let's Encrypt) or configure `ca_cert` / `pinned_cert_sha256` in `config.toml`.

**Permission denied on `allowed_paths`** — Verify every path in `allowed_paths` exists and is readable/writable by the user running the node. An empty list (`allowed_paths = []`) leaves the node read-only.

**Node fails to start after reboot** — Confirm the service was installed (`systemctl --user status hermes-node` or launchd entry). Check the audit log (`cat ~/.hermes-nodes/audit.log`).

**Stderr not captured** — Since v0.2.0, stderr is captured from the shell executor. If you see unexpected output, check if the command is producing stderr (file not found, permission errors, etc.).

**Config file not found** — Ensure `~/.hermes-nodes/config.toml` exists. Run `hermes-node pair` to create it, or pass `--config <path>` to every subcommand.

**Daemon not responding to status** — Run `hermes-node status` to check if the daemon is running. If the status file is stale (PID not running), the `(not running)` indicator will appear.

## Contributing

See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for the full guidelines.

Quick summary:
- **Go 1.22+** required for development. End users do not need Go.
- Run `go test ./...` and `go test -race ./...` before opening a PR.
- Run `gofmt -l .` — must produce no output.
- Commit format: `<type>(<scope>): <imperative summary>` — e.g. `feat(cli): add hermes-node status subcommand`.
- Reference the issue / discussion in the PR body.
- Wire-format changes must update `PROTOCOL.md` in the same commit.

## FAQ

- **Q: Does the node require Hermes Agent on the laptop?** A: No. The binary is independent — it will continue to function even if all AI agent software is removed from the machine.

- **Q: Can the server bypass the path allowlist?** A: No. The allowlist is enforced on the laptop. The server never sees its contents.

- **Q: What happens if the laptop is stolen?** A: The token is in `~/.hermes-nodes/config.toml` (mode 0600). Revoke the token on the server with `hermes node revoke --name <name>` — revocation is immediate.

- **Q: Will v2 add keychain support?** A: Yes. OS keychain token storage is a roadmap item for v2.

- **Q: How do I uninstall?** A: Run `hermes-node uninstall` to remove the binary and service. Add `--purge` to also remove `~/.hermes-nodes/`. On Windows, use `install.ps1 --Uninstall`.

- **Q: How do I update?** A: Run `hermes-node update` to self-update from GitHub Releases. Pass `--version <tag>` to pin a specific release.

- **Q: How do I check if the daemon is running?** A: Run `hermes-node status`. It reads the status file written by the daemon and shows connection state, PID, session ID, and uptime.

- **Q: Can I reload config without restarting?** A: Send `SIGHUP` to the daemon process to reload `log_level`. Other changes require a restart.

- **Q: What does `--version` show?** A: The version, Go version, commit SHA, and build date. Example: `hermes-node v0.2.0 go1.26.3 abc12345 2026-06-22`.

## Related

- **[hermes-nodes-plugin](https://github.com/blaspat/hermes-nodes-plugin)** — the Hermes Agent plugin (the "brain" server side)
- **[Hermes Agent](https://github.com/NousResearch/hermes-agent)** — the agent framework this plugs into
- **[PROTOCOL.md](./PROTOCOL.md)** — the wire protocol contract between node and brain
- **[SECURITY-REVIEW.md](./SECURITY-REVIEW.md)** — threat model and disclosure policy for the node

---

License: [MIT](./LICENSE) | Author: [Blasius Patrick](https://github.com/blaspat)
