// Regression tests for the chunked read/write hardening fixes.
//
// These drive the handlers directly (no conn-pair rig) because each
// case is about handler-local state: transfer caps, file mode, mode
// validation, assembly pruning, and heartbeat touches.
package wire

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func chunkMsg(seq int, data []byte, more bool, extra map[string]any) map[string]any {
	m := map[string]any{
		"seq":  seq,
		"data": base64.StdEncoding.EncodeToString(data),
		"more": more,
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func wantWriteResult(t *testing.T, env Envelope) WriteResultPayload {
	t.Helper()
	wr, ok := env.Payload.(WriteResultPayload)
	if !ok {
		t.Fatalf("payload is %T, want WriteResultPayload", env.Payload)
	}
	return wr
}

func noTempFiles(t *testing.T, dir string) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dir, ".hermes-write-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// A sender that declares a small size_bytes and then streams more must
// be cut off mid-stream, not after the bytes have hit the disk.
func TestWriteChunk_RejectsOversizeStream(t *testing.T) {
	dir := t.TempDir()
	fsys := NewFileSystem([]string{dir}, &recordingAudit{})
	fsys.FsChunkMaxBytes = 1 << 20

	payload := []byte("0123456789abcdef") // 16 bytes, declared as 4
	sum := sha256.Sum256(payload)
	hdr := map[string]any{
		"path":       filepath.Join(dir, "out.bin"),
		"mode":       "overwrite",
		"size_bytes": 4,
		"sha256":     hex.EncodeToString(sum[:]),
	}

	env, err := fsys.WriteChunkHandler(context.Background(), "req-1", chunkMsg(1, payload, false, hdr))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got := wantWriteResult(t, env).Error; got != "size_mismatch" {
		t.Fatalf("error = %q, want size_mismatch", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.bin")); !os.IsNotExist(err) {
		t.Fatal("target must not exist after an aborted stream")
	}
	noTempFiles(t, dir)
	if len(fsys.writes) != 0 {
		t.Fatalf("assembly not torn down: %d left", len(fsys.writes))
	}
}

// CreateTemp makes 0600; the applied file must keep the target's mode
// (or 0644 for a new file), matching the legacy write path.
func TestWriteChunk_PreservesTargetMode(t *testing.T) {
	dir := t.TempDir()
	fsys := NewFileSystem([]string{dir}, &recordingAudit{})
	fsys.FsChunkMaxBytes = 1 << 20

	write := func(name string, mode string, data []byte) {
		t.Helper()
		sum := sha256.Sum256(data)
		hdr := map[string]any{
			"path":       filepath.Join(dir, name),
			"mode":       mode,
			"size_bytes": len(data),
			"sha256":     hex.EncodeToString(sum[:]),
		}
		env, err := fsys.WriteChunkHandler(context.Background(), "req-"+name, chunkMsg(1, data, false, hdr))
		if err != nil {
			t.Fatalf("%s: handler: %v", name, err)
		}
		if wr := wantWriteResult(t, env); wr.Status != "ok" {
			t.Fatalf("%s: status %q (%s)", name, wr.Status, wr.Error)
		}
	}

	existing := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(existing, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("run.sh", "overwrite", []byte("new"))
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("existing target mode = %o, want 755", got)
	}

	write("fresh.txt", "overwrite", []byte("new"))
	info, err = os.Stat(filepath.Join(dir, "fresh.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("new file mode = %o, want 644", got)
	}
}

// A typo'd mode must fail loudly instead of silently overwriting.
func TestWriteChunk_RejectsUnknownMode(t *testing.T) {
	dir := t.TempDir()
	fsys := NewFileSystem([]string{dir}, &recordingAudit{})
	fsys.FsChunkMaxBytes = 1 << 20

	data := []byte("x")
	sum := sha256.Sum256(data)
	hdr := map[string]any{
		"path":       filepath.Join(dir, "out.bin"),
		"mode":       "appendd",
		"size_bytes": len(data),
		"sha256":     hex.EncodeToString(sum[:]),
	}

	env, err := fsys.WriteChunkHandler(context.Background(), "req-1", chunkMsg(1, data, false, hdr))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got := wantWriteResult(t, env).Error; got != "bad_request" {
		t.Fatalf("error = %q, want bad_request", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.bin")); !os.IsNotExist(err) {
		t.Fatal("unknown mode must not write the target")
	}
	noTempFiles(t, dir)
}

// create mode against an existing target must fail before any chunk is
// accepted, not after the whole transfer.
func TestWriteChunk_CreateFailsBeforeStreaming(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys := NewFileSystem([]string{dir}, &recordingAudit{})
	fsys.FsChunkMaxBytes = 1 << 20

	data := []byte("new")
	sum := sha256.Sum256(data)
	hdr := map[string]any{
		"path":       target,
		"mode":       "create",
		"size_bytes": len(data),
		"sha256":     hex.EncodeToString(sum[:]),
	}

	env, err := fsys.WriteChunkHandler(context.Background(), "req-1", chunkMsg(1, data, true, hdr))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got := wantWriteResult(t, env).Error; got != "file_exists" {
		t.Fatalf("error = %q, want file_exists", got)
	}
	noTempFiles(t, dir)
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("target was modified: %q", b)
	}
}

// An assembly whose sender vanished must be pruned by the next seq=1,
// temp file included.
func TestWriteChunk_PrunesStaleAssembly(t *testing.T) {
	dir := t.TempDir()
	fsys := NewFileSystem([]string{dir}, &recordingAudit{})
	fsys.FsChunkMaxBytes = 1 << 20

	now := time.Now()
	fsys.now = func() time.Time { return now }

	open := func(id, name string) {
		t.Helper()
		data := []byte("abc")
		sum := sha256.Sum256(data)
		hdr := map[string]any{
			"path":       filepath.Join(dir, name),
			"mode":       "overwrite",
			"size_bytes": 10,
			"sha256":     hex.EncodeToString(sum[:]),
		}
		env, err := fsys.WriteChunkHandler(context.Background(), id, chunkMsg(1, data, true, hdr))
		if err != nil {
			t.Fatalf("%s: handler: %v", id, err)
		}
		if env.Type != "" {
			t.Fatalf("%s: seq=1 with more=true must not answer, got %v", id, env.Type)
		}
	}

	open("req-stale", "stale.bin")
	staleTmp := fsys.writes["req-stale"].tmpPath

	now = now.Add(11 * time.Minute)
	open("req-fresh", "fresh.bin")

	if _, ok := fsys.writes["req-stale"]; ok {
		t.Error("stale assembly still tracked")
	}
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Errorf("stale temp file still on disk: %s", staleTmp)
	}
	if _, ok := fsys.writes["req-fresh"]; !ok {
		t.Error("fresh assembly was pruned")
	}
}

// A long read streams nothing inbound; each chunk must bump the
// heartbeat clock or the watchdog kills the connection mid-file.
func TestReadHandler_TouchesPerChunk(t *testing.T) {
	dir := t.TempDir()
	seedFile(t, dir, "f.bin", 3*1024)

	fsys := NewFileSystem([]string{dir}, &recordingAudit{})
	fsys.ChunkSize = 1024
	fsys.FsChunkMaxBytes = 1 << 20
	fsys.SetServerCapabilities([]string{"read_chunk"})

	touches, dataChunks := 0, 0
	fsys.Touch = func() { touches++ }
	fsys.WriteFunc = func(_ context.Context, env Envelope) error {
		if p, ok := env.Payload.(ReadChunkPayload); ok && p.More {
			dataChunks++
		}
		return nil
	}

	if _, err := fsys.ReadHandler(context.Background(), "req-1", map[string]any{
		"path": filepath.Join(dir, "f.bin"),
	}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if dataChunks != 3 || touches != dataChunks {
		t.Fatalf("dataChunks=%d touches=%d, want 3/3", dataChunks, touches)
	}
}

// If the final frame can't be sent the waiter would hang until timeout;
// the handler must surface the error so the connection recycles.
func TestReadHandler_FinalFrameErrorSurfaces(t *testing.T) {
	dir := t.TempDir()
	seedFile(t, dir, "f.bin", 2*1024)

	fsys := NewFileSystem([]string{dir}, &recordingAudit{})
	fsys.ChunkSize = 1024
	fsys.FsChunkMaxBytes = 1 << 20
	fsys.SetServerCapabilities([]string{"read_chunk"})
	fsys.WriteFunc = func(_ context.Context, env Envelope) error {
		if p, ok := env.Payload.(ReadChunkPayload); ok && !p.More {
			return os.ErrClosed
		}
		return nil
	}

	_, err := fsys.ReadHandler(context.Background(), "req-1", map[string]any{
		"path": filepath.Join(dir, "f.bin"),
	})
	if err == nil {
		t.Fatal("want an error when the final frame cannot be sent")
	}
	if !strings.Contains(err.Error(), "final read_chunk") {
		t.Fatalf("error = %v, want it to mention the final read_chunk", err)
	}
}
