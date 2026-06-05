// Tests for the `read` and `write` call handlers (Task 1.8).
//
// Strategy: same conn pair rig as handler_exec_test.go, register
// FileSystem.ReadHandler / WriteHandler on a Dispatcher, drive
// calls from the server side, and assert the response shape and
// side effects (file contents, allowlist enforcement, audit rows).
//
// We use realOS + t.TempDir() throughout. An in-memory FileIO
// would be nice for hermetic tests, but internal/fs.Check walks
// the host filesystem during its canonicalize step, so the
// in-memory layer can't satisfy it. (A follow-up issue tracks
// the canonicalize bug; once that's fixed, the in-memory layer
// becomes viable.)
//
// One subtest per concern keeps failures easy to read.
package wire

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/blaspat/hermes-nodes/internal/audit"
)

// TestReadHandler_HappyPath drives a `read` call against an
// on-disk file and asserts: status=ok, content_b64 round-trips
// to the original bytes, size_bytes matches the on-disk size, the
// request id is echoed, and the read actually happened.
//
// We use realOS + t.TempDir() rather than the in-memory
// recordingFileIO because the path check (internal/fs.Check)
// walks the host filesystem; the in-memory layer can't
// satisfy that surface (and would also mask the canonicalize
// bug filed in the follow-up issue).
func TestReadHandler_HappyPath(t *testing.T) {
	dir := t.TempDir()
	const fname = "note.txt"
	const content = "hello, hermes\n"
	if err := os.WriteFile(filepath.Join(dir, fname), []byte(content), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	rec := &recordingAudit{}
	fsys := NewFileSystem([]string{dir}, rec)
	if err := d.Register(TypeRead, fsys.ReadHandler); err != nil {
		t.Fatalf("Register read: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":   "req-read-ok",
		"type": "read",
		"path": filepath.Join(dir, fname),
	})
	resp := readEnvelope(t, pair)

	if resp["type"] != "read_result" {
		t.Fatalf("response type: got %q, want read_result", resp["type"])
	}
	if resp["id"] != "req-read-ok" {
		t.Errorf("response id: got %q, want req-read-ok", resp["id"])
	}
	if resp["status"] != "ok" {
		t.Errorf("response status: got %q, want ok", resp["status"])
	}
	encoded, _ := resp["content_b64"].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("content_b64 not valid base64: %v", err)
	}
	if string(decoded) != content {
		t.Errorf("content_b64: decoded to %q, want %q", decoded, content)
	}
	if size, _ := resp["size_bytes"].(float64); int(size) != len(content) {
		t.Errorf("size_bytes: got %v, want %d", resp["size_bytes"], len(content))
	}

	// Audit row captured.
	entries := rec.snapshot()
	if len(entries) != 1 {
		t.Fatalf("audit entries: got %d, want 1", len(entries))
	}
	if entries[0].Action != "read" {
		t.Errorf("audit action: got %q, want read", entries[0].Action)
	}
	if entries[0].Status != "ok" {
		t.Errorf("audit status: got %q, want ok", entries[0].Status)
	}
}

// TestReadHandler_PathNotAllowed is the security guardrail: a path
// outside the allowlist must return status=error,
// error=path_not_allowed, with the file system untouched and the
// audit log showing the rejection.
//
// We use realOS + t.TempDir() rather than the in-memory
// recordingFileIO because the path check (internal/fs.Check) does
// its own canonicalize/parent-walk that depends on the host
// filesystem; the in-memory layer can't satisfy that surface.
func TestReadHandler_PathNotAllowed(t *testing.T) {
	allowedDir := t.TempDir()
	deniedDir := t.TempDir()
	deniedFile := filepath.Join(deniedDir, "secret.txt")
	if err := os.WriteFile(deniedFile, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("seed denied file: %v", err)
	}

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	rec := &recordingAudit{}
	fsys := NewFileSystem([]string{allowedDir}, rec)
	if err := d.Register(TypeRead, fsys.ReadHandler); err != nil {
		t.Fatalf("Register read: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":   "req-read-deny",
		"type": "read",
		"path": deniedFile,
	})
	resp := readEnvelope(t, pair)

	if resp["type"] != "read_result" {
		t.Fatalf("response type: got %q, want read_result (denial still gets a result)", resp["type"])
	}
	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error", resp["status"])
	}
	if resp["error"] != "path_not_allowed" {
		t.Errorf("response error: got %q, want path_not_allowed", resp["error"])
	}
	if resp["content_b64"] != nil && resp["content_b64"] != "" {
		t.Errorf("content_b64 on denial: got %q, want empty", resp["content_b64"])
	}
	// Audit captured the denial. (We don't assert on stat/read
	// counts here because realOS is used; the meaningful
	// assertion is that the response itself says
	// path_not_allowed — if the call had hit the filesystem,
	// the test would have read the file.)
	entries := rec.snapshot()
	if len(entries) != 1 {
		t.Fatalf("audit entries: got %d, want 1", len(entries))
	}
	if entries[0].Status != "error" {
		t.Errorf("audit status: got %q, want error", entries[0].Status)
	}
}

// TestReadHandler_FileNotFound verifies the file_not_found branch:
// a path inside the allowlist that doesn't exist returns
// status=error, error=file_not_found (NOT path_not_allowed).
func TestReadHandler_FileNotFound(t *testing.T) {
	allowedDir := t.TempDir()
	missing := filepath.Join(allowedDir, "missing.txt")

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	rec := &recordingAudit{}
	fsys := NewFileSystem([]string{allowedDir}, rec)
	if err := d.Register(TypeRead, fsys.ReadHandler); err != nil {
		t.Fatalf("Register read: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":   "req-read-missing",
		"type": "read",
		"path": missing,
	})
	resp := readEnvelope(t, pair)

	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error", resp["status"])
	}
	if resp["error"] != "file_not_found" {
		t.Errorf("response error: got %q, want file_not_found", resp["error"])
	}
	entries := rec.snapshot()
	if len(entries) != 1 || entries[0].Status != "error" {
		t.Errorf("audit row: got %d entries (statuses %v), want 1 error row", len(entries), entryStatuses(entries))
	}
}

// TestReadHandler_AllowlistUnset is the "operator trusts
// everything" path: a FileSystem with no allowed list reads any
// file. Mirrors the exec handler's CwdAllowlistUnset test.
func TestReadHandler_AllowlistUnset(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(target, []byte("ok"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	fsys := NewFileSystem(nil, nil) // nil allowed
	if err := d.Register(TypeRead, fsys.ReadHandler); err != nil {
		t.Fatalf("Register read: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":   "req-read-open",
		"type": "read",
		"path": target,
	})
	resp := readEnvelope(t, pair)

	if resp["status"] != "ok" {
		t.Errorf("response status: got %q, want ok (no allowlist = no check)", resp["status"])
	}
	if encoded, _ := resp["content_b64"].(string); encoded == "" {
		t.Errorf("content_b64 empty on success")
	}
}

// TestReadHandler_OnDiskRoundtrip goes through realOS to prove the
// wire shape matches what real bytes look like on disk. We write
// a file to t.TempDir(), set the allowlist to that dir, and round-
// trip a read.
func TestReadHandler_OnDiskRoundtrip(t *testing.T) {
	dir := t.TempDir()
	const fname = "real.txt"
	const content = "real-on-disk\n"
	if err := os.WriteFile(filepath.Join(dir, fname), []byte(content), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	fsys := NewFileSystem([]string{dir}, nil)
	if err := d.Register(TypeRead, fsys.ReadHandler); err != nil {
		t.Fatalf("Register read: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":   "req-read-real",
		"type": "read",
		"path": filepath.Join(dir, fname),
	})
	resp := readEnvelope(t, pair)

	if resp["status"] != "ok" {
		t.Fatalf("response status: got %q, want ok", resp["status"])
	}
	encoded, _ := resp["content_b64"].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if string(decoded) != content {
		t.Errorf("decoded: got %q, want %q", decoded, content)
	}
}

// TestWriteHandler_HappyPath drives a `write` call and asserts
// the file ends up on disk, the response reports the right
// bytes_written, and the audit row is recorded.
func TestWriteHandler_HappyPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	rec := &recordingAudit{}
	fsys := NewFileSystem([]string{dir}, rec)
	if err := d.Register(TypeWrite, fsys.WriteHandler); err != nil {
		t.Fatalf("Register write: %v", err)
	}
	_, _ = runDispatcher(t, d)

	const payload = "written via write handler\n"
	writeServerJSON(t, pair.server, map[string]any{
		"id":          "req-write-ok",
		"type":        "write",
		"path":        target,
		"content_b64": base64.StdEncoding.EncodeToString([]byte(payload)),
		"mode":        "overwrite",
	})
	resp := readEnvelope(t, pair)

	if resp["type"] != "write_result" {
		t.Fatalf("response type: got %q, want write_result", resp["type"])
	}
	if resp["status"] != "ok" {
		t.Errorf("response status: got %q, want ok (detail=%v)", resp["status"], resp["error_detail"])
	}
	if written, _ := resp["bytes_written"].(float64); int(written) != len(payload) {
		t.Errorf("bytes_written: got %v, want %d", resp["bytes_written"], len(payload))
	}
	// File is on disk with the right contents.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != payload {
		t.Errorf("file contents: got %q, want %q", got, payload)
	}

	// Audit row.
	entries := rec.snapshot()
	if len(entries) != 1 {
		t.Fatalf("audit entries: got %d, want 1", len(entries))
	}
	if entries[0].Action != "write" {
		t.Errorf("audit action: got %q, want write", entries[0].Action)
	}
	if entries[0].Status != "ok" {
		t.Errorf("audit status: got %q, want ok", entries[0].Status)
	}
}

// TestWriteHandler_PathNotAllowed is the write-side guardrail: a
// path outside the allowlist must not touch the filesystem and
// must return error=path_not_allowed.
//
// We use realOS + t.TempDir() (not the in-memory recordingFileIO)
// for the same reason as TestReadHandler_PathNotAllowed: the
// allowlist check (internal/fs.Check) walks the host filesystem.
func TestWriteHandler_PathNotAllowed(t *testing.T) {
	allowedDir := t.TempDir()
	deniedDir := t.TempDir()
	deniedPath := filepath.Join(deniedDir, "leak.txt")

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	rec := &recordingAudit{}
	fsys := NewFileSystem([]string{allowedDir}, rec)
	if err := d.Register(TypeWrite, fsys.WriteHandler); err != nil {
		t.Fatalf("Register write: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":          "req-write-deny",
		"type":        "write",
		"path":        deniedPath,
		"content_b64": "aGk=",
	})
	resp := readEnvelope(t, pair)

	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error", resp["status"])
	}
	if resp["error"] != "path_not_allowed" {
		t.Errorf("response error: got %q, want path_not_allowed", resp["error"])
	}
	// Nothing should have been written under deniedDir.
	if _, err := os.Stat(deniedPath); err == nil {
		t.Errorf("denied file was created on disk: %s", deniedPath)
	}
	entries := rec.snapshot()
	if len(entries) != 1 || entries[0].Status != "error" {
		t.Errorf("audit row: got %d entries (statuses %v), want 1 error row", len(entries), entryStatuses(entries))
	}
}

// TestWriteHandler_AppendMode verifies the protocol's "append"
// mode: a second write to the same path keeps the original
// content and tacks the new bytes on the end.
func TestWriteHandler_AppendMode(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log.txt")
	if err := os.WriteFile(logPath, []byte("line1\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	fsys := NewFileSystem([]string{dir}, nil)
	if err := d.Register(TypeWrite, fsys.WriteHandler); err != nil {
		t.Fatalf("Register write: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":          "req-write-append",
		"type":        "write",
		"path":        logPath,
		"content_b64": base64.StdEncoding.EncodeToString([]byte("line2\n")),
		"mode":        "append",
	})
	resp := readEnvelope(t, pair)

	if resp["status"] != "ok" {
		t.Fatalf("response status: got %q, want ok (detail=%v)", resp["status"], resp["error_detail"])
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	want := "line1\nline2\n"
	if string(got) != want {
		t.Errorf("appended file: got %q, want %q", got, want)
	}
}

// TestWriteHandler_CreateRefusesExisting verifies that mode=create
// refuses to clobber an existing file. The response is status=error
// with a clear error_detail and the original file is unchanged.
func TestWriteHandler_CreateRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.txt")
	const original = "do not clobber me"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	fsys := NewFileSystem([]string{dir}, nil)
	if err := d.Register(TypeWrite, fsys.WriteHandler); err != nil {
		t.Fatalf("Register write: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":          "req-write-create",
		"type":        "write",
		"path":        target,
		"content_b64": base64.StdEncoding.EncodeToString([]byte("replacement")),
		"mode":        "create",
	})
	resp := readEnvelope(t, pair)

	// Create against an existing file must NOT succeed.
	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error (create must refuse existing file)", resp["status"])
	}
	if resp["error"] != "io_error" {
		t.Errorf("response error: got %q, want io_error", resp["error"])
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != original {
		t.Errorf("file was clobbered: got %q, want %q (create must refuse)", got, original)
	}
}

// TestWriteHandler_RoundTripOnDisk exercises the full create +
// read flow against realOS + t.TempDir to prove the wire shape
// matches what real bytes look like on disk.
func TestWriteHandler_RoundTripOnDisk(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rt.txt")
	const payload = "round-trip via realOS\n"

	// Phase 1: write the file.
	{
		pair := newConnPair(t)
		d := newTestDispatcher(t, pair.client)
		fsys := NewFileSystem([]string{dir}, nil)
		if err := d.Register(TypeWrite, fsys.WriteHandler); err != nil {
			t.Fatalf("Register write: %v", err)
		}
		_, _ = runDispatcher(t, d)

		writeServerJSON(t, pair.server, map[string]any{
			"id":          "req-rt-write",
			"type":        "write",
			"path":        target,
			"content_b64": base64.StdEncoding.EncodeToString([]byte(payload)),
		})
		writeResp := readEnvelope(t, pair)
		if writeResp["status"] != "ok" {
			t.Fatalf("write: status=%q detail=%v", writeResp["status"], writeResp["error_detail"])
		}
	}

	// Phase 2: read it back.
	{
		pair := newConnPair(t)
		d := newTestDispatcher(t, pair.client)
		fsys := NewFileSystem([]string{dir}, nil)
		if err := d.Register(TypeRead, fsys.ReadHandler); err != nil {
			t.Fatalf("Register read: %v", err)
		}
		_, _ = runDispatcher(t, d)

		writeServerJSON(t, pair.server, map[string]any{
			"id":   "req-rt-read",
			"type": "read",
			"path": target,
		})
		readResp := readEnvelope(t, pair)
		if readResp["status"] != "ok" {
			t.Fatalf("read: status=%q detail=%v", readResp["status"], readResp["error_detail"])
		}
		encoded, _ := readResp["content_b64"].(string)
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("base64 decode: %v", err)
		}
		if string(decoded) != payload {
			t.Errorf("roundtrip: got %q, want %q", decoded, payload)
		}
	}
}

// TestWriteHandler_BadBase64 covers the "client sent garbage"
// path. A non-base64 content_b64 must return status=error,
// error=io_error with a clear detail. The handler rejects bad
// base64 BEFORE the file size check, so a server can't bypass the
// 10MB cap by sending a 16MB string of garbage.
func TestWriteHandler_BadBase64(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.txt")

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	fsys := NewFileSystem([]string{dir}, nil)
	if err := d.Register(TypeWrite, fsys.WriteHandler); err != nil {
		t.Fatalf("Register write: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":          "req-write-bad",
		"type":        "write",
		"path":        target,
		"content_b64": "this is not base64 !!!",
	})
	resp := readEnvelope(t, pair)

	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error", resp["status"])
	}
	if resp["error"] != "io_error" {
		t.Errorf("response error: got %q, want io_error", resp["error"])
	}
	if _, err := os.Stat(target); err == nil {
		t.Errorf("file was written despite bad base64: %s", target)
	}
}

// TestReadHandler_FileTooLarge covers the 10MB cap path: a file
// larger than MaxFileBytes must return status=error,
// error=file_too_large. PROTOCOL.md §3.9 requires this error
// code (rather than truncation) so the caller knows the bytes
// are incomplete and can switch to a streaming mechanism.
func TestReadHandler_FileTooLarge(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	// Write just past the cap to trigger the size branch.
	if err := os.WriteFile(big, make([]byte, MaxFileBytes+1), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	fsys := NewFileSystem([]string{dir}, nil)
	if err := d.Register(TypeRead, fsys.ReadHandler); err != nil {
		t.Fatalf("Register read: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":   "req-read-big",
		"type": "read",
		"path": big,
	})
	resp := readEnvelope(t, pair)

	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error", resp["status"])
	}
	if resp["error"] != "file_too_large" {
		t.Errorf("response error: got %q, want file_too_large", resp["error"])
	}
}

// TestWriteHandler_FileTooLarge covers the write side of the
// 10MB cap. A decoded payload larger than MaxFileBytes must
// return file_too_large and leave the file untouched.
func TestWriteHandler_FileTooLarge(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "big.bin")

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	fsys := NewFileSystem([]string{dir}, nil)
	if err := d.Register(TypeWrite, fsys.WriteHandler); err != nil {
		t.Fatalf("Register write: %v", err)
	}
	_, _ = runDispatcher(t, d)

	// 10MB + 1 byte, base64-encoded. We avoid the giant string
	// literal by using the bytes() builder via the std
	// base64 encoder at the test site.
	big := make([]byte, MaxFileBytes+1)
	writeServerJSON(t, pair.server, map[string]any{
		"id":          "req-write-big",
		"type":        "write",
		"path":        target,
		"content_b64": base64.StdEncoding.EncodeToString(big),
	})
	resp := readEnvelope(t, pair)

	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error", resp["status"])
	}
	if resp["error"] != "file_too_large" {
		t.Errorf("response error: got %q, want file_too_large", resp["error"])
	}
	if _, err := os.Stat(target); err == nil {
		t.Errorf("oversized payload was written to disk: %s", target)
	}
}

// entryStatuses is a test helper that returns the status of each
// audit entry, used in failure messages.
func entryStatuses(entries []audit.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Status
	}
	return out
}
