// Package fs provides filesystem utilities for the hermes-nodes agent,
// including path-allowlist enforcement that rejects symlinks pointing
// outside a configured set of roots.
package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotAllowed is returned by Check when a target resolves to a path
// outside every configured allowed root. It is a sentinel so callers
// (audit, wire dispatch) can distinguish policy rejection from I/O
// failure without parsing error strings.
var ErrNotAllowed = errors.New("path escapes allowlist")

// Check resolves target against the given allowed roots and reports
// whether access should be granted.
//
// The returned canonical string is the absolute, symlink-resolved path
// that the caller will actually touch on disk. For non-existent targets
// it is the cleaned absolute path (best-effort: the parent directory is
// resolved if it exists).
//
// A target is allowed only if its canonical form sits inside one of
// roots after both roots and target have been symlink-resolved. This
// means a symlink whose target lives outside every root is rejected,
// even if the link itself is inside a root.
func Check(roots []string, target string) (bool, string, error) {
	if len(roots) == 0 {
		return false, "", errors.New("allowlist: no roots configured")
	}
	if target == "" {
		return false, "", errors.New("allowlist: empty target")
	}

	// Resolve every root. A root that does not exist is a configuration
	// error (the operator asked us to allow a directory we can't even see),
	// so we surface that rather than silently treating it as a no-op.
	resolvedRoots := make([]string, 0, len(roots))
	for _, r := range roots {
		rr, err := canonicalize(r)
		if err != nil {
			return false, "", fmt.Errorf("allowlist: resolve root %q: %w", r, err)
		}
		resolvedRoots = append(resolvedRoots, rr)
	}

	canonical, err := canonicalize(target)
	if err != nil {
		return false, "", fmt.Errorf("allowlist: resolve target %q: %w", target, err)
	}

	for _, root := range resolvedRoots {
		if pathContains(root, canonical) {
			return true, canonical, nil
		}
	}
	return false, canonical, ErrNotAllowed
}

// canonicalize returns the absolute, symlink-resolved form of path.
// For paths that don't yet exist, the deepest existing ancestor is
// resolved and the remainder is joined back on. This keeps the check
// useful for write operations where the target file is being created.
func canonicalize(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// EvalSymlinks requires the path to exist. Walk up until we find
	// an ancestor that does, resolve it, then rejoin the tail.
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// Tail is the part we'll re-attach after resolving the nearest
	// existing ancestor.
	tail := ""
	cur := abs
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root without finding anything
			// that exists. Fall back to the cleaned absolute path.
			return filepath.Clean(abs), nil
		}
		r, perr := filepath.EvalSymlinks(parent)
		if perr == nil {
			if tail == "" {
				return r, nil
			}
			return filepath.Join(r, tail), nil
		}
		if !errors.Is(perr, os.ErrNotExist) {
			return "", perr
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// pathContains reports whether child is the same as parent or sits
// inside it. Both arguments must be cleaned absolute paths.
func pathContains(parent, child string) bool {
	if parent == child {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(parent, sep) {
		parent += sep
	}
	return strings.HasPrefix(child, parent)
}
