 # hermes-node  
 
A standalone Go binary that pairs a remote laptop with a Hermes Agent brain (running on a VPS) and exposes its local shell + esystem to the agent over an authenticated, encrypted WebSocket connection.  
 
he node is the *arm* in a brain‑and‑arm architecture. The brain is the server; this binary is the arm. The arm connects outbound — inbound ports required on the laptop.  
 
**Status:** v0.1.0. Protocol and architecture are stable; implementation in progress.  
 
--  
## Installation
  
### Install the binary  
  
**Mac / Linux**  
```bash  
curl -sSL https://raw.githubusercontent.com/blaspat/hermes-nodes/main/install/install.sh | sh  
```  
  
**Windows (PowerShell)**  
```powershell  
irm https://raw.githubusercontent.com/blaspat/hermes-nodes/main/install/install.ps1 | iex  
```  
  
Both installers:  
- Download the latest release binary for your OS/arch  
- Drop it in `~/.local/bin/hermes-node` (or `%LOCALAPPDATA%\\Programs\\hermes-node\\` on Windows)  
- Register it as a background service (launchd / systemd --user / Task Scheduler) so it auto‑starts on boot  
- Print next‑step instructions for pairing with your Hermes brain  
  
**No admin rights required. No Python. No npm. One static binary.**  
  
### (Dev) Build from source  
If no release exists for your platform, use `--from-source` to compile the latest `main`:  
```bash  
curl -sSL https://raw.githubusercontent.com/blaspat/hermes-nodes/main/install/install.sh | sh -s -- --from-source  
```  
  
Flags: `--no-service`, `--uninstall`, `--print-layout`, `--dry-run` – see `install.sh` for details.  
  
---  
## Pair with a Hermes brain  
  
The pairing flow is handled by the **[hermes‑nodes‑plugin](https://github.com/blaspat/hermes-nodes-plugin)** Python server on the . The binary itself only consumes the pairing token.  
  
### 1. Get a token from the VPS  
```bash  
hermes node pair --name work-laptop  
# → Pairing token for “work‑laptop”: abcdef1234567890…  
#    Run on the laptop:  
#      hermes-node pair --server wss://vps.yourdomain.com:6969 --token abcdef1234567890…  
```  
  
### 2. Run the node on the laptop  
```bash  
hermes-node pair --server wss://vps.yourdomain.com:6969 --token <TOKEN>  
# writes ~/.hermes-nodes/config.toml and connects to the brain  
```  
  
*Tip:* After a successful connection you’ll see a line like:  
`hermes-node 0.1.0: connected to wss://vps.yourdomain.com:6969 as work-laptop (5 allowed paths)`  
  
---  
## Configuration (`~/.hermes-nodes/config.toml`)  
  
```toml  
[node]  
server_url = "wss://vps.yourdomain.com:6969"   # required  
name = "work-laptop"                            # required, must match server token  
allowed_paths = ["/home/user", "/tmp"]      # required, empty = read‑only  
log_path = "/home/user/.hermes-nodes/audit.log"   # default  
  
[server]  
# TLS options – trust OS bundle by default (public CAs work out‑of‑the‑box)  
# ca_cert = "/home/user/.hermes-nodes/my-ca.pem"   # custom CA  
# pinned_cert_sha256 = "a1b2c3d4…"               # full cert pinning  
```  
  
**Security notes**  
- Token is stored in plaintext; file mode is `0600`.  
- `allowed_paths` is enforced **on the laptop** – the server cannot bypass it.  
- Default TLS uses the OS CA bundle; set `ca_cert` or `pinned_cert_sha256` only for self‑signed certs.  
  
---  
## What the node can do (v1)  
  
- ✅ Run shell commands with persistent cwd + env across calls  
- ✅ Read files (paths inside `allowed_paths` only)  
- ✅ Write/append files (paths inside `allowed_paths` only)  
- ✅ Stream stdout/stderr back to the brain in real time  
- ✅ Auto‑reconnect on brief network blips (exponential back‑off)  
- ✅ Survive reboots (background service)  
- ✅ Audit‑log every call to `audit.log`  
  
---  
## What the node cannot do (v1, by design)  
  
- ❌ Camera, screen, browser, mic, push notifications, location  
- ❌ Live file watcher / auto‑sync  
- ❌ Interactive REPLs (`vim`, `python`, etc.)  
- ❌ Multi‑server pairing (one node → one brain)  
- ❌ GUI pairing flow (text token only; v2 may add QR codes)  
- ❌ Cross‑platform state sync (laptop cwd/env is per‑laptop)  
  
---  
## Verify the node is ready  
After pairing, you should see a short “connected” line in the terminal. To double‑check:  
```bash  
hermes-node --config ~/.hermes-nodes/config.toml --version  
# prints the version string (e.g., `hermes-node 0.2.1`)  
```  
If you get a version string without errors, the node is up and listening.  
  
---  
## Troubleshooting (common pitfalls)  
  
**1. TLS / cert errors**  
- Ensure the server uses a public CA (Let’s Encrypt) or configure `ca_cert` / `pinned_cert_sha256` in `config.toml`.  
- For self‑signed certs, copy the server’s CA cert to `~/.hermes-nodes/my-ca.pem` and set `ca_cert` accordingly.  
  
**2. Permission denied on allowed_paths**  
- Verify that every path listed under `allowed_paths` exists and is readable/writable by the user running the node.  
- An empty `allowed_paths = []` leaves the node read‑only – remove the brackets to enable file access.  
  
**3. Node fails to start after reboot**  
- Confirm the background service was installed correctly (`systemctl --user status hermes-node` or launchd entry).  
- Check the audit log (`cat ~/.hermes-nodes/audit.log` for startup messages).  
  
---  
## Architecture diagram  
  
```  
┌───────────────────────┐  outbound WSS  ┌───────────────────────┐  
│ Laptop                │ ─────────────► │ VPS (Hermes brain)    │  
│ hermes‑node (Go)      │ ◄───────────── │ hermes‑nodes‑plugin   │  
│  • shell exec         │   commands     │  • Python server      │  
│  • file read/write    │   + results    │  • token auth         │  
│  • audit log          │                │  • registers as env   │  
└───────────────────────┘                └───────────────────────┘  
```  
  
**Same protocol on both sides:** see [`PROTOCOL.md`](./PROTOCOL.md).  
  
---  
## Development (for contributors)  
  
```bash  
# Build for all platforms  
./scripts/build.sh  
  
# Run unit tests  
go test ./...  
  
# Run e2e tests (requires Python test harness from hermes‑nodes‑plugin)  
go test ./tests/e2e/... -tags=e2e  
```  
  
**Requires Go 1.22+** for the development build. End users do **not** need Go installed.  

See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for the PR workflow, branch naming, and commit conventions.

---  
## Cross‑compile matrix  
  
| OS      | Arch    | Binary name                    |  
|---------|---------|--------------------------------|  
| Linux   | amd64   | `hermes-node-linux-amd64`      |  
| Linux   | arm64   | `hermes-node-linux-arm64`      |  
| macOS   | amd64   | `hermes-node-darwin-amd64`     |  
| macOS   | arm64   | `hermes-node-darwin-arm64`     |  
| Windows | amd64   | `hermes-node-windows-amd64.exe`|  
| Windows | arm64   | `hermes-node-windows-arm64.exe`|  

## Security

The pairing flow generates a one-time token that is hashed (Fernet) at rest. See [`SECURITY.md`](./SECURITY.md) for the threat model and disclosure policy.

## Related

- **[hermes-nodes-plugin](https://github.com/blaspat/hermes-nodes-plugin)** — the Hermes Agent plugin (the "brain")
- **[Hermes Agent](https://github.com/NousResearch/hermes-agent)** — the agent framework this plugs into

## License  
MIT — see [LICENSE](./LICENSE).  
