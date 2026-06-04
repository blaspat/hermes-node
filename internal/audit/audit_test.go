// Package audit tests — exercises Write round-trip and rotation.
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func sampleEntry(action, target string, durationMs int64, exitCode int, status string) Entry {
	return Entry{
		TS:         time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
		Action:     action,
		Target:     target,
		DurationMs: durationMs,
		ExitCode:   exitCode,
		Status:     status,
	}
}

// TestWriteRoundTrip writes 3 entries to a fresh log and reads them back
// as JSONL, asserting the on-disk shape matches the struct.
func TestWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	w, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	entries := []Entry{
		sampleEntry("exec", "ls -la", 42, 0, "ok"),
		sampleEntry("read", "/home/patrick/note.txt", 5, 0, "ok"),
		sampleEntry("exec", "rm -rf /tmp/x", 12, 1, "error"),
	}
	for _, e := range entries {
		if err := w.Write(e); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// Read the file back line-by-line and assert JSON shape.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	var got []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		got = append(got, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(got) != len(entries) {
		t.Fatalf("entry count: got %d, want %d", len(got), len(entries))
	}
	for i := range entries {
		if !got[i].TS.Equal(entries[i].TS) {
			t.Errorf("[%d] ts: got %v, want %v", i, got[i].TS, entries[i].TS)
		}
		if got[i].Action != entries[i].Action {
			t.Errorf("[%d] action: got %q, want %q", i, got[i].Action, entries[i].Action)
		}
		if got[i].Target != entries[i].Target {
			t.Errorf("[%d] target: got %q, want %q", i, got[i].Target, entries[i].Target)
		}
		if got[i].DurationMs != entries[i].DurationMs {
			t.Errorf("[%d] duration_ms: got %d, want %d", i, got[i].DurationMs, entries[i].DurationMs)
		}
		if got[i].ExitCode != entries[i].ExitCode {
			t.Errorf("[%d] exit_code: got %d, want %d", i, got[i].ExitCode, entries[i].ExitCode)
		}
		if got[i].Status != entries[i].Status {
			t.Errorf("[%d] status: got %q, want %q", i, got[i].Status, entries[i].Status)
		}
	}
}

// TestWriteAppendsAcrossInstances ensures that reopening the log appends
// to the existing file (not truncates it) — critical for an append-only
// audit log.
func TestWriteAppendsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	w, err := New(path)
	if err != nil {
		t.Fatalf("New 1: %v", err)
	}
	if err := w.Write(sampleEntry("exec", "echo 1", 1, 0, "ok")); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}

	w2, err := New(path)
	if err != nil {
		t.Fatalf("New 2: %v", err)
	}
	defer w2.Close()
	if err := w2.Write(sampleEntry("exec", "echo 2", 1, 0, "ok")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}

	// Count non-empty lines in the file.
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 lines after reopen+write, got %d (file=%q)", len(lines), contents)
	}
}

// TestRotationShiftsAndCaps ensures that when the log exceeds maxBytes,
// a new file is started, the old one becomes audit.log.1, and any older
// rotations are bumped (and the oldest is dropped past keep).
func TestRotationShiftsAndCaps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	// maxBytes=100, keep=2: after the second rotation we should have
	// active (c) + .1 (b) + .2 (a) = 3 files total.
	w, err := newWithLimits(path, 100, 2)
	if err != nil {
		t.Fatalf("newWithLimits: %v", err)
	}
	defer w.Close()

	// Each line is roughly 100+ bytes — rotation kicks in on the 2nd
	// write, and the 3rd write triggers a second shift.
	payload := strings.Repeat("a", 60) // inflates the target field

	write := func(action string) {
		e := Entry{
			TS:         time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
			Action:     action,
			Target:     payload,
			DurationMs: 1,
			ExitCode:   0,
			Status:     "ok",
		}
		if err := w.Write(e); err != nil {
			t.Fatalf("Write %s: %v", action, err)
		}
	}

	write("a")
	write("b") // forces first rotation: active(a) -> .1, fresh active gets b
	write("c") // forces second rotation: .1(a) -> .2, active(b) -> .1, fresh active gets c

	// On disk we expect: audit.log (just c), audit.log.1 (b), audit.log.2 (a).
	// Nothing past .2 should exist.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected %s.1 to exist: %v", path, err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf("expected %s.2 to exist: %v", path, err)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("expected %s.3 to NOT exist, got err=%v", path, err)
	}

	// Sanity: the current log contains only the most recent write's action.
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(contents), `"action":"c"`) {
		t.Errorf("expected current log to contain action c, got: %s", contents)
	}
}

// TestWriteAfterClose ensures that writing to a closed Writer returns
// an error rather than panicking — the kind of failure mode an
// integration test in a deferred-execution path would hit.
func TestWriteAfterClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	w, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Write(sampleEntry("exec", "echo", 1, 0, "ok")); err == nil {
		t.Fatal("expected error writing to closed writer, got nil")
	}
	// Second Close must be a no-op (not a panic, not a new error).
	if err := w.Close(); err != nil {
		t.Errorf("double Close should be a no-op, got: %v", err)
	}
}

// TestNewRejectsBadArgs covers the input-validation branches.
func TestNewRejectsBadArgs(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Error("New(\"\") should fail, got nil")
	}
	if _, err := newWithLimits("/tmp/x", 0, 5); err == nil {
		t.Error("newWithLimits(maxBytes=0) should fail, got nil")
	}
	if _, err := newWithLimits("/tmp/x", 100, -1); err == nil {
		t.Error("newWithLimits(keep=-1) should fail, got nil")
	}
}

// TestRotationKeepsExactlyKeepFiles bounds-checks: after many rotations we
// should have exactly keep+1 files (current + keep rotations).
func TestRotationKeepsExactlyKeepFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	keep := 3

	w, err := newWithLimits(path, 50, keep)
	if err != nil {
		t.Fatalf("newWithLimits: %v", err)
	}
	defer w.Close()

	for i := 0; i < 20; i++ {
		e := Entry{
			TS:     time.Date(2026, 6, 4, 12, 0, 0, i, time.UTC),
			Action: "exec",
			Target: strings.Repeat("x", 30),
		}
		if err := w.Write(e); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	matches, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	sort.Strings(matches)
	// Expect: audit.log + audit.log.1 + audit.log.2 + audit.log.3 = 4
	want := keep + 1
	if len(matches) != want {
		t.Errorf("file count: got %d, want %d (files=%v)", len(matches), want, matches)
	}
}
