package logger

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  Level
		ok    bool
	}{
		{"debug", LevelDebug, true},
		{"DEBUG", LevelDebug, true},
		{"Debug", LevelDebug, true},
		{"info", LevelInfo, true},
		{"INFO", LevelInfo, true},
		{"warn", LevelWarn, true},
		{"WARN", LevelWarn, true},
		{"warning", LevelWarn, true},
		{"WARNING", LevelWarn, true},
		{"error", LevelError, true},
		{"ERROR", LevelError, true},
		{"", LevelInfo, false},
		{"bogus", LevelInfo, false},
		{"trace", LevelInfo, false},
	}
	for _, tt := range tests {
		got, ok := ParseLevel(tt.input)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ParseLevel(%q) = (%v, %v), want (%v, %v)", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

func TestLevelFiltering(t *testing.T) {
	tests := []struct {
		name     string
		level    Level
		logFn    func(l *Logger)
		want     int // expected occurrences of the message substring
		message  string
	}{
		{
			name:    "debug suppressed at INFO",
			level:   LevelInfo,
			logFn:   func(l *Logger) { l.Debug("hidden") },
			want:    0,
			message: "hidden",
		},
		{
			name:    "info visible at INFO",
			level:   LevelInfo,
			logFn:   func(l *Logger) { l.Info("hello") },
			want:    1,
			message: "hello",
		},
		{
			name:    "debug visible at DEBUG",
			level:   LevelDebug,
			logFn:   func(l *Logger) { l.Debug("verbose") },
			want:    1,
			message: "verbose",
		},
		{
			name:    "info visible at DEBUG",
			level:   LevelDebug,
			logFn:   func(l *Logger) { l.Info("all good") },
			want:    1,
			message: "all good",
		},
		{
			name:    "warn visible at WARN",
			level:   LevelWarn,
			logFn:   func(l *Logger) { l.Warn("careful") },
			want:    1,
			message: "careful",
		},
		{
			name:    "warn visible at ERROR",
			level:   LevelError,
			logFn:   func(l *Logger) { l.Warn("hidden") },
			want:    0,
			message: "hidden",
		},
		{
			name:    "error visible at ERROR",
			level:   LevelError,
			logFn:   func(l *Logger) { l.Error("boom") },
			want:    1,
			message: "boom",
		},
		{
			name:    "info suppressed at WARN",
			level:   LevelWarn,
			logFn:   func(l *Logger) { l.Info("silent") },
			want:    0,
			message: "silent",
		},
		{
			name:    "info suppressed at ERROR",
			level:   LevelError,
			logFn:   func(l *Logger) { l.Info("gone") },
			want:    0,
			message: "gone",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := NewWithWriters(tt.level, &buf, &buf)
			tt.logFn(l)
			got := strings.Count(buf.String(), tt.message)
			if got != tt.want {
				t.Errorf("expected %d occurrence(s) of %q, got %d; output=%q", tt.want, tt.message, got, buf.String())
			}
		})
	}
}

func TestStdoutVsStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	l := NewWithWriters(LevelInfo, &stdout, &stderr)

	l.Info("hello")
	l.Warn("warning")
	l.Error("error")
	l.Debug("verbose")

	if stdout.Len() == 0 {
		t.Error("expected stdout to have output")
	}
	if stderr.Len() == 0 {
		t.Error("expected stderr to have output")
	}
	if !strings.Contains(stdout.String(), "hello") {
		t.Errorf("stdout missing 'hello': %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Errorf("stderr missing 'warning': %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "error") {
		t.Errorf("stderr missing 'error': %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "verbose") {
		t.Errorf("stdout should not contain debug: %q", stdout.String())
	}
}

func TestLevelPrefix(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithWriters(LevelDebug, &buf, &buf)
	l.Info("info msg")
	l.Warn("warn msg")
	l.Error("error msg")
	l.Debug("debug msg")

	out := buf.String()
	for _, want := range []string{"DEBUG", "INFO", "WARN", "ERROR"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing level prefix %q: %q", want, out)
		}
	}
}

func TestPrintf(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithWriters(LevelWarn, &buf, &buf)
	l.Printf("bridge %d", 42)

	if !strings.Contains(buf.String(), "bridge 42") {
		t.Errorf("Printf output missing message: %q", buf.String())
	}
}

func TestSetLevel(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithWriters(LevelError, &buf, &buf)

	// At ERROR level, debug/info/warn should be suppressed.
	l.Info("should be hidden")
	if strings.Contains(buf.String(), "hidden") {
		t.Errorf("info message visible at ERROR level")
	}

	// After switching to DEBUG, all messages are visible.
	l.SetLevel(LevelDebug)
	l.Debug("now visible")
	if !strings.Contains(buf.String(), "visible") {
		t.Errorf("debug message not visible after SetLevel(DEBUG)")
	}
}
