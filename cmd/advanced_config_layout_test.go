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

func TestReviewShowsFastStatus(t *testing.T) {
	chatgpt := providerFrom("gpt", "https://api.openai.com/v1", "openai_responses")
	chatgpt.OAuthProvider = "gpt"
	chatgpt.FastMode = true
	m := NewAdvancedConfigModel(&chatgpt)
	m.page = 4
	view := m.View().Content
	if !strings.Contains(view, "Fast") || !strings.Contains(view, "on") {
		t.Fatalf("review page missing Fast=on: %q", view)
	}

	// Page 0 OAuth credentials also surfaces the pin.
	m.page = 0
	view = m.View().Content
	if !strings.Contains(view, "Fast") || !strings.Contains(view, "on") {
		t.Fatalf("oauth credentials page missing Fast=on: %q", view)
	}

	off := providerFrom("plain", "https://example.com/v1", "openai")
	m = NewAdvancedConfigModel(&off)
	m.page = 4
	view = m.View().Content
	if !strings.Contains(view, "Fast") || !strings.Contains(view, "off") {
		t.Fatalf("review page missing Fast=off: %q", view)
	}
}

func TestMaxOutputEditableForChatGPTOAuthAndManagedForCodexAndKiro(t *testing.T) {
	chatgpt := providerFrom("gpt", "https://api.openai.com/v1", "openai_responses")
	chatgpt.OAuthProvider = "gpt"
	m := NewAdvancedConfigModel(&chatgpt)
	if m.maxOutputUpstreamManaged() {
		t.Fatal("ChatGPT OAuth should allow client max output editing")
	}
	if m.canToggleOpenAIProtocol() {
		t.Fatal("OAuth protocol must be read-only on review")
	}
	m.page = 4
	view := m.View().Content
	if strings.Contains(view, "Upstream managed") || !strings.Contains(view, "Max Output") {
		t.Fatalf("ChatGPT OAuth review did not render editable Max Output: %q", view)
	}
	m.cursor = m.page4MaxOutCursor()
	next, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
	m = next.(*AdvancedConfigModel)
	if got := m.p.Env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"]; got != "16000" {
		t.Fatalf("ChatGPT OAuth max output after right = %q, want 16000", got)
	}
	legacyChatGPT := providerFrom("chatgpt", "https://api.openai.com/v1", "openai_responses")
	legacyChatGPT.OAuthProvider = "chatgpt"
	if NewAdvancedConfigModel(&legacyChatGPT).maxOutputUpstreamManaged() {
		t.Fatal("legacy chatgpt OAuth alias should allow client max output editing")
	}

	copilot := providerFrom("copilot", "oauth://codex", "openai_responses")
	copilot.OAuthProvider = "copilot"
	m = NewAdvancedConfigModel(&copilot)
	if !m.maxOutputUpstreamManaged() {
		t.Fatal("Copilot OAuth should treat max output as upstream-managed")
	}
	if m.availabilitySmokeTestModel() != lowCostProbeModel {
		t.Fatalf("Copilot smoke model = %q, want %q", m.availabilitySmokeTestModel(), lowCostProbeModel)
	}

	gemini := providerFrom("gemini", "oauth://gemini", "openai")
	gemini.OAuthProvider = "gemini"
	m = NewAdvancedConfigModel(&gemini)
	if m.maxOutputUpstreamManaged() {
		t.Fatal("Gemini OAuth should allow max output editing")
	}

	kiro := providerFrom("kiro", "oauth://kiro", "anthropic")
	kiro.OAuthProvider = "kiro"
	m = NewAdvancedConfigModel(&kiro)
	if !m.maxOutputUpstreamManaged() {
		t.Fatal("Kiro OAuth should treat max output as upstream-managed")
	}
	m.page = 4
	view = m.View().Content
	if !strings.Contains(view, "Upstream managed") {
		t.Fatalf("Kiro review should show Upstream managed, got %q", view)
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
	view = m.View().Content
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
	// Max Output is managed upstream for Codex, so moving up must not park on it.
	if m.cursor == m.page4MaxOutCursor() {
		t.Fatalf("up from Tools landed on the disabled Max Output row %d", m.cursor)
	}
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = next.(*AdvancedConfigModel)
	if m.cursor == m.page4MaxOutCursor() {
		t.Fatalf("down landed on the disabled Max Output row %d", m.cursor)
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

func TestPage2BlocksOneMWhenBackendWindowIsSmaller(t *testing.T) {
	p := provider.Provider{
		Type:        "openai_responses",
		Endpoint:    "https://example.test/v1",
		OpusModel:   "small-window",
		SonnetModel: "big-window",
		HaikuModel:  "unknown-window",
	}
	m := NewAdvancedConfigModel(&p)
	m.page = 2
	m.modelContextWindows = map[string]int{
		"small-window": 272_000,
		"big-window":   1_050_000,
	}

	view := m.View().Content
	if !strings.Contains(view, "backend 272K") || !strings.Contains(view, "no 1M") {
		t.Fatalf("expected the Opus row to explain why 1M is unavailable: %q", view)
	}

	// Opus: 1M must not be selectable.
	m.cursor = 0
	next, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*AdvancedConfigModel)
	if m.oneMSlots["opus"] {
		t.Fatal("1M was enabled for a model whose backend window is 272K")
	}

	// Sonnet: a 1M-class window stays selectable.
	m.cursor = 1
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*AdvancedConfigModel)
	if !m.oneMSlots["sonnet"] {
		t.Fatal("1M must remain selectable for a 1M-class model")
	}

	// Unknown window: the catalog is advisory, so keep it editable.
	m.cursor = 2
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*AdvancedConfigModel)
	if !m.oneMSlots["haiku"] {
		t.Fatal("1M must stay editable when the window is unknown")
	}

	// An existing marker on a blocked slot can still be cleared.
	m.oneMSlots["opus"] = true
	m.cursor = 0
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*AdvancedConfigModel)
	if m.oneMSlots["opus"] {
		t.Fatal("a stale [1m] marker on a blocked slot must be removable")
	}
}

func TestPage2DefaultsToClaudeAutoCompact(t *testing.T) {
	// A provider carrying the old Switch-safe preset must open on Claude default,
	// because ccl no longer writes context env at all.
	p := provider.Provider{
		Type:     "openai_responses",
		Endpoint: "https://example.test/v1",
		Env: map[string]string{
			maxContextTokensEnv:  maxContext300K,
			autoCompactWindowEnv: compactWindow300K,
		},
	}
	m := NewAdvancedConfigModel(&p)
	if m.compactPreset != compactPresetDefault {
		t.Fatalf("compact preset = %v, want Claude default", m.compactPreset)
	}
	m.page = 2
	view := m.View().Content
	if !strings.Contains(view, "(●)") || !strings.Contains(view, "Claude Code default") {
		t.Fatalf("Auto Compact should default to Claude Code default: %q", view)
	}
}

func TestCredentialsPageResolvesClickToField(t *testing.T) {
	p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1", APIKey: "sk-test"}
	m := NewAdvancedConfigModel(&p)
	m.page = 0
	m.width = 100
	m.height = 30

	view := m.View()
	if view.MouseMode == tea.MouseModeNone {
		t.Fatal("credentials page must report mouse clicks so fields can be focused")
	}
	if view.OnMouse == nil {
		t.Fatal("credentials page has no mouse handler")
	}
	lines := strings.Split(view.Content, "\n")

	// Every field is reachable by clicking its label row and the value row below it.
	for cursor, label := range credentialFieldLabels {
		labelRow := -1
		for i, line := range lines {
			if strings.Contains(line, label) {
				labelRow = i
				break
			}
		}
		if labelRow < 0 {
			t.Fatalf("label %q is not on the credentials page", label)
		}
		for _, row := range []int{labelRow, labelRow + 1} {
			got, ok := credentialFieldAtLine(lines, row)
			if !ok || got != cursor {
				t.Fatalf("click on row %d resolved to (%d, %t), want cursor %d", row, got, ok, cursor)
			}
		}
	}

	// A click far away from any field must be ignored rather than stealing focus.
	if _, ok := credentialFieldAtLine(lines, 0); ok {
		t.Fatal("click on the top padding row resolved to a field")
	}

	// The resolved click focuses the API key input.
	next, _ := m.Update(focusCredentialFieldMsg{cursor: 1})
	m = next.(*AdvancedConfigModel)
	if m.cursor != 1 || !m.keyInput.Focused() || m.urlInput.Focused() {
		t.Fatalf("cursor=%d url_focused=%t key_focused=%t, want the key input focused",
			m.cursor, m.urlInput.Focused(), m.keyInput.Focused())
	}
	next, _ = m.Update(focusCredentialFieldMsg{cursor: 0})
	m = next.(*AdvancedConfigModel)
	if m.cursor != 0 || !m.urlInput.Focused() || m.keyInput.Focused() {
		t.Fatalf("cursor=%d url_focused=%t key_focused=%t, want the endpoint input focused",
			m.cursor, m.urlInput.Focused(), m.keyInput.Focused())
	}
}

func TestOAuthCredentialsPageLeavesMouseAlone(t *testing.T) {
	// OAuth providers have no editable fields here, so the terminal keeps its own
	// selection behaviour.
	p := provider.Provider{Type: "openai_responses", OAuthProvider: "gpt", Endpoint: "oauth://codex"}
	m := NewAdvancedConfigModel(&p)
	m.page = 0
	m.width = 100
	m.height = 30
	if view := m.View(); view.MouseMode != tea.MouseModeNone || view.OnMouse != nil {
		t.Fatal("OAuth credentials page must not capture the mouse")
	}
}
