// hermes-node is the Go binary that pairs a laptop with a remote Hermes
// Agent brain. Two subcommands:
//
//	hermes-node pair --server <wss-url> --token <token> [--name <name>] [--config <path>]
//	  Write a fresh config.toml with the supplied values, mode 0600. The
//	  operator runs this once after install; the file is the long-lived
//	  pairing artifact (see SECURITY-REVIEW.md).
//
//	hermes-node run [--config <path>]
//	  Long-lived background service. Loads the config, opens the audit log,
//	  connects to the server, and stays connected across drops via the
//	  reconnect supervisor. Blocks on signals.
//
//	hermes-node --version | --help
//	  Print version / usage and exit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"

	"github.com/blaspat/hermes-nodes/internal/audit"
	"github.com/blaspat/hermes-nodes/internal/config"
	execer "github.com/blaspat/hermes-nodes/internal/exec"
	"github.com/blaspat/hermes-nodes/internal/wire"
)

// version is set at build time via -ldflags "-X main.version=...". The
// default "dev" identifies a build made with `go run` or `go build` from
// source, not a tagged release.
var version = "dev"

const usage = `hermes-node — pair a laptop with a Hermes Agent brain

Usage:
  hermes-node pair --server <wss-url> --token <token> [--config <path>]
  hermes-node run [--config <path>]
  hermes-node --version
  hermes-node --help

Flags:
  --config <path>   load/save config from this path
                    (default: ~/.hermes-nodes/config.toml)

After pairing, run the node as a background service. See README.md for
the install and pair flows.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. stdout/stderr are injected so tests
// can assert on output and so the binary can be re-exec'd from main()
// without leaking file descriptors across a test boundary.
func run(args []string, stdout, stderr io.Writer) int {
	// We do two passes: first extract the global --help / --version /
	// --config flags (which may appear before OR after the
	// subcommand), then dispatch the subcommand. A single FlagSet
	// doesn't gracefully handle flags-after-subcommand, and the
	// alternative (passing --config to every subcommand's FlagSet
	// separately) duplicates the parsing logic.
	showVersion, showHelp, configPath, defaultCfgErr, subArgs, err := parseGlobalArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}

	if *showVersion {
		fmt.Fprintf(stdout, "hermes-node %s\n", version)
		return 0
	}
	if *showHelp {
		fmt.Fprint(stdout, usage)
		return 0
	}

	if len(subArgs) == 0 {
		fmt.Fprintln(stderr, "hermes-node: missing subcommand (run 'hermes-node --help')")
		return 2
	}

	// The `run` subcommand is the only one that must have a usable
	// config path; surface defaultCfgErr here (and only here) so
	// version/help work even when $HOME is unset, and pair does
	// too because pair is also a write path that will surface a
	// clearer error from config.Save.
	if (subArgs[0] == "run" || subArgs[0] == "pair") && *configPath == "" && defaultCfgErr != nil {
		fmt.Fprintf(stderr, "hermes-node: %v (pass --config <path> to override)\n", defaultCfgErr)
		return 1
	}

	switch subArgs[0] {
	case "pair":
		return runPair(subArgs[1:], *configPath, stdout, stderr)
	case "run":
		return runRunWithSignalCtx(*configPath, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hermes-node: unknown subcommand %q (run 'hermes-node --help')\n", subArgs[0])
		return 2
	}
}

// runRunWithSignalCtx is the production entry point for `hermes-node
// run`. It wires the signal handler and then calls runRun.
//
// We split signal-handling from the run loop so the test harness can
// drive runRun directly with its own context — Go's signal package
// only listens for OS signals, which a unit test can't easily send
// to its own goroutine without subprocess gymnastics.
func runRunWithSignalCtx(configPath string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runRun(ctx, configPath, stdout, stderr)
}

// parseGlobalArgs separates the global flags (which can appear anywhere
// in args) from the subcommand and its own args. This is the boring
// bookkeeping that lets `--config` work both before and after the
// subcommand keyword.
//
// Algorithm: a single pass over args, collecting non-global tokens
// into subArgs as we go. Global flags (`--config` and its value,
// `--version`, `--help`) are never appended. The first non-global
// token is the subcommand keyword; everything after it is the
// subcommand's own arg list, handed back verbatim to its FlagSet.
//
// The second return value is the error from deriving the default
// config path (i.e. $HOME is unset). It is propagated to the caller
// so the run subcommand can fail loudly; the version / help / pair
// subcommands don't need a path and can proceed without it.
//
// This means the subcommand keyword itself must not begin with `-`.
// That's the same constraint the standard Go `flag` package enforces.
func parseGlobalArgs(args []string) (showVersion, showHelp *bool, configPath *string, defaultCfgErr error, subArgs []string, err error) {
	version := false
	help := false
	cfg, derr := defaultConfigPath()
	defaultCfgErr = derr
	if derr != nil {
		// We can still parse args and dispatch subcommands
		// that don't need a path. The run subcommand will
		// surface this error itself.
		cfg = ""
	}
	subArgs = []string{}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--version":
			version = true
			continue
		case "-h", "--help":
			help = true
			continue
		case "--config":
			if i+1 >= len(args) {
				return nil, nil, nil, nil, nil, errors.New("hermes-node: --config requires a path argument")
			}
			cfg = args[i+1]
			i++ // skip the value
			continue
		}
		if v, ok := stripFlagValue(a, "--config"); ok {
			cfg = v
			continue
		}
		subArgs = append(subArgs, a)
	}
	return &version, &help, &cfg, defaultCfgErr, subArgs, nil
}

// stripFlagValue handles --name=value form for a single known flag.
// Returns (value, true) if arg is `--name=value`, else ("", false).
func stripFlagValue(arg, name string) (string, bool) {
	prefix := name + "="
	if len(arg) > len(prefix) && arg[:len(prefix)] == prefix {
		return arg[len(prefix):], true
	}
	return "", false
}

// runPair writes a fresh config.toml. The file must not exist already —
// the operator's manual `rm` is the explicit "start over" signal. We
// don't prompt for confirmation because the data we're writing (URL +
// name + token) is one-shot: re-typing it costs more than re-typing the
// `rm`.
func runPair(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hermes-node pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		server = fs.String("server", "", "server WSS URL (e.g. wss://vps.example.com:6969)")
		token  = fs.String("token", "", "pairing token issued by the server")
		name   = fs.String("name", "", "node name (must match the one registered on the server, default: config filename)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *server == "" {
		fmt.Fprintln(stderr, "hermes-node pair: --server is required")
		return 2
	}
	if *token == "" {
		fmt.Fprintln(stderr, "hermes-node pair: --token is required")
		return 2
	}

	// The pair subcommand writes a minimal config. allowed_paths and
	// log_path stay at their operator-edited defaults (empty / default);
	// the operator adds allowed_paths by editing the file before
	// running `hermes-node run`. We intentionally don't ask for
	// allowed_paths at pair time — it would make the prompt long and
	// there's no validation we can do at this point (the paths may
	// not exist yet on a fresh machine).
	nodeName := *name
	if nodeName == "" {
		nodeName = filepath.Base(configPath) // sane default; operator edits
	}
	cfg := &config.Config{
		Node: config.NodeConfig{
			ServerURL: *server,
			Name:      nodeName,
			Token:     *token,
		},
	}
	if err := config.Save(configPath, cfg); err != nil {
		fmt.Fprintf(stderr, "hermes-node pair: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "hermes-node: paired. Config written to %s (mode 0600).\n", configPath)
	fmt.Fprintf(stdout, "\n")
	fmt.Fprintf(stdout, "  Before starting the service, edit %s to set:\n", configPath)
	fmt.Fprintf(stdout, "    [node].name           — must match the name the server issued the token for.\n")
	fmt.Fprintf(stdout, "                           The default here is the filename; the server will\n")
	fmt.Fprintf(stdout, "                           reject mismatched names with auth_err/invalid_token\n")
	fmt.Fprintf(stdout, "                           (PROTOCOL.md §3.4 / §3.5) and the node will silently\n")
	fmt.Fprintf(stdout, "                           fail to authenticate.\n")
	fmt.Fprintf(stdout, "    [node].allowed_paths  — list of filesystem roots exec/read/write can touch.\n")
	fmt.Fprintf(stdout, "                           Empty list (the default) means every path is\n")
	fmt.Fprintf(stdout, "                           rejected by the handlers. See SECURITY-REVIEW.md.\n")
	fmt.Fprintf(stdout, "\n")
	fmt.Fprintf(stdout, "  Then start the service: hermes-node run\n")
	return 0
}

// runRun is the long-lived service. It blocks until ctx is cancelled
// (clean shutdown) and exits 0 on a clean shutdown, 1 on a fatal
// error. A recoverable error inside the supervisor (network drop,
// server bye) never bubbles up — the supervisor's reconnect cycle
// handles it.
//
// ctx is supplied by the caller. Production wires signal handling
// (see runRunWithSignalCtx); tests pass a deadline-bounded ctx.
func runRun(ctx context.Context, configPath string, stdout, stderr io.Writer) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: %v\n", err)
		return 1
	}

	auditLog, err := audit.New(cfg.Node.LogPath)
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: open audit log: %v\n", err)
		return 1
	}
	defer func() {
		if cerr := auditLog.Close(); cerr != nil {
			fmt.Fprintf(stderr, "hermes-node: close audit log: %v\n", cerr)
		}
	}()

	fmt.Fprintf(stdout, "hermes-node %s: connecting to %s as %q (%d allowed paths)\n",
		version, cfg.Node.ServerURL, cfg.Node.Name, len(cfg.Node.AllowedPaths))

	// prevSession tracks the persistent shell from the previous
	// (re)connect, so we can close it before allocating a new
	// one. Without this, a flaky-network operator leaks one
	// bash PID per reconnect (Go does not reap subprocesses
	// when a *Session reference is dropped). Quinn's review
	// flagged this as the only finding a real operator could
	// trip over.
	var (
		prevSessionMu sync.Mutex
		prevSession   *execer.Session
	)

	// Build the TLS config once, outside the Dialer closure.
	// VerifyConnection captures the pin (if any) by closure, so
	// the same config is safe to reuse across reconnects.
	tlsCfg, err := config.BuildTLSConfig(cfg.Server)
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: %v\n", err)
		return 1
	}

	sup, err := wire.NewSupervisor(wire.SupervisorOptions{
		Dialer: func(ctx context.Context) (*wire.Client, error) {
			return wire.Connect(ctx, wire.DialOptions{
				ServerURL:    cfg.Node.ServerURL,
				NodeName:     cfg.Node.Name,
				Token:        cfg.Node.Token,
				NodeVersion:  version,
				Platform:     runtime.GOOS,
				Arch:         runtime.GOARCH,
				Capabilities: []string{"exec", "read", "write"},
				TLSConfig:    tlsCfg,
			})
		},
		// Setup is invoked once per (re)connect. We build a fresh
		// persistent shell here so each connection starts with a
		// clean bash. The protocol doesn't promise shell state
		// survives reconnects (the server re-issues calls on
		// reconnect), so this is the right grain.
		Setup: func(ctx context.Context, c *wire.Client, d *wire.Dispatcher, p *wire.Pinger) error {
			// Bump the watchdog clock on every received frame.
			d.OnRead = p.MarkAlive
			// Surface wire-level errors (handler panics, write
			// failures) to the operator via stderr AND the audit
			// log, with the panic stack included so a postmortem
			// grep has the trace, not just the panic value.
			// Without the OnError hook the panic-recovery path
			// in dispatch.go is invisible to the operator.
			d.OnError = func(err error, env wire.Envelope) {
				stack := debug.Stack()
				fmt.Fprintf(stderr, "hermes-node: dispatch error: %v (type=%s id=%s)\n%s\n",
					err, env.Type, env.ID, stack)
				_ = auditLog.Write(audit.Entry{
					Action: "dispatch_error",
					Target: err.Error() + "\n" + string(stack),
					Status: "error",
				})
			}

			// Close the previous session before opening a new
			// one so a flaky-network operator doesn't leak bash
			// PIDs across reconnects. Close is idempotent and
			// safe even if the previous session's bash already
			// exited (failPending on the demuxer is a no-op).
			prevSessionMu.Lock()
			old := prevSession
			prevSession = nil
			prevSessionMu.Unlock()
			if old != nil {
				_ = old.Close()
			}

			session, err := execer.NewSession(ctx)
			if err != nil {
				return fmt.Errorf("start shell: %w", err)
			}
			// Publish the new session so the next Setup call
			// can close it. We intentionally do NOT close it
			// here — the session is meant to live for the
			// lifetime of the connection.
			prevSessionMu.Lock()
			prevSession = session
			prevSessionMu.Unlock()

			execHandler := wire.NewExecHandler(
				execer.NewSessionAdapter(session),
				cfg.Node.AllowedPaths,
				auditLog,
			)
			fsys := wire.NewFileSystem(cfg.Node.AllowedPaths, auditLog)
			if err := d.Register(wire.TypeExec, execHandler.Handle); err != nil {
				return fmt.Errorf("register exec: %w", err)
			}
			if err := d.Register(wire.TypeRead, fsys.ReadHandler); err != nil {
				return fmt.Errorf("register read: %w", err)
			}
			if err := d.Register(wire.TypeWrite, fsys.WriteHandler); err != nil {
				return fmt.Errorf("register write: %w", err)
			}
			return nil
		},
		AuditLog: auditLog,
	})
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: build supervisor: %v\n", err)
		return 1
	}

	// Supervisor.Run blocks until ctx is cancelled (clean shutdown)
	// or the dialer returns a fatal error (config-invalid, etc.).
	// Network drops, heartbeats, server-byes are all recoverable
	// and do NOT cause Run to return.
	runErr := sup.Run(ctx)
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		fmt.Fprintf(stderr, "hermes-node: supervisor exited: %v\n", runErr)
		return 1
	}
	fmt.Fprintln(stdout, "hermes-node: clean shutdown")
	return 0
}

// defaultConfigPath returns the operator-default location for
// config.toml. It mirrors defaultLogPath in internal/config —
// returns an error if the home directory cannot be determined,
// rather than silently producing a relative path that would
// resolve to the current working directory and confuse the
// operator with a "file not found" error whose path is a lie.
func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory for default config path: %w", err)
	}
	return filepath.Join(home, ".hermes-nodes", "config.toml"), nil
}
