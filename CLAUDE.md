# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common Development Commands

- Build verify binary: `go build -o /tmp/ccl-verify .`
- Full build (CI): `go build ./...`
- Run all tests: `go test ./...`
- Race test (CI equivalent): `go test -race ./...`
- Single test: `go test ./cmd -run TestRunAuthGrokWithoutAliasDerivesName`
- Package tests: `go test ./internal/oauthproxy ./internal/claude ./cmd`
- Format check: `gofmt -l .` (must be empty)
- Vet: `go vet ./...`

## High-Level Architecture

Data flow: `main.go` → `cmd/` (Cobra commands) → provider/config parsing → `internal/providersession` (runtime shape) → `internal/claude` (settings.json generation + launcher) → `internal/oauthproxy` (loopback runtime when needed).

- `cmd/` handles semantics, providers, OAuth, TUI (`go-tui`). TUI components implement `Render/KeyMap/HandleMouse/Watchers`; use `WithDisplay(tui.DisplayFlex)` for flex.
- `internal/config` manages `~/.ccl/config.yaml` (migration from old, atomic write 0600).
- `internal/provider.Provider` defines persistent config (protocol, models, OAuth, env). `internal/providersession.Session` copies it.
- `internal/claude/launcher.go`: Prepare session, generate temp `settings.json` (Concise output + responseLanguage from `ccl lang`), cleanup env, launch `claude --settings ...`.
- All runtimes bound to 127.0.0.1 with session key; `internal/oauthproxy` owns data-plane for proxy providers.
- OAuth in `~/.ccl/auth/*.json`; cloud sync via `internal/cloudsync`.

Current branch: `feature/go-tui-migration`.