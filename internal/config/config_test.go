package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoad_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
name = "work-laptop"
token = "test-token-do-not-use-in-prod"
allowed_paths = ["/Users/patrick", "/tmp"]
log_path = "/Users/patrick/.hermes-node/audit.log"

[server]
ca_cert = "/Users/patrick/.hermes-node/my-ca.pem"
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
	if cfg.Node.Token != "test-token-do-not-use-in-prod" {
		t.Errorf("Node.Token = %q, want %q", cfg.Node.Token, "test-token-do-not-use-in-prod")
	}
	if got, want := cfg.Node.AllowedPaths, []string{"/Users/patrick", "/tmp"}; !equalStrings(got, want) {
		t.Errorf("Node.AllowedPaths = %v, want %v", got, want)
	}
	if cfg.Node.LogPath != "/Users/patrick/.hermes-node/audit.log" {
		t.Errorf("Node.LogPath = %q, want %q", cfg.Node.LogPath, "/Users/patrick/.hermes-node/audit.log")
	}
	if cfg.Server.CACert != "/Users/patrick/.hermes-node/my-ca.pem" {
		t.Errorf("Server.CACert = %q, want %q", cfg.Server.CACert, "/Users/patrick/.hermes-node/my-ca.pem")
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
token = "ci-runner-token"
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
token = "x"
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
token = "x"
allowed_paths = ["/tmp"]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load returned nil error; want validation error for missing node name")
	}
}

func TestLoad_RequiresToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
name = "work-laptop"
allowed_paths = ["/tmp"]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load returned nil error; want validation error for missing token")
	}
	if !strings.Contains(err.Error(), "token is required") {
		t.Errorf("error = %q, want it to mention 'token is required'", err.Error())
	}
}

func TestLoad_RejectsLooseFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file mode bits are not enforced on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
name = "work-laptop"
token = "x"
`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load returned nil error; want error for loose file mode")
	}
	if !strings.Contains(err.Error(), "0600") {
		t.Errorf("error = %q, want it to mention 0600", err.Error())
	}
}

func TestLoad_AcceptsTightFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file mode bits are not enforced on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
name = "work-laptop"
token = "x"
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("Load with 0600: %v", err)
	}
}

func TestSave_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.toml")
	cfg := &Config{
		Node: NodeConfig{
			ServerURL:    "wss://vps.example.com:6969",
			Name:         "work-laptop",
			Token:        "round-trip-token",
			AllowedPaths: []string{"/home/user"},
		},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if got.Node.Token != cfg.Node.Token {
		t.Errorf("Node.Token = %q, want %q", got.Node.Token, cfg.Node.Token)
	}
	if got.Node.Name != cfg.Node.Name {
		t.Errorf("Node.Name = %q, want %q", got.Node.Name, cfg.Node.Name)
	}
}

func TestSave_RefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := &Config{
		Node: NodeConfig{ServerURL: "wss://x", Name: "y", Token: "z"},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	// Second save with the same path must fail so a re-pair doesn't
	// silently clobber a working config (the user's manual `rm` is
	// the explicit "I want to start over" signal).
	if err := Save(path, cfg); err == nil {
		t.Fatal("second Save returned nil error; want an error refusing to overwrite")
	}
}

// TestSave_CreatesFile0600 is the unit-level counterpart to the
// integration check in TestRun_PairSubcommand_WritesConfig. Saving
// a fresh config must produce a file with mode 0600 on Unix so the
// pre-shared token is not world-readable. The integration test
// covers the round-trip; this one localises the mode guarantee.
func TestSave_CreatesFile0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file mode bits are not enforced on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := &Config{
		Node: NodeConfig{ServerURL: "wss://x", Name: "y", Token: "z"},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %#o, want 0o600", perm)
	}
}

func TestSave_RejectsIncompleteConfig(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		cfg  *Config
	}{
		{"nil", nil},
		{"missing url", &Config{Node: NodeConfig{Name: "n", Token: "t"}}},
		{"missing name", &Config{Node: NodeConfig{ServerURL: "wss://x", Token: "t"}}},
		{"missing token", &Config{Node: NodeConfig{ServerURL: "wss://x", Name: "n"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Save(filepath.Join(dir, "cfg.toml"), tc.cfg); err == nil {
				t.Errorf("Save(%s) returned nil error; want validation error", tc.name)
			}
		})
	}
}

func TestLoad_AppliesDefaultLogLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
name = "ci-runner"
token = "ci-runner-token"
allowed_paths = ["/tmp"]
log_path = "/tmp/audit.log"
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Node.LogLevel != "info" {
		t.Errorf("Node.LogLevel = %q, want default %q", cfg.Node.LogLevel, "info")
	}
}

func TestLoad_ParsesLogLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
name = "ci-runner"
token = "ci-runner-token"
allowed_paths = ["/tmp"]
log_path = "/tmp/audit.log"
log_level = "debug"
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Node.LogLevel != "debug" {
		t.Errorf("Node.LogLevel = %q, want %q", cfg.Node.LogLevel, "debug")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/config.toml"); err == nil {
		t.Fatal("Load returned nil error; want error for missing file")
	}
}

func TestBackoffInitialDuration_Defaults(t *testing.T) {
	var n NodeConfig
	if d := n.BackoffInitialDuration(); d != time.Second {
		t.Errorf("default BackoffInitialDuration = %v, want 1s", d)
	}
}

func TestBackoffInitialDuration_Parses(t *testing.T) {
	n := NodeConfig{BackoffInitial: "5s"}
	if d := n.BackoffInitialDuration(); d != 5*time.Second {
		t.Errorf("BackoffInitialDuration = %v, want 5s", d)
	}
}

func TestBackoffInitialDuration_Invalid(t *testing.T) {
	n := NodeConfig{BackoffInitial: "bogus"}
	if d := n.BackoffInitialDuration(); d != time.Second {
		t.Errorf("invalid BackoffInitialDuration = %v, want default 1s", d)
	}
}

func TestBackoffInitialDuration_ZeroOrNegative(t *testing.T) {
	for _, input := range []string{"0s", "-1s"} {
		n := NodeConfig{BackoffInitial: input}
		if d := n.BackoffInitialDuration(); d != time.Second {
			t.Errorf("BackoffInitial=%q: got %v, want 1s", input, d)
		}
	}
}

func TestBackoffMaxDuration_Defaults(t *testing.T) {
	var n NodeConfig
	if d := n.BackoffMaxDuration(); d != 60*time.Second {
		t.Errorf("default BackoffMaxDuration = %v, want 60s", d)
	}
}

func TestBackoffMaxDuration_Parses(t *testing.T) {
	n := NodeConfig{BackoffMax: "120s"}
	if d := n.BackoffMaxDuration(); d != 120*time.Second {
		t.Errorf("BackoffMaxDuration = %v, want 120s", d)
	}
}

func TestBackoffFactorValue_Defaults(t *testing.T) {
	var n NodeConfig
	if f := n.BackoffFactorValue(); f != 2.0 {
		t.Errorf("default BackoffFactorValue = %f, want 2.0", f)
	}
}

func TestBackoffFactorValue_Returns(t *testing.T) {
	n := NodeConfig{BackoffFactor: 3.0}
	if f := n.BackoffFactorValue(); f != 3.0 {
		t.Errorf("BackoffFactorValue = %f, want 3.0", f)
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
