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
	if m.p.Type != "openai" {
		t.Fatalf("protocol = %q, want Chat", m.p.Type)
	}
}

func TestPreferredDetectionPreservesSelection(t *testing.T) {
	for _, typ := range []string{"openai", "openai_responses"} {
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
			result := detectProtocolAndModelsPreferred(server.URL, "fake-key", typ)
			if result.err != nil || result.protocol != typ || calls != 1 {
				t.Fatalf("result=%+v calls=%d", result, calls)
			}
		})
	}
}
