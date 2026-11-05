// Tests for chunked read/write (PROTOCOL.md §3.9a, §3.10b, §3.11a).
//
// Strategy: same conn-pair rig as handler_fs_test.go. Chunked reads
// are driven by sending a `read` call with a FileSystem that has
// WriteFunc + capabilities set, then collecting the read_chunk
// envelopes the handler streams. Chunked writes are driven by
// feeding write_chunk envelopes through the registered handler and
// asserting the on-disk result + write_result shape.
package wire

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/blaspat/hermes-node/internal/audit"
)

func seedFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	// Deterministic pseudo-random payload: repeating pattern with
	// positional marker so truncation / corruption is visible.
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte((i*31 + 7) % 256)
	}
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return p
}

// TestReadHandler_ChunkedHappyPath streams a >chunk-size file via
// read_chunk envelopes and asserts: intermediate chunks carry data +
// more=true, the final chunk (more=false) carries status/size/sha256,
// and concatenating chunk data reconstructs the exact bytes.
func TestReadHandler_ChunkedHappyPath(t *testing.T) {
	dir := t.TempDir()
	const fname = "big.bin"
	const size = 3 * 1024 * 1024 // 3 MiB → 4 chunks at 1 MiB
	orig, _ := os.ReadFile(
		seedFile(t, dir, fname, size),
	)

	rec := &recordingAudit{}
	fsys := NewFileSystem([]string{dir}, rec)
	fsys.ChunkSize = 1024 * 1024
	fsys.FsChunkMaxBytes = 512 * 1024 * 1024
	fsys.SetServerCapabilities([]string{"read_chunk"})

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	fsys.WriteFunc = d.WriteEnvelope
	if err := d.Register(TypeRead, fsys.ReadHandler); err != nil {
		t.Fatalf("Register read: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":   "req-read-chunk",
		"type": "read",
		"path": filepath.Join(dir, fname),
	})

	// Collect streamed envelopes until we see the final chunk.
	var chunks []map[string]any
	for {
		resp := readEnvelope(t, pair)
		if resp["type"] != "read_chunk" {
			t.Fatalf("expected read_chunk stream, got %v", resp["type"])
		}
		chunks = append(chunks, resp)
		if !chunkMore(resp) {
			break
		}
		if len(chunks) > 100 {
			t.Fatal("no final read_chunk after 100 chunks")
		}
	}

	if len(chunks) != 4 {
		t.Fatalf("want 4 chunks for 3 MiB @ 1 MiB, got %d", len(chunks))
	}
	// Reassemble.
	var reassembled []byte
	for i, c := range chunks {
		data := c["data"].(string)
		dec, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			t.Fatalf("chunk %d: bad base64: %v", i, err)
		}
		reassembled = append(reassembled, dec...)
	}
	if len(reassembled) != size {
		t.Fatalf("reassembled %d bytes, want %d", len(reassembled), size)
	}
	for i := range reassembled {
		if reassembled[i] != orig[i] {
			t.Fatalf("byte mismatch at %d: got %d want %d", i, reassembled[i], orig[i])
		}
	}
	// Final chunk metadata.
	final := chunks[len(chunks)-1]
	if final["more"] != false {
		t.Errorf("final chunk more: got %v, want false", final["more"])
	}
	if final["status"] != "ok" {
		t.Errorf("final status: got %v, want ok", final["status"])
	}
	if sz, _ := final["size_bytes"].(float64); int(sz) != size {
		t.Errorf("final size_bytes: got %v, want %d", final["size_bytes"], size)
	}
	sum := sha256.Sum256(orig)
	wantSHA := hex.EncodeToString(sum[:])
	if final["sha256"] != wantSHA {
		t.Errorf("final sha256: got %v, want %s", final["sha256"], wantSHA)
	}
	// Seqs must be 1..N with no gaps.
	for i, c := range chunks {
		if seq, _ := c["seq"].(float64); int(seq) != i+1 {
			t.Errorf("chunk %d seq: got %v, want %d", i, c["seq"], i+1)
		}
	}
	// Intermediate chunks must not carry result metadata.
	for i := 0; i < len(chunks)-1; i++ {
		if chunks[i]["more"] != true {
			t.Errorf("chunk %d more: got %v, want true", i, chunks[i]["more"])
		}
		if _, ok := chunks[i]["sha256"]; ok {
			t.Errorf("chunk %d must not carry sha256", i)
		}
	}
	// Audit row still captured for the read.
	entries := rec.snapshot()
	if len(entries) != 1 || entries[0].Action != "read" || entries[0].Status != "ok" {
		t.Errorf("audit: got %+v", entries)
	}
}

// TestReadHandler_LegacyWhenServerUnchunked verifies the fallback:
// without read_chunk in the server capabilities, a large-ish file
// (≤ MaxFileBytes) still returns a single read_result envelope.
func TestReadHandler_LegacyWhenServerUnchunked(t *testing.T) {
	dir := t.TempDir()
	const fname = "small.txt"
	const content = "legacy path still works\n"
	if err := os.WriteFile(filepath.Join(dir, fname), []byte(content), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := &recordingAudit{}
	fsys := NewFileSystem([]string{dir}, rec)
	fsys.ChunkSize = 1024 * 1024
	fsys.FsChunkMaxBytes = 512 * 1024 * 1024
	// No capabilities set → serverSupportsReadChunk stays false.

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	if err := d.Register(TypeRead, fsys.ReadHandler); err != nil {
		t.Fatalf("Register read: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":   "req-read-legacy",
		"type": "read",
		"path": filepath.Join(dir, fname),
	})
	resp := readEnvelope(t, pair)
	if resp["type"] != "read_result" {
		t.Fatalf("response type: got %q, want read_result", resp["type"])
	}
}

// TestReadHandler_FileTooLargeForChunkCap: a file above
// FsChunkMaxBytes gets a structured file_too_large read_result even
// when the server supports read_chunk.
func TestReadHandler_FileTooLargeForChunkCap(t *testing.T) {
	dir := t.TempDir()
	const fname = "huge.bin"
	// Tiny cap so we don't allocate a huge file: FsChunkMaxBytes = 1 MiB.
	if err := os.WriteFile(filepath.Join(dir, fname), make([]byte, 2*1024*1024), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := &recordingAudit{}
	fsys := NewFileSystem([]string{dir}, rec)
	fsys.ChunkSize = 1024 * 1024
	fsys.FsChunkMaxBytes = 1024 * 1024 // 1 MiB cap; 2 MiB file exceeds it
	fsys.SetServerCapabilities([]string{"read_chunk"})

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	fsys.WriteFunc = d.WriteEnvelope
	if err := d.Register(TypeRead, fsys.ReadHandler); err != nil {
		t.Fatalf("Register read: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":   "req-read-huge",
		"type": "read",
		"path": filepath.Join(dir, fname),
	})
	resp := readEnvelope(t, pair)
	if resp["type"] != "read_result" {
		t.Fatalf("response type: got %q, want read_result", resp["type"])
	}
	if resp["error"] != "file_too_large" {
		t.Errorf("error: got %v, want file_too_large", resp["error"])
	}
}

// TestWriteChunk_HappyPath feeds a chunked write through the
// dispatcher and asserts the on-disk file + write_result shape.
func TestWriteChunk_HappyPath(t *testing.T) {
	dir := t.TempDir()
	const target = "out.bin"
	// 2.5 MiB of payload at 1 MiB chunks → 3 chunks.
	payload := make([]byte, 2*1024*1024+512*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	sum := sha256.Sum256(payload)
	shex := hex.EncodeToString(sum[:])

	rec := &recordingAudit{}
	fsys := NewFileSystem([]string{dir}, rec)
	fsys.ChunkSize = 1024 * 1024
	fsys.FsChunkMaxBytes = 512 * 1024 * 1024

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	if err := d.Register(TypeWrite, fsys.WriteHandler); err != nil {
		t.Fatalf("Register write: %v", err)
	}
	if err := d.Register(TypeWriteChunk, fsys.WriteChunkHandler); err != nil {
		t.Fatalf("Register write_chunk: %v", err)
	}
	cancel, _ := runDispatcher(t, d)

	sendChunk := func(seq int, data []byte, more bool, extra map[string]any) {
		msg := map[string]any{
			"id":   "req-write-chunk",
			"type": "write_chunk",
			"seq":  seq,
			"data": base64.StdEncoding.EncodeToString(data),
			"more": more,
		}
		for k, v := range extra {
			msg[k] = v
		}
		writeServerJSON(t, pair.server, msg)
	}

	const chunk = 1024 * 1024
	n := 0
	// First chunk carries the header fields.
	extra := map[string]any{
		"path":       filepath.Join(dir, target),
		"mode":       "overwrite",
		"size_bytes": len(payload),
		"sha256":     shex,
	}
	for off := 0; off < len(payload); off += chunk {
		end := off + chunk
		if end > len(payload) {
			end = len(payload)
		}
		n++
		sendChunk(n, payload[off:end], end < len(payload), extra)
		extra = nil
	}

	// Final write_result must arrive.
	resp := readEnvelope(t, pair)
	if resp["type"] != "write_result" {
		t.Fatalf("response type: got %q, want write_result", resp["type"])
	}
	if resp["status"] != "ok" {
		t.Fatalf("status: got %v, want ok (detail=%v)", resp["status"], resp["error_detail"])
	}
	got, err := os.ReadFile(filepath.Join(dir, target))
	if err != nil {
		t.Fatalf("target missing: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("wrote %d bytes, want %d", len(got), len(payload))
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte mismatch at %d", i)
		}
	}
	// No orphan temp files left in dir.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want 1: %v", len(entries), entries)
	}
	// write_result carries the verified sha.
	if resp["sha256"] != shex {
		t.Errorf("write_result sha256: got %v, want %s", resp["sha256"], shex)
	}
	cancel()
}

// TestWriteChunk_Shamismatch errors the write and leaves no target.
func TestWriteChunk_Shamismatch(t *testing.T) {
	dir := t.TempDir()
	const target = "out.bin"
	payload := []byte("some payload bytes")
	sum := sha256.Sum256(payload)
	badSHA := hex.EncodeToString(sum[:])
	badSHA = badSHA[:len(badSHA)-2] + "00"

	rec := &recordingAudit{}
	fsys := NewFileSystem([]string{dir}, rec)
	fsys.ChunkSize = 1024 * 1024
	fsys.FsChunkMaxBytes = 512 * 1024 * 1024

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	if err := d.Register(TypeWrite, fsys.WriteHandler); err != nil {
		t.Fatalf("Register write: %v", err)
	}
	if err := d.Register(TypeWriteChunk, fsys.WriteChunkHandler); err != nil {
		t.Fatalf("Register write_chunk: %v", err)
	}
	cancel, _ := runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":         "req-write-badsha",
		"type":       "write_chunk",
		"seq":        1,
		"data":       base64.StdEncoding.EncodeToString(payload),
		"more":       false,
		"path":       filepath.Join(dir, target),
		"mode":       "overwrite",
		"size_bytes": len(payload),
		"sha256":     badSHA,
	})
	resp := readEnvelope(t, pair)
	if resp["type"] != "write_result" {
		t.Fatalf("response type: got %q, want write_result", resp["type"])
	}
	if resp["status"] != "error" || resp["error"] != "sha_mismatch" {
		t.Fatalf("got status=%v error=%v, want error/sha_mismatch", resp["status"], resp["error"])
	}
	if _, err := os.Stat(filepath.Join(dir, target)); !os.IsNotExist(err) {
		t.Errorf("target must not exist after sha_mismatch")
	}
	cancel()
}

// TestWriteChunk_SizeMismatch errors when accumulated bytes != size.
func TestWriteChunk_SizeMismatch(t *testing.T) {
	dir := t.TempDir()
	const target = "out.bin"
	payload := []byte("0123456789")

	rec := &recordingAudit{}
	fsys := NewFileSystem([]string{dir}, rec)
	fsys.FsChunkMaxBytes = 512 * 1024 * 1024
	sum := sha256.Sum256(payload)

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	if err := d.Register(TypeWrite, fsys.WriteHandler); err != nil {
		t.Fatalf("Register write: %v", err)
	}
	if err := d.Register(TypeWriteChunk, fsys.WriteChunkHandler); err != nil {
		t.Fatalf("Register write_chunk: %v", err)
	}
	cancel, _ := runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":         "req-write-badsize",
		"type":       "write_chunk",
		"seq":        1,
		"data":       base64.StdEncoding.EncodeToString(payload),
		"more":       false,
		"path":       filepath.Join(dir, target),
		"mode":       "overwrite",
		"size_bytes": 999,
		"sha256":     hex.EncodeToString(sum[:]),
	})
	resp := readEnvelope(t, pair)
	if resp["type"] != "write_result" || resp["status"] != "error" ||
		resp["error"] != "size_mismatch" {
		t.Fatalf("got %+v, want error/size_mismatch", resp)
	}
	cancel()
}

// TestWriteChunk_CreateRefusesExisting: mode=create with an existing
// target must fail without clobbering.
func TestWriteChunk_CreateRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	const target = "exists.txt"
	if err := os.WriteFile(filepath.Join(dir, target), []byte("keep me"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	payload := []byte("nope")
	sum := sha256.Sum256(payload)

	rec := &recordingAudit{}
	fsys := NewFileSystem([]string{dir}, rec)
	fsys.FsChunkMaxBytes = 512 * 1024 * 1024

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	if err := d.Register(TypeWrite, fsys.WriteHandler); err != nil {
		t.Fatalf("Register write: %v", err)
	}
	if err := d.Register(TypeWriteChunk, fsys.WriteChunkHandler); err != nil {
		t.Fatalf("Register write_chunk: %v", err)
	}
	cancel, _ := runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":         "req-write-create",
		"type":       "write_chunk",
		"seq":        1,
		"data":       base64.StdEncoding.EncodeToString(payload),
		"more":       false,
		"path":       filepath.Join(dir, target),
		"mode":       "create",
		"size_bytes": len(payload),
		"sha256":     hex.EncodeToString(sum[:]),
	})
	resp := readEnvelope(t, pair)
	if resp["type"] != "write_result" || resp["status"] != "error" ||
		resp["error"] != "file_exists" {
		t.Fatalf("got %+v, want error/file_exists", resp)
	}
	got, _ := os.ReadFile(filepath.Join(dir, target))
	if string(got) != "keep me" {
		t.Fatalf("existing target clobbered: %q", got)
	}
	cancel()
}

// TestWriteChunk_PathNotAllowed: out-of-allowlist target is rejected
// on the first chunk with path_not_allowed, no temp file created.
func TestWriteChunk_PathNotAllowed(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir() // sibling, genuinely outside the allowlist
	payload := []byte("x")
	sum := sha256.Sum256(payload)

	rec := &recordingAudit{}
	// Allowlist = dir only; target is dir/other/out.txt (sibling).
	fsys := NewFileSystem([]string{dir}, rec)
	fsys.FsChunkMaxBytes = 512 * 1024 * 1024

	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	if err := d.Register(TypeWrite, fsys.WriteHandler); err != nil {
		t.Fatalf("Register write: %v", err)
	}
	if err := d.Register(TypeWriteChunk, fsys.WriteChunkHandler); err != nil {
		t.Fatalf("Register write_chunk: %v", err)
	}
	cancel, _ := runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":         "req-write-denied",
		"type":       "write_chunk",
		"seq":        1,
		"data":       base64.StdEncoding.EncodeToString(payload),
		"more":       false,
		"path":       filepath.Join(other, "out.txt"),
		"mode":       "overwrite",
		"size_bytes": 1,
		"sha256":     hex.EncodeToString(sum[:]),
	})
	resp := readEnvelope(t, pair)
	if resp["type"] != "write_result" || resp["status"] != "error" ||
		resp["error"] != "path_not_allowed" {
		t.Fatalf("got %+v, want error/path_not_allowed", resp)
	}
	cancel()
}

var _ = audit.Entry{}

// chunkMore reads the "more" flag from a decoded read_chunk map.
func chunkMore(m map[string]any) bool {
	v, _ := m["more"].(bool)
	return v
}
