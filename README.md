1|# hermes-node  
2|  
3|A standalone Go binary that pairs a remote laptop with a Hermes Agent brain (running on a VPS) and exposes its local shell + filesystem to the agent over an authenticated, encrypted WebSocket connection.  
4|  
5|The node is the *arm* in a brain‑and‑arm architecture. The brain is the server; this binary is the arm. The arm connects outbound — no inbound ports required on the laptop.  
6|  
7|> **Status:** pre‑v0.1.0. Protocol and architecture are stable; implementation in progress.  
8|  
9|---  
10|## Install – User‑Facing Workflow  
11|  
12|### 1️⃣ Install the binary  
13|  
14|**Mac / Linux**  
15|```bash  
16|curl -sSL https://raw.githubusercontent.com/blaspat/hermes-nodes/main/install/install.sh | sh  
17|```  
18|  
19|**Windows (PowerShell)**  
20|```powershell  
21|irm https://raw.githubusercontent.com/blaspat/hermes-nodes/main/install/install.ps1 | iex  
22|```  
23|  
24|Both installers:  
25|- Download the latest release binary for your OS/arch  
26|- Drop it in `~/.local/bin/hermes-node` (or `%LOCALAPPDATA%\\Programs\\hermes-node\\` on Windows)  
27|- Register it as a background service (launchd / systemd --user / Task Scheduler) so it auto‑starts on boot  
28|- Print next‑step instructions for pairing with your Hermes brain  
29|  
30|**No admin rights required. No Python. No npm. One static binary.**  
31|  
32|### 1️⃣ (Dev) Build from source  
33|If no release exists for your platform, use `--from-source` to compile the latest `main`:  
34|```bash  
35|curl -sSL https://raw.githubusercontent.com/blaspat/hermes-nodes/main/install/install.sh | sh -s -- --from-source  
36|```  
37|  
38|Flags: `--no-service`, `--uninstall`, `--print-layout`, `--dry-run` – see `install.sh` for details.  
39|  
40|---  
41|## Pair with a Hermes brain  
42|  
43|The pairing flow is handled by the **[hermes‑nodes‑plugin](https://github.com/blaspat/hermes-nodes-plugin)** Python server on the VPS. The binary itself only consumes the pairing token.  
44|  
45|### 1️⃣ Get a token from the VPS  
46|```bash  
47|hermes node pair --name work-laptop  
48|# → Pairing token for “work‑laptop”: abcdef1234567890…  
49|#    Run on the laptop:  
50|#      hermes-node pair --server wss://vps.yourdomain.com:6969 --token abcdef1234567890…  
51|```  
52|  
53|### 2️⃣ Run the node on the laptop  
54|```bash  
55|hermes-node pair --server wss://vps.yourdomain.com:6969 --token <TOKEN>  
56|# writes ~/.hermes-nodes/config.toml and connects to the brain  
57|```  
58|  
59|*Tip:* After a successful connection you’ll see a line like:  
60|`hermes-node 0.2.1: connected to wss://vps.yourdomain.com:6969 as work-laptop (5 allowed paths)`  
61|  
62|---  
63|## Configuration (`~/.hermes-nodes/config.toml`)  
64|  
65|```toml  
66|[node]  
67|server_url = "wss://vps.yourdomain.com:6969"   # required  
68|name = "work-laptop"                            # required, must match server token  
69|allowed_paths = ["/home/patrick", "/tmp"]      # required, empty = read‑only  
70|log_path = "/home/patrick/.hermes-nodes/audit.log"   # default  
71|  
72|[server]  
73|# TLS options – trust OS bundle by default (public CAs work out‑of‑the‑box)  
74|# ca_cert = "/home/patrick/.hermes-nodes/my-ca.pem"   # custom CA  
75|# pinned_cert_sha256 = "a1b2c3d4…"               # full cert pinning  
76|```  
77|  
78|**Security notes**  
79|- Token is stored in plaintext; file mode is `0600`.  
80|- `allowed_paths` is enforced **on the laptop** – the server cannot bypass it.  
81|- Default TLS uses the OS CA bundle; set `ca_cert` or `pinned_cert_sha256` only for self‑signed certs.  
82|  
83|---  
84|## What the node can do (v1)  
85|  
86|- ✅ Run shell commands with persistent cwd + env across calls  
87|- ✅ Read files (paths inside `allowed_paths` only)  
88|- ✅ Write/append files (paths inside `allowed_paths` only)  
89|- ✅ Stream stdout/stderr back to the brain in real time  
90|- ✅ Auto‑reconnect on brief network blips (exponential back‑off)  
91|- ✅ Survive reboots (background service)  
92|- ✅ Audit‑log every call to `audit.log`  
93|  
94|---  
95|## What the node cannot do (v1, by design)  
96|  
97|- ❌ Camera, screen, browser, mic, push notifications, location  
98|- ❌ Live file watcher / auto‑sync  
99|- ❌ Interactive REPLs (`vim`, `python`, etc.)  
100|- ❌ Multi‑server pairing (one node → one brain)  
101|- ❌ GUI pairing flow (text token only; v2 may add QR codes)  
102|- ❌ Cross‑platform state sync (laptop cwd/env is per‑laptop)  
103|  
104|---  
105|## Verify the node is ready  
106|After pairing, you should see a short “connected” line in the terminal. To double‑check:  
107|```bash  
108|hermes-node --config ~/.hermes-nodes/config.toml --version  
109|# prints the version string (e.g., `hermes-node 0.2.1`)  
110|```  
111|If you get a version string without errors, the node is up and listening.  
112|  
113|---  
114|## Troubleshooting (common pitfalls)  
115|  
116|**1. TLS / cert errors**  
117|- Ensure the server uses a public CA (Let’s Encrypt) or configure `ca_cert` / `pinned_cert_sha256` in `config.toml`.  
118|- For self‑signed certs, copy the server’s CA cert to `~/.hermes-nodes/my-ca.pem` and set `ca_cert` accordingly.  
119|  
120|**2. Permission denied on allowed_paths**  
121|- Verify that every path listed under `allowed_paths` exists and is readable/writable by the user running the node.  
122|- An empty `allowed_paths = []` leaves the node read‑only – remove the brackets to enable file access.  
123|  
124|**3. Node fails to start after reboot**  
125|- Confirm the background service was installed correctly (`systemctl --user status hermes-node` or launchd entry).  
126|- Check the audit log (`cat ~/.hermes-nodes/audit.log` for startup messages).  
127|  
128|---  
129|## Architecture diagram  
130|  
131|```  
132|┌───────────────────────┐  outbound WSS  ┌───────────────────────┐  
133|│ Laptop                 │ ─────────────► │ VPS (Hermes brain)   │  
134|│ hermes‑node (Go)       │ ◄───────────── │ hermes‑nodes‑plugin   │  
135|│  • shell exec          │   commands     │  • Python server      │  
136|│  • file read/write     │   + results    │  • token auth         │  
137|│  • audit log           │               │  • registers as env   │  
138|└───────────────────────┘                └───────────────────────┘  
139|```  
140|  
141|**Same protocol on both sides:** see [`PROTOCOL.md`](./PROTOCOL.md).  
142|  
143|---  
144|## Development (for contributors)  
145|  
146|```bash  
147|# Build for all platforms  
148|./scripts/build.sh  
149|  
149|# Run unit tests  
149|go test ./...  
150|  
151|# Run e2e tests (requires Python test harness from hermes‑nodes‑plugin)  
152|go test ./tests/e2e/... -tags=e2e  
153|```  
154|  
155|**Requires Go 1.22+** for the development build. End users do **not** need Go installed.  
156|  
157|---  
158|## Cross‑compile matrix  
159|  
160|| OS      | Arch    | Binary name                     |  
161|||---------|---------|--------------------------------|  
162||| Linux   | amd64   | `hermes-node-linux-amd64`      |  
163||| Linux   | arm64   | `hermes-node-linux-arm64`      |  
164||| macOS   | amd64   | `hermes-node-darwin-amd64`     |  
165||| macOS   | arm64   | `hermes-node-darwin-arm64`     |  
166||| Windows | amd64   | `hermes-node-windows-amd64.exe`|  
167||| Windows | arm64   | `hermes-node-windows-arm64.exe`|  
168|  
169|---  
170|## License  
171|MIT — see [LICENSE](./LICENSE).  