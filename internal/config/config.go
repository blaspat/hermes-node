// Package config loads the hermes-node configuration from a TOML file.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is the full set of values read from config.toml.
type Config struct {
	Node   NodeConfig   `toml:"node"`
	Server ServerConfig `toml:"server"`
}

// NodeConfig describes this node: where it connects, what it's called, what
// it is allowed to touch on disk, and where to write the audit log.
type NodeConfig struct {
	ServerURL    string   `toml:"server_url"`
	Name         string   `toml:"name"`
	AllowedPaths []string `toml:"allowed_paths"`
	LogPath      string   `toml:"log_path"`
}

// ServerConfig describes how the node should validate the server's TLS cert.
// Empty fields mean "use the OS CA bundle" — works for Let's Encrypt out of
// the box.
type ServerConfig struct {
	CACert           string `toml:"ca_cert"`
	PinnedCertSHA256 string `toml:"pinned_cert_sha256"`
}

// Load reads, parses, and validates the TOML file at path. It applies the
// default audit log path when none is configured.
func Load(path string) (*Config, error) {
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

	if cfg.Node.LogPath == "" {
		defaultPath, err := defaultLogPath()
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		cfg.Node.LogPath = defaultPath
	}

	return &cfg, nil
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
