# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

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

Release binary (matches CI): `scripts/build-release.sh` with `GOOS`/`GOARCH`/`OUTPUT`/`VERSION`; uses `-trimpath -ldflags="-s -w -X github.com/claude-code-launch/ccl/cmd.Version=..."`. Optional `GOOGLE_OAUTH_CLIENT_SECRET` ldflag for cloud sync.

There is no Makefile or golangci-lint config.

### Do not run the `ccl` binary as a test harness

Unknown first args are **not** errors: `cmd/root.go` forwards them to Claude Code and starts a billed session (`ccl resume`, `ccl -p "..."`, a quoted typo like `./ccl "provider --help"`). Prefer `go test` and `--help` with unquoted subcommand args. `ccl statusline` is the one exception — it is intercepted at the top of `cmd.Execute` and reads a Claude Code payload from stdin.

Bare `ccl lang` is interactive; use `ccl lang zh` / `ccl lang en`. Do not run `set` / `map` / `oauth` / `cloud` / `import` against the developer's real `~/.ccl/` unless asked.

`~/.ccl/config.yaml` stores plaintext API keys — do not dump it.

## High-Level Architecture

`ccl` launches Anthropic's Claude Code CLI against other model sources. Claude Code always speaks Anthropic Messages; ccl either points it at a real Anthropic-compatible gateway or starts a **127.0.0.1** loopback runtime that translates.

Data flow:

`main.go` → `cmd/` (Cobra + TUI) → `internal/config` + `internal/provider` → `internal/providersession.Prepare` → `internal/claude` (temp `settings.json`, env cleanup, `claude --settings ...`) → `internal/oauthproxy` when a proxy is required.

`providersession.Session` is a **copy** of the persisted provider. `UseProxy` is true for OpenAI-compatible types, `modelsdev`, or any non-empty `OAuthProvider` (including AutoClaw). Direct Anthropic API-key providers skip the proxy; Claude Code talks upstream with `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` (`anthropicAuth: bearer`).

Loopback runtimes bind `127.0.0.1` and a random per-session key that is never written back to config. Upstream OAuth secrets live in `~/.ccl/auth/*.json` (0600), bound by `oauthAccountCredential`.

### Protocol ownership (`internal/oauthproxy`)

Canonical notes: `internal/oauthproxy/doc.go`. Do not reintroduce an external provider process (no qodercli, no Codex CLI).

| Ingress | Runtime | Notes |
|---|---|---|
| Anthropic API key | none (direct) | Claude Code hits `/v1/messages`; `ccl set` still needs `/v1/models` to auto-detect |
| OpenAI Chat API key / Kimi / WorkBuddy | `openai_chat_*.go` | Messages ↔ Chat Completions |
| Codex Responses API key / GPT OAuth | `codex_responses_*.go` + `internal/codexidentity` | All `openai_responses` gateways get CCL-owned Codex identity (`Originator: codex_cli_rs`, UA, `stream=true`, `store=false`). Protocol is `type`, not guessed from `/codex` in the URL |
| Grok OAuth | `xai_*.go` + `codex_responses_*.go` | cli-chat-proxy Responses; Grok Build identity headers; live `/models` catalog with 4.6/4.5 fallback |
| Gemini OAuth | `gemini_*.go` | Antigravity conversion |
| Copilot | `copilot_*.go` | Per-model Chat / Responses / Messages from the account catalog; persisted `type` is only `openai_responses` for local dispatch |
| Kiro | `kiro_*.go` | Portal PKCE (default) or `--kiro-auth builder`; Amazon Q + EventStream |
| Qoder | `qoder_*.go` | Browser OAuth, COSY, WAF; `session_type=qodercli` is a wire field only |
| models.dev mixed | `mixed_runtime.go` + `anthropic_passthrough.go` | `ModelProtocols` map per model |
| AutoClaw / ZCode | `autoclaw_*.go` | CCL-owned Anthropic-to-OpenAI Chat adapter to `{origin}/autoclaw-proxy/proxy/autoclaw`; imports encrypted desktop `auth.json`, refreshes the session, and sends desktop-compatible managed-proxy headers without starting AutoClaw |

Shared 429/5xx fast retry is `retry.go` (500ms + 1s, then relay status/body/`Retry-After` untouched). Per-backend 401 refresh happens **inside** one attempt. Kiro has its own 1/2/4s loop; WorkBuddy/Copilot inner gateways must not wrap the outer retry (would 3×3).

`ccl set` Auto Configure only GETs `/models` metadata for generic gateways. OAuth subscriptions have no Auto Configure row at all: `configureOAuthRuntime` copies the catalog straight from the loopback runtime (`runtime.Models()`), so a subscription is never probed as a generic endpoint. Per-slot `[1m]` markers live in `live().oneMSlots`; a slot that kept its model keeps its marker, so re-detection cannot re-open a window the user closed.

### Config, launch, TUI

- `internal/config`: `~/.ccl/config.yaml`, migrate `~/.cc/config.yaml`, atomic write 0600. Load may rewrite inferred `oauthProvider` / OAuth `type`.
- Global fields: `active_provider`, `lang`, `bypass_mode` (not `auto_mode`), `log_level` (`off` default; `ccl log`).
- `internal/claude`: settings pin `outputStyle: Concise`, `language` from `ccl lang`, and a `statusLine` running `ccl statusline` (a per-provider `statuslineDisabled` opts out, since `--settings` outranks the user's `~/.claude/settings.json`); context presets Default / Balanced 500K / 800K are also exported as env because Claude Code has ignored settings-only auto-compact. Provider `Env` overrides defaults except proxy transport keys.
- TUI (`cmd/advanced_config.go`, `github.com/grindlemire/go-tui`): components implement `Render` / `KeyMap` / `HandleMouse` / `Watchers`; use `tui.WithDisplay(tui.DisplayFlex)`. Single-page set wizard.

### i18n layering

User-facing Chinese lives in `cmd/` via `locale.T`. `internal/` must not contain Chinese string literals (`internal/protocol/no_chinese_strings_test.go`); `internal/locale` is the exception.

### Cloud sync

`internal/cloudsync` encrypts `config.yaml` + `auth/*.json` to iCloud / Google Drive. Root `ccl login/push/pull/...` are aliases of `ccl cloud ...`. Google client secret may be ldflag-injected; PKCE is the real auth boundary.

## Product facts that affect code changes

ccl is a launcher + protocol proxy, not a fork of Claude Code. Extra CLI args after a non-ccl command are passed through.

OAuth public names: `gpt` `gemini` `grok` `copilot` `qoder` `kimi` `kiro` `workbuddy` `autoclaw`. `ccl oauth chatgpt` is gone; old configs may still store `chatgpt`/`codex` as `oauthProvider`. Slot defaults: `internal/provider/oauth_defaults.go` (empty slots only; missing catalog entries are cleared at launch; generated Grok 4.5/4.3/3-mini IDs migrate when the new default is in the catalog).

`bypass` injects `--dangerously-skip-permissions`. `ccl status` is cloud sync, not provider health — use `ccl doctor`.
