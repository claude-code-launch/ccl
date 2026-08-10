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

func TestPageUpDownStaysWithinVisibleRows(t *testing.T) {
	// Moving up/down must stay inside the single page's visible row set, wrapping
	// at the ends instead of drifting into a disabled row.
	cp := providerFrom("path-gateway", "https://example.com/codex", "openai_responses")
	m := NewAdvancedConfigModel(&cp)
	enterDetectedReview(m, "path-max")
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

// enterDetectedReview puts a model into the post-detection state: a populated
// pool, discovered flag set, and the cursor on a model row.
func enterDetectedReview(m *AdvancedConfigModel, models ...string) *AdvancedConfigModel {
	m.modelPool = append([]string(nil), models...)
	m.modelPoolFromDiscovery = true
	m.p.Model = strings.Join(models, ",")
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

func TestPage2MapsOldContextPresetToDefault(t *testing.T) {
	p := provider.Provider{
		Type:     "openai_responses",
		Endpoint: "https://example.test/v1",
		Env: map[string]string{
			maxContextTokensEnv:  "300000",
			autoCompactWindowEnv: "200000",
		},
	}
	m := NewAdvancedConfigModel(&p)
	if m.compactPreset != compactPresetDefault {
		t.Fatalf("compact preset = %v, want Default", m.compactPreset)
	}
	// The legacy preset is dropped, not surfaced, so the saved provider clears the
	// stale context env on apply.
	if !hasUnsupportedContextConfig(*m.p) {
		t.Fatal("old compact env should be retired on save")
	}
}

func TestCredentialsPageResolvesClickToField(t *testing.T) {
	p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1", APIKey: "sk-test"}
	m := NewAdvancedConfigModel(&p)
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

	// Endpoint and API Key are reachable by clicking their label rows.
	for _, field := range []struct {
		row   configRowKind
		label string
	}{
		{rowEndpoint, "Endpoint URL"},
		{rowAPIKey, "API Key"},
	} {
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
		got, ok := rowAtLine(lines, labelRow)
		if !ok || got != field.row {
			t.Fatalf("click on row %d resolved to (%v, %t), want %v", labelRow, got, ok, field.row)
		}
	}

	// Prose that merely mentions a label must not steal focus.
	if _, ok := rowAtLine([]string{"  detection uses the API Key you entered"}, 0); ok {
		t.Fatal("a hint mentioning \"API Key\" resolved to the field")
	}

	// A click far away from any field must be ignored rather than stealing focus.
	if _, ok := rowAtLine(lines, 0); ok {
		t.Fatal("click on the top padding row resolved to a field")
	}

	// The resolved click focuses the API key input.
	next, _ := m.Update(focusRowMsg{row: rowAPIKey})
	m = next.(*AdvancedConfigModel)
	if m.cursor != m.mainRowIndex(rowAPIKey) || !m.keyInput.Focused() || m.urlInput.Focused() {
		t.Fatalf("cursor=%d url_focused=%t key_focused=%t, want the key input focused",
			m.cursor, m.urlInput.Focused(), m.keyInput.Focused())
	}
	next, _ = m.Update(focusRowMsg{row: rowEndpoint})
	m = next.(*AdvancedConfigModel)
	if m.cursor != m.mainRowIndex(rowEndpoint) || !m.urlInput.Focused() || m.keyInput.Focused() {
		t.Fatalf("cursor=%d url_focused=%t key_focused=%t, want the endpoint input focused",
			m.cursor, m.urlInput.Focused(), m.keyInput.Focused())
	}
}

func TestOAuthPageSupportsMouseClick(t *testing.T) {
	// The single page captures mouse clicks for every row, OAuth included, so
	// model rows / Save / Cancel are reachable without the keyboard.
	p := provider.Provider{Type: "openai_responses", OAuthProvider: "gpt", Endpoint: "oauth://codex"}
	m := NewAdvancedConfigModel(&p)
	enterDetectedReview(m, "gpt-5.6-sol", "gpt-5.6-terra")
	m.width = 100
	m.height = 30
	if view := m.View(); view.MouseMode == tea.MouseModeNone || view.OnMouse == nil {
		t.Fatal("single page must capture mouse clicks for clickable rows")
	}
	// Clicking the Opus row focuses it.
	lines := strings.Split(m.View().Content, "\n")
	opusLine := -1
	for i, line := range lines {
		if strings.Contains(line, "Opus") {
			opusLine = i
			break
		}
	}
	if opusLine < 0 {
		t.Fatal("Opus row not rendered")
	}
	row, ok := rowAtLine(lines, opusLine)
	if !ok || row != rowOpus {
		t.Fatalf("click on Opus resolved to (%v, %t), want rowOpus", row, ok)
	}
}

func TestSinglePageBlocksOneMForMixedCaseModelIDs(t *testing.T) {
	// The catalog is keyed by lowercased model id; a gateway serving mixed-case ids
	// must not slip past the check. On the single page the 1M marker is toggled by
	// Space on a model row toggles the per-slot marker.
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
		// rowAPIKey is at visible-row index 1 (the Show/Hide button row was
		// removed when the key became a plaintext textarea).
		{"api key", 1, func(m *AdvancedConfigModel) string { return m.keyInput.Value() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, r := range []rune{'q', 'k', 'j', 'h', 'l'} {
				p := provider.Provider{Type: "openai"}
				m := NewAdvancedConfigModel(&p)
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
	m := NewAdvancedMappingModel(&p, []string{"kimi-k2", "qwen3-coder", "glm-4.6"}, nil)
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
	m := NewAdvancedMappingModel(&p, []string{"qmodel_38max", "dfmodel"}, metadata)
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

// TestChangingModelClearsBlockedOneMMarker verifies that picking a new model for
// a slot drops a [1m] marker when the backend window rules 1M out for the new
// model. toggleOneMAtRow refuses to enable such a marker, so leaving it enabled
// would send a non-1M model with the [1m] suffix at save time.
func TestChangingModelClearsBlockedOneMMarker(t *testing.T) {
	p := provider.Provider{Type: "openai", OpusModel: "gpt-5.6-sol"}
	m := NewAdvancedMappingModel(&p, []string{"gpt-5.6-sol", "small-window-model"}, indexModelInfos([]protocol.ModelInfo{
		{ID: "gpt-5.6-sol", ContextWindow: 1_000_000},
		{ID: "small-window-model", ContextWindow: 128_000},
	}))
	m.cursor = m.mainRowIndex(rowOpus)
	m.oneMSlots["opus"] = true // user enabled [1m] on Opus (allowlist-confirmed)
	if m.oneMSlotBlocked("gpt-5.6-sol") {
		t.Fatalf("gpt-5.6-sol should not be blocked; its 1M window allows the marker")
	}
	if !m.oneMSlotBlocked("small-window-model") {
		t.Fatalf("small-window-model should be blocked: 128K window rules 1M out")
	}

	// Open the picker and select the small-window model on the Opus row.
	next, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*AdvancedConfigModel)
	if !m.filterInput.Focused() {
		t.Fatalf("enter on the Opus row did not open the model picker")
	}
	m.filterInput.SetValue("small-window")
	m.updateFilteredPool()
	m.slotListCursor = 0
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*AdvancedConfigModel)
	if p.OpusModel != "small-window-model" {
		t.Fatalf("Opus model = %q, want small-window-model", p.OpusModel)
	}
	if m.oneMSlots["opus"] {
		t.Fatalf("1M marker should be cleared after picking a blocked model, but it is still enabled")
	}
}

// TestChangingModelKeepsOneMMarkerOnNonBlockedModel verifies the marker survives
// when the new model is not backend-blocked (the advisory path).
func TestChangingModelKeepsOneMMarkerOnNonBlockedModel(t *testing.T) {
	p := provider.Provider{Type: "openai", OpusModel: "gpt-5.6-sol"}
	m := NewAdvancedMappingModel(&p, []string{"gpt-5.6-sol", "other-model"}, indexModelInfos([]protocol.ModelInfo{
		{ID: "gpt-5.6-sol", ContextWindow: 1_000_000},
		{ID: "other-model"}, // no advertised window → not blocked
	}))
	m.cursor = m.mainRowIndex(rowOpus)
	m.oneMSlots["opus"] = true

	next, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*AdvancedConfigModel)
	m.filterInput.SetValue("other-model")
	m.updateFilteredPool()
	m.slotListCursor = 0
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*AdvancedConfigModel)
	if p.OpusModel != "other-model" {
		t.Fatalf("Opus model = %q, want other-model", p.OpusModel)
	}
	if !m.oneMSlots["opus"] {
		t.Fatalf("1M marker should stay for a non-blocked model, but it was cleared")
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
			m.cursor = tc.cursor
			if _, cmd := m.Update(typeKey('q')); !quits(cmd) {
				t.Fatal("q no longer quits")
			}
		})
	}
}

func TestViewDoesNotMutateTheModel(t *testing.T) {
	// View is a renderer: bubbletea may call it without a following Update, so
	// state changed here makes the frame and the model disagree.
	p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1", Model: "model-a,model-b"}
	m := NewAdvancedConfigModel(&p)
	m.cursor = 1
	m.width = 100
	m.height = 30

	view := m.View()
	if m.cursor != 1 {
		t.Fatalf("View() moved the cursor to %d", m.cursor)
	}
	if strings.TrimSpace(view.Content) == "" {
		t.Fatal("View() rendered a blank frame")
	}
}
