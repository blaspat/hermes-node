package exec

import (
	"os"
	"path/filepath"
	"strings"
)

// Sandbox is the set of paths a sandboxed shell may touch.
//
// Write roots get read+write; read roots get read only. HomeDir, when set,
// is denied outright for both and only re-opened through the roots above —
// that is what stops a command from reading ~/.ssh or ~/Downloads just
// because the process happens to run as the user.
type Sandbox struct {
	HomeDir string
	Read    []string
	Write   []string
}

// sandboxWriteDefaults are the directories toolchains must write to for a
// sandboxed shell to stay usable: temp space and build caches. None of them
// hold source, so allowing writes there does not weaken the allowlist for
// code.
func sandboxWriteDefaults() []string {
	roots := []string{os.TempDir(), "/private/tmp"}
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		roots = append(roots, cache)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, d := range []string{".m2", ".npm", ".gradle", ".cache", ".yarn", ".cargo", "go"} {
			roots = append(roots, filepath.Join(home, d))
		}
	}
	return roots
}

// sandboxReadDefaults are the paths under $HOME that git, maven, npm, go and
// the language toolchains themselves read on every run. Denying these would
// make the sandbox useless for real work, and none of them are project
// source or user data.
//
// The toolchain entries are not optional niceties: on a typical macOS dev
// box the JDK lives in ~/Library/Java, Homebrew in ~/homebrew (or
// ~/opt/homebrew), and node/python/ruby runtimes under ~/.nvm, ~/.pyenv,
// ~/.rbenv. Denying them does not harden anything — they are binaries — it
// just makes `java`, `go` and `node` fail to launch.
func sandboxReadDefaults() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	dirs := []string{
		// Config the VCS and build tools read on every invocation.
		".gitconfig", ".git-credentials", ".ssh", ".netrc", ".npmrc", ".yarnrc", ".config", ".local",
		// Toolchain install roots. Binaries, not user data.
		"Library/Java", "homebrew", "opt/homebrew",
		".sdkman", ".nvm", ".volta", ".asdf", ".pyenv", ".rbenv", ".rustup",
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, filepath.Join(home, d))
	}
	return out
}

// sandboxDeviceFiles are the character devices a shell writes to routinely.
// A bare `(deny file-write*)` blocks these too, which breaks `2>/dev/null`
// everywhere.
var sandboxDeviceFiles = []string{
	"/dev/null",
	"/dev/tty",
	"/dev/stdout",
	"/dev/stderr",
	"/dev/dtracehelper",
}

// sandboxDenyReadRoots are the locations that hold user data. Reads there
// are denied unless the path is explicitly allowed. System trees (/usr,
// /System, /Library, /bin, ...) stay readable on purpose: denying them
// breaks dyld and every toolchain while protecting nothing the user cares
// about.
var sandboxDenyReadRoots = []string{"/Volumes"}

// SandboxProfile renders a seatbelt (sandbox-exec) profile.
//
// Writes are denied everywhere except the write roots. Reads are denied
// under HomeDir and the user-data roots, then re-opened for the read roots.
// Rules are evaluated last-match-wins per operation, so the allow lists must
// come after the denies.
func SandboxProfile(sb Sandbox) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")

	b.WriteString("(deny file-write*)\n(allow file-write*\n")
	for _, r := range sb.Write {
		if r == "" {
			continue
		}
		b.WriteString("  (subpath \"" + escapeSeatbelt(r) + "\")\n")
	}
	for _, d := range sandboxDeviceFiles {
		b.WriteString("  (literal \"" + escapeSeatbelt(d) + "\")\n")
	}
	b.WriteString("  (subpath \"/dev/fd\")\n)\n")

	denyRead := append([]string{}, sandboxDenyReadRoots...)
	if sb.HomeDir != "" {
		denyRead = append([]string{sb.HomeDir}, denyRead...)
	}
	b.WriteString("(deny file-read*")
	for _, r := range denyRead {
		b.WriteString("\n  (subpath \"" + escapeSeatbelt(r) + "\")")
	}
	b.WriteString(")\n")

	// Denying $HOME outright also denies the directory itself, which breaks
	// path traversal: getcwd() walks up stat-ing every parent, so a shell
	// sitting in an allowed project reports "cannot access parent
	// directories". Anything that walks up looking for a marker file breaks
	// with it — mvn's basedir search walks from the project to / and dies on
	// $HOME with a misleading "Not a directory". Metadata is names, sizes
	// and modes, not file contents, so allow it back: stat works, reading a
	// file still does not, and readdir still needs file-read-data.
	if sb.HomeDir != "" {
		b.WriteString("(allow file-read-metadata (subpath \"" + escapeSeatbelt(sb.HomeDir) + "\"))\n")
	}

	readRoots := append(append([]string{}, sb.Read...), sb.Write...)
	if len(readRoots) > 0 {
		b.WriteString("(allow file-read*\n")
		for _, r := range readRoots {
			if r == "" {
				continue
			}
			b.WriteString("  (subpath \"" + escapeSeatbelt(r) + "\")\n")
		}
		b.WriteString(")\n")
	}
	return b.String()
}

// escapeSeatbelt escapes a path for a seatbelt string literal.
func escapeSeatbelt(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}
