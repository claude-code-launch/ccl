// Package oauthproxy implements ccl's local subscription and protocol runtimes.
//
// Claude Code talks to an Anthropic Messages endpoint. CCL directly owns every
// Codex Responses data plane: API-key gateways, GPT subscriptions, and Copilot
// models whose catalog endpoint is Responses. Manual OpenAI Chat plus
// Gemini/Grok/Kimi/Claude subscriptions still use the embedded CLIProxyAPI SDK.
// Copilot Chat and native Messages models use CPA behind CCL's protocol router.
// Kiro and Qoder use CCL's direct runtimes. Direct Anthropic API-key gateways
// bypass this package entirely.
//
// Error recovery follows the data-plane owner. CPA-backed providers use CPA's
// native retry/cooldown and Retry-After handling without a CCL result hook.
// Codex Responses refreshes GPT OAuth once after a 401 and otherwise preserves
// upstream status/Retry-After. Copilot, Qoder, and Kiro keep only the recovery
// behavior required by their upstreams; notably Kiro rotates credentials and
// retries burst 429s after 1s, 2s, and 4s.
//
// # Compatibility boundary with CLIProxyAPI
//
// Several behaviors below are deliberate workarounds for SDK gaps. Treat them
// as a regression checklist whenever the pinned
// github.com/router-for-me/CLIProxyAPI/v7 version changes:
//
//  1. Codex Responses ownership (codex_responses_*.go)
//     CCL owns Messages-to-Responses translation, Codex identity headers, GPT
//     token refresh, upstream errors, Responses SSE decoding, and usage. CPA's
//     codex executor must never be inserted into these paths. Its source may be
//     consulted for compatibility, but a CPA upgrade must not change the wire.
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
//     providers do not share models or refresh tokens. GPT login is initiated
//     by CPA's authenticator, but CCL reads and refreshes the selected token in
//     its direct Responses runtime.
//
//  5. Model registration cleanup
//     CPA runtime Stop unregisters every auth ID from
//     cliproxy.GlobalModelRegistry so a later provider does not inherit another
//     backend's routes. CCL direct runtimes do not register global models.
//
//  6. GitHub Copilot direct gateway (copilot_runtime.go)
//     Copilot does not use CLIProxyAPI OAuth credentials. ccl authenticates
//     with GitHub, discovers the account's authoritative model catalog, and
//     routes each model according to its advertised Chat, Responses, or
//     Messages endpoint. Responses models use CCL's Codex converter; only Chat
//     and native Messages use a CPA compatibility child. Do not bypass the
//     Copilot gateway's own client identity or credential rotation.
//
//  7. Qoder direct runtime (qoder_*.go)
//     Qoder browser OAuth, refresh, COSY signing, WAF body encoding, model
//     discovery, and Anthropic Messages translation all run in this process.
//     The upstream request's session_type="qodercli" is a protocol identity
//     field only; do not replace the direct runtime with a qodercli subprocess.
//
//  8. Kiro direct runtime (kiro_*.go)
//     Kiro Portal PKCE / Builder ID auth, credential refresh, model discovery,
//     Messages-to-Amazon-Q conversion, retry, and AWS EventStream decoding all
//     run in ccl. Do not route Kiro traffic through CPA unless the complete
//     direct-runtime behavior is deliberately replaced and regression-tested.
//
// When upgrading CLIProxyAPI, run at least:
//
//	go test ./internal/oauthproxy ./internal/claude ./cmd
//
// and manually exercise ccl oauth gpt, ccl oauth copilot, ccl oauth qoder,
// ccl oauth kiro, an openai_responses API-key provider, and a plain
// openai(chat) provider with
// streaming + tool calls.
package oauthproxy
