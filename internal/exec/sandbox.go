package exec

import (
	"os"
	"path/filepath"
	"strings"
)

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

// SandboxProfile renders a seatbelt (sandbox-exec) profile that allows
// everything except file writes outside roots.
//
// Reads stay unrestricted on purpose: locking them down breaks every
// toolchain (dyld, /etc, /usr, the compiler's own headers) and buys little
// against a caller who can already read the allowlisted tree — while writes
// are what actually damages the machine.
func SandboxProfile(roots []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n(allow file-write*\n")
	for _, r := range roots {
		if r == "" {
			continue
		}
		b.WriteString("  (subpath \"" + escapeSeatbelt(r) + "\")\n")
	}
	for _, d := range sandboxDeviceFiles {
		b.WriteString("  (literal \"" + escapeSeatbelt(d) + "\")\n")
	}
	b.WriteString("  (subpath \"/dev/fd\")\n)\n")
	return b.String()
}

// escapeSeatbelt escapes a path for a seatbelt string literal.
func escapeSeatbelt(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}
