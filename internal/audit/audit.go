// Package audit writes the node's append-only JSONL audit log.
//
// Each entry is a single JSON object terminated by '\n'. The writer
// appends in O_APPEND mode so multiple processes (or a crash + restart)
// cannot interleave partial records. When the active log exceeds
// maxBytes, it is rotated: the previous .1, .2, ... are shifted up, and
// the active log is renamed to .1. At most keep rotated files are
// retained, so disk usage is bounded by (keep+1) * maxBytes.
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// DefaultMaxBytes is the rotation threshold: 10 MiB.
	DefaultMaxBytes int64 = 10 * 1024 * 1024
	// DefaultKeep is how many rotated files are retained.
	DefaultKeep = 5
)

// Entry is one row in the JSONL audit log. TS is encoded as RFC3339
// (e.g. "2026-06-04T12:00:00Z") so the log is grep-friendly and decouples
// the on-disk format from Go's time.Time JSON representation.
type Entry struct {
	TS         time.Time `json:"ts"`
	Action     string    `json:"action"`
	Target     string    `json:"target"`
	DurationMs int64     `json:"duration_ms"`
	ExitCode   int       `json:"exit_code"`
	Status     string    `json:"status"`
}

// Writer is a goroutine-safe append-only JSONL audit logger with size-based
// rotation. The zero value is not usable; construct one with New or
// newWithLimits.
type Writer struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	maxBytes int64
	keep     int
}

// New returns a Writer that appends to path. The file and any parent
// directories are created if they do not already exist. The writer uses
// DefaultMaxBytes and DefaultKeep for rotation. The caller must call
// Close when done.
func New(path string) (*Writer, error) {
	return newWithLimits(path, DefaultMaxBytes, DefaultKeep)
}

// newWithLimits is the test seam for rotation thresholds. The log file
// is created with mode 0600 because audit entries can carry sensitive
// operational data (paths, commands, exit codes) and should not be
// world-readable on a multi-user host.
func newWithLimits(path string, maxBytes int64, keep int) (*Writer, error) {
	if path == "" {
		return nil, fmt.Errorf("audit: path is required")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("audit: maxBytes must be > 0, got %d", maxBytes)
	}
	if keep < 0 {
		return nil, fmt.Errorf("audit: keep must be >= 0, got %d", keep)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	return &Writer{path: path, file: f, maxBytes: maxBytes, keep: keep}, nil
}

// Write appends one entry as a single JSON object + '\n'. The write is
// serialised by an internal mutex so callers from multiple goroutines
// will not interleave. If appending would push the file past maxBytes,
// the writer rotates first.
func (w *Writer) Write(e Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.maybeRotateLocked(); err != nil {
		return err
	}

	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("audit: marshal entry: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.file.Write(data); err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	// Sync so the entry survives a crash or power loss. Skipping this
	// would mean a successful Write() return value is a lie for an
	// audit log — exactly the kind of silent failure the spec exists
	// to prevent.
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("audit: sync: %w", err)
	}
	return nil
}

// Close flushes and closes the underlying file. It is safe to call
// exactly once; calling on a nil-receiver or already-closed writer is
// safe and returns nil.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// maybeRotateLocked checks the current file size and rotates if it has
// reached maxBytes. Caller must hold w.mu.
func (w *Writer) maybeRotateLocked() error {
	if w.file == nil {
		return fmt.Errorf("audit: writer is closed")
	}
	info, err := w.file.Stat()
	if err != nil {
		return fmt.Errorf("audit: stat: %w", err)
	}
	if info.Size() < w.maxBytes {
		return nil
	}
	return w.rotateLocked()
}

// rotateLocked shifts the current file out of the way. The active log is
// renamed to .1, the previous .1 to .2, and so on. Files beyond .keep
// are removed. Finally a fresh active file is opened. Caller holds w.mu.
func (w *Writer) rotateLocked() error {
	// Close the active file so we can rename it on every platform.
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("audit: close before rotate: %w", err)
	}
	w.file = nil

	// Walk the existing rotations from oldest to newest, shifting up
	// and dropping anything beyond the keep window. The new .1 slot
	// is freed by the final shift loop iteration.
	for i := w.keep; i >= 1; i-- {
		older := w.path + "." + fmt.Sprintf("%d", i+1)
		newer := w.path + "." + fmt.Sprintf("%d", i)
		if i == w.keep {
			// Drop the oldest if it exists.
			if err := os.Remove(older); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("audit: remove %s: %w", older, err)
			}
			continue
		}
		if _, err := os.Stat(newer); err == nil {
			if err := os.Rename(newer, older); err != nil {
				return fmt.Errorf("audit: rename %s -> %s: %w", newer, older, err)
			}
		}
	}

	// Move the active log to .1, then reopen the active log fresh.
	if err := os.Rename(w.path, w.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("audit: rename %s -> %s.1: %w", w.path, w.path+".1", err)
	}

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("audit: reopen %s: %w", w.path, err)
	}
	w.file = f
	return nil
}
