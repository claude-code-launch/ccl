package cmd

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestTruncateMiddleASCII(t *testing.T) {
	got := truncateMiddle("https://example.com/very/long/path/to/resource", 24)
	if !strings.Contains(got, "…") {
		t.Fatalf("expected ellipsis, got %q", got)
	}
	if lipgloss.Width(got) > 24 {
		t.Fatalf("width %d > 24 for %q", lipgloss.Width(got), got)
	}
	if !strings.Contains(got, "http") && !strings.Contains(got, "resource") {
		t.Fatalf("expected head or tail retained, got %q", got)
	}
}

func TestTruncateMiddleCJK(t *testing.T) {
	s := strings.Repeat("中", 20)
	got := truncateMiddle(s, 18)
	if got == "…" || got == "" {
		t.Fatalf("CJK truncate degenerated to %q", got)
	}
	if lipgloss.Width(got) > 18 {
		t.Fatalf("width %d > 18 for %q", lipgloss.Width(got), got)
	}
	if !strings.Contains(got, "…") || !strings.Contains(got, "中") {
		t.Fatalf("expected CJK content with ellipsis, got %q", got)
	}
}

func TestTruncateMiddleEmoji(t *testing.T) {
	s := strings.Repeat("😀", 12)
	got := truncateMiddle(s, 10)
	if got == "…" {
		t.Fatalf("emoji truncate degenerated")
	}
	if lipgloss.Width(got) > 10 {
		t.Fatalf("width %d > 10 for %q", lipgloss.Width(got), got)
	}
}

func TestMaxOutputUpstreamManagedForCodexAndOAuth(t *testing.T) {
	oauth := providerFrom("chatgpt", "https://api.openai.com/v1", "openai_responses")
	oauth.OAuthProvider = "chatgpt"
	m := NewAdvancedConfigModel(&oauth)
	if !m.maxOutputUpstreamManaged() {
		t.Fatal("ChatGPT OAuth should treat max output as upstream-managed")
	}

	codex := providerFrom("codex", "https://example.com/codex", "openai_responses")
	m = NewAdvancedConfigModel(&codex)
	if !m.maxOutputUpstreamManaged() {
		t.Fatal("dedicated /codex endpoint should be upstream-managed")
	}

	plain := providerFrom("plain", "https://example.com/v1", "openai_responses")
	m = NewAdvancedConfigModel(&plain)
	if m.maxOutputUpstreamManaged() {
		t.Fatal("plain responses should allow max output editing")
	}

	m.page = 4
	view := m.View().Content
	if strings.Contains(view, "Upstream managed") {
		// plain should show editable control
		t.Fatalf("plain responses unexpectedly shows Upstream managed: %q", view)
	}

	m = NewAdvancedConfigModel(&codex)
	m.page = 4
	view = m.View().Content
	if !strings.Contains(view, "Upstream managed") {
		t.Fatalf("codex review should show Upstream managed, got %q", view)
	}
}

func providerFrom(name, endpoint, typ string) provider.Provider {
	return provider.Provider{Name: name, Endpoint: endpoint, Type: typ, APIKey: "k", Model: "m"}
}

func TestReviewCompactsBelow28Lines(t *testing.T) {
	p := providerFrom("p", "https://example.com/v1", "openai")
	m := NewAdvancedConfigModel(&p)
	m.page = 4
	m.width = 100

	m.height = 24
	h24 := lipgloss.Height(m.View().Content)
	m.height = 30
	h30 := lipgloss.Height(m.View().Content)
	if h24 > 24 {
		// Allow slight overshoot from outer chrome; must still be tighter than full.
		if h24 >= h30 {
			t.Fatalf("expected compact 24-row view shorter than 30-row: h24=%d h30=%d", h24, h30)
		}
	}
	if h30 < h24 {
		t.Fatalf("unexpected heights h24=%d h30=%d", h24, h30)
	}
}
