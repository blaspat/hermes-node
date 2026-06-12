package fs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheck_AllowedPathPasses(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "sub", "file.txt")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	allowed, canonical, err := Check([]string{root}, inside)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatalf("expected allowed=true for path inside root")
	}
	// Canonical must be the real, resolved path — not whatever the
	// caller handed in (could have had ./, ../, or symlinks).
	wantCanonical, _ := filepath.EvalSymlinks(inside)
	if canonical != wantCanonical {
		t.Fatalf("canonical = %q, want %q", canonical, wantCanonical)
	}
}

func TestCheck_DisallowedPathFails(t *testing.T) {
	allowedRoot := t.TempDir()
	otherRoot := t.TempDir()

	outside := filepath.Join(otherRoot, "secret.txt")
	if err := os.WriteFile(outside, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	allowed, canonical, err := Check([]string{allowedRoot}, outside)
	if err == nil {
		t.Fatalf("expected error, got nil (allowed=%v, canonical=%q)", allowed, canonical)
	}
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("expected ErrNotAllowed, got %v", err)
	}
	if allowed {
		t.Fatalf("expected allowed=false for path outside root")
	}
	if canonical == "" {
		t.Fatalf("expected canonical to be returned even on rejection (for audit logging)")
	}
}

func TestCheck_SymlinkEscapeFails(t *testing.T) {
	allowedRoot := t.TempDir()
	evilRoot := t.TempDir()

	// A real file that lives outside the allowlist.
	secret := filepath.Join(evilRoot, "secret.txt")
	if err := os.WriteFile(secret, []byte("shh"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlink inside the allowlist that points at the secret.
	link := filepath.Join(allowedRoot, "escape")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	// Asking for the symlink itself: even though the link path is
	// inside the allowlist, its resolved target is not. The check must
	// follow the link and reject.
	allowed, canonical, err := Check([]string{allowedRoot}, link)
	if err == nil {
		t.Fatalf("expected error for symlink escape, got nil (allowed=%v, canonical=%q)", allowed, canonical)
	}
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("expected ErrNotAllowed, got %v", err)
	}
	if allowed {
		t.Fatalf("expected allowed=false when symlink target escapes root")
	}

	// And the resolved canonical must point at the *real* file, not
	// the symlink, so audit logs and downstream code see the truth.
	wantCanonical, _ := filepath.EvalSymlinks(secret)
	if canonical != wantCanonical {
		t.Fatalf("canonical = %q, want resolved target %q", canonical, wantCanonical)
	}
}

func TestCheck_SymlinkEscapeViaNestedPath(t *testing.T) {
	// Same idea, but the target path goes *through* the symlink rather
	// than pointing at the link itself. This catches checks that only
	// look at the first component.
	allowedRoot := t.TempDir()
	evilRoot := t.TempDir()

	secret := filepath.Join(evilRoot, "secret.txt")
	if err := os.WriteFile(secret, []byte("shh"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(allowedRoot, "escape")
	if err := os.Symlink(evilRoot, link); err != nil {
		t.Fatal(err)
	}

	// Resolve the link first to get a path that is *literally* inside
	// the allowed root, then ask the check to validate it. A naïve
	// check that doesn't re-evaluate symlinks would accept this.
	viaLink := filepath.Join(link, "secret.txt")
	allowed, _, err := Check([]string{allowedRoot}, viaLink)
	if err == nil {
		t.Fatalf("expected error for nested symlink escape, got nil (allowed=%v)", allowed)
	}
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("expected ErrNotAllowed, got %v", err)
	}
}

func TestCheck_NonexistentTargetInsideRoot(t *testing.T) {
	// Write paths: the file doesn't exist yet, but the parent
	// directory does. The check should still allow it (we resolve
	// the nearest existing ancestor and join the rest back on).
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "newfile.txt")
	shallow := filepath.Join(root, "newfile.txt")

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{
			name: "single-level-leaf-missing",
			path: shallow,
			want: shallow,
		},
		{
			name: "multi-level-parents-missing",
			path: deep,
			want: deep,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowed, canonical, err := Check([]string{root}, tc.path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !allowed {
				t.Fatalf("expected allowed=true for non-existent path under root")
			}
			if canonical != tc.want {
				t.Errorf("canonical = %q, want %q", canonical, tc.want)
			}
		})
	}
}

func TestCheck_EmptyConfigRejected(t *testing.T) {
	if _, _, err := Check(nil, "/tmp"); err == nil {
		t.Fatalf("expected error for nil roots")
	}
	if _, _, err := Check([]string{}, "/tmp"); err == nil {
		t.Fatalf("expected error for empty roots")
	}
}

func TestCheck_MultipleRoots(t *testing.T) {
	// A target under the *second* root should be allowed even though
	// it's not under the first.
	root1 := t.TempDir()
	root2 := t.TempDir()
	inRoot2 := filepath.Join(root2, "thing.txt")
	if err := os.WriteFile(inRoot2, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	allowed, _, err := Check([]string{root1, root2}, inRoot2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatalf("expected allowed=true for path under second root")
	}
}
