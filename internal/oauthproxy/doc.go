// Package oauthproxy implements ccl's local subscription and protocol runtimes.
//
// Claude Code talks to an Anthropic Messages endpoint. Manual OpenAI Chat and
// Responses gateways plus GPT/Gemini/Grok/Kimi/Claude subscriptions use the
// embedded CLIProxyAPI SDK for their data plane. Copilot is a hybrid: ccl owns
// auth, model discovery, token exchange, retry, and upstream routing while CPA
// owns protocol conversion. Kiro and Qoder use ccl's direct runtimes and their
// request data planes never enter CPA. Direct Anthropic API-key gateways bypass
// this package entirely.
//
// Error recovery follows the data-plane owner. CPA-backed providers use CPA's
// native retry/cooldown and Retry-After handling without a CCL result hook.
// Copilot, Qoder, and Kiro keep only the recovery behavior required by their
// upstreams; notably Kiro rotates credentials and retries burst 429s after
// 1s, 2s, and 4s.
//
// # Compatibility boundary with CLIProxyAPI
//
// Several behaviors below are deliberate workarounds for SDK gaps. Treat them
// as a regression checklist whenever the pinned
// github.com/router-for-me/CLIProxyAPI/v7 version changes:
//
//  1. Responses translation ownership (runtime.go)
//     CPA's codex-api-key executor owns Claude Messages to Responses request
//     translation, upstream request serialization, and Responses-to-Claude
//     streaming conversion. CCL passes the real upstream base URL directly to
//     CPA and must not place another request-rewriting proxy after it.
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
//  6. GitHub Copilot direct gateway (copilot_runtime.go)
//     Copilot does not use CLIProxyAPI OAuth credentials. ccl authenticates
//     with GitHub, discovers the account's authoritative model catalog, and
//     routes each model to its advertised Chat, Responses, or Messages
//     endpoint directly. Do not add synthetic request identity headers without
//     testing the real Copilot API: they can change model visibility or
//     entitlement decisions.
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
// ccl oauth kiro, an
// openai_responses API-key provider, and a plain openai(chat) provider with
// streaming + tool calls.
//
// CPA names its API-key Responses executor and YAML block "codex" internally.
// ccl treats that as an SDK implementation detail: every API-key Responses
// gateway follows the same path and receives no synthetic Codex client headers.
package oauthproxy
