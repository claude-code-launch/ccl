// Package oauthproxy implements ccl's local subscription and protocol runtimes.
//
// Claude Code talks to an Anthropic Messages endpoint. CCL directly owns every
// data plane: Codex Responses (GPT and openai_responses API-key gateways),
// OpenAI Chat (manual API-key providers, Grok, Kimi, WorkBuddy, and Copilot Chat
// models), the native-Anthropic Messages passthrough (models.dev
// @ai-sdk/anthropic models and Copilot native-Messages models), and Gemini
// (Antigravity conversion). Copilot's mixed catalog, Kiro, and Qoder run
// entirely on CCL-owned runtimes too. Direct Anthropic API-key gateways bypass
// this package altogether. AutoClaw uses a CCL-owned Anthropic-to-OpenAI Chat
// runtime with its desktop OAuth refresh session and managed-proxy headers.
//
// Error recovery follows the data-plane owner. Every CCL-owned data plane
// first applies the shared fast-retry loop (retry.go): an upstream 429 or 5xx
// outcome is retried twice more after 500ms and 1s — Retry-After hints are
// not honored inside the loop, they are relayed on final failure so Claude
// Code runs its own full backoff over the untouched status/body/Retry-After.
// Per-provider recovery happens INSIDE one attempt: Codex Responses, Grok, and
// Kimi refresh once after a 401; WorkBuddy refreshes once after a 401/403;
// Gemini also falls back from the daily to the prod Antigravity base on
// network errors, 429s and 5xx responses (so one attempt is up to 2 upstream
// calls); Qoder rotates credentials and re-signs COSY per attempt. Three layers
// are deliberately outside the loop: Kiro keeps its own longer 1/2/4s budget
// with per-round credential rotation (stacking would double the backoff), and
// the WorkBuddy and Copilot loopback gateways are the INNER hop of a two-hop
// path — the outer chat/responses service owns the fast retry, so wrapping
// the gateway too would retry 3×3 = 9 times per request.
//
// # Direct data planes
//
// Each backend's data plane is CCL-owned end-to-end. Treat these as a
// regression checklist for keeping provider traffic on the corresponding CCL
// runtime:
//
//  1. Codex Responses ownership (codex_responses_*.go)
//     CCL owns Messages-to-Responses translation, Codex identity headers, GPT
//     token refresh, upstream errors, Responses SSE decoding, and usage.
//
//  2. GitHub Copilot direct gateway (copilot_runtime.go)
//     Copilot authenticates with GitHub, discovers the account's authoritative model catalog, and
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
//     run in ccl. Keep Kiro's direct request path and upstream identity intact.
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
//  7. AutoClaw runtime (autoclaw_*.go)
//     AutoClaw's desktop login is stored in an encrypted auth.json. `ccl oauth
//     autoclaw` imports that completed session into ~/.ccl/auth (0600), and
//     the loopback adapter converts Claude Messages to the managed OpenAI Chat
//     endpoint. CCL refreshes access_token with refresh_token, sends the
//     desktop-compatible X-Authorization/X-Request-Model/X-Harness-Type
//     headers, and never starts or modifies AutoClaw at runtime.
//
//  8. Session credentials
//     All runtimes bind 127.0.0.1 only and use a random per-session API key
//     that is never written back to ~/.ccl/config.yaml. OAuth credentials live
//     under ~/.ccl/auth and are filtered per backend so multi-login providers do
//     not share models or refresh tokens.
package oauthproxy
