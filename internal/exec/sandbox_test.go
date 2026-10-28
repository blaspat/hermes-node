package exec

import (
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
