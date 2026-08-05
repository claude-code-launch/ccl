package cmd

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/claude-code-launch/ccl/internal/protocol"
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
	enterDetectedReview(m, "m")
	view := m.View().Content
	if !strings.Contains(view, "Fast") || !strings.Contains(view, "‹ On ›") {
		t.Fatalf("review page missing Fast=on: %q", view)
	}

	off := providerFrom("plain", "https://example.com/v1", "openai")
	m = NewAdvancedConfigModel(&off)
	enterDetectedReview(m, "m")
	view = m.View().Content
	if !strings.Contains(view, "Fast") || !strings.Contains(view, "‹ Off ›") {
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
	enterDetectedReview(m, "m")
	view := m.View().Content
	if strings.Contains(view, "Upstream managed") || !strings.Contains(view, "Max Output") {
		t.Fatalf("ChatGPT OAuth review did not render editable Max Output: %q", view)
	}
	m.cursor = m.mainRowIndex(rowMaxOutput)
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

	copilot := providerFrom("copilot", "oauth://copilot", "openai_responses")
	copilot.OAuthProvider = "copilot"
	m = NewAdvancedConfigModel(&copilot)
	if !m.maxOutputUpstreamManaged() {
		t.Fatal("Copilot OAuth should treat max output as upstream-managed")
	}
	if m.availabilitySmokeTestModel() != "" {
		t.Fatalf("Copilot should use its discovered model catalog, got smoke model %q", m.availabilitySmokeTestModel())
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
	enterDetectedReview(m, "m")
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

	enterDetectedReview(m, "m")
	view = m.View().Content
	if strings.Contains(view, "Upstream managed") {
		t.Fatalf("plain responses unexpectedly shows Upstream managed: %q", view)
	}

	m = NewAdvancedConfigModel(&codex)
	enterDetectedReview(m, "m")
	view = m.View().Content
	if !strings.Contains(view, "Upstream managed") {
		t.Fatalf("codex review should show Upstream managed, got %q", view)
	}
}

func TestPageUpDownStaysWithinVisibleRows(t *testing.T) {
	// Moving up/down must stay inside the single page's visible row set, wrapping
	// at the ends instead of drifting into a disabled row.
	cp := providerFrom("codex", "https://example.com/codex", "openai_responses")
	m := NewAdvancedConfigModel(&cp)
	enterDetectedReview(m, "codex-max")
	rows := len(m.visibleRows())
	m.cursor = 0
	next, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	m = next.(*AdvancedConfigModel)
	if m.cursor != rows-1 {
		t.Fatalf("up from the first row landed on %d, want the last row %d", m.cursor, rows-1)
	}
	m.cursor = rows - 1
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = next.(*AdvancedConfigModel)
	if m.cursor != 0 {
		t.Fatalf("down from the last row landed on %d, want the first row 0", m.cursor)
	}
}

func providerFrom(name, endpoint, typ string) provider.Provider {
	return provider.Provider{Name: name, Endpoint: endpoint, Type: typ, APIKey: "k", Model: "m"}
}

// enterDetectedReview puts a model into the post-detection single-page state:
// a populated pool, discovered flag set, and the cursor on a model row. It is the
// single-page equivalent of the old "m.page = 4" idiom.
func enterDetectedReview(m *AdvancedConfigModel, models ...string) *AdvancedConfigModel {
	m.modelPool = append([]string(nil), models...)
	m.modelPoolFromDiscovery = true
	m.p.Model = strings.Join(models, ",")
	m.page = 4
	if len(models) > 0 {
		m.cursor = m.mainRowIndex(rowOpus)
	}
	return m
}

func TestReviewFitsCommonTerminalHeights(t *testing.T) {
	p := providerFrom("p", "https://example.com/v1", "openai")
	m := NewAdvancedConfigModel(&p)
	enterDetectedReview(m, "model-a", "model-b", "model-c")
	m.width = 100

	// The single page is scrollable: at every terminal height, moving the cursor
	// to the Save row scrolls it into view, and the rendered frame never exceeds
	// the terminal.
	for _, h := range []int{24, 26, 27, 28, 30} {
		m.height = h
		m.cursor = m.mainRowIndex(rowSave)
		m.keepCursorVisible()
		view := m.View().Content
		got := lipgloss.Height(view)
		if got > h {
			t.Fatalf("terminal height %d rendered %d lines (overflow)\n%s", h, got, view)
		}
		if !strings.Contains(view, "Save & Activate") {
			t.Fatalf("Save not visible at height %d", h)
		}
	}
}

func TestSinglePageBlocksOneMWhenBackendWindowIsSmaller(t *testing.T) {
	p := provider.Provider{
		Type:        "openai_responses",
		Endpoint:    "https://example.test/v1",
		OpusModel:   "small-window",
		SonnetModel: "big-window",
		HaikuModel:  "unknown-window",
	}
	m := NewAdvancedConfigModel(&p)
	enterDetectedReview(m, "small-window", "big-window", "unknown-window")
	m.modelContextWindows = map[string]int{
		"small-window": 272_000,
		"big-window":   1_050_000,
	}

	// Opus: 1M must not be selectable via Space.
	m.cursor = m.mainRowIndex(rowOpus)
	next, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: ' '}))
	m = next.(*AdvancedConfigModel)
	if m.oneMSlots["opus"] {
		t.Fatal("1M was enabled for a model whose backend window is 272K")
	}

	// Sonnet: a 1M-class window stays selectable.
	m.cursor = m.mainRowIndex(rowSonnet)
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: ' '}))
	m = next.(*AdvancedConfigModel)
	if !m.oneMSlots["sonnet"] {
		t.Fatal("1M must remain selectable for a 1M-class model")
	}

	// Unknown window: the catalog is advisory, so keep it editable.
	m.cursor = m.mainRowIndex(rowHaiku)
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: ' '}))
	m = next.(*AdvancedConfigModel)
	if !m.oneMSlots["haiku"] {
		t.Fatal("1M must stay editable when the window is unknown")
	}

	// An existing marker on a blocked slot can still be cleared.
	m.oneMSlots["opus"] = true
	m.cursor = m.mainRowIndex(rowOpus)
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: ' '}))
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
	// The legacy preset is dropped, not surfaced, so the saved provider clears the
	// stale context env on apply.
	if m.compactState.legacy {
		t.Fatalf("legacy compact state should be recognized as default, got %+v", m.compactState)
	}
}

func TestCredentialsPageResolvesClickToField(t *testing.T) {
	p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1", APIKey: "sk-test"}
	m := NewAdvancedConfigModel(&p)
	m.page = 4
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
	for _, field := range credentialFields {
		labelRow := -1
		for i, line := range lines {
			if strings.Contains(line, field.label) {
				labelRow = i
				break
			}
		}
		if labelRow < 0 {
			t.Fatalf("label %q is not on the credentials page", field.label)
		}
		for _, row := range []int{labelRow, labelRow + 1} {
			got, ok := credentialFieldAtLine(lines, row)
			if !ok || got != field.cursor {
				t.Fatalf("click on row %d resolved to (%d, %t), want cursor %d", row, got, ok, field.cursor)
			}
		}
	}

	// Prose that merely mentions a label must not steal focus.
	if _, ok := credentialFieldAtLine([]string{"  detection uses the API Key you entered"}, 0); ok {
		t.Fatal("a hint mentioning \"API Key\" resolved to the field")
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
	m.page = 4
	m.width = 100
	m.height = 30
	if view := m.View(); view.MouseMode != tea.MouseModeNone || view.OnMouse != nil {
		t.Fatal("OAuth credentials page must not capture the mouse")
	}
}

func TestSinglePageBlocksOneMForMixedCaseModelIDs(t *testing.T) {
	// The catalog is keyed by lowercased model id; a gateway serving mixed-case ids
	// must not slip past the check. On the single page the 1M marker is toggled by
	// Space on a model row, not by Enter on the removed page 2.
	p := provider.Provider{
		Type:      "openai",
		Endpoint:  "https://example.test/v1",
		OpusModel: "GLM-4.6",
	}
	m := NewAdvancedConfigModel(&p)
	m.modelContextWindows = map[string]int{"glm-4.6": 200_000}

	if !m.oneMSlotBlocked("GLM-4.6") {
		t.Fatal("a 200K model must block 1M regardless of id casing")
	}
	enterDetectedReview(m, "GLM-4.6")
	m.cursor = m.mainRowIndex(rowOpus)
	next, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: ' '}))
	m = next.(*AdvancedConfigModel)
	if m.oneMSlots["opus"] {
		t.Fatal("1M was enabled for a 200K model with a mixed-case id")
	}
}

func TestSelectingCustomCompactOptsOutOfContextPolicy(t *testing.T) {
	// Custom promises hand-set context env survives, which only holds when the
	// provider opts out of ccl's context policy.
	p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1"}
	m := NewAdvancedConfigModel(&p)

	m.selectCompactPreset(1) // Custom
	if m.compactPreset != compactPresetPreserve {
		t.Fatalf("compact preset = %v, want Custom", m.compactPreset)
	}
	if !provider.ContextBudgetIsManual(*m.p) {
		t.Fatalf("Custom did not opt out of the context policy: %#v", m.p.Env)
	}

	m.selectCompactPreset(0) // Claude Code default
	if provider.ContextBudgetIsManual(*m.p) {
		t.Fatalf("the default choice must not keep the opt-out: %#v", m.p.Env)
	}
}

// typeKey builds the KeyPressMsg a terminal sends for a printable character.
func typeKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)})
}

// quits reports whether cmd is the quit command, by identity rather than by
// running it: a key that reaches a text input comes back as the cursor's blink
// cmd, which blocks for the whole blink interval when called.
func quits(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	return reflect.ValueOf(cmd).Pointer() == reflect.ValueOf(tea.Cmd(tea.Quit)).Pointer()
}

func TestTextInputsKeepSingleLetterKeysInsteadOfActingOnThem(t *testing.T) {
	// "q" quit and the vim aliases are also ordinary characters. While a text
	// input owns the keyboard they must be typed, not obeyed: an API key
	// containing "q", or filtering for "qwen"/"kimi", used to quit the TUI or
	// silently move the cursor and insert the letter at the same time.
	for _, tc := range []struct {
		name   string
		cursor int
		field  func(*AdvancedConfigModel) string
	}{
		{"endpoint", 0, func(m *AdvancedConfigModel) string { return m.urlInput.Value() }},
		{"api key", 1, func(m *AdvancedConfigModel) string { return m.keyInput.Value() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, r := range []rune{'q', 'k', 'j', 'h', 'l'} {
				p := provider.Provider{Type: "openai"}
				m := NewAdvancedConfigModel(&p)
				m.page = 4
				m.cursor = tc.cursor

				next, cmd := m.Update(typeKey(r))
				m = next.(*AdvancedConfigModel)

				if quits(cmd) {
					t.Fatalf("typing %q into the %s quit the TUI", r, tc.name)
				}
				if m.cursor != tc.cursor {
					t.Fatalf("typing %q moved the cursor %d -> %d", r, tc.cursor, m.cursor)
				}
				if got := tc.field(m); got != string(r) {
					t.Fatalf("%s = %q after typing %q, want the character inserted", tc.name, got, r)
				}
			}
		})
	}
}

func TestSlotFilterTypesLettersThatAreAlsoShortcuts(t *testing.T) {
	// The slot picker navigates with j/k, but only until the filter is focused:
	// there, "kimi" has to be typeable. Arrow keys stay unambiguous.
	p := provider.Provider{Type: "openai"}
	m := NewAdvancedConfigModelAtPage1(&p, []string{"kimi-k2", "qwen3-coder", "glm-4.6"})
	m.cursor = 0
	m.filterInput.Focus()

	for _, r := range "kimi" {
		next, cmd := m.Update(typeKey(r))
		m = next.(*AdvancedConfigModel)
		if quits(cmd) {
			t.Fatalf("typing %q into the slot filter quit the TUI", r)
		}
	}
	if got := m.filterInput.Value(); got != "kimi" {
		t.Fatalf("filter = %q, want %q", got, "kimi")
	}
	if len(m.filteredPool) != 1 || m.filteredPool[0] != "kimi-k2" {
		t.Fatalf("filtered pool = %v, want only kimi-k2", m.filteredPool)
	}

	// The list still moves with the arrow keys while the filter has focus.
	m.filterInput.SetValue("")
	m.updateFilteredPool()
	m.slotListCursor = 0
	next, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = next.(*AdvancedConfigModel)
	if m.slotListCursor != 1 {
		t.Fatalf("slot list cursor = %d after ↓, want 1", m.slotListCursor)
	}
}

func TestSlotPickerDisplaysCatalogMetadataButPersistsModelID(t *testing.T) {
	p := provider.Provider{Type: "anthropic", SubagentModel: "dfmodel"}
	rate := 0.5
	flashRate := 0.1
	metadata := indexModelInfos([]protocol.ModelInfo{
		{
			ID: "qmodel_38max", DisplayName: "Qwen3.8-Max", ContextWindow: 1_000_000, RateMultiplier: &rate,
			IsNew: true, PromotionAvailable: true,
		},
		{ID: "dfmodel", DisplayName: "DeepSeek-V4-Flash", ContextWindow: 1_000_000, RateMultiplier: &flashRate, IsNew: true},
	})
	m := NewAdvancedConfigModelAtPage1WithMetadata(&p, []string{"qmodel_38max", "dfmodel"}, metadata)
	m.cursor = m.mainRowIndex(rowOpus)

	// Enter on a model row opens the slot picker overlay on the single page.
	next, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*AdvancedConfigModel)
	if !m.filterInput.Focused() {
		t.Fatalf("enter on the Opus row did not open the model picker")
	}
	view := m.View().Content
	for _, want := range []string{"Qwen3.8-Max", "qmodel_38max", "0.5x", "new", "off-peak discount"} {
		if !strings.Contains(view, want) {
			t.Fatalf("model picker missing %q: %q", want, view)
		}
	}

	// Filtering by the friendly name still keeps the internal ID as the list
	// value selected and persisted into the slot.
	m.filterInput.SetValue("Qwen3.8")
	m.updateFilteredPool()
	if len(m.filteredPool) != 1 || m.filteredPool[0] != "qmodel_38max" {
		t.Fatalf("filtered model IDs = %v", m.filteredPool)
	}
	m.slotListCursor = 0
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*AdvancedConfigModel)
	if p.OpusModel != "qmodel_38max" {
		t.Fatalf("saved Opus model = %q, want internal ID", p.OpusModel)
	}
	if strings.Contains(p.OpusModel, "Qwen3.8-Max") {
		t.Fatalf("display label leaked into slot mapping: %q", p.OpusModel)
	}

	// Back on the main page, the same display projection is used while the
	// persisted values remain internal IDs for requests.
	m.filterInput.Blur()
	view = m.View().Content
	for _, want := range []string{"Qwen3.8-Max", "DeepSeek-V4-Flash"} {
		if !strings.Contains(view, want) {
			t.Fatalf("main page missing friendly model %q: %q", want, view)
		}
	}
}

func TestQuitKeyStillWorksWhereNoTextInputHasFocus(t *testing.T) {
	// The shortcut must keep working on buttons and for OAuth providers, whose
	// credentials page has no editable field at all.
	for _, tc := range []struct {
		name   string
		p      provider.Provider
		cursor int
	}{
		{"api key provider, cursor on a button", provider.Provider{Type: "openai"}, 2},
		{"oauth provider", provider.Provider{Type: "openai", OAuthProvider: "codex"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewAdvancedConfigModel(&tc.p)
			m.page = 4
			m.cursor = tc.cursor
			if _, cmd := m.Update(typeKey('q')); !quits(cmd) {
				t.Fatal("q no longer quits")
			}
		})
	}
}

func TestViewDoesNotMutateTheModel(t *testing.T) {
	// View is a renderer: bubbletea may call it without a following Update, so
	// state changed here makes the frame and the model disagree. The single page
	// renders identically regardless of any stale page value; View must never
	// assign m.page or m.cursor.
	for _, page := range []int{0, 1, 2, 3, 4, 5, 99} {
		p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1", Model: "model-a,model-b"}
		m := NewAdvancedConfigModel(&p)
		m.page = page
		m.cursor = 1
		m.width = 100
		m.height = 30

		beforePage, beforeCursor := m.page, m.cursor
		view := m.View()

		if m.page != beforePage || m.cursor != beforeCursor {
			t.Fatalf("View() on page %d moved the model to page=%d cursor=%d", page, m.page, m.cursor)
		}
		if strings.TrimSpace(view.Content) == "" {
			t.Fatalf("View() on page %d rendered a blank frame", page)
		}
	}
}
