// Package wire: the `read` and `write` call handlers.
//
// This file is the bridge between the dispatch loop (dispatch.go) and
// the local filesystem for the read/write capabilities. The protocol
// keeps these deliberately small (PROTOCOL.md §3.8-3.11) so the
// filesystem surface is just "give me bytes" / "take these bytes";
// every byte still has to clear the path allowlist first.
//
// For each `read` envelope the server sends, ReadHandler:
//  1. Decodes the payload into ReadPayload (path).
//  2. Validates path against the configured allowlist using
//     internal/fs.Check. A denied path becomes a read_result with
//     status=error, error=path_not_allowed.
//  3. Reads the file (capped at MaxFileBytes; larger files return
//     error=file_too_large so the caller knows to grab it some
//     other way).
//  4. Base64-encodes the bytes into the result and reports the
//     original on-disk size. PROTOCOL.md §3.9 says size_bytes is
//     the on-disk size, not the encoded length, so a server-side
//     quota check is meaningful.
//  5. Audits the call: every read attempt is logged with
//     action=read, target=path, status. Audit failures don't fail
//     the read (it already happened) but are surfaced via OnError.
//
// For each `write` envelope the server sends, WriteHandler mirrors
// the above with the appropriate flips:
//  1. Decodes WritePayload (path, content_b64, optional mode).
//  2. Validates path against the allowlist.
//  3. Decodes the base64 content and bounds the *decoded* size at
//     MaxFileBytes. A larger payload returns error=file_too_large.
//     We decode-then-check rather than check-then-decode so a
//     malicious client can't ship a 16 MB base64 blob that decodes
//     to 12 MB on disk.
//  4. Writes to disk honouring mode (overwrite / create / append).
//     "create" refuses to clobber an existing file; the other two
//     follow the obvious semantics.
//  5. Audits the call.
//
// Both handlers take an AuditWriter so tests can drive a
// recordingAudit and assert on the audit row directly.
package wire

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaspat/hermes-node/internal/audit"
	fspkg "github.com/blaspat/hermes-node/internal/fs"
)

// MaxFileBytes is the per-call cap applied to read and write. It
// matches PROTOCOL.md §3.8-3.11 (10 MB) and is the same number the
// exec handler uses for output streams so the protocol's "10 MB"
// surface is consistent across capabilities. A file larger than this
// gets a file_too_large error rather than a truncated body, so the
// server knows to fetch via a different mechanism (exec + a streamer
// tool, or pair-mode rsync, both out of scope for v0.1).
const MaxFileBytes = 10 * 1024 * 1024

// FileIO is the subset of the os package the handlers use. Defining
// it as an interface keeps the dispatch flow mockable in tests
// without touching the real filesystem. Production wiring uses
// realOS{} (defined below) which delegates straight to os.
type FileIO interface {
	Stat(path string) (os.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	// Open returns a readable stream of the file, used by the
	// chunked read path to stream large files without loading them
	// fully into memory (PROTOCOL.md §3.9a).
	Open(path string) (io.ReadCloser, error)
	WriteFile(path string, data []byte, perm os.FileMode, mode WriteMode) (int, error)
}

// WriteMode is the protocol's write.mode value, mapped to the
// package's own enum so we don't have to repeat string comparisons
// at every callsite.
type WriteMode int

const (
	// WriteOverwrite replaces the file if it exists, or creates
	// it if it doesn't. This is the protocol default.
	WriteOverwrite WriteMode = iota
	// WriteCreate creates the file and refuses to clobber an
	// existing one. The error returned is *os.PathError-like
	// (we surface a structured file_exists via error_detail).
	WriteCreate
	// WriteAppend opens (or creates) the file in append mode
	// and writes the payload after any existing content.
	WriteAppend
)

// realOS is the production FileIO. Stat is split out from ReadFile
// so the read handler can do an existence probe + size check before
// loading the bytes into memory.
type realOS struct{}

func (realOS) Stat(path string) (os.FileInfo, error) { return os.Stat(path) }
func (realOS) ReadFile(path string) ([]byte, error)  { return os.ReadFile(path) }
func (realOS) Open(path string) (io.ReadCloser, error) {
	return os.Open(path)
}
func (realOS) WriteFile(path string, data []byte, perm os.FileMode, mode WriteMode) (int, error) {
	flags := os.O_WRONLY | os.O_CREATE
	switch mode {
	case WriteOverwrite:
		flags |= os.O_TRUNC
	case WriteCreate:
		// O_EXCL with O_CREATE makes open fail if the file
		// already exists. We surface that as file_exists via
		// the WriteFile caller.
		flags |= os.O_EXCL
	case WriteAppend:
		flags |= os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, perm)
	if err != nil {
		return 0, err
	}
	n, err := f.Write(data)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return n, err
}

// FileSystem is the shared dependency for read and write handlers.
// FileIO is the os shim (mockable in tests); Allowed is the path
// allowlist; AuditLog is the audit writer. nil/empty Allowed
// rejects all paths (deny-by-default). Operators who want wide-
// open access must configure an explicit root, e.g.
// allowed_paths = ["/"].
type FileSystem struct {
	IO       FileIO
	Allowed  []string
	AuditLog AuditWriter

	// WriteFunc sends envelopes on the wire; required for the
	// chunked read path (PROTOCOL.md §3.9a). Set from the
	// dispatcher's WriteEnvelope, mirroring ExecHandler.
	WriteFunc func(ctx context.Context, env Envelope) error

	// ChunkSize is the byte size of each read_chunk / write_chunk
	// payload. Reuses the exec chunk_size config knob.
	ChunkSize int

	// FsChunkMaxBytes caps the total size of a chunked read or
	// write (0 = unlimited).
	FsChunkMaxBytes int

	// serverSupportsReadChunk is set via SetServerCapabilities when
	// the server advertises "read_chunk" in its hello_ack.
	serverSupportsReadChunk bool

	// writes tracks in-flight chunked-write assemblies, keyed by
	// request id. Populated by WriteChunkHandler as chunks arrive;
	// removed when the final chunk applies the write.
	writes map[string]*writeAssembly

	// now is the clock used for the audit entry's TS. Tests may
	// override it to assert timestamps deterministically.
	now func() time.Time
}

// writeAssembly is the in-flight state for one chunked write, keyed
// by request id. The dispatcher is single-goroutine, so no lock.
type writeAssembly struct {
	path      string
	mode      WriteMode
	sizeBytes int64
	expSHA    string
	hasher    hash.Hash
	tmp       *os.File
	tmpPath   string
	total     int64
	seq       int
}

// NewFileSystem returns a FileSystem wired with realOS.
func NewFileSystem(allowed []string, auditLog AuditWriter) *FileSystem {
	return &FileSystem{
		IO:       realOS{},
		Allowed:  allowed,
		AuditLog: auditLog,
		writes:   make(map[string]*writeAssembly),
		now:      time.Now,
	}
}

// WireHandler adapts a typed handler function (e.g. ReadHandler)
// to the generic Dispatcher callback signature.
func (fsys *FileSystem) ReadHandler(ctx context.Context, requestID string, payload map[string]any) (Envelope, error) {
	var p ReadPayload
	if err := reMarshalInto(payload, &p); err != nil {
		fsys.audit("read", "", "rejected", 0)
		return NewErrorEnvelope(requestID, ErrorPayload{
			Code:   5000,
			Reason: "internal_error",
			Detail: fmt.Sprintf("decode read payload: %v", err),
		}), nil
	}
	if p.Path == "" {
		fsys.audit("read", "", "rejected", 0)
		return NewErrorEnvelope(requestID, ErrorPayload{
			Code:   4000,
			Reason: "bad_request",
			Detail: "read.path is required",
		}), nil
	}

	// Pre-flight allowlist. Deny first, audit the denial,
	// return a structured error. The fs.Check resolution
	// collapses symlinks and non-existent paths into a
	// canonical string so a "deny first" report is meaningful
	// even when the file doesn't exist.
	allowed, canonical, err := checkAllowed(fsys.Allowed, p.Path)
	if err != nil || !allowed {
		auditTarget := p.Path
		if canonical != "" && canonical != p.Path {
			auditTarget = fmt.Sprintf("%q (resolved %q)", p.Path, canonical)
		}
		fsys.audit("read", auditTarget, "error", 0)
		errDetail := fmt.Sprintf("%q is not in the configured allowlist", p.Path)
		if canonical != "" && canonical != p.Path {
			errDetail = fmt.Sprintf("%q is not in the configured allowlist (resolved %q)", p.Path, canonical)
		}
		return NewReadResultEnvelope(requestID, ReadResultPayload{
			Status:      "error",
			Error:       "path_not_allowed",
			ErrorDetail: errDetail,
		}), nil
	}

	// Stat before reading. We want a distinct file_not_found
	// error so the server can decide whether to retry (network
	// mount slow to converge) or surface to the operator.
	// Use the canonical (symlink-resolved) path — NOT the raw
	// p.Path — to close the TOCTOU window where a symlink
	// swap between checkAllowed and Stat would bypass the
	// allowlist.
	info, err := fsys.IO.Stat(canonical)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fsys.audit("read", canonical, "error", 0)
			return NewReadResultEnvelope(requestID, ReadResultPayload{
				Status:      "error",
				Error:       "file_not_found",
				ErrorDetail: fmt.Sprintf("%q does not exist", canonical),
			}), nil
		}
		fsys.audit("read", canonical, "error", 0)
		return NewReadResultEnvelope(requestID, ReadResultPayload{
			Status:      "error",
			Error:       "io_error",
			ErrorDetail: fmt.Sprintf("stat: %v", err),
		}), nil
	}

	// Chunked read (PROTOCOL.md §3.9a): when the server advertises
	// read_chunk and the file exceeds the chunk size, stream it in
	// read_chunk envelopes instead of one base64 read_result. The
	// legacy 10 MB cap still bounds the non-chunked path.
	useChunks := fsys.serverSupportsReadChunk &&
		fsys.WriteFunc != nil && fsys.ChunkSize > 0 &&
		info.Size() > int64(fsys.ChunkSize)
	if useChunks {
		if fsys.FsChunkMaxBytes > 0 && info.Size() > int64(fsys.FsChunkMaxBytes) {
			fsys.audit("read", canonical, "error", 0)
			return NewReadResultEnvelope(requestID, ReadResultPayload{
				Status:      "error",
				Error:       "file_too_large",
				ErrorDetail: fmt.Sprintf("%q is %d bytes; chunked cap is %d", canonical, info.Size(), fsys.FsChunkMaxBytes),
			}), nil
		}
		return fsys.streamRead(ctx, requestID, canonical, info.Size())
	}

	if info.Size() > MaxFileBytes {
		fsys.audit("read", canonical, "error", 0)
		return NewReadResultEnvelope(requestID, ReadResultPayload{
			Status:      "error",
			Error:       "file_too_large",
			ErrorDetail: fmt.Sprintf("%q is %d bytes; cap is %d", canonical, info.Size(), MaxFileBytes),
		}), nil
	}

	data, err := fsys.IO.ReadFile(canonical)
	if err != nil {
		fsys.audit("read", canonical, "error", 0)
		return NewReadResultEnvelope(requestID, ReadResultPayload{
			Status:      "error",
			Error:       "io_error",
			ErrorDetail: fmt.Sprintf("read: %v", err),
		}), nil
	}

	fsys.audit("read", canonical, "ok", int64(len(data)))
	return NewReadResultEnvelope(requestID, ReadResultPayload{
		Status:     "ok",
		ContentB64: base64.StdEncoding.EncodeToString(data),
		SizeBytes:  int64(len(data)),
	}), nil
}

// streamRead opens the file and streams it as read_chunk envelopes.
// The final chunk (more=false) carries status/size/sha256. On an I/O
// error mid-stream we emit a final chunk with status=error so the
// server's assembly can be torn down cleanly. We return NoResponse so
// the dispatcher skips the normal (single) response write — we already
// sent our own result frames via WriteFunc.
func (fsys *FileSystem) streamRead(ctx context.Context, requestID, path string, size int64) (Envelope, error) {
	f, err := fsys.IO.Open(path)
	if err != nil {
		fsys.audit("read", path, "error", 0)
		_ = fsys.WriteFunc(ctx, NewReadChunkEnvelope(requestID, ReadChunkPayload{
			Seq: 1, Data: "", More: false,
			Status: "error", Error: "io_error",
			ErrorDetail: fmt.Sprintf("open: %v", err),
		}))
		return Envelope{NoResponse: true}, nil
	}
	defer f.Close()

	hash := sha256.New()
	buf := make([]byte, fsys.ChunkSize)
	seq := 0
	status := "ok"
	var errDetail string
	// sendChunk writes one read_chunk. more=false marks the last
	// data-bearing frame; a zero-byte file produces no data frames.
	sendChunk := func(data []byte, more bool) error {
		seq++
		_, _ = hash.Write(data)
		payload := ReadChunkPayload{
			Seq:  seq,
			Data: base64.StdEncoding.EncodeToString(data),
			More: more,
		}
		return fsys.WriteFunc(ctx, NewReadChunkEnvelope(requestID, payload))
	}
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			// Peek ahead cheaply: we only know "this is the last
			// frame" after the read that hit EOF, so send this
			// frame with more=true and a following empty final
			// frame when more data was still possible. The single
			// read case (file smaller than the buffer) is caught
			// by the rerr==EOF check on the next loop pass.
			if err := sendChunk(buf[:n], true); err != nil {
				status = "error"
				errDetail = fmt.Sprintf("write chunk: %v", err)
				break
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			status = "error"
			errDetail = fmt.Sprintf("read: %v", rerr)
			break
		}
	}

	final := ReadChunkPayload{
		Seq:       seq + 1,
		More:      false,
		Status:    status,
		SizeBytes: size,
		SHA256:    hex.EncodeToString(hash.Sum(nil)),
	}
	if status == "error" {
		final.Error = "io_error"
		final.ErrorDetail = errDetail
	}
	_ = fsys.WriteFunc(context.Background(), NewReadChunkEnvelope(requestID, final))
	if status == "ok" {
		fsys.audit("read", path, "ok", size)
	} else {
		fsys.audit("read", path, "error", size)
	}
	return Envelope{NoResponse: true}, nil
}

// SetServerCapabilities checks the server's advertised capabilities
// (from its hello_ack) and enables the chunked read path when
// "read_chunk" is present. Call after the handshake, mirroring
// ExecHandler.SetServerCapabilities.
func (fsys *FileSystem) SetServerCapabilities(caps []string) {
	for _, c := range caps {
		if c == "read_chunk" {
			fsys.serverSupportsReadChunk = true
			return
		}
	}
}

// WriteChunkHandler is the wire.Handler entry point for `write_chunk`
// calls (PROTOCOL.md §3.11a). The dispatcher registers it for
// TypeWriteChunk; one call handles a single chunk of one write.
//
// On the first chunk (seq=1) the path/mode/size/sha256 header fields
// open a temp file in the target's directory. Each chunk's base64
// payload is appended to that temp file. On the final chunk
// (more=false) the accumulated bytes are verified against the header's
// size + sha256, then applied atomically under the requested write
// mode; the write_result envelope is returned so the server's
// original `write` waiter resolves.
func (fsys *FileSystem) WriteChunkHandler(ctx context.Context, requestID string, payload map[string]any) (Envelope, error) {
	var p WriteChunkPayload
	if err := reMarshalInto(payload, &p); err != nil {
		fsys.audit("write", "", "rejected", 0)
		return NewWriteResultEnvelope(requestID, WriteResultPayload{
			Status:      "error",
			Error:       "io_error",
			ErrorDetail: fmt.Sprintf("decode write_chunk: %v", err),
		}), nil
	}

	asm, exists := fsys.writes[requestID]
	if !exists {
		if p.Seq != 1 {
			fsys.audit("write", p.Path, "error", 0)
			return NewWriteResultEnvelope(requestID, WriteResultPayload{
				Status:      "error",
				Error:       "bad_sequence",
				ErrorDetail: fmt.Sprintf("first write_chunk has seq %d, want 1", p.Seq),
			}), nil
		}
		if p.Path == "" || p.SHA256 == "" {
			fsys.audit("write", "", "rejected", 0)
			return NewWriteResultEnvelope(requestID, WriteResultPayload{
				Status:      "error",
				Error:       "bad_request",
				ErrorDetail: "write_chunk seq=1 requires path and sha256",
			}), nil
		}
		// Pre-flight allowlist — deny before allocating the temp
		// file, same convention as the legacy write path.
		allowed, canonical, err := checkAllowed(fsys.Allowed, p.Path)
		if err != nil || !allowed {
			auditTarget := p.Path
			if canonical != "" && canonical != p.Path {
				auditTarget = fmt.Sprintf("%q (resolved %q)", p.Path, canonical)
			}
			fsys.audit("write", auditTarget, "error", 0)
			return NewWriteResultEnvelope(requestID, WriteResultPayload{
				Status:      "error",
				Error:       "path_not_allowed",
				ErrorDetail: fmt.Sprintf("%q is not in the configured allowlist", auditTarget),
			}), nil
		}
		// Cap the total transfer up front so we don't stream a
		// 10 GB file before the size check.
		if fsys.FsChunkMaxBytes > 0 && p.SizeBytes > int64(fsys.FsChunkMaxBytes) {
			fsys.audit("write", canonical, "error", 0)
			return NewWriteResultEnvelope(requestID, WriteResultPayload{
				Status:      "error",
				Error:       "file_too_large",
				ErrorDetail: fmt.Sprintf("%q is %d bytes; chunked cap is %d", canonical, p.SizeBytes, fsys.FsChunkMaxBytes),
			}), nil
		}
		// The temp file lives in the target's directory so the
		// final apply can be an atomic same-filesystem rename.
		// (The legacy single-shot write uses the open flags from
		// WriteFile; the chunked path assembles first, then moves.)
		tmp, err := os.CreateTemp(filepath.Dir(canonical), ".hermes-write-*")
		if err != nil {
			fsys.audit("write", canonical, "error", 0)
			return NewWriteResultEnvelope(requestID, WriteResultPayload{
				Status:      "error",
				Error:       "io_error",
				ErrorDetail: fmt.Sprintf("create temp: %v", err),
			}), nil
		}
		asm = &writeAssembly{
			path:      canonical,
			mode:      writeModeFor(p.Mode),
			sizeBytes: p.SizeBytes,
			expSHA:    p.SHA256,
			hasher:    sha256.New(),
			tmp:       tmp,
			tmpPath:   tmp.Name(),
		}
		fsys.writes[requestID] = asm
	} else {
		if p.Seq != asm.seq+1 {
			fsys.abortWrite(requestID, asm)
			fsys.audit("write", asm.path, "error", 0)
			return NewWriteResultEnvelope(requestID, WriteResultPayload{
				Status:      "error",
				Error:       "bad_sequence",
				ErrorDetail: fmt.Sprintf("write_chunk seq %d, want %d", p.Seq, asm.seq+1),
			}), nil
		}
	}
	asm.seq = p.Seq

	// Append this chunk's bytes to the temp file + hasher.
	if p.Data != "" {
		data, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			fsys.abortWrite(requestID, asm)
			fsys.audit("write", asm.path, "error", 0)
			return NewWriteResultEnvelope(requestID, WriteResultPayload{
				Status:      "error",
				Error:       "bad_base64",
				ErrorDetail: fmt.Sprintf("chunk seq %d: %v", p.Seq, err),
			}), nil
		}
		if _, err := asm.tmp.Write(data); err != nil {
			fsys.abortWrite(requestID, asm)
			fsys.audit("write", asm.path, "error", 0)
			return NewWriteResultEnvelope(requestID, WriteResultPayload{
				Status:      "error",
				Error:       "io_error",
				ErrorDetail: fmt.Sprintf("write temp: %v", err),
			}), nil
		}
		_, _ = asm.hasher.Write(data)
		asm.total += int64(len(data))
	}

	if p.More {
		// More chunks coming — keep the assembly open. No response.
		return Envelope{NoResponse: true}, nil
	}

	// Final chunk: verify + apply.
	sum := hex.EncodeToString(asm.hasher.Sum(nil))
	if asm.total != asm.sizeBytes {
		fsys.abortWrite(requestID, asm)
		fsys.audit("write", asm.path, "error", 0)
		return NewWriteResultEnvelope(requestID, WriteResultPayload{
			Status:      "error",
			Error:       "size_mismatch",
			ErrorDetail: fmt.Sprintf("got %d bytes, want %d", asm.total, asm.sizeBytes),
		}), nil
	}
	if !strings.EqualFold(sum, asm.expSHA) {
		fsys.abortWrite(requestID, asm)
		fsys.audit("write", asm.path, "error", 0)
		return NewWriteResultEnvelope(requestID, WriteResultPayload{
			Status:      "error",
			Error:       "sha_mismatch",
			ErrorDetail: fmt.Sprintf("computed %s, want %s", sum, asm.expSHA),
		}), nil
	}

	applyErr, bytes := fsys.applyChunkedWrite(asm)
	if applyErr != "" {
		fsys.audit("write", asm.path, "error", 0)
		return NewWriteResultEnvelope(requestID, WriteResultPayload{
			Status:      "error",
			Error:       applyErr,
			ErrorDetail: fmt.Sprintf("apply %q: %s", asm.path, applyErr),
		}), nil
	}
	fsys.audit("write", asm.path, "ok", bytes)
	return NewWriteResultEnvelope(requestID, WriteResultPayload{
		Status:       "ok",
		BytesWritten: bytes,
		SHA256:       sum,
	}), nil
}

// writeModeFor maps the protocol's write.mode string to the package
// enum. Mirrors the inline switch in the legacy write path; an
// unknown value falls back to overwrite (the protocol default).
func writeModeFor(s string) WriteMode {
	switch s {
	case "create":
		return WriteCreate
	case "append":
		return WriteAppend
	case "", "overwrite":
		return WriteOverwrite
	default:
		return WriteOverwrite
	}
}

// abortWrite tears down a chunked-write assembly on any error,
// removing the temp file and the registry entry so a retry starts
// clean.
func (fsys *FileSystem) abortWrite(requestID string, asm *writeAssembly) {
	if asm.tmp != nil {
		_ = asm.tmp.Close()
		asm.tmp = nil
	}
	if asm.tmpPath != "" {
		_ = os.Remove(asm.tmpPath)
	}
	delete(fsys.writes, requestID)
}

// applyChunkedWrite moves the assembled temp file into place under
// the requested mode. The temp file lives in the target's directory,
// so overwrite/create are atomic renames (zero extra memory); only
// append has to stream the temp back through the target, since it
// concatenates onto existing content.
func (fsys *FileSystem) applyChunkedWrite(asm *writeAssembly) (string, int64) {
	_ = asm.tmp.Close()
	switch asm.mode {
	case WriteOverwrite, WriteCreate:
		// Create refuses to clobber an existing target (the legacy
		// write path's file_exists semantics).
		if asm.mode == WriteCreate {
			if _, err := fsys.IO.Stat(asm.path); err == nil {
				_ = os.Remove(asm.tmpPath)
				return "file_exists", 0
			}
		}
		if err := os.Rename(asm.tmpPath, asm.path); err != nil {
			_ = os.Remove(asm.tmpPath)
			return "io_error", 0
		}
		return "", asm.total
	case WriteAppend:
		// Append streams the temp onto the existing target. This is
		// the only mode that needs the bytes in memory; it's also
		// the rare one.
		// ponytail: temp loaded whole — fine because append targets are
		// usually log-sized; if someone appends 512 MiB to a log, that
		// path allocates 512 MiB. Swap for io.Copy(temp→open target) if
		// it ever matters.
		data, err := os.ReadFile(asm.tmpPath)
		if err != nil {
			_ = os.Remove(asm.tmpPath)
			return "io_error", 0
		}
		_ = os.Remove(asm.tmpPath)
		n, err := fsys.IO.WriteFile(asm.path, data, fsys.permForTarget(asm.path), WriteAppend)
		if err != nil {
			return "io_error", 0
		}
		return "", int64(n)
	default:
		_ = os.Remove(asm.tmpPath)
		return "io_error", 0
	}
}

// permForTarget returns the permission bits to use when (re)creating
// the target. Mirrors the legacy write path's "follow the existing
// file, else 0644" convention.
func (fsys *FileSystem) permForTarget(path string) os.FileMode {
	if info, err := fsys.IO.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0o644
}

// WriteHandler is the wire.Handler entry point for `write` calls.
// Safe to register on a Dispatcher as TypeWrite -> h.Handle.
func (fsys *FileSystem) WriteHandler(ctx context.Context, requestID string, payload map[string]any) (Envelope, error) {
	var p WritePayload
	if err := reMarshalInto(payload, &p); err != nil {
		fsys.audit("write", "", "rejected", 0)
		return NewErrorEnvelope(requestID, ErrorPayload{
			Code:   5000,
			Reason: "internal_error",
			Detail: fmt.Sprintf("decode write payload: %v", err),
		}), nil
	}
	if p.Path == "" {
		fsys.audit("write", "", "rejected", 0)
		return NewErrorEnvelope(requestID, ErrorPayload{
			Code:   4000,
			Reason: "bad_request",
			Detail: "write.path is required",
		}), nil
	}
	if p.ContentB64 == "" {
		fsys.audit("write", "", "rejected", 0)
		return NewErrorEnvelope(requestID, ErrorPayload{
			Code:   4000,
			Reason: "bad_request",
			Detail: "write.content_b64 is required",
		}), nil
	}

	// Pre-flight allowlist. Same convention as the read path.
	allowed, canonical, err := checkAllowed(fsys.Allowed, p.Path)
	if err != nil || !allowed {
		auditTarget := p.Path
		if canonical != "" && canonical != p.Path {
			auditTarget = fmt.Sprintf("%q (resolved %q)", p.Path, canonical)
		}
		fsys.audit("write", auditTarget, "error", 0)
		errDetail := fmt.Sprintf("%q is not in the configured allowlist", p.Path)
		if canonical != "" && canonical != p.Path {
			errDetail = fmt.Sprintf("%q is not in the configured allowlist (resolved %q)", p.Path, canonical)
		}
		return NewWriteResultEnvelope(requestID, WriteResultPayload{
			Status:      "error",
			Error:       "path_not_allowed",
			ErrorDetail: errDetail,
		}), nil
	}

	// Decode the payload. We do this before the size check so
	// the cap applies to the on-disk size, not the wire size
	// (base64 expansion is ~4/3).
	data, err := base64.StdEncoding.DecodeString(p.ContentB64)
	if err != nil {
		fsys.audit("write", canonical, "error", 0)
		return NewWriteResultEnvelope(requestID, WriteResultPayload{
			Status:      "error",
			Error:       "io_error",
			ErrorDetail: fmt.Sprintf("base64 decode: %v", err),
		}), nil
	}
	if int64(len(data)) > MaxFileBytes {
		fsys.audit("write", canonical, "error", 0)
		return NewWriteResultEnvelope(requestID, WriteResultPayload{
			Status:      "error",
			Error:       "file_too_large",
			ErrorDetail: fmt.Sprintf("decoded payload is %d bytes; cap is %d", len(data), MaxFileBytes),
		}), nil
	}

	mode := WriteOverwrite
	switch p.Mode {
	case "":
		// Protocol default is overwrite; keep that the
		// zero-value behaviour so an unset mode field
		// behaves correctly.
	case "overwrite":
		mode = WriteOverwrite
	case "create":
		mode = WriteCreate
	case "append":
		mode = WriteAppend
	default:
		fsys.audit("write", canonical, "error", 0)
		return NewWriteResultEnvelope(requestID, WriteResultPayload{
			Status:      "error",
			Error:       "io_error",
			ErrorDetail: fmt.Sprintf("unknown mode %q (want create, overwrite, or append)", p.Mode),
		}), nil
	}

	// Pick a file mode. The path allowlist already covers
	// operator intent for "where"; 0o644 is the standard
	// "user-writable, world-readable" Unix default and the
	// protocol doesn't carry a permissions field, so we
	// follow the platform convention. Append/create both
	// need write+read for the user, and we don't try to
	// create a directory tree — a write to a non-existent
	// parent returns ENOENT and surfaces as io_error.
	const filePerm os.FileMode = 0o644
	n, err := fsys.IO.WriteFile(canonical, data, filePerm, mode)
	if err != nil {
		fsys.audit("write", canonical, "error", int64(0))
		errCode := "io_error"
		errDetail := fmt.Sprintf("write: %v", err)
		if mode == WriteCreate && errors.Is(err, fs.ErrExist) {
			// O_EXCL failure: a file already exists at
			// this path. The protocol doesn't have a
			// dedicated code for this; io_error with a
			// clear detail is the honest report.
			errCode = "io_error"
			errDetail = fmt.Sprintf("create refused: %q already exists", canonical)
		}
		return NewWriteResultEnvelope(requestID, WriteResultPayload{
			Status:       "error",
			Error:        errCode,
			ErrorDetail:  errDetail,
			BytesWritten: 0,
		}), nil
	}

	fsys.audit("write", canonical, "ok", int64(n))
	return NewWriteResultEnvelope(requestID, WriteResultPayload{
		Status:       "ok",
		BytesWritten: int64(n),
	}), nil
}

// checkAllowed is a thin wrapper around fs.Check that returns
// (false, path, fs.ErrNotAllowed) when the allowed list is nil/empty
// (deny-by-default). Operators who want wide-open access must
// configure an explicit root, e.g. allowed_paths = ["/"].
func checkAllowed(allowed []string, path string) (bool, string, error) {
	if len(allowed) == 0 {
		return false, path, fspkg.ErrNotAllowed
	}
	return fspkg.Check(allowed, path)
}

// audit writes an audit row for a read/write call. The duration is
// not meaningful for these (they're synchronous os calls), so we
// report 0 to keep the field consistent with the exec handler's
// "0 = no timing measured" sentinel. The bytes argument is recorded
// inline in Target as `path (N bytes)` so a postmortem grep can
// surface the row even when the event was a zero-byte write
// (which may signal tampering or a config reset).
func (fsys *FileSystem) audit(action, target, status string, bytes int64) {
	if fsys.AuditLog == nil {
		return
	}
	row := audit.Entry{
		TS:         fsys.now(),
		Action:     action,
		Target:     fmt.Sprintf("%s (%d bytes)", target, bytes),
		DurationMs: 0,
		ExitCode:   0,
		Status:     status,
	}
	_ = fsys.AuditLog.Write(row)
}
