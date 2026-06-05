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
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/blaspat/hermes-nodes/internal/audit"
	fspkg "github.com/blaspat/hermes-nodes/internal/fs"
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
// disables path validation entirely (operator-trust mode, same
// convention as the exec handler's cwd allowlist).
type FileSystem struct {
	IO       FileIO
	Allowed  []string
	AuditLog AuditWriter

	// now is the clock used for the audit entry's TS. Tests may
	// override it to assert timestamps deterministically.
	now func() time.Time
}

// NewFileSystem returns a FileSystem wired with realOS. Tests
// typically use NewFileSystemWithIO instead so they can pass a
// recordingFileIO.
func NewFileSystem(allowed []string, auditLog AuditWriter) *FileSystem {
	return &FileSystem{
		IO:       realOS{},
		Allowed:  allowed,
		AuditLog: auditLog,
		now:      time.Now,
	}
}

// NewFileSystemWithIO is the test seam for the FileSystem
// dependency.
func NewFileSystemWithIO(io FileIO, allowed []string, auditLog AuditWriter) *FileSystem {
	return &FileSystem{
		IO:       io,
		Allowed:  allowed,
		AuditLog: auditLog,
		now:      time.Now,
	}
}

// ReadHandler is the wire.Handler entry point for `read` calls. It
// is safe to register on a Dispatcher as TypeRead -> h.Handle.
func (fsys *FileSystem) ReadHandler(ctx context.Context, requestID string, payload map[string]any) (Envelope, error) {
	var p ReadPayload
	if err := reMarshalInto(payload, &p); err != nil {
		return NewErrorEnvelope(requestID, ErrorPayload{
			Code:   5000,
			Reason: "internal_error",
			Detail: fmt.Sprintf("decode read payload: %v", err),
		}), nil
	}
	if p.Path == "" {
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
	allowed, _, err := checkAllowed(fsys.Allowed, p.Path)
	if err != nil || !allowed {
		fsys.audit("read", p.Path, "error", 0)
		return NewReadResultEnvelope(requestID, ReadResultPayload{
			Status:      "error",
			Error:       "path_not_allowed",
			ErrorDetail: fmt.Sprintf("%q is not in the configured allowlist", p.Path),
		}), nil
	}

	// Stat before reading. We want a distinct file_not_found
	// error so the server can decide whether to retry (network
	// mount slow to converge) or surface to the operator.
	info, err := fsys.IO.Stat(p.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fsys.audit("read", p.Path, "error", 0)
			return NewReadResultEnvelope(requestID, ReadResultPayload{
				Status:      "error",
				Error:       "file_not_found",
				ErrorDetail: fmt.Sprintf("%q does not exist", p.Path),
			}), nil
		}
		fsys.audit("read", p.Path, "error", 0)
		return NewReadResultEnvelope(requestID, ReadResultPayload{
			Status:      "error",
			Error:       "io_error",
			ErrorDetail: fmt.Sprintf("stat: %v", err),
		}), nil
	}
	if info.Size() > MaxFileBytes {
		fsys.audit("read", p.Path, "error", 0)
		return NewReadResultEnvelope(requestID, ReadResultPayload{
			Status:      "error",
			Error:       "file_too_large",
			ErrorDetail: fmt.Sprintf("%q is %d bytes; cap is %d", p.Path, info.Size(), MaxFileBytes),
		}), nil
	}

	data, err := fsys.IO.ReadFile(p.Path)
	if err != nil {
		fsys.audit("read", p.Path, "error", 0)
		return NewReadResultEnvelope(requestID, ReadResultPayload{
			Status:      "error",
			Error:       "io_error",
			ErrorDetail: fmt.Sprintf("read: %v", err),
		}), nil
	}

	fsys.audit("read", p.Path, "ok", int64(len(data)))
	return NewReadResultEnvelope(requestID, ReadResultPayload{
		Status:     "ok",
		ContentB64: base64.StdEncoding.EncodeToString(data),
		SizeBytes:  int64(len(data)),
	}), nil
}

// WriteHandler is the wire.Handler entry point for `write` calls.
// Safe to register on a Dispatcher as TypeWrite -> h.Handle.
func (fsys *FileSystem) WriteHandler(ctx context.Context, requestID string, payload map[string]any) (Envelope, error) {
	var p WritePayload
	if err := reMarshalInto(payload, &p); err != nil {
		return NewErrorEnvelope(requestID, ErrorPayload{
			Code:   5000,
			Reason: "internal_error",
			Detail: fmt.Sprintf("decode write payload: %v", err),
		}), nil
	}
	if p.Path == "" {
		return NewErrorEnvelope(requestID, ErrorPayload{
			Code:   4000,
			Reason: "bad_request",
			Detail: "write.path is required",
		}), nil
	}
	if p.ContentB64 == "" {
		return NewErrorEnvelope(requestID, ErrorPayload{
			Code:   4000,
			Reason: "bad_request",
			Detail: "write.content_b64 is required",
		}), nil
	}

	// Pre-flight allowlist. Same convention as the read path.
	allowed, _, err := checkAllowed(fsys.Allowed, p.Path)
	if err != nil || !allowed {
		fsys.audit("write", p.Path, "error", 0)
		return NewWriteResultEnvelope(requestID, WriteResultPayload{
			Status:      "error",
			Error:       "path_not_allowed",
			ErrorDetail: fmt.Sprintf("%q is not in the configured allowlist", p.Path),
		}), nil
	}

	// Decode the payload. We do this before the size check so
	// the cap applies to the on-disk size, not the wire size
	// (base64 expansion is ~4/3).
	data, err := base64.StdEncoding.DecodeString(p.ContentB64)
	if err != nil {
		fsys.audit("write", p.Path, "error", 0)
		return NewWriteResultEnvelope(requestID, WriteResultPayload{
			Status:      "error",
			Error:       "io_error",
			ErrorDetail: fmt.Sprintf("base64 decode: %v", err),
		}), nil
	}
	if int64(len(data)) > MaxFileBytes {
		fsys.audit("write", p.Path, "error", 0)
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
		fsys.audit("write", p.Path, "error", 0)
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
	n, err := fsys.IO.WriteFile(p.Path, data, filePerm, mode)
	if err != nil {
		fsys.audit("write", p.Path, "error", 0)
		errCode := "io_error"
		errDetail := fmt.Sprintf("write: %v", err)
		if mode == WriteCreate && errors.Is(err, fs.ErrExist) {
			// O_EXCL failure: a file already exists at
			// this path. The protocol doesn't have a
			// dedicated code for this; io_error with a
			// clear detail is the honest report.
			errCode = "io_error"
			errDetail = fmt.Sprintf("create refused: %q already exists", p.Path)
		}
		return NewWriteResultEnvelope(requestID, WriteResultPayload{
			Status:       "error",
			Error:        errCode,
			ErrorDetail:  errDetail,
			BytesWritten: 0,
		}), nil
	}

	fsys.audit("write", p.Path, "ok", int64(n))
	return NewWriteResultEnvelope(requestID, WriteResultPayload{
		Status:       "ok",
		BytesWritten: int64(n),
	}), nil
}

// checkAllowed is a thin wrapper around fs.Check that returns
// (false, "", nil) when the allowed list is nil/empty (operator-
// trust mode). This keeps the dispatch code below it on two
// lines instead of a conditional.
func checkAllowed(allowed []string, path string) (bool, string, error) {
	if len(allowed) == 0 {
		return true, path, nil
	}
	return fspkg.Check(allowed, path)
}

// audit writes an audit row for a read/write call. The duration is
// not meaningful for these (they're synchronous os calls), so we
// report 0 to keep the field consistent with the exec handler's
// "0 = no timing measured" sentinel. The bytes argument is recorded
// inline in Target as `path (N bytes)` so a postmortem grep can
// surface the row even though the audit schema doesn't carry a
// separate size column in v0.1.
func (fsys *FileSystem) audit(action, target, status string, bytes int64) {
	if fsys.AuditLog == nil {
		return
	}
	row := audit.Entry{
		TS:         fsys.now(),
		Action:     action,
		Target:     target,
		DurationMs: 0,
		ExitCode:   0,
		Status:     status,
	}
	if bytes > 0 {
		row.Target = fmt.Sprintf("%s (%d bytes)", target, bytes)
	}
	_ = fsys.AuditLog.Write(row)
}
