# Hermes Nodes Protocol

The wire protocol spoken between a Hermes Agent brain (Python, on the server/VPS) and a Hermes Node (Go, on the laptop/remote machine). This document is the contract — both implementations must conform.

**Version:** `0.1.0-draft`
**Transport:** WebSocket Secure (WSS) on a single bidirectional connection
**Encoding:** JSON, UTF-8, one message per WebSocket frame
**Direction:** Node → Server (outbound dial only on the node side; no inbound ports required on the laptop)

---

## 1. Connection lifecycle

```
Node                                       Server
 │                                            │
 │── WSS Dial (TLS 1.3) ─────────────────────►│
 │                                            │
 │◄───── WSS Handshake (HTTP 101) ───────────│
 │                                            │
 │── {type: "hello", protocol_version, ...} ─►│  (1) Node introduces itself
 │                                            │
 │◄── {type: "hello_ack", protocol_version,  │  (2) Server accepts (or rejects)
 │      session_id, server_time} ────────────│
 │                                            │
 │── {type: "auth", node_name, token} ───────►│  (3) Node authenticates
 │                                            │
 │◄── {type: "auth_ok"}  OR                  │
 │     {type: "auth_err", reason, code} ─────│  (4) Server validates
 │                                            │
 │         (close 1000 on auth_err)            │
 │                                            │
 │  ◄──── {type: "exec", request_id, ...} ────│  (5) Server sends a call
 │── {type: "exec_result", request_id, ...} ─►│  (6) Node responds
 │                                            │
 │   (heartbeats every 30s, see §6)            │
 │                                            │
 │── {type: "bye", reason} ──────────────────►│  (7) Graceful disconnect
 │◄── close 1000 ─────────────────────────────│
```

**Failure modes:**
- If the server receives a `hello` with an unsupported `protocol_version`, it sends `hello_err` and closes with code `4002`. The node logs the version mismatch and exits.
- If the server receives any message before `auth` completes (other than `hello`), it closes with code `4003`.
- If the node receives any message before `auth_ok`, it closes with code `4003`.
- TLS failures → standard WebSocket close, node retries with backoff.

---

## 2. Message envelope

Every message is a JSON object with the same outer shape:

```json
{
  "type": "<string, see §3>",
  "id": "<string, optional — server-generated for calls, node-generated for unsolicited>",
  "ts": "<RFC3339 timestamp, milliseconds, UTC>"
}
```

- `type` is the discriminator (see §3 for the full list).
- `id` is the request correlation ID. **Server-generated IDs are UUIDv4.** Nodes must echo the same `id` on the response message.
- `ts` is informational; the server is the source of truth for timing.

**Reserved `type` strings** (must not be used for application extensions):
`hello`, `hello_ack`, `hello_err`, `auth`, `auth_ok`, `auth_err`, `exec`, `exec_result`, `read`, `read_result`, `write`, `write_result`, `ping`, `pong`, `error`, `bye`.

---

## 3. Message types

### 3.1 `hello` (node → server)
First message after WSS upgrade.

```json
{
  "type": "hello",
  "protocol_version": "0.1.0",
  "node_name": "work-laptop",
  "node_version": "0.1.0",
  "platform": "darwin",
  "arch": "arm64",
  "capabilities": ["exec", "read", "write"]
}
```

- `node_name` is the human-readable name the node will be addressed as. Must match the name the token was bound to (server validates on `auth`).
- `capabilities` is a hint, not a contract — server may probe before trusting it.
- `protocol_version` follows semver. Minor-version compatible within a major version.

### 3.2 `hello_ack` (server → node)
Acceptance.

```json
{
  "type": "hello_ack",
  "protocol_version": "0.1.0",
  "session_id": "uuid-v4",
  "server_time": "2026-06-04T10:00:00.000Z"
}
```

### 3.3 `hello_err` (server → node)
Version mismatch or other protocol-level rejection.

```json
{
  "type": "hello_err",
  "reason": "unsupported_protocol_version",
  "code": 4002,
  "server_max_version": "0.1.0"
}
```

### 3.4 `auth` (node → server)
Present the pre-shared token.

```json
{
  "type": "auth",
  "node_name": "work-laptop",
  "token": "<opaque-string>"
}
```

The token is the raw value returned by `hermes node pair` on the server. Compare in constant time.

### 3.5 `auth_ok` / `auth_err` (server → node)

`auth_ok`:
```json
{ "type": "auth_ok", "session_id": "uuid-v4" }
```

`auth_err`:
```json
{ "type": "auth_err", "reason": "invalid_token" | "unknown_node" | "revoked", "code": 4001 }
```

On `auth_err`, the server closes the WSS with code `4001` immediately after sending.

### 3.6 `exec` (server → node)
Run a shell command.

```json
{
  "type": "exec",
  "id": "uuid-v4",
  "command": "pytest tests/ -q",
  "cwd": "~/code/myapp",        // optional, default = last cwd
  "env": {"FOO": "bar"},         // optional, merged into session env
  "timeout_ms": 60000            // optional, default 60000, max 600000
}
```

### 3.7 `exec_result` (node → server)

```json
{
  "type": "exec_result",
  "id": "uuid-v4",            // echoes the request id
  "status": "ok" | "error" | "timeout",
  "exit_code": 0,             // 0 on success, non-zero on error
  "stdout": "<string, base64-or-utf8 — see encoding note>",
  "stderr": "<string>",
  "duration_ms": 1234,
  "truncated": false          // true if stdout/stderr exceeded 10MB cap
}
```

**Output encoding:** stdout/stderr are UTF-8 strings. If a process produces non-UTF-8 bytes, the node replaces invalid sequences with U+FFFD. (We considered base64, but it makes the common case painful and the rare case still requires an explicit decision. v1 keeps it UTF-8.)

**Truncation:** if either stream exceeds 10 MB, the node stops reading at 10 MB, sets `truncated: true`, and reports the partial bytes. Server surfaces a warning to the caller.

### 3.8 `read` (server → node)

```json
{
  "type": "read",
  "id": "uuid-v4",
  "path": "/Users/patrick/code/myapp/src/x.py"
}
```

### 3.9 `read_result` (node → server)

```json
{
  "type": "read_result",
  "id": "uuid-v4",
  "status": "ok" | "error",
  "content_b64": "<base64>",     // present on ok
  "size_bytes": 1234,            // present on ok
  "error": "path_not_allowed",   // present on error
  "error_detail": "..."          // human-readable, present on error
}
```

Files are read as bytes and base64-encoded in the result. The 10 MB cap applies; larger files return `error: "file_too_large"`.

### 3.10 `write` (server → node)

```json
{
  "type": "write",
  "id": "uuid-v4",
  "path": "/Users/patrick/code/myapp/src/x.py",
  "content_b64": "<base64>",
  "mode": "create" | "overwrite" | "append"   // default "overwrite"
}
```

### 3.11 `write_result` (node → server)

```json
{
  "type": "write_result",
  "id": "uuid-v4",
  "status": "ok" | "error",
  "bytes_written": 1234,
  "error": "path_not_allowed" | "io_error",
  "error_detail": "..."
}
```

### 3.12 `ping` / `pong` (bidirectional)
Keepalive.

```json
{ "type": "ping", "ts": "..." }
{ "type": "pong", "ts": "...", "echo_ts": "..." }
```

- Either side may send `ping`. The peer must respond with `pong` echoing the `ts`.
- Default cadence: 30s. Timeout: 60s without a `pong` → drop connection.
- `ping`/`pong` are not subject to the `id` correlation rule (they use `ts` instead).

### 3.13 `error` (bidirectional)
Out-of-band error not tied to a specific call.

```json
{
  "type": "error",
  "code": 5000,
  "reason": "internal_error",
  "detail": "..."
}
```

### 3.14 `bye` (bidirectional)
Graceful shutdown.

```json
{ "type": "bye", "reason": "node_shutdown" | "server_shutdown" | "revoked" | "rotate_token" }
```

After `bye`, the sender closes the WSS with code `1000`.

### 3.15 Handler faults

A registered handler is a function the node dispatches a server-originated call to. If the handler **panics** during dispatch, the node MUST recover, record the panic value in its local log, and synthesise a response of:

```json
{
  "type": "error",
  "id": "<request id of the call that panicked>",
  "code": 5000,
  "reason": "internal_error",
  "detail": "handler panic: <panic value>"
}
```

The connection is **NOT** closed. Clients cannot distinguish on the wire between "the handler returned an error" and "the handler panicked" — both are 5000/internal_error envelopes whose `detail` carries the failure cause. The next call on the same connection is dispatched normally.

If writing the synthesised error envelope itself fails (e.g. the conn is wedged), the dispatcher tears the connection down and the supervisor reconnects with backoff. The panic is still recorded in the local log before the conn is closed.

A handler that wants to fail cleanly should return an error; panics are reserved for "this should never happen" conditions the author did not anticipate.

---

## 4. Error codes

**WebSocket close codes:**
| Code | Meaning |
|------|---------|
| 1000 | Normal closure |
| 4001 | Auth failed |
| 4002 | Protocol version mismatch |
| 4003 | Message out of order |
| 4004 | Rate limit exceeded |
| 4005 | Server shutting down |
| 4006 | Token revoked (after successful auth, mid-session) |

**Application error codes** (in `error` messages or `*_result.status=error`):
| Code | Reason | Meaning |
|------|--------|---------|
| 1000 | ok | Success |
| 2001 | path_not_allowed | Path failed allowlist check |
| 2002 | file_not_found | Read/write on non-existent file |
| 2003 | file_too_large | File exceeded 10 MB cap |
| 2004 | io_error | Underlying I/O error |
| 3001 | exec_timeout | Command exceeded timeout |
| 3002 | exec_failed | Non-zero exit, but command ran |
| 5000 | internal_error | Node/server bug |
| 5001 | unknown_message | Server sent a `type` the node doesn't recognize |

---

## 5. Versioning

- Protocol version is semver: `MAJOR.MINOR.PATCH`.
- Within the same `MAJOR`, the server supports all `MINOR` versions <= its own.
- A `MAJOR` bump is the only thing that can break compatibility.
- Both sides declare their version in `hello` / `hello_ack`. The server may downgrade the agreed version (e.g. node says 0.3.0, server says 0.1.0) — both must then implement the lower of the two.

---

## 6. Heartbeat

- Every 30s, either side may send a `ping`.
- If no `pong` (or any other message) is received within 60s, the connection is considered dead and must be closed.
- On the node side, dead connection → reconnect with exponential backoff (1s, 2s, 4s, 8s, ..., max 60s).
- On the server side, dead connection → mark node as offline in the registry, do not retry.

---

## 7. Security

- **TLS:** required. WSS only. Self-signed certs are allowed on private deployments if the node pins the CA (out of scope for v1 client config but supported).
- **Token:** 32 bytes, base64url-encoded, generated with `secrets.token_urlsafe(32)`. Compared in constant time on the server.
- **Token storage:**
  - On the server: encrypted at rest with Fernet (symmetric AES-128 + HMAC) using a key from `HERMES_NODES_TOKEN_KEY` in `~/.hermes/.env`.
  - On the node: plaintext in `~/.hermes-nodes/config.toml` with file mode `0600`. Future v2: OS keychain.
- **Allowlist:** the node enforces a path allowlist for `read` and `write` independently of the server. The server does not see the allowlist contents.
- **Audit:** every call is logged on both sides with the same fields (timestamp, node name, action, duration, exit code, request id, status). Audit logs are append-only.

---

## 8. Size limits

- WSS frame size: 16 MB max (configurable server-side, default 16 MB to comfortably fit a 10 MB `write` request + envelope).
- Output cap: 10 MB per stream per `exec` call. Larger → truncated with `truncated: true`.
- File size cap: 10 MB for `read`/`write`. Larger → error `file_too_large`.
- Message rate limit: 100 calls/second per node, sliding window. Excess → server closes with `4004`.

---

## 9. Reference examples

### 9.1 Full happy-path exec

```jsonc
// node → server
{ "type": "hello", "protocol_version": "0.1.0", "node_name": "work-laptop", "node_version": "0.1.0", "platform": "darwin", "arch": "arm64", "capabilities": ["exec", "read", "write"] }

// server → node
{ "type": "hello_ack", "protocol_version": "0.1.0", "session_id": "a1b2c3d4-...", "server_time": "2026-06-04T10:00:00.000Z" }

// node → server
{ "type": "auth", "node_name": "work-laptop", "token": "abc123..." }

// server → node
{ "type": "auth_ok", "session_id": "a1b2c3d4-..." }

// server → node
{ "type": "exec", "id": "r-001", "command": "pytest tests/ -q", "timeout_ms": 60000 }

// node → server
{ "type": "exec_result", "id": "r-001", "status": "ok", "exit_code": 0, "stdout": "====== test session starts ======\n...\n5 passed in 0.42s", "stderr": "", "duration_ms": 423, "truncated": false }
```

### 9.2 Auth failure

```jsonc
// node → server
{ "type": "hello", "protocol_version": "0.1.0", ... }
{ "type": "auth", "node_name": "work-laptop", "token": "WRONG" }

// server → node
{ "type": "auth_err", "reason": "invalid_token", "code": 4001 }

// server closes WSS with code 4001
```

### 9.3 Path-blocked read

```jsonc
// server → node
{ "type": "read", "id": "r-002", "path": "/etc/shadow" }

// node → server
{ "type": "read_result", "id": "r-002", "status": "error", "error": "path_not_allowed", "error_detail": "/etc/shadow is not in the configured allowlist" }
```

---

## 10. Future directions (not in v1)

- `event` messages (node → server, unsolicited notifications)
- `stream` messages (incremental stdout chunks instead of a single big result)
- `proxy` messages (server asks node to forward a connection to a third party)
- Multi-server federation
- Capability negotiation for camera/screen/browser
