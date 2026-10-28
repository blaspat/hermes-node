package exec

import (
	"strings"
	"testing"
)

func TestSandboxProfile_DeniesWritesOutsideRoots(t *testing.T) {
	p := SandboxProfile([]string{"/Users/x/code", "/private/tmp"})

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

func TestSandboxProfile_EscapesQuotes(t *testing.T) {
	p := SandboxProfile([]string{`/tmp/we"ird\path`})
	if !strings.Contains(p, `(subpath "/tmp/we\"ird\\path")`) {
		t.Errorf("quote/backslash not escaped:\n%s", p)
	}
}

func TestSandboxProfile_EmptyRootsStillDenies(t *testing.T) {
	p := SandboxProfile(nil)
	if !strings.Contains(p, "(deny file-write*)") {
		t.Errorf("profile without roots must still deny writes:\n%s", p)
	}
}
