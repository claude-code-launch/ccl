// Package oauthproxy implements ccl's local subscription and protocol runtimes.
//
// Claude Code talks to an Anthropic Messages endpoint. CCL directly owns every
// data plane: Codex Responses (GPT and openai_responses API-key gateways),
// OpenAI Chat (manual API-key providers, Grok, Kimi, WorkBuddy, and Copilot Chat
// models), the native-Anthropic Messages passthrough (models.dev
// @ai-sdk/anthropic models and Copilot native-Messages models), and Gemini
// (Antigravity conversion). Copilot's mixed catalog, Kiro, Qoder, and
// Command Code (a direct /alpha/generate NDJSON data plane) run entirely on
// CCL-owned runtimes too. Direct Anthropic API-key gateways bypass
// this package altogether. CLIProxyAPI is no longer a dependency.
//
// Error recovery follows the data-plane owner. CCL-owned data planes refresh
// OAuth once after a 401 and otherwise preserve upstream status/Retry-After:
// Codex Responses, Grok, and Kimi refresh once;
// WorkBuddy refreshes once after a 401/403; Gemini also falls back from the
// daily to the prod Antigravity base on network errors, 429s and 5xx responses.
// Copilot, Qoder, and Kiro keep only the recovery behavior required by their
// upstreams; notably Kiro rotates credentials and retries burst 429s after 1s,
// 2s, and 4s.
//
// # Direct data planes
//
// Each backend's data plane is CCL-owned end-to-end. Treat these as a
// regression checklist rather than routing any of them back through CLIProxyAPI:
//
//  1. Codex Responses ownership (codex_responses_*.go)
//     CCL owns Messages-to-Responses translation, Codex identity headers, GPT
//     token refresh, upstream errors, Responses SSE decoding, and usage. CPA's
//     codex executor must never be inserted into these paths.
//
//  2. GitHub Copilot direct gateway (copilot_runtime.go)
//     Copilot does not use CLIProxyAPI OAuth credentials. ccl authenticates
//     with GitHub, discovers the account's authoritative model catalog, and
//     routes each model according to its advertised Chat, Responses, or
//     Messages endpoint — all three served by CCL data planes. Do not bypass
//     the Copilot gateway's own client identity or credential rotation.
//
//  3. Qoder direct runtime (qoder_*.go)
//     Qoder browser OAuth, refresh, COSY signing, WAF body encoding, model
//     discovery, and Anthropic Messages translation all run in this process.
//     The upstream request's session_type="qodercli" is a protocol identity
//     field only; do not replace the direct runtime with a qodercli subprocess.
//
//  4. Kiro direct runtime (kiro_*.go)
//     Kiro Portal PKCE / Builder ID auth, credential refresh, model discovery,
//     Messages-to-Amazon-Q conversion, retry, and AWS EventStream decoding all
//     run in ccl. Do not route Kiro traffic through CPA.
//
//  5. WorkBuddy runtime (workbuddy_*.go)
//     CCL owns the official external-link login polling, credential refresh,
//     /v3/config model catalog, WorkBuddy identity/session headers, and the
//     Anthropic Messages <-> OpenAI Chat Completions conversion (via the shared
//     chatCompletionsService).
//
//  6. Native Messages passthrough (anthropic_passthrough.go)
//     The anthropicPassthroughService serves native Anthropic models for
//     models.dev @ai-sdk/anthropic models and Copilot native-Messages models
//     (static key). It resolves the upstream credential and refreshes once
//     after a 401.
//
//  7. Command Code direct runtime (commandcode_*.go)
//     CCL owns the /alpha/generate NDJSON conversion, client identity, device
//     registration handshake, error mapping, usage accounting, and the static
//     model catalog. Command Code answers are surfaced as Anthropic Messages
//     text blocks so the protocol gap is invisible to Claude Code. Credentials
//     arrive on two paths: `ccl oauth commandcode` mirrors the official CLI
//     login (open the studio "Get API key" page, accept the key back through
//     the loopback callback or as a manual paste, validate via /alpha/whoami),
//     and `ccl import commandcode` reads the official CLI's long-lived key from
//     ~/.commandcode/auth.json and validates it the same way. Both store the
//     result under ~/.ccl/auth/commandcode.json.
//     Do not route Command Code traffic through a third-party proxy.
//
//  8. Session credentials
//     All runtimes bind 127.0.0.1 only and use a random per-session API key
//     that is never written back to ~/.ccl/config.yaml. OAuth credentials live
//     under ~/.ccl/auth and are filtered per backend so multi-login providers do
//     not share models or refresh tokens.
package oauthproxy
