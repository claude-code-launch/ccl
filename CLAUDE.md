# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common Commands for Development

- **Build the binary**: `go build -o ccl .` (or `-o /tmp/ccl-verify` for local verification as suggested in README)
- **Run all tests**: `go test ./...` (CI uses `go test -race ./...`)
- **Run specific test**: `go test ./cmd -run TestRunAuthGrokWithoutAliasDerivesName`
- **Regression after CLIProxyAPI upgrade**: `go test ./internal/oauthproxy ./internal/claude ./cmd`
- **Check formatting**: `gofmt -l .` (must output nothing)
- **Vet**: `go vet ./...`
- **CI workflow**: See `.github/workflows/ci.yml` for sequence: gofmt → race test → vet → build

## High-Level Architecture

The `ccl` is a Go CLI that acts as a multi-model gateway launcher for Claude Code.

**Data flow**:
- `cmd/` (Cobra commands) → `internal/claude` (session launcher with settings JSON and proxy) → `internal/oauthproxy` (backend runtime with protocol translation)

**Key entry points** (read these first for big picture):
- `cmd/root.go`: Command tree, alias handling, argument passthrough to Claude Code, provider resolution.
- `internal/claude/launcher.go`: Core session startup - provider setup, env building, settings file generation, process execution.
- `internal/oauthproxy/`: Runtime for different providers (CLIProxyAPI for most, direct adapters for Copilot/Qoder/Kiro).
- `internal/provider/provider.go`: Model mapping and provider types.
- `internal/cloudsync/`: For `ccl cloud` sync features.

**Two launch paths**:
1. Explicit ccl commands (white-listed in `isCclCommand`).
2. Passthrough of unknown args to real Claude Code (use `--help` or correct subcommands for testing).

**Configuration**: `~/.ccl/config.yaml` (providers), `~/.ccl/auth/` (credentials), logs in `~/.ccl/logs/`.

This structure requires reading the above files to understand the multi-provider translation, runtime proxies, and session management.
