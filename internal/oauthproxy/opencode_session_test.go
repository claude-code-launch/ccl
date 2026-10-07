package oauthproxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// dialTo returns a client that sends every request to server whatever host the
// URL names, so a test can exercise the opencode.ai host gate locally.
func dialTo(server *httptest.Server) *http.Client {
	address := server.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}}
}

// TestOpenCodeReceivesTheClaudeCodeSession pins the OpenCode Go fix end to
// end: both adapters that build fresh upstream requests copy Claude Code's
// session ID into x-opencode-session, and only for an OpenCode host.
func TestOpenCodeReceivesTheClaudeCodeSession(t *testing.T) {
	for _, backend := range []string{"chat", "responses"} {
		for _, host := range []string{"opencode.ai", "other.example"} {
			t.Run(backend+"/"+host, func(t *testing.T) {
				var mu sync.Mutex
				var got string
				var seen bool
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					got, seen = r.Header.Get(openCodeSessionHeader), true
					mu.Unlock()
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/json")
					if backend == "chat" {
						_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
					} else {
						_, _ = io.WriteString(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
					}
				}))
				defer upstream.Close()

				endpoint := "http://" + host + "/zen/go/v1"
				routes := []runtimeModelRoute{{Alias: "m", Name: "m"}}
				var handler http.Handler
				if backend == "chat" {
					service := newChatCompletionsService("local", endpoint, routes, "key", NewUsageTracker())
					service.client = dialTo(upstream)
					handler = service.handler()
				} else {
					service := newCodexResponsesService("local", endpoint, routes, &codexStaticAuthorizer{token: "key"}, NewUsageTracker())
					service.client = dialTo(upstream)
					handler = service.handler()
				}

				request := httptest.NewRequest(http.MethodPost, "/v1/messages",
					strings.NewReader(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
				request.Header.Set("x-api-key", "local")
				request.Header.Set(claudeCodeSessionHeader, "0b3f846a-2502-40af-a13e-26620a6d305a")
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
				}

				mu.Lock()
				defer mu.Unlock()
				if !seen {
					t.Fatal("upstream was not called")
				}
				want := ""
				if host == "opencode.ai" {
					want = "0b3f846a-2502-40af-a13e-26620a6d305a"
				}
				if got != want {
					t.Fatalf("%s = %q, want %q", openCodeSessionHeader, got, want)
				}
			})
		}
	}
}

func TestApplyOpenCodeSessionRules(t *testing.T) {
	incoming := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	incoming.Header.Set(claudeCodeSessionHeader, " session-1 ")
	ctx := withClientSession(context.Background(), incoming)

	for host, want := range map[string]string{
		"opencode.ai":         "session-1",
		"go.opencode.ai":      "session-1",
		"OpenCode.AI.":        "session-1",
		"notopencode.ai":      "",
		"opencode.ai.evil":    "",
		"api.openai.com":      "",
		"chatgpt.com":         "",
		"opencode.example.ai": "",
	} {
		upstream, err := http.NewRequest(http.MethodPost, "https://"+host+"/v1/chat/completions", nil)
		if err != nil {
			t.Fatal(err)
		}
		applyOpenCodeSession(ctx, upstream)
		if got := upstream.Header.Get(openCodeSessionHeader); got != want {
			t.Fatalf("host %q: %s = %q, want %q", host, openCodeSessionHeader, got, want)
		}
	}

	// An explicit value is never overwritten.
	upstream, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/go/v1", nil)
	upstream.Header.Set(openCodeSessionHeader, "explicit")
	applyOpenCodeSession(ctx, upstream)
	if got := upstream.Header.Get(openCodeSessionHeader); got != "explicit" {
		t.Fatalf("explicit session overwritten: %q", got)
	}

	// No client session, nothing to send.
	upstream, _ = http.NewRequest(http.MethodPost, "https://opencode.ai/zen/go/v1", nil)
	applyOpenCodeSession(withClientSession(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil)), upstream)
	if got := upstream.Header.Get(openCodeSessionHeader); got != "" {
		t.Fatalf("session invented without a client session: %q", got)
	}
	var noContext context.Context
	applyOpenCodeSession(noContext, upstream)
	applyOpenCodeSession(ctx, nil)
	if withClientSession(ctx, nil) != ctx {
		t.Fatal("a nil incoming request changed the context")
	}
}
