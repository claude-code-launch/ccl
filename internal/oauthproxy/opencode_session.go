package oauthproxy

import (
	"context"
	"net/http"
	"strings"
)

// OpenCode Go rejects requests without a stable per-conversation ID in
// x-opencode-session (HTTP 400 MissingSessionID). Claude Code already sends one
// as X-Claude-Code-Session-Id, and the Anthropic passthrough forwards it
// unchanged, which Go accepts. The Chat Completions and Responses adapters build
// fresh upstream requests, so they carry the ID over explicitly — and only to
// OpenCode, so the other gateways sharing these adapters keep their own wire
// identity.
const (
	claudeCodeSessionHeader = "X-Claude-Code-Session-Id"
	openCodeSessionHeader   = "X-Opencode-Session"
	// claudeCodeCompactionHeader is one of the gateway hint headers ccl turns on
	// for its loopback sessions (CLAUDE_CODE_GATEWAY_HINT_HEADERS=1).
	claudeCodeCompactionHeader = "X-Claude-Code-Compaction"
)

// cclFastModeHeader is how a ccl session asks its loopback runtime for fast
// responses: the launcher adds it through ANTHROPIC_CUSTOM_HEADERS when the
// provider has Fast on, since Claude Code only sends its own speed field for
// the models it knows support fast mode.
const cclFastModeHeader = "X-Ccl-Fast-Mode"

// claudeCodeHintHeaders are the routing hints Claude Code sends when gateway
// hint headers are on, plus ccl's own fast-mode header. ccl's runtimes read
// them; they are removed before a request is passed through to a third-party
// upstream, which might reject unknown headers.
var claudeCodeHintHeaders = []string{
	cclFastModeHeader,
	"X-Claude-Code-Request-Class",
	"X-Claude-Code-Agent-Type",
	claudeCodeCompactionHeader,
	"X-Claude-Code-Context-Compacted",
	"X-Claude-Code-Prev-Tool-Durations",
	"X-Claude-Code-Prompt-Id",
}

type clientSessionKey struct{}

// withClientSession records the Claude Code session ID of an incoming request.
func withClientSession(ctx context.Context, incoming *http.Request) context.Context {
	if incoming == nil {
		return ctx
	}
	id := strings.TrimSpace(incoming.Header.Get(claudeCodeSessionHeader))
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, clientSessionKey{}, id)
}

func clientSession(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(clientSessionKey{}).(string)
	return id
}

// applyOpenCodeSession stamps an OpenCode upstream request with the client's
// session ID. A request that already carries one is left alone.
func applyOpenCodeSession(ctx context.Context, request *http.Request) {
	if request == nil || request.URL == nil || !isOpenCodeHost(request.URL.Hostname()) {
		return
	}
	if request.Header.Get(openCodeSessionHeader) != "" {
		return
	}
	if id := clientSession(ctx); id != "" {
		request.Header.Set(openCodeSessionHeader, id)
	}
}

func isOpenCodeHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
}
