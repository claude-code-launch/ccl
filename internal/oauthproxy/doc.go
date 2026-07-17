// Package oauthproxy embeds CLIProxyAPI as ccl's OpenAI-family runtime.
//
// Production traffic for openai / openai_responses / OAuth providers goes
// through this package only. Claude Code talks to a loopback Anthropic
// Messages endpoint that CLIProxyAPI exposes; ccl never uses a second local
// protocol-translation proxy for those providers.
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
//     Embedded runtimes bind 127.0.0.1 only and use a random per-session API
//     key that is never written back to ~/.ccl/config.yaml. OAuth credentials
//     live under ~/.ccl/auth and are filtered per backend so multi-login
//     providers do not share models or refresh tokens.
//
//  5. Model registration cleanup
//     Stop unregisters every auth ID from cliproxy.GlobalModelRegistry so a
//     later provider does not inherit another backend's routes.
//
// When upgrading CLIProxyAPI, run at least:
//
//	go test ./internal/oauthproxy ./internal/claude ./cmd
//
// and manually exercise ccl auth chatgpt, an openai_responses API-key
// provider, and a plain openai(chat) provider with streaming + tool calls.
package oauthproxy
