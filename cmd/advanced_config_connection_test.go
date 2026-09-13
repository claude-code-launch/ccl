package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	tui "github.com/grindlemire/go-tui"
)

func TestConnectionNavigationClearsEditingFocus(t *testing.T) {
	for _, key := range []tui.Key{tui.KeyUp, tui.KeyDown, tui.KeyTab} {
		p := providerFrom("fake", "https://example.invalid/v1", "openai")
		m := NewAdvancedConfigModel(&p)
		m.cursor = m.mainRowIndex(rowAPIKey)
		m.keyFocused = true
		m.handleKey(tui.KeyEvent{Key: key})
		if m.keyFocused || m.urlFocused {
			t.Fatalf("key %v left stale input focus", key)
		}
	}
}

func TestConnectionSelectorsBeforeDetection(t *testing.T) {
	p := providerFrom("fake", "https://example.invalid/v1", "")
	m := NewAdvancedConfigModel(&p)
	if m.connectionReady() {
		t.Fatal("new connection unexpectedly ready")
	}
	m.cursor = m.mainRowIndex(rowProtocol)
	m.adjustReviewField(1)
	if m.p.Type != "openai" {
		t.Fatalf("first protocol = %q", m.p.Type)
	}
	m.adjustReviewField(1)
	m.adjustReviewField(1)
	if m.p.Type != "anthropic" {
		t.Fatalf("protocol = %q", m.p.Type)
	}
	m.cursor = m.mainRowIndex(rowAuth)
	m.adjustReviewField(1)
	if m.p.AnthropicAuth != anthropicAuthXAPIKey {
		t.Fatalf("auth = %q", m.p.AnthropicAuth)
	}
	m.adjustReviewField(1)
	if m.p.AnthropicAuth != anthropicAuthBearer {
		t.Fatalf("auth = %q", m.p.AnthropicAuth)
	}
	m.cursor = m.mainRowIndex(rowProtocol)
	m.adjustReviewField(1)
	if m.p.AnthropicAuth != "" {
		t.Fatal("anthropic auth leaked to OpenAI")
	}
	m.adjustReviewField(-1)
	if m.p.AnthropicAuth != anthropicAuthBearer {
		t.Fatal("auth choice lost")
	}
}

func TestPreferredDetectionPreservesSelection(t *testing.T) {
	for _, typ := range []string{"anthropic", "openai_responses"} {
		t.Run(typ, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer fake-key" || r.Header.Get("x-api-key") != "" {
					t.Error("wrong selected auth")
				}
				fmt.Fprint(w, `{"data":[{"id":"fake-model","object":"model"}]}`)
			}))
			defer server.Close()
			result := detectProtocolAndModelsPreferred(server.URL, "fake-key", typ, "bearer")
			if result.err != nil || result.protocol != typ || calls != 1 {
				t.Fatalf("result=%+v calls=%d", result, calls)
			}
		})
	}
}

func TestPreferredDetectionFallsBack(t *testing.T) {
	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if r.Header.Get("x-api-key") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"fake-model","object":"model"}]}`)
	}))
	defer server.Close()
	result := detectProtocolAndModelsPreferred(server.URL, "fake-key", "anthropic", "x-api-key")
	if result.err != nil || result.protocol != "openai" || len(auths) != 2 || auths[0] != "" || auths[1] != "Bearer fake-key" {
		t.Fatalf("result=%+v auths=%v", result, auths)
	}
}
