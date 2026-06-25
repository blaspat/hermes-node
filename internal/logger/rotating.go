// Package logger: rotating file writer with size-based rotation.
//
// RotatingFile is an io.WriteCloser that rotates the active log file
// when it exceeds a configured max size. Rotated files are numbered
// .1, .2, ... up to keep count; older files are removed.
package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	// DefaultMaxBytes is the rotation threshold: 10 MiB.
	DefaultMaxBytes int64 = 10 * 1024 * 1024
	// DefaultKeep is how many rotated files are retained.
	DefaultKeep = 5
)

// RotatingFile is an io.WriteCloser that writes to a file and
// automatically rotates it when it exceeds maxBytes. At most
// keep rotated files (.1, .2, ...) are retained.
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	maxBytes int64
	keep     int
}

// NewRotatingFile opens or creates path and returns a RotatingFile
// that rotates when the file exceeds maxBytes. keep controls how
// many historic .N files are retained. If maxBytes <= 0 it defaults
// to DefaultMaxBytes; if keep <= 0 it defaults to DefaultKeep.
func NewRotatingFile(path string, maxBytes int64, keep int) (*RotatingFile, error) {
	if path == "" {
		return nil, fmt.Errorf("rotating: path is required")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if keep <= 0 {
		keep = DefaultKeep
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("rotating: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("rotating: open %s: %w", path, err)
	}
	return &RotatingFile{path: path, file: f, maxBytes: maxBytes, keep: keep}, nil
}

// Write appends p to the active file. If writing would push the
// file past maxBytes the file is rotated first.
func (w *RotatingFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return 0, fmt.Errorf("rotating: writer is closed")
	}

	if err := w.maybeRotateLocked(); err != nil {
		return 0, err
	}
	return w.file.Write(p)
}

// Close closes the underlying file. Safe to call multiple times.
func (w *RotatingFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// Path returns the active file path.
func (w *RotatingFile) Path() string { return w.path }

// maybeRotateLocked checks the current file size and rotates if it
// has reached maxBytes. Caller must hold w.mu.
func (w *RotatingFile) maybeRotateLocked() error {
	info, err := w.file.Stat()
	if err != nil {
		return fmt.Errorf("rotating: stat: %w", err)
	}
	if info.Size() < w.maxBytes {
		return nil
	}
	return w.rotateLocked()
}

// rotateLocked shifts the current file out of the way. The active
// log is renamed to .1, the previous .1 to .2, and so on. Files
// beyond .keep are removed. Finally a fresh active file is opened.
// Caller holds w.mu.
func (w *RotatingFile) rotateLocked() error {
	// Close the active file so we can rename it.
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("rotating: close before rotate: %w", err)
	}
	w.file = nil

	// Walk existing rotations from oldest to newest, shifting up
	// and dropping anything beyond the keep window.
	for i := w.keep; i >= 1; i-- {
		older := w.path + "." + fmt.Sprintf("%d", i+1)
		newer := w.path + "." + fmt.Sprintf("%d", i)
		if i == w.keep {
			// Drop the oldest.
			_ = os.Remove(older)
			continue
		}
		if _, err := os.Stat(newer); err == nil {
			_ = os.Rename(newer, older)
		}
	}

	// Move the active log to .1, then reopen fresh.
	_ = os.Rename(w.path, w.path+".1")

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("rotating: reopen %s: %w", w.path, err)
	}
	w.file = f
	return nil
}
