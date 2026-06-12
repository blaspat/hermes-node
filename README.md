# hermes-node
> Standalone Go binary that pairs a remote laptop with a Hermes Agent brain over WSS and exposes the laptop's shell + filesystem to the agent over an authenticated, encrypted WebSocket connection. The node is the *arm* in a brain-and-arm architecture — it connects outbound, so no inbound ports required on the laptop.

**Status:** v0.1.0. Protocol and architecture are stable; implementation in progress.

## Table of Contents
- [Prerequisites](#prerequisites)
- [Installation](#installation)
- [Core Features](#core-features)
- [Usage](#usage)
- [Architecture](#architecture)
- [Contributing](#contributing)
- [FAQ](#faq)
- [Related](#related)

## Prerequisites
- **End users:** nothing — the binary is static and self-contained. No Python, no Node, no runtime.
- **Development contributors:** Go 1.22+, git, bash.

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

If no release exists for your platform yet:

```bash
curl -sSL https://raw.githubusercontent.com/blaspat/hermes-nodes/main/install/install.sh | sh -s -- --from-source
```

Requires Go 1.22+ and git. Flags: `--no-service`, `--uninstall`, `--print-layout`, `--dry-run`.

### Binary matrix

The build script (`./scripts/build.sh`) cross-compiles for these targets:

| OS      | Arch   | Binary name                         |
|---------|--------|-------------------------------------|
| Linux   | amd64  | `hermes-node-linux-amd64`           |
| Linux   | arm64  | `hermes-node-linux-arm64`           |
| macOS   | amd64  | `hermes-node-darwin-amd64`          |
| macOS   | arm64  | `hermes-node-darwin-arm64`          |
| Windows | amd64  | `hermes-node-windows-amd64.exe`     |
| Windows | arm64  | `hermes-node-windows-arm64.exe`     |

## Core Features
- **Remote shell execution** — persistent `bash` session with preserved cwd and env across calls. Commands run as the user who started the node.
- **Remote file read/write** — read and write files through a WSS tunnel, with per-path allowlisting enforced on the laptop.
- **Real-time streaming** — stdout and stderr stream back to the brain in real time with a 10 MB per-stream cap.
- **Auto-reconnect** — exponential backoff (1s → 60s max) on network drops. Survives reboots as a background service.
- **Full audit log** — every call is recorded in append-only JSONL with automatic rotation at 50 MB (keeps 5 files).
- **TLS 1.3 required** — public CAs work out of the box; custom CA and cert pinning supported for self-signed deployments.
- **Deny-by-default security** — empty `allowed_paths` rejects all paths. The allowlist is enforced on the laptop — the server cannot bypass it.

## Usage

### 1. Get a pairing token from the VPS

```bash
hermes node pair --name work-laptop
```

The server prints a pairing token. Copy it.

### 2. Pair the laptop

```bash
hermes-node pair --server wss://vps.yourdomain.com:6969 --token <TOKEN>
```

Writes `~/.hermes-nodes/config.toml` (mode 0600) and prints next-step instructions.

### 3. Edit the config

```toml
[node]
server_url = "wss://vps.yourdomain.com:6969"
name = "work-laptop"                    # must match the server's token binding
allowed_paths = ["/home/user", "/tmp"]  # filesystem roots the agent can touch
log_path = "/home/user/.hermes-nodes/audit.log"

[server]
# ca_cert = "/home/user/.hermes-nodes/my-ca.pem"   # custom CA for self-signed certs
# pinned_cert_sha256 = "a1b2c3d4..."               # leaf cert pin
```

**Security notes:**
- Token is stored in plaintext; file mode is `0600`.
- `allowed_paths` is enforced **on the laptop** — the server cannot bypass it.
- Default TLS uses the OS CA bundle; set `ca_cert` or `pinned_cert_sha256` only for self-signed certs.

### 4. Start the node

```bash
hermes-node run
```

You should see a line like:
```
hermes-node 0.1.0: connected to wss://vps.yourdomain.com:6969 as work-laptop (5 allowed paths)
```

### What it can do (v1)

- ✅ Run shell commands with persistent cwd + env across calls
- ✅ Read files — paths inside `allowed_paths` only
- ✅ Write/append files — paths inside `allowed_paths` only
- ✅ Stream stdout/stderr back to the brain in real time
- ✅ Auto-reconnect on brief network blips (exponential backoff)
- ✅ Survive reboots (background service)
- ✅ Audit-log every call to `audit.log`

### What it cannot do (v1, by design)

- ❌ Camera, screen, browser, mic, push notifications, location
- ❌ Live file watcher / auto-sync
- ❌ Interactive REPLs (`vim`, `python`, etc.)
- ❌ Multi-server pairing (one node → one brain)
- ❌ GUI pairing flow (text token only)
- ❌ Cross-platform state sync (cwd/env is per-laptop)

### Troubleshooting

**TLS / cert errors** — Ensure the server uses a public CA (Let's Encrypt) or configure `ca_cert` / `pinned_cert_sha256` in `config.toml`.

**Permission denied on `allowed_paths`** — Verify every path in `allowed_paths` exists and is readable/writable by the user running the node. An empty list (`allowed_paths = []`) leaves the node read-only.

**Node fails to start after reboot** — Confirm the service was installed (`systemctl --user status hermes-node` or launchd entry). Check the audit log (`cat ~/.hermes-nodes/audit.log`).

## Architecture

```
┌───────────────────────┐  outbound WSS  ┌───────────────────────┐
│ Laptop                │ ──────────────►│ VPS (Hermes brain)    │
│ hermes-node (Go)      │ ◄────────────── │ hermes-nodes-plugin   │
│  • shell exec         │   commands     │  • Python server      │
│  • file read/write    │   + results    │  • token auth         │
│  • audit log          │                │  • registers as env   │
└───────────────────────┘                └───────────────────────┘
```

Same protocol on both sides — see [`PROTOCOL.md`](./PROTOCOL.md).

## Contributing

See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for the full guidelines.

Quick summary:
- **Go 1.22+** required for development. End users do not need Go.
- Run `go test ./...` and `go test -race ./...` before opening a PR.
- Run `gofmt -l .` — must produce no output.
- Commit format: `<type>(<scope>): <imperative summary>` — e.g. `fix(wire): audit handler panics before attempting wire response`.
- Reference the issue / discussion in the PR body.
- Wire-format changes must update `PROTOCOL.md` in the same commit.

## FAQ

- **Q: Does the node require Hermes Agent on the laptop?** A: No. The binary is independent — it will continue to function even if all AI agent software is removed from the machine.
- **Q: Can the server bypass the path allowlist?** A: No. The allowlist is enforced on the laptop. The server never sees its contents.
- **Q: What happens if the laptop is stolen?** A: The token is in `~/.hermes-nodes/config.toml` (mode 0600). Revoke the token on the server with `hermes node revoke --name <name>` — revocation is immediate.
- **Q: Will v2 add keychain support?** A: Yes. OS keychain token storage is a roadmap item for v2.
- **Q: How do I uninstall?** A: Run `hermes-node revoke`, delete the binary from `~/.local/bin/`, and remove `~/.hermes-nodes/`. See [SECURITY-REVIEW.md](./SECURITY-REVIEW.md) for per-platform details.

## Related

- **[hermes-nodes-plugin](https://github.com/blaspat/hermes-nodes-plugin)** — the Hermes Agent plugin (the "brain" server side)
- **[Hermes Agent](https://github.com/NousResearch/hermes-agent)** — the agent framework this plugs into
- **[PROTOCOL.md](./PROTOCOL.md)** — the wire protocol contract between node and brain
- **[SECURITY-REVIEW.md](./SECURITY-REVIEW.md)** — threat model and disclosure policy for the node

---

License: [MIT](./LICENSE) | Author: [Blasius Patrick](https://github.com/blaspat)
