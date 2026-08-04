// Package oauthproxy implements ccl's local subscription and protocol runtimes.
//
// Production traffic for openai / openai_responses / OAuth providers goes
// through this package only. Claude Code talks to a loopback Anthropic
// Messages endpoint. Most providers use embedded CLIProxyAPI; Kiro uses ccl's
// direct Messages-to-Amazon-Q adapter and AWS EventStream decoder; Qoder uses
// ccl's direct OAuth/COSY/SSE adapter and never invokes Qoder CLI.
//
// # Compatibility boundary with CLIProxyAPI
//
// Several behaviors below are deliberate workarounds for SDK gaps. Treat them
// as a regression checklist whenever the pinned
// github.com/router-for-me/CLIProxyAPI/v7 version changes:
//
//  1. responsesCompatibilityProxy (responses_compat.go)
//     Placed in front of every Responses upstream (plain and Codex). It:
//     (a) rewrites completed-only streams into a normal output_text.delta
//     because CLIProxyAPI's streaming Claude translator currently ignores
//     text that only appears in response.completed;
//     (b) ensures response.created precedes any content event and drops a
//     late real created after a synthetic one, so the translator never
//     emits content before message_start or a second message_start; and
//     (c) for plain Responses, strips residual Codex headers/body that the
//     SDK's codex-api-key executor always injects (codex-tui UA, Session_id,
//     client_metadata, Originator) and replaces UA with ccl-openai-responses.
//     Dedicated Codex bases still inject full Codex client identity.
//     Remove or shrink once the SDK exposes a non-Codex Responses executor.
//
//  2. Runtime.Stop shutdown ordering (runtime.go)
//     Service.Run performs its own deferred Shutdown after the run context is
//     canceled. Calling Shutdown concurrently with that final path races inside
//     CLIProxyAPI, so Stop waits up to 5s for Run to exit and only force-calls
//     Shutdown on timeout. Keep that order when changing teardown.
//
//  3. Log / stdout isolation (silenceSDKLogs, silenceStdout)
//     CLIProxyAPI uses logrus and may write startup noise to stdout. ccl
//     temporarily silences both while the embedded service becomes ready, and
//     keeps logrus discarded after the last runtime stops because refresh
//     workers can still log after Shutdown. Nested starts use reference counts.
//
//  4. Session credentials
//     All runtimes bind 127.0.0.1 only and use a random per-session API
//     key that is never written back to ~/.ccl/config.yaml. OAuth credentials
//     live under ~/.ccl/auth and are filtered per backend so multi-login
//     providers do not share models or refresh tokens.
//
//  5. Model registration cleanup
//     Stop unregisters every auth ID from cliproxy.GlobalModelRegistry so a
//     later provider does not inherit another backend's routes.
//
//  6. CCL cooldown override (codex_cooldown.go)
//     GPT OAuth and ordinary API-key runtimes shorten 408/5xx failures to 2s
//     and 401/429 failures to 10s. The result hook updates the SDK manager after
//     MarkResult and clears the SDK registry's longer 401/429 side effects.
//     Kiro has an independent direct adapter and does not use this policy.
//
//  7. GitHub Copilot direct gateway (copilot_runtime.go)
//     Copilot does not use CLIProxyAPI OAuth credentials. ccl authenticates
//     with GitHub, discovers the account's authoritative model catalog, and
//     routes each model to its advertised Chat, Responses, or Messages
//     endpoint before the local compatibility layer. Do not add synthetic
//     request identity headers without testing the real Copilot API: they can
//     change model visibility or entitlement decisions.
//
//  8. Qoder direct runtime (qoder_*.go)
//     Qoder browser OAuth, refresh, COSY signing, WAF body encoding, model
//     discovery, and Anthropic Messages translation all run in this process.
//     The upstream request's session_type="qodercli" is a protocol identity
//     field only; do not replace the direct runtime with a qodercli subprocess.
//
// When upgrading CLIProxyAPI, run at least:
//
//	go test ./internal/oauthproxy ./internal/claude ./cmd
//
// and manually exercise ccl oauth gpt, ccl oauth copilot, ccl oauth qoder,
// ccl oauth kiro, an
// openai_responses API-key provider, and a plain openai(chat) provider with
// streaming + tool calls.
//
// Note: dedicated Codex bases still set Originator to embeddedCodexOriginator
// ("codex_cli_rs") for custom API-key Codex endpoints. That is independent of
// CLIProxyAPI's default codex-tui User-Agent for OAuth/SDK-managed requests.
package oauthproxy
