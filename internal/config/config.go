// Package config loads the hermes-node configuration from a TOML file.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/BurntSushi/toml"
)

// Config is the full set of values read from config.toml.
type Config struct {
	Node   NodeConfig   `toml:"node"`
	Server ServerConfig `toml:"server"`
}

// NodeConfig describes this node: where it connects, what it's called, what
// it is allowed to touch on disk, where to write the audit log, and the
// pre-shared token used during the protocol's `auth` exchange.
//
// Token is a 32-byte base64url string per PROTOCOL.md §7 / SECURITY-REVIEW.md.
// SECURITY-REVIEW.md acknowledges it is stored in plaintext in v1; Load
// verifies the file mode is 0600 on Unix and refuses to surface the token
// to callers that pass a file with looser permissions, so a chmod slip
// during a manual edit can't accidentally widen the token's visibility.
type NodeConfig struct {
	ServerURL    string   `toml:"server_url"`
	Name         string   `toml:"name"`
	Token        string   `toml:"token"`
	AllowedPaths []string `toml:"allowed_paths"`
	LogPath      string   `toml:"log_path"`
	LogLevel     string   `toml:"log_level"`
}

// ServerConfig describes how the node should validate the server's TLS cert.
// Empty fields mean "use the OS CA bundle" — works for Let's Encrypt out of
// the box.
type ServerConfig struct {
	CACert           string `toml:"ca_cert"`
	PinnedCertSHA256 string `toml:"pinned_cert_sha256"`
}

// Load reads, parses, and validates the TOML file at path. It applies the
// default audit log path when none is configured, and on Unix verifies the
// file mode is 0600 — the config carries a pre-shared auth token, and a
// chmod slip during a manual edit must not silently widen its visibility.
func Load(path string) (*Config, error) {
	if err := checkFileMode(path); err != nil {
		return nil, err
	}

	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if cfg.Node.ServerURL == "" {
		return nil, fmt.Errorf("config: [node].server_url is required")
	}
	if cfg.Node.Name == "" {
		return nil, fmt.Errorf("config: [node].name is required")
	}
	if cfg.Node.Token == "" {
		return nil, fmt.Errorf("config: [node].token is required (run 'hermes-node pair' to set it)")
	}

	if cfg.Node.LogPath == "" {
		defaultPath, err := defaultLogPath()
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		cfg.Node.LogPath = defaultPath
	}

	if cfg.Node.LogLevel == "" {
		cfg.Node.LogLevel = "info"
	}

	return &cfg, nil
}

// Save writes cfg to path as a TOML file. The file is created with mode 0600
// on Unix (the config carries a pre-shared token; see SECURITY-REVIEW.md).
// Existing files are not overwritten — pair must be the only writer, and
// the user is expected to delete the file manually if they want to start
// over. This matches SECURITY-REVIEW.md's stance that the config is a
// long-lived artifact, not a per-run state file.
func Save(path string, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config: cannot save nil config")
	}
	if cfg.Node.ServerURL == "" {
		return fmt.Errorf("config: [node].server_url is required")
	}
	if cfg.Node.Name == "" {
		return fmt.Errorf("config: [node].name is required")
	}
	if cfg.Node.Token == "" {
		return fmt.Errorf("config: [node].token is required")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", filepath.Dir(path), err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("config: %s already exists; delete it manually to re-pair", path)
		}
		return fmt.Errorf("config: create %s: %w", path, err)
	}
	defer f.Close()

	enc := toml.NewEncoder(f)
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("config: encode %s: %w", path, err)
	}
	return nil
}

// checkFileMode enforces 0600 on Unix so a manually-edited config with a
// loose mode doesn't silently expose the token. On Windows, file modes
// don't carry Unix semantics, so the check is a no-op (the equivalent
// "tight ACL" guarantee would need a separate implementation; out of
// scope for v1).
func checkFileMode(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		// os.Stat already wrapped by toml.DecodeFile below, so a
		// not-found here will surface with a clear message. We
		// don't pre-empt it.
		return nil
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return fmt.Errorf("config: %s has mode %#o, must be 0600 (token is plaintext; chmod 600 %s)",
			path, mode, path)
	}
	return nil
}

// defaultLogPath returns the audit log path used when none is configured:
// $HOME/.hermes-nodes/audit.log. It returns an error if the home directory
// cannot be determined, rather than silently producing a relative path that
// would resolve to the current working directory.
func defaultLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory for default log path: %w", err)
	}
	return filepath.Join(home, ".hermes-nodes", "audit.log"), nil
}
