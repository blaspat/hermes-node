# hermes-nodes

A standalone Go binary that pairs with a [Hermes Agent](https://github.com/NousResearch/hermes-agent) brain (running on a VPS) and exposes its local shell + filesystem to the agent over an authenticated, encrypted WebSocket connection.

The node is the "arm" in a brain-and-arm architecture. The brain is the server; this binary is the arm. The arm connects outbound — no inbound ports required on the laptop.

> **Status:** pre-v0.1.0. Protocol and architecture are stable; implementation in progress.

## Install

### Mac / Linux

```bash
curl -sSL https://raw.githubusercontent.com/blaspat/hermes-nodes/main/install/install.sh | sh
```

### Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/blaspat/hermes-nodes/main/install/install.ps1 | iex
```

Both installers:
- Download the latest release binary for your OS/arch
- Drop it in `~/.local/bin/hermes-node` (or `%LOCALAPPDATA%\Programs\hermes-node\` on Windows, and added to the user PATH)
- Set up a user-level background service (launchd / systemd --user / Task Scheduler) so the node auto-starts on boot
- Print next-step instructions for pairing with your Hermes brain

**No admin rights required. No Python. No npm. One static binary.**

## Pair with a Hermes brain

After installing, you need a token from the server. On your VPS:

```bash
hermes node pair --name work-laptop
# prints: Pairing token for "work-laptop":
#         abcdef1234567890...
#         Run on the laptop:
#           hermes-node pair --server wss://vps.yourdomain.com:6969 --token abcdef1234567890...
```

Then on the laptop:

```bash
hermes-node pair --server wss://vps.yourdomain.com:6969 --token abcdef1234567890...
# writes ~/.hermes-nodes/config.toml
# connects, authenticates, and goes idle in the background
```

That's the whole flow. The agent (or any Hermes brain) can now run shell commands and read/write files on this laptop.

## Configuration

`~/.hermes-nodes/config.toml`:

```toml
[node]
server_url = "wss://vps.yourdomain.com:6969"   # required
name = "work-laptop"                            # required, must match server token
allowed_paths = ["/Users/patrick", "/tmp"]      # required, empty list = read-only filesystem
log_path = "/Users/patrick/.hermes-nodes/audit.log"   # default

[server]
# TLS verification: by default the node uses the OS CA bundle, so
# Let's Encrypt and other public CAs just work. You only need to set
# one of the options below for self-signed certs or extra hardening.
#
# Option 1: trust a custom CA (homelab, internal CA, self-signed dev cert)
# ca_cert = "/Users/patrick/.hermes-nodes/my-ca.pem"
#
# Option 2: full cert pinning — node refuses to connect if the cert changes
# pinned_cert_sha256 = "a1b2c3d4..."
```

**Security notes:**
- The token is stored in plaintext in this file. File mode is `0600`.
- `allowed_paths` is enforced **on the laptop**. The server cannot bypass it.
- If `allowed_paths` is empty, all filesystem operations are denied (exec still works).
- **Default TLS trust** uses the OS CA bundle. If your server uses a public CA (e.g. Let's Encrypt), no extra config needed.

## What the node can do (v1)

- ✅ Run shell commands with persistent cwd + env across calls
- ✅ Read files (paths inside `allowed_paths` only)
- ✅ Write/append files (paths inside `allowed_paths` only)
- ✅ Stream stdout/stderr back to the brain in real time
- ✅ Survive brief network blips (auto-reconnect with exponential backoff)
- ✅ Survive reboots (background service on the OS)
- ✅ Audit log every call to `audit.log`

## What the node cannot do (v1, by design)

- ❌ Camera, screen, browser, mic, push notifications, location
- ❌ Live file watcher / auto-sync
- ❌ Interactive REPLs (no `vim` over WSS, no `python` REPL)
- ❌ Multi-server pairing (one node pairs to one brain)
- ❌ GUI pairing flow (text token only; v2 may add QR codes)
- ❌ Cross-platform sync of state (laptop cwd/env is per-laptop)

## Architecture

```
┌──────────────────────────────┐         outbound WSS         ┌──────────────────────────────┐
│ Company Laptop               │ ────────────────────────────► │ VPS (Hermes brain)           │
│                              │   (laptop → VPS, no inbound) │                              │
│  hermes-node (Go binary)     │ ◄──────────────────────────── │  hermes-nodes-plugin         │
│  - shell exec                │   commands + results         │  (Python, pip-installed)     │
│  - file read/write           │                               │  - node_server.py            │
│  - cwd persistence           │                               │  - registers as a Hermes env  │
│  - audit log                 │                               │  - the agent calls via tools  │
└──────────────────────────────┘                               └──────────────────────────────┘
```

**Same protocol on both sides:** see [`PROTOCOL.md`](./PROTOCOL.md).

## Development

```bash
# Build for all platforms
./scripts/build.sh

# Run unit tests
go test ./...

# Run e2e tests (requires Python test harness from hermes-nodes-plugin)
go test ./tests/e2e/... -tags=e2e
```

**Requires Go 1.22+** for the development build. End users do not need Go installed — they get a pre-compiled binary.

## Cross-compile matrix

| OS      | Arch    | Binary name                       |
|---------|---------|-----------------------------------|
| Linux   | amd64   | `hermes-node-linux-amd64`         |
| Linux   | arm64   | `hermes-node-linux-arm64`         |
| macOS   | amd64   | `hermes-node-darwin-amd64`        |
| macOS   | arm64   | `hermes-node-darwin-arm64`        |
| Windows | amd64   | `hermes-node-windows-amd64.exe`   |
| Windows | arm64   | `hermes-node-windows-arm64.exe`   |

## Security

See [`SECURITY.md`](./SECURITY.md) for the threat model, the audit-log format, and what's enforced where.

See the **server-side** [`SECURITY-REVIEW.md`](https://github.com/blaspat/hermes-nodes-plugin/blob/main/SECURITY-REVIEW.md) for the document a corporate security team would actually want to read before approving installation on a company laptop.

## Related

- **[hermes-nodes-plugin](https://github.com/blaspat/hermes-nodes-plugin)** — the Python server-side plugin that runs on the VPS
- **[Hermes Agent](https://github.com/NousResearch/hermes-agent)** — the agent framework this plugs into

## License

MIT — see [LICENSE](./LICENSE).
