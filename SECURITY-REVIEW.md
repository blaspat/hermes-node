# hermes-nodes: Security Model

See [`SECURITY.md`](https://github.com/blaspat/hermes-nodes-plugin/blob/main/SECURITY.md) in the server-side plugin repo for the full threat model. This is a one-page summary of what's enforced on the **node** (laptop) side, written for a corporate security team.

---

## What runs on the laptop

A single Go binary called `hermes-node`. It is:

- **One static file** (no installer, no DLLs, no registry entries outside the user's profile)
- **Open source** — full source visible in this repository
- **~1000 lines of Go** — reviewable in an afternoon
- **Cross-compiled** for macOS (Intel + Apple Silicon), Linux, Windows

The binary lives at:
- macOS / Linux: `~/.local/bin/hermes-node`
- Windows: `%LOCALAPPDATA%\hermes-nodes\hermes-node.exe`

Configuration lives at `~/.hermes-nodes/config.toml`. Logs at `~/.hermes-nodes/audit.log`.

## What it does

- Opens an **outbound** WebSocket to a server URL you specify (default port 8443)
- Authenticates with a pre-shared token
- On request from the server: runs shell commands, reads files, writes files
- Writes an audit log entry for every call

## What it does NOT do

The node binary does **none** of these things:

- ❌ Access the camera, microphone, screen, or location
- ❌ Capture keystrokes or clipboard
- ❌ Open any inbound network ports
- ❌ Modify system settings, install software, or change firewall rules
- ❌ Read files outside the operator-configured `allowed_paths` list
- ❌ Write to disk outside `~/.hermes-nodes/`
- ❌ Load or execute dynamic code
- ❌ Phone home, check for updates, or send telemetry of any kind
- ❌ Run as root or with elevated privileges

The binary is hermes-independent — it does not require Hermes Agent to be installed or running on the laptop. It will continue to function even if all other AI agent software is removed from the machine.

## What the server can and cannot do to the laptop

The server can:

- ✅ Run shell commands inside the persistent shell session, as the user who started the node
- ✅ Read any file under `allowed_paths`
- ✅ Write or append to any file under `allowed_paths`

The server **cannot**:

- ❌ Read or write files outside `allowed_paths` (enforced laptop-side, not server-side)
- ❌ Run commands as root (the node runs with the same privileges as the user who started it)
- ❌ Bypass the allowlist through symlinks (paths are canonicalized before checking)
- ❌ Exfiltrate data without an audit log entry (every `read` is logged)
- ❌ Cause unbounded output (10 MB cap per `exec` stream)
- ❌ Cause unbounded CPU (60s default timeout per `exec`)

## Network behavior

- **Outbound only.** The node dials the server; it never accepts inbound connections. The laptop's firewall does not need to be modified.
- **TLS 1.3** required. Self-signed certs require explicit operator action.
- **Heartbeat every 30 seconds.** Dropped connections are detected within 60 seconds; the node then retries with exponential backoff (max 1 attempt/minute).
- **No DNS lookups** beyond the initial server URL resolution.

## Token security

- Token format: 32 random bytes, base64url-encoded (~256 bits of entropy)
- Stored in plaintext in `~/.hermes-nodes/config.toml` with file mode `0600`
- **v1 limitation:** the token is not in the OS keychain. Roadmap item for v2.

If the laptop is stolen while the node is running, the attacker has the token and can impersonate the node until the token is revoked on the server (`hermes node revoke --name <name>`). Revocation is immediate.

## Audit log

Every call is logged to `~/.hermes-nodes/audit.log` in JSONL format. Fields:

- `ts` (UTC timestamp)
- `action` (`exec`, `read`, `write`, `auth`, `revoke`)
- `request_id` (joinable to the server-side log)
- `duration_ms`
- `exit_code` (for `exec`)
- `status` (`ok`, `error`, `timeout`, `denied`)
- `command_summary` (first 200 chars, for `exec`)

The log is append-only. There is no built-in way to delete entries. The log rotates at 50 MB (keeps last 5 files).

## How to uninstall

```bash
# Stop the service
hermes-node revoke

# Remove the binary
rm ~/.local/bin/hermes-node          # macOS/Linux
rm %LOCALAPPDATA%\hermes-nodes\hermes-node.exe   # Windows

# Remove config and logs (optional)
rm -rf ~/.hermes-nodes
```

On macOS, also unload the launchd service:
```bash
launchctl unload ~/Library/LaunchAgents/com.blaspat.hermes-node.plist
rm ~/Library/LaunchAgents/com.blaspat.hermes-node.plist
```

On Windows, the Task Scheduler entry is named `HermesNode`. Delete it via `taskschd.msc` or `Unregister-ScheduledTask -TaskName HermesNode`.

## Source and provenance

- Source: <https://github.com/blaspat/hermes-nodes>
- License: MIT
- Build process: documented in the repo (`scripts/build.sh`)
- Releases: GitHub Releases page; binaries are reproducible from a clean checkout with Go 1.22+

## Reporting a vulnerability

File an issue on the GitHub repo or contact the maintainer directly. Please don't disclose publicly until we've had a chance to fix.
