# AGENTS.md

This file provides guidance to Codex (Codex.ai/code) when working with code in this repository.

## Common Development Commands

Go 1.26 (see `go.mod`). CI (`.github/workflows/ci.yml`) is the source of truth:

```bash
gofmt -l .                 # must be empty
go test -race ./...
go vet ./...
go build ./...
```

Local / targeted:

```bash
go build -o /tmp/ccl-verify .
go test ./cmd -run TestRunAuthGrokWithoutAliasDerivesName
go test ./internal/oauthproxy ./internal/codex ./cmd
```

Release binary (matches CI): `scripts/build-release.sh` with `GOOS`/`GOARCH`/`OUTPUT`/`VERSION`; uses `-trimpath -ldflags="-s -w -X github.com/claude-code-launch/ccl/cmd.Version=..."`. `GOOGLE_OAUTH_CLIENT_ID` / `GOOGLE_OAUTH_CLIENT_SECRET` inject the Google Drive Desktop client (required by the release workflow; dev builds use `CCL_GOOGLE_OAUTH_CLIENT_ID/_SECRET`).

### Do not run the `ccl` binary as a test harness

Unknown first args are **not** errors: `cmd/root.go` forwards them to Grok Build and can start a billed session (`ccl --resume`, `ccl -p "..."`, a quoted typo like `./ccl "provider --help"`). Prefer `go test` and `--help` with unquoted subcommand args. Retired commands fail before dispatch.

Bare `ccl lang` is interactive; use `ccl lang zh` / `ccl lang en`. Do not run `set` / `map` / `use` / `oauth` / `cloud` / `provider` (mutating subcommands) against the developer's real `~/.ccl/` unless asked.

`~/.ccl/config.yaml` stores plaintext API keys — do not dump it.

## Architecture and subsystem instructions

CCL launches Grok Build and connects Codex to API/OAuth providers. Shared runtimes serve Responses and Chat Completions on loopback.

Before editing a subsystem, read its relevant section in [docs/development-architecture.md](docs/development-architecture.md). Use `rg -n '^##|^###' docs/development-architecture.md` to locate it, then read only the relevant section:

- Standalone proxy and Codex: **High-Level Architecture**.
- Provider converters, OAuth, retries and model discovery: **Protocol ownership** and `internal/oauthproxy/doc.go`.
- Configuration, environment/settings precedence and TUI: **Config, launch, TUI**.
- Encrypted cloud state and Google OAuth: **Cloud sync**.

Keep these invariants across changes:

- All config writes use `config.Update(fn)` (cmd helpers: `updateProvider` / `replaceProvider`); never hand-build YAML or mutate persisted providers while preparing a session.
- Bind runtime/control services to `127.0.0.1`; use per-session credentials and never persist them into providers. OAuth reads only its bound credential. Preserve the separate native/CCL Codex routing allow-list and credential boundary.
- Test proxy lifecycle and inference with isolated profiles, local fake upstreams and fake credentials.
- Codex CCL models follow the provider context setting: Default 200K / marked `[1m]` 1M, Balanced 500K / 800K, with an 85% compact threshold. Native Responses routes pass through. Converted models adapt native client `tool_search` to function calls and load definitions from `tool_search_output`; Codex owns search, execution and permissions.
- User-facing Chinese belongs in `cmd/` through `locale.T`; `internal/` has no Chinese literals except `internal/locale`.
- Provider management lives under `ccl provider`, cloud sync under `ccl cloud`; preserve the existing public command layout. Unknown root arguments launch Grok Build, so use isolated tests rather than ad hoc binary invocations.

When a subsystem contract changes, update the matching reference section. Keep implementation detail in the reference instead of growing these startup instructions.
