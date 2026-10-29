package exec

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSandboxProfile_DeniesWritesOutsideRoots(t *testing.T) {
	p := SandboxProfile(Sandbox{Write: []string{"/Users/x/code", "/private/tmp"}})

	for _, want := range []string{
		"(deny file-write*)",
		`(subpath "/Users/x/code")`,
		`(subpath "/private/tmp")`,
		`(literal "/dev/null")`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %q:\n%s", want, p)
		}
	}

	// deny must come before the allow list, otherwise the allow is dead.
	if strings.Index(p, "(deny file-write*)") > strings.Index(p, "(allow file-write*") {
		t.Errorf("deny must precede allow:\n%s", p)
	}
}

func TestSandboxProfile_DeniesReadsUnderHome(t *testing.T) {
	p := SandboxProfile(Sandbox{
		HomeDir: "/Users/x",
		Read:    []string{"/Users/x/.gitconfig"},
		Write:   []string{"/Users/x/code"},
	})

	if !strings.Contains(p, `(deny file-read*`) {
		t.Fatalf("profile does not deny reads:\n%s", p)
	}
	// Home must be inside the read deny, before the read allow.
	denyIdx := strings.Index(p, "(deny file-read*")
	allowIdx := strings.Index(p, "(allow file-read*")
	if allowIdx < denyIdx {
		t.Fatalf("read allow must follow the read deny:\n%s", p)
	}
	denyBlock := p[denyIdx:allowIdx]
	if !strings.Contains(denyBlock, `(subpath "/Users/x")`) {
		t.Errorf("home not denied for reads:\n%s", p)
	}
	allowBlock := p[allowIdx:]
	if !strings.Contains(allowBlock, `(subpath "/Users/x/.gitconfig")`) {
		t.Errorf("read root not re-allowed:\n%s", p)
	}
	// Write roots must also be readable — a shell that can write a file but
	// not read it back is broken.
	if !strings.Contains(allowBlock, `(subpath "/Users/x/code")`) {
		t.Errorf("write root not re-allowed for reads:\n%s", p)
	}
}

func TestSandboxProfile_EscapesQuotes(t *testing.T) {
	p := SandboxProfile(Sandbox{Write: []string{`/tmp/we"ird\path`}})
	if !strings.Contains(p, `(subpath "/tmp/we\"ird\\path")`) {
		t.Errorf("quote/backslash not escaped:\n%s", p)
	}
}

func TestSandboxProfile_EmptyRootsStillDenies(t *testing.T) {
	p := SandboxProfile(Sandbox{})
	if !strings.Contains(p, "(deny file-write*)") {
		t.Errorf("profile without roots must still deny writes:\n%s", p)
	}
	if strings.Contains(p, "(allow file-read*\n") {
		t.Errorf("no read allow expected without roots:\n%s", p)
	}
}

// TestSandboxProfile_AllowsMetadataUnderHome covers the traversal fix. Denying
// $HOME outright also denies the directory itself, so getcwd() and any tool
// that walks up looking for a marker file (mvn's basedir search) fail with a
// misleading "Not a directory". Metadata must be allowed back, after the deny.
func TestSandboxProfile_AllowsMetadataUnderHome(t *testing.T) {
	p := SandboxProfile(Sandbox{
		HomeDir: "/Users/x",
		Write:   []string{"/Users/x/code"},
	})

	const want = `(allow file-read-metadata (subpath "/Users/x"))`
	if !strings.Contains(p, want) {
		t.Fatalf("profile missing metadata allow %q:\n%s", want, p)
	}
	// Last-match-wins: an allow placed before the deny is dead.
	if strings.Index(p, want) < strings.Index(p, "(deny file-read*") {
		t.Errorf("metadata allow must follow the read deny:\n%s", p)
	}
	// The whole of $HOME must still be denied for content reads.
	if !strings.Contains(p, `(subpath "/Users/x")`) {
		t.Errorf("home no longer denied for reads:\n%s", p)
	}
}

// TestSandboxReadDefaults_IncludesToolchainRoots pins the paths that made
// java, go and node unlaunchable when the read deny landed: the JDK lives
// under ~/Library/Java and Homebrew under ~/homebrew on a macOS dev box.
func TestSandboxReadDefaults_IncludesToolchainRoots(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	got := sandboxReadDefaults()
	for _, d := range []string{"Library/Java", "homebrew", ".sdkman", ".nvm"} {
		want := filepath.Join(home, d)
		if !slices.Contains(got, want) {
			t.Errorf("read defaults missing toolchain root %q (got %v)", want, got)
		}
	}
}
