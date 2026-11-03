# CLAUDE.md

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
go test ./internal/oauthproxy ./internal/claude ./cmd
```

Release binary (matches CI): `scripts/build-release.sh` with `GOOS`/`GOARCH`/`OUTPUT`/`VERSION`; uses `-trimpath -ldflags="-s -w -X github.com/claude-code-launch/ccl/cmd.Version=..."`. `GOOGLE_OAUTH_CLIENT_ID` / `GOOGLE_OAUTH_CLIENT_SECRET` inject the Google Drive Desktop client (required by the release workflow; dev builds use `CCL_GOOGLE_OAUTH_CLIENT_ID/_SECRET`).

Web UI (`internal/webui/frontend`, Node >= 22):

```bash
npm ci && npm test && npm run build   # build writes ../web, which must be committed
npm run dev                           # hot reload against a running `ccl serve` (CCL_SERVE_URL)
```

There is no Makefile or golangci-lint config.

### Do not run the `ccl` binary as a test harness

Unknown first args are **not** errors: `cmd/root.go` forwards them to Claude Code and starts a billed session (`ccl resume`, `ccl -p "..."`, a quoted typo like `./ccl "provider --help"`). Prefer `go test` and `--help` with unquoted subcommand args. `ccl statusline` is intercepted before config load.

Bare `ccl lang` is interactive; use `ccl lang zh` / `ccl lang en`. Do not run `set` / `map` / `use` / `oauth` / `cloud` / `provider` (mutating subcommands) against the developer's real `~/.ccl/` unless asked.

`~/.ccl/config.yaml` stores plaintext API keys — do not dump it.

## Architecture and subsystem instructions

CCL launches Claude Code against API/OAuth providers and owns loopback protocol adapters. Entry flow: `main.go` → `cmd/` → `internal/providersession` → `internal/claude` / `internal/oauthproxy`. It is a launcher, not a Claude Code fork; extra CLI arguments pass through.

Before editing a subsystem, read its relevant section in [docs/development-architecture.md](docs/development-architecture.md). Use `rg -n '^##|^###' docs/development-architecture.md` to locate it, then read only the relevant section:

- ACP, Claude accounts, standalone proxy and Codex: **High-Level Architecture**.
- Provider converters, OAuth, retries and model discovery: **Protocol ownership** and `internal/oauthproxy/doc.go`.
- Configuration, environment/settings precedence and TUI: **Config, launch, TUI**.
- Browser UI, sessions, permissions, worktrees, workspace APIs and server lifecycle: **Browser UI**.
- Encrypted cloud state and Google OAuth: **Cloud sync**.

Keep these invariants across changes:

- All config writes use `config.Update(fn)` (cmd helpers: `updateProvider` / `replaceProvider`); never hand-build YAML or mutate persisted providers while preparing a session.
- Bind runtime/control services to `127.0.0.1`; use per-session credentials and never persist them into providers. OAuth reads only its bound credential. Preserve the separate native/CCL Codex routing allow-list and credential boundary.
- ACP framing, capabilities, tools, permissions and persistence belong to `@agentclientprotocol/claude-agent-acp`. Decorate its public lifecycle; do not implement a second ACP parser or permission bridge. Never test ACP, proxy start/stop or model inference on a real user profile or paid model.
- Claude subscription profiles use Claude's native login and settings; never silently switch identities or overwrite conflicting authentication.
- Codex CCL models follow the provider context setting: Default 200K / marked `[1m]` 1M, Balanced 500K / 800K, with an 85% compact threshold. Native Responses routes pass through. Converted models adapt native client `tool_search` to function calls and load definitions from `tool_search_output`; Codex owns search, execution and permissions.
- After changing `internal/webui/frontend`, run its tests/build and include the generated `internal/webui/web` files. They are embedded and committed so `go install` needs no Node.js.
- User-facing Chinese belongs in `cmd/` through `locale.T`; `internal/` has no Chinese literals except `internal/locale`.
- Provider management lives under `ccl provider`, cloud sync under `ccl cloud`; preserve the existing public command layout. Unknown root arguments launch Claude Code, so use isolated tests rather than ad hoc binary invocations.

When a subsystem contract changes, update the matching reference section. Keep implementation detail in the reference instead of growing these startup instructions.
