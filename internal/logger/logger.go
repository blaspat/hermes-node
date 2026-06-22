// Package logger provides a simple leveled logger for hermes-node.
//
// Levels: DEBUG < INFO < WARN < ERROR. Messages at or above the
// configured level are written; below it they are suppressed. INFO
// and DEBUG go to stdout; WARN and ERROR go to stderr so they are
// visible in the operator's terminal even when stdout is redirected
// to a file.
//
// The zero value is a no-op logger (LevelError). Construct one with
// New or NewWithWriters.
package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

// Level controls which messages are emitted.
type Level int

const (
	// LevelDebug emits everything.
	LevelDebug Level = iota
	// LevelInfo emits info, warn, and error.
	LevelInfo
	// LevelWarn emits warn and error.
	LevelWarn
	// LevelError emits only errors.
	LevelError
)

var levelNames = map[Level]string{
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
}

// ParseLevel converts a string to a Level. Returns LevelInfo and
// false when the string is not recognised.
func ParseLevel(s string) (Level, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return LevelDebug, true
	case "INFO":
		return LevelInfo, true
	case "WARN", "WARNING":
		return LevelWarn, true
	case "ERROR":
		return LevelError, true
	default:
		return LevelInfo, false
	}
}

// Logger is a goroutine-safe leveled logger. Create one with New
// or NewWithWriters; the zero value silently drops everything at
// LevelError and above (i.e. errors are still printed).
type Logger struct {
	level Level
	info  *log.Logger
	err   *log.Logger
}

// New returns a Logger that writes to os.Stdout (debug/info) and
// os.Stderr (warn/error).
func New(level Level) *Logger {
	return NewWithWriters(level, os.Stdout, os.Stderr)
}

// NewWithWriters returns a Logger that writes info-level messages to
// infoOut and warn/error messages to errOut. Use this in tests to
// capture output, or when main.go needs to inject its own writers.
func NewWithWriters(level Level, infoOut, errOut io.Writer) *Logger {
	flags := log.Ldate | log.Ltime
	return &Logger{
		level: level,
		info:  log.New(infoOut, "", flags),
		err:   log.New(errOut, "", flags),
	}
}

// Level returns the logger's current level.
func (l *Logger) Level() Level { return l.level }

// Debug logs at DEBUG level to stdout.
func (l *Logger) Debug(format string, args ...any) {
	if l.level > LevelDebug {
		return
	}
	l.info.Output(2, "DEBUG hermes-node: "+fmt.Sprintf(format, args...))
}

// Info logs at INFO level to stdout.
func (l *Logger) Info(format string, args ...any) {
	if l.level > LevelInfo {
		return
	}
	l.info.Output(2, "INFO hermes-node: "+fmt.Sprintf(format, args...))
}

// Warn logs at WARN level to stderr.
func (l *Logger) Warn(format string, args ...any) {
	if l.level > LevelWarn {
		return
	}
	l.err.Output(2, "WARN hermes-node: "+fmt.Sprintf(format, args...))
}

// Error logs at ERROR level to stderr.
func (l *Logger) Error(format string, args ...any) {
	if l.level > LevelError {
		return
	}
	l.err.Output(2, "ERROR hermes-node: "+fmt.Sprintf(format, args...))
}

// Printf is a bridge for code that currently uses log.Printf.
// It logs at WARN level to stderr. This lets us replace bare
// log.Printf calls without changing their semantics.
func (l *Logger) Printf(format string, args ...any) {
	l.Warn(format, args...)
}

// Since logs a duration since a given start time at DEBUG level.
// Intended for ad-hoc instrumentation: defer log.Since(time.Now(), "label")
func (l *Logger) Since(start time.Time, label string) {
	l.Debug("%s took %s", label, time.Since(start).Round(time.Microsecond))
}
