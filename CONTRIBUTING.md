# Contributing

Thanks for looking at Guvna. It is a deliberately small, opinionated project — read [docs/VISION.md](docs/VISION.md) before proposing features; non-goals there are real.

## Development

Requirements: Go 1.26+ (see `go.mod`), Docker if you want the image.

```sh
go build ./...
go vet ./...
go test -race ./...
gofmt -l .          # must print nothing
```

CI runs exactly these four on every PR.

## Conventions

- Config-as-code: behavior belongs in YAML/env, not flags sprawl. New knobs go in `config.yaml` with sane defaults.
- Keep the binary static and dependency-light (`CGO_ENABLED=0` builds must keep working).
- Every internal package carries table-driven tests; new code should too.
- Commits use a scope prefix matching the touched area: `chains: …`, `config: …`, `keypool: …`, `docs: …`.
- No secrets in code, tests, or fixtures — keys come from env, always.

## Pull requests

1. Open an issue first for anything that changes architecture, config surface, or API shape.
2. Keep PRs single-topic; rebase on main.
3. Update docs in the same PR when behavior or config changes.

## Areas that especially welcome help

- Additional provider adapters (anything OpenAI-compatible is usually ~20 lines)
- Windows/macOS support for the CLI
- Telemetry views / export
