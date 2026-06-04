package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
name = "work-laptop"
allowed_paths = ["/Users/patrick", "/tmp"]
log_path = "/Users/patrick/.hermes-nodes/audit.log"

[server]
ca_cert = "/Users/patrick/.hermes-nodes/my-ca.pem"
pinned_cert_sha256 = "a1b2c3d4e5f6"
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Node.ServerURL != "wss://vps.example.com:6969" {
		t.Errorf("Node.ServerURL = %q, want %q", cfg.Node.ServerURL, "wss://vps.example.com:6969")
	}
	if cfg.Node.Name != "work-laptop" {
		t.Errorf("Node.Name = %q, want %q", cfg.Node.Name, "work-laptop")
	}
	if got, want := cfg.Node.AllowedPaths, []string{"/Users/patrick", "/tmp"}; !equalStrings(got, want) {
		t.Errorf("Node.AllowedPaths = %v, want %v", got, want)
	}
	if cfg.Node.LogPath != "/Users/patrick/.hermes-nodes/audit.log" {
		t.Errorf("Node.LogPath = %q, want %q", cfg.Node.LogPath, "/Users/patrick/.hermes-nodes/audit.log")
	}
	if cfg.Server.CACert != "/Users/patrick/.hermes-nodes/my-ca.pem" {
		t.Errorf("Server.CACert = %q, want %q", cfg.Server.CACert, "/Users/patrick/.hermes-nodes/my-ca.pem")
	}
	if cfg.Server.PinnedCertSHA256 != "a1b2c3d4e5f6" {
		t.Errorf("Server.PinnedCertSHA256 = %q, want %q", cfg.Server.PinnedCertSHA256, "a1b2c3d4e5f6")
	}
}

func TestLoad_AppliesDefaultLogPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
name = "ci-runner"
allowed_paths = ["/tmp"]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	want, err := defaultLogPath()
	if err != nil {
		t.Fatalf("defaultLogPath: %v", err)
	}
	if cfg.Node.LogPath != want {
		t.Errorf("Node.LogPath = %q, want default %q", cfg.Node.LogPath, want)
	}
}

func TestLoad_RequiresServerURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[node]
name = "work-laptop"
allowed_paths = ["/tmp"]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load returned nil error; want validation error for missing server_url")
	}
}

func TestLoad_RequiresName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
allowed_paths = ["/tmp"]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load returned nil error; want validation error for missing node name")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/config.toml"); err == nil {
		t.Fatal("Load returned nil error; want error for missing file")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
