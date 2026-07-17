package cmd

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
	chatgpt := providerFrom("chatgpt", "https://api.openai.com/v1", "openai_responses")
	chatgpt.OAuthProvider = "chatgpt"
	m := NewAdvancedConfigModel(&chatgpt)
	if !m.maxOutputUpstreamManaged() {
		t.Fatal("ChatGPT OAuth should treat max output as upstream-managed")
	}
	if m.canToggleOpenAIProtocol() {
		t.Fatal("OAuth protocol must be read-only on review")
	}

	gemini := providerFrom("gemini", "oauth://gemini", "openai")
	gemini.OAuthProvider = "gemini"
	m = NewAdvancedConfigModel(&gemini)
	if m.maxOutputUpstreamManaged() {
		t.Fatal("Gemini OAuth should allow max output editing")
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
		t.Fatalf("plain responses unexpectedly shows Upstream managed: %q", view)
	}

	m = NewAdvancedConfigModel(&codex)
	m.page = 4
	view = m.View().Content
	if !strings.Contains(view, "Upstream managed") {
		t.Fatalf("codex review should show Upstream managed, got %q", view)
	}
}

func TestPage4UpFromToolsSkipsDisabledMaxOutput(t *testing.T) {
	codex := providerFrom("codex", "https://example.com/codex", "openai_responses")
	m := NewAdvancedConfigModel(&codex)
	m.page = 4
	m.cursor = m.page4ToolsCursor()
	next, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	m = next.(*AdvancedConfigModel)
	if m.cursor != m.page4CompactCursor() {
		t.Fatalf("up from Tools landed on cursor %d, want Compact %d (skip Max Output)", m.cursor, m.page4CompactCursor())
	}
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = next.(*AdvancedConfigModel)
	if m.cursor != m.page4ToolsCursor() {
		t.Fatalf("down from Compact landed on cursor %d, want Tools %d", m.cursor, m.page4ToolsCursor())
	}
}

func providerFrom(name, endpoint, typ string) provider.Provider {
	return provider.Provider{Name: name, Endpoint: endpoint, Type: typ, APIKey: "k", Model: "m"}
}

func TestReviewFitsCommonTerminalHeights(t *testing.T) {
	p := providerFrom("p", "https://example.com/v1", "openai")
	m := NewAdvancedConfigModel(&p)
	m.page = 4
	m.width = 100
	m.manualConfig = true

	for _, h := range []int{24, 26, 27, 28, 30} {
		m.height = h
		view := m.View().Content
		got := lipgloss.Height(view)
		if got > h {
			t.Fatalf("terminal height %d rendered %d lines (overflow)\n%s", h, got, view)
		}
		if !strings.Contains(view, "Apply & Finish") {
			t.Fatalf("Apply not visible at height %d", h)
		}
	}
}
