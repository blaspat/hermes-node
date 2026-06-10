# Contributing to hermes-nodes

Thanks for your interest. hermes-nodes is a small Go binary (≈1000 LOC) with
a single maintainer, a stable protocol, and a focus on security over features.

## Code of conduct

Be respectful. Assume good faith. This is a security-sensitive codebase —
disclose vulnerabilities privately (see [SECURITY-REVIEW.md](./SECURITY-REVIEW.md))
before filing public issues.

## What we're looking for

- **Bug fixes** with a regression test
- **Security hardening** with a documented threat model
- **PROTOCOL.md drift fixes** — the protocol is canonical in
  [`hermes-nodes/PROTOCOL.md`](./PROTOCOL.md) on the Go side. If you change
  wire behavior, update PROTOCOL.md in the same commit.
- **Cross-compile / install-script fixes** — anything that makes
  `install/install.sh` or `install/install.ps1` work on a real laptop we
  haven't tested on

We're not looking for feature creep. The v1 feature set is locked.

## Development setup

- Go 1.22+
- `go test ./...` must pass
- `go test -race ./...` must pass (run before opening a PR)
- `gofmt -l .` must produce no output
- `go vet ./...` must produce no output

```bash
# Build for all platforms
./scripts/build.sh

# Run unit tests
go test ./...

# Run with race detector
go test -race ./...

# Run e2e tests (requires the Python test harness from hermes-nodes-plugin)
go test ./tests/e2e/... -tags=e2e
```

## Commit conventions

- One logical change per commit
- Commit subject in the form `<type>(<scope>): <imperative summary>` —
  e.g. `fix(wire): audit handler panics before attempting wire response`
- Types: `feat`, `fix`, `chore`, `refactor`, `test`, `docs`
- Sign off your commits (`git commit -s`) — the project uses the
  [Developer Certificate of Origin](https://developercertificate.org/)

## Pull requests

- Open a PR against `main`
- Reference the card / issue / discussion in the PR body
- Include before/after evidence for behavioral changes (test output, curl
  transcript, etc.)
- Expect review comments. The maintainer reviews for security implications
  first, correctness second, style third.

## Cross-repo changes

hermes-nodes shares a wire protocol with
[hermes-nodes-plugin](https://github.com/blaspat/hermes-nodes-plugin). If your
change touches the wire format:

1. Update PROTOCOL.md on the Go side first (canonical)
2. Update PROTOCOL.md on the Python side to match (auto-generated, but verify)
3. Land both PRs in the same release window — releasing one without the other
   breaks the protocol

## Reporting vulnerabilities

See [SECURITY-REVIEW.md](./SECURITY-REVIEW.md#reporting-a-vulnerability).
Don't disclose publicly until we've had a chance to fix.
