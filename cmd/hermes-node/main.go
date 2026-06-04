// hermes-node is the Go binary that pairs a remote laptop with a Hermes
// Agent brain. Run it as a long-lived background service after pairing.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/blaspat/hermes-nodes/internal/config"
)

// version is set at build time via -ldflags "-X main.version=...". The
// default "dev" identifies a build made with `go run` or `go build` from
// source, not a tagged release.
var version = "dev"

const usage = `hermes-node — pair a laptop with a Hermes Agent brain

Usage:
  hermes-node [flags]

Flags:
  --config <path>   load config from this path (default: ~/.hermes-nodes/config.toml)
  --version         print version and exit
  --help            print this message and exit

After pairing, the node runs as a background service, accepting exec, read,
and write calls from the brain over an authenticated WSS connection. See
README.md for the install and pair flows.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hermes-node", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		showVersion = fs.Bool("version", false, "print version and exit")
		configPath  = fs.String("config", "", "load config from this path")
		showHelp    = fs.Bool("help", false, "print usage and exit")
	)
	fs.BoolVar(showHelp, "h", false, "alias for --help")

	if err := fs.Parse(args); err != nil {
		// flag already printed the error
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

	if *configPath == "" {
		fmt.Fprintln(stderr, "hermes-node: --config <path> is required (no default yet)")
		fmt.Fprintln(stderr, "Run `hermes-node --help` for usage.")
		return 1
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "hermes-node %s: connected to %s as %q (%d allowed paths)\n",
		version, cfg.Node.ServerURL, cfg.Node.Name, len(cfg.Node.AllowedPaths))
	return 0
}
