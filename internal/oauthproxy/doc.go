// Package oauthproxy implements ccl's local subscription and protocol runtimes.
//
// Clients talk to the OpenAI Responses or Chat Completions loopback endpoints.
// CCL directly owns every data plane: Codex Responses (GPT and openai_responses
// API-key gateways), OpenAI Chat (manual API-key providers, Kimi, and WorkBuddy),
// Grok (cli-chat-proxy Responses with Grok Build identity), Gemini (Antigravity
// conversion), and Cursor (Connect bidirectional agent stream). Kiro and Qoder
// run entirely on CCL-owned runtimes too. AutoClaw uses a CCL-owned OpenAI Chat
// runtime with its desktop OAuth refresh session and managed-proxy headers.
//
// Error recovery follows the data-plane owner. Every CCL-owned data plane
// first applies the shared fast-retry loop (retry.go): an upstream 429 or 5xx
// outcome is retried at most twice more after 500ms and 1s. A longer
// Retry-After ends the fast loop immediately and is relayed so the client
// can honor the upstream wait without CCL sending more requests first.
// Per-provider recovery happens INSIDE one attempt: Codex Responses, Grok, and
// Kimi refresh once after a 401; WorkBuddy refreshes once after a 401/403;
// Gemini starts from its discovered catalog base URL and falls back to the
// secondary Antigravity base (daily <-> prod) on network errors, 429s
// and 5xx responses (one attempt is then up to 2 upstream calls); Qoder uses
// its single bound credential and re-signs COSY per attempt. Two layers are
// deliberately outside the loop: Kiro keeps its own longer 1/2/4s budget with
// per-round credential rotation (stacking would double the backoff), and the
// WorkBuddy loopback gateway is the INNER hop of a two-hop path — the outer
// chat service owns the fast retry, so wrapping the gateway too would retry
// 3×3 = 9 times per request.
//
// # Direct data planes
//
// Each backend's data plane is CCL-owned end-to-end. Treat these as a
// regression checklist for keeping provider traffic on the corresponding CCL
// runtime:
//
//  1. Codex Responses ownership (codex_responses_*.go)
//     CCL owns Responses forwarding and tool flattening, Codex identity headers,
//     GPT token refresh, upstream errors, Responses SSE decoding, and usage.
//
//  2. Qoder direct runtime (qoder_*.go)
//     Qoder browser OAuth, refresh, COSY signing, WAF body encoding, model
//     discovery, and Responses translation all run in this process.
//     The upstream request's session_type="qodercli" is a protocol identity
//     field only; do not replace the direct runtime with a qodercli subprocess.
//
//  3. Kiro direct runtime (kiro_*.go)
//     Kiro Portal PKCE / Builder ID auth, credential refresh, model discovery,
//     Responses-to-Amazon-Q conversion, retry, and AWS EventStream decoding all
//     run in ccl. Keep Kiro's direct request path and upstream identity intact.
//
//  4. WorkBuddy runtime (workbuddy_*.go)
//     CCL owns the official external-link login polling, credential refresh,
//     /v3/config model catalog, WorkBuddy identity/session headers, and the
//     OpenAI Responses <-> OpenAI Chat Completions conversion (via the shared
//     chatCompletionsService). Codex effort controls are resolved centrally
//     from the local models.dev cache, not WorkBuddy reasoning details.
//     WorkBuddy has two public deployments that run
//     the same plugin API and chat protocol — www.workbuddy.ai (workbuddy) and
//     www.workbuddy.cn (workbuddycn). One implementation serves both; a
//     workbuddyDeployment carries the origin, and the public OAuth provider
//     name is also the credential `type`, so the two account stores never mix.
//
//  5. AutoClaw runtime (autoclaw_*.go)
//     `ccl oauth autoclaw` owns the Google browser callback and Aliyun human
//     verification page, then stores the returned refreshable session under
//     ~/.ccl/auth (0600). `ccl oauth autoclaw --from-desktop` remains the optional path for
//     copying AutoClaw's encrypted desktop auth.json. The loopback adapter
//     converts Responses to the managed OpenAI Chat endpoint, refreshes
//     access_token with refresh_token, sends the desktop-compatible
//     X-Authorization/X-Request-Model/X-Harness-Type headers, and never starts
//     or modifies AutoClaw at runtime.
//
//  6. Gemini Antigravity runtime (gemini_*.go)
//     `ccl oauth gemini` discovers the bound account's Antigravity catalog via
//     loadCodeAssist and fetchAvailableModels, caches validated wire IDs and
//     modalities per account digest, groups thinking variants into families,
//     normalizes readable display aliases to canonical catalog IDs, and
//     translates OpenAI Responses to Antigravity streams.
//
//  7. Cursor direct runtime (cursor_*.go)
//     `ccl oauth cursor` runs PKCE sign-in and polls for tokens. At runtime it
//     bridges OpenAI Responses over Cursor's Connect bidirectional agent
//     stream (Run + GetUsableModels), parking the open stream across tool calls
//     so tool results resume the active turn while the client owns execution.
//
//  8. Session credentials
//     All runtimes bind 127.0.0.1 only and use a random per-session API key
//     that is never written back to ~/.ccl/config.yaml. OAuth credentials live
//     under ~/.ccl/auth and are filtered per backend so multi-login providers do
//     not share models or refresh tokens.
package oauthproxy
