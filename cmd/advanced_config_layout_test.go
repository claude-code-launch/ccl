package cmd

import (
	"strings"
	"testing"

	tui "github.com/grindlemire/go-tui"

	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestTruncateMiddleASCII(t *testing.T) {
	got := truncateMiddle("https://example.com/very/long/path/to/resource", 24)
	if !strings.Contains(got, "…") {
		t.Fatalf("expected ellipsis, got %q", got)
	}
	if tui.StringWidth(got) > 24 {
		t.Fatalf("width %d > 24 for %q", tui.StringWidth(got), got)
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
	if tui.StringWidth(got) > 18 {
		t.Fatalf("width %d > 18 for %q", tui.StringWidth(got), got)
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
	if tui.StringWidth(got) > 10 {
		t.Fatalf("width %d > 10 for %q", tui.StringWidth(got), got)
	}
}

func TestReviewShowsFastStatus(t *testing.T) {
	chatgpt := providerFrom("gpt", "https://api.openai.com/v1", "openai_responses")
	chatgpt.OAuthProvider = "gpt"
	chatgpt.FastMode = true
	m := NewAdvancedConfigModel(&chatgpt)
	enterDetectedReview(m, "m")
	view := renderView(t, m)
	if !strings.Contains(view, "Fast") || !strings.Contains(view, "‹ On ›") {
		t.Fatalf("review page missing Fast=on: %q", view)
	}

	off := providerFrom("plain", "https://example.com/v1", "openai")
	m = NewAdvancedConfigModel(&off)
	enterDetectedReview(m, "m")
	view = renderView(t, m)
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
	m.handleKey(tui.KeyEvent{Key: tui.KeyUp})
	if m.cursor != rows-1 {
		t.Fatalf("up from the first row landed on %d, want the last row %d", m.cursor, rows-1)
	}
	m.cursor = rows - 1
	m.handleKey(tui.KeyEvent{Key: tui.KeyDown})
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
	m.live().modelPool = append([]string(nil), models...)
	m.live().modelPoolFromDiscovery = true
	m.p.Model = strings.Join(models, ",")
	if len(models) > 0 {
		m.cursor = m.mainRowIndex(rowOpus)
	}
	return m
}

func TestReviewFitsCommonTerminalHeights(t *testing.T) {
	p := providerFrom("p", "https://example.com/v1", "openai")

	// The scroll anchoring matches the label the page actually renders, and that
	// label is localized: a Chinese session shows “保存并激活” where an English
	// one shows “Save & Activate”. The layout guarantee must hold for both, so
	// exercise each language explicitly instead of inheriting the machine's
	// config.yaml language.
	savedLang := locale.Current()
	t.Cleanup(func() { locale.SetLanguage(savedLang) })

	for _, tc := range []struct {
		lang     string
		saveText string
	}{
		{lang: "en", saveText: "Save & Activate"},
		{lang: "zh-CN", saveText: "保存并激活"},
	} {
		locale.SetLanguage(tc.lang)
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
			view := renderView(t, m)
			got := strings.Count(view, "\n") + 1
			if got > h {
				t.Fatalf("[%s] terminal height %d rendered %d lines (overflow)\n%s", tc.lang, h, got, view)
			}
			if !strings.Contains(view, tc.saveText) {
				t.Fatalf("[%s] Save (%s) not visible at height %d", tc.lang, tc.saveText, h)
			}
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
	m.live().modelContextWindows = map[string]int{
		"small-window": 272_000,
		"big-window":   1_050_000,
	}

	// Opus: 1M must not be selectable via Space.
	m.cursor = m.mainRowIndex(rowOpus)
	m.handleKey(tui.KeyEvent{Key: tui.KeyRune, Rune: ' '})
	if m.live().oneMSlots["opus"] {
		t.Fatal("1M was enabled for a model whose backend window is 272K")
	}

	// Sonnet: a 1M-class window stays selectable.
	m.cursor = m.mainRowIndex(rowSonnet)
	m.handleKey(tui.KeyEvent{Key: tui.KeyRune, Rune: ' '})
	if !m.live().oneMSlots["sonnet"] {
		t.Fatal("1M must remain selectable for a 1M-class model")
	}

	// Unknown window: the catalog is advisory, so keep it editable.
	m.cursor = m.mainRowIndex(rowHaiku)
	m.handleKey(tui.KeyEvent{Key: tui.KeyRune, Rune: ' '})
	if !m.live().oneMSlots["haiku"] {
		t.Fatal("1M must stay editable when the window is unknown")
	}

	// An existing marker on a blocked slot can still be cleared.
	m.live().oneMSlots["opus"] = true
	m.cursor = m.mainRowIndex(rowOpus)
	m.handleKey(tui.KeyEvent{Key: tui.KeyRune, Rune: ' '})
	if m.live().oneMSlots["opus"] {
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
	if m.live().compactPreset != compactPresetDefault {
		t.Fatalf("compact preset = %v, want Default", m.live().compactPreset)
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

	var clicker tui.MouseListener = m
	_ = clicker // the single page is a MouseListener; clicks are routed by the app
	lines := strings.Split(renderView(t, m), "\n")

	// Endpoint and API Key are reachable by clicking their label rows.
	for _, field := range []struct {
		row   configRowKind
		label string
	}{
		{rowEndpoint, locale.T("端点 URL", "Endpoint URL")},
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
	m.handleFocusRow(rowAPIKey)
	if m.cursor != m.mainRowIndex(rowAPIKey) || !m.keyFocused || m.urlFocused {
		t.Fatalf("cursor=%d url_focused=%t key_focused=%t, want the key input focused",
			m.cursor, m.urlFocused, m.keyFocused)
	}
	m.handleFocusRow(rowEndpoint)
	if m.cursor != m.mainRowIndex(rowEndpoint) || !m.urlFocused || m.keyFocused {
		t.Fatalf("cursor=%d url_focused=%t key_focused=%t, want the endpoint input focused",
			m.cursor, m.urlFocused, m.keyFocused)
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
	var clicker tui.MouseListener = m
	_ = clicker // the single page is a MouseListener; clicks are routed by the app
	// Clicking the Opus row focuses it.
	lines := strings.Split(renderView(t, m), "\n")
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
	m.live().modelContextWindows = map[string]int{"glm-4.6": 200_000}

	if !m.oneMSlotBlocked("GLM-4.6") {
		t.Fatal("a 200K model must block 1M regardless of id casing")
	}
	enterDetectedReview(m, "GLM-4.6")
	m.cursor = m.mainRowIndex(rowOpus)
	m.handleKey(tui.KeyEvent{Key: tui.KeyRune, Rune: ' '})
	if m.live().oneMSlots["opus"] {
		t.Fatal("1M was enabled for a 200K model with a mixed-case id")
	}
}

func TestTextInputsKeepSingleLetterKeysInsteadOfActingOnThem(t *testing.T) {
	// "q" quit and the vim aliases are also ordinary characters. While a text
	// input owns the keyboard they must be typed, not obeyed: an API key
	// containing "q", or filtering for "qwen"/"kimi", used to quit the TUI or
	// silently move the cursor and insert the letter at the same time.
	for _, tc := range []struct {
		name  string
		row   configRowKind
		field func(*AdvancedConfigModel) string
	}{
		{"endpoint", rowEndpoint, func(m *AdvancedConfigModel) string { return m.urlText.Get() }},
		{"api key", rowAPIKey, func(m *AdvancedConfigModel) string { return m.keyText.Get() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, r := range []rune{'q', 'k', 'j', 'h', 'l'} {
				p := provider.Provider{Type: "openai"}
				m := NewAdvancedConfigModel(&p)
				m.cursor = m.mainRowIndex(tc.row)

				m.handleKey(keyPress(r))
				if m.cursor != m.mainRowIndex(tc.row) {
					t.Fatalf("typing %q moved the cursor %d -> %d", r, tc.row, m.cursor)
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
	m.filterFocused = true

	for _, r := range "kimi" {
		m.handleKey(keyPress(r))
	}
	if got := m.filterText.Get(); got != "kimi" {
		t.Fatalf("filter = %q, want %q", got, "kimi")
	}
	if len(m.filteredPool) != 1 || m.filteredPool[0] != "kimi-k2" {
		t.Fatalf("filtered pool = %v, want only kimi-k2", m.filteredPool)
	}

	// The list still moves with the arrow keys while the filter has focus.
	m.filterText.Set("")
	m.updateFilteredPool()
	m.slotListCursor = 0
	m.handleKey(tui.KeyEvent{Key: tui.KeyDown})
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
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if !m.filterFocused {
		t.Fatalf("enter on the Opus row did not open the model picker")
	}
	view := renderView(t, m)
	for _, want := range []string{"Qwen3.8-Max", "qmodel_38max", "0.5x", "new", "off-peak discount"} {
		if !strings.Contains(view, want) {
			t.Fatalf("model picker missing %q: %q", want, view)
		}
	}

	// Filtering by the friendly name still keeps the internal ID as the list
	// value selected and persisted into the slot.
	m.filterText.Set("Qwen3.8")
	m.updateFilteredPool()
	if len(m.filteredPool) != 1 || m.filteredPool[0] != "qmodel_38max" {
		t.Fatalf("filtered model IDs = %v", m.filteredPool)
	}
	m.slotListCursor = 0
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if p.OpusModel != "qmodel_38max" {
		t.Fatalf("saved Opus model = %q, want internal ID", p.OpusModel)
	}
	if strings.Contains(p.OpusModel, "Qwen3.8-Max") {
		t.Fatalf("display label leaked into slot mapping: %q", p.OpusModel)
	}

	// Back on the main page, the same display projection is used while the
	// persisted values remain internal IDs for requests.
	m.filterFocused = false
	view = renderView(t, m)
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
	m.live().oneMSlots["opus"] = true // user enabled [1m] on Opus (allowlist-confirmed)
	if m.oneMSlotBlocked("gpt-5.6-sol") {
		t.Fatalf("gpt-5.6-sol should not be blocked; its 1M window allows the marker")
	}
	if !m.oneMSlotBlocked("small-window-model") {
		t.Fatalf("small-window-model should be blocked: 128K window rules 1M out")
	}

	// Open the picker and select the small-window model on the Opus row.
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if !m.filterFocused {
		t.Fatalf("enter on the Opus row did not open the model picker")
	}
	m.filterText.Set("small-window")
	m.updateFilteredPool()
	m.slotListCursor = 0
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if p.OpusModel != "small-window-model" {
		t.Fatalf("Opus model = %q, want small-window-model", p.OpusModel)
	}
	if m.live().oneMSlots["opus"] {
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
	m.live().oneMSlots["opus"] = true

	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	m.filterText.Set("other-model")
	m.updateFilteredPool()
	m.slotListCursor = 0
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if p.OpusModel != "other-model" {
		t.Fatalf("Opus model = %q, want other-model", p.OpusModel)
	}
	if !m.live().oneMSlots["opus"] {
		t.Fatalf("1M marker should stay for a non-blocked model, but it was cleared")
	}
}

func TestQuitKeyStillWorksWhereNoTextInputHasFocus(t *testing.T) {
	// The shortcut must keep working on buttons and for OAuth providers, whose
	// credentials page has no editable field at all: when no input owns the
	// keyboard, "q" asks the app to stop rather than typing anywhere.
	for _, tc := range []struct {
		name string
		p    provider.Provider
		row  configRowKind
	}{
		{"api key provider, cursor on a button", provider.Provider{Type: "openai"}, rowTest},
		{"oauth provider", provider.Provider{Type: "openai", OAuthProvider: "codex"}, rowTest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewAdvancedConfigModel(&tc.p)
			m.cursor = m.mainRowIndex(tc.row)
			m.handleKey(keyPress('q'))
			if !m.quitRequested {
				t.Fatalf("q did not quit the %s", tc.name)
			}
			if m.urlText.Get() != "" || m.keyText.Get() != "" {
				t.Fatalf("q typed into an input: url=%q key=%q", m.urlText.Get(), m.keyText.Get())
			}
		})
	}
}

func TestViewDoesNotMutateTheModel(t *testing.T) {
	// Render is a pure renderer: the framework may call it without a preceding
	// key handler, so state changed here must not be moved by the render itself.
	p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1", Model: "model-a,model-b"}
	m := NewAdvancedConfigModel(&p)
	m.cursor = 1
	m.width = 100
	m.height = 30

	view := renderView(t, m)
	if m.cursor != 1 {
		t.Fatalf("Render moved the cursor to %d", m.cursor)
	}
	if strings.TrimSpace(view) == "" {
		t.Fatal("Render rendered a blank frame")
	}
}

// testModelsDevProvider returns a minimal models.dev catalog entry with one
// model, enough to exercise applyModelsDevProvider and the picker flow.
func testModelsDevProvider() modelsdev.Provider {
	return modelsdev.Provider{
		ID:   "test-gw",
		Name: "Test Gateway",
		NPM:  "@ai-sdk/openai-compatible",
		API:  "https://gw.example/v1",
		Models: map[string]modelsdev.Model{
			"test-model": {ID: "test-model", Name: "Test Model"},
		},
	}
}

// TestSourceStepperPresentForManualAndAbsentForOAuth verifies the Source row
// leads the Connection section for a manual provider and is absent for OAuth
// (whose Connection block is subscription metadata with no source choice).
func TestSourceStepperPresentForManualAndAbsentForOAuth(t *testing.T) {
	manual := NewAdvancedConfigModel(&provider.Provider{Type: "openai"})
	if manual.mainRowIndex(rowSource) != 0 {
		t.Fatalf("manual provider: rowSource index = %d, want 0", manual.mainRowIndex(rowSource))
	}
	oauth := NewAdvancedConfigModel(&provider.Provider{Type: "openai", OAuthProvider: "gpt"})
	if oauth.mainRowIndex(rowSource) != -1 {
		t.Fatalf("OAuth provider: rowSource index = %d, want -1 (absent)", oauth.mainRowIndex(rowSource))
	}
}

// TestSwitchSourcePreservesBothDrafts verifies the Custom ↔ models.dev toggle
// keeps each side's endpoint/key independently across a full round trip.
func TestSwitchSourcePreservesBothDrafts(t *testing.T) {
	p := provider.Provider{Type: "openai", Endpoint: "https://custom.example/v1", APIKey: "custom-key"}
	m := NewAdvancedConfigModel(&p)

	// Custom → models.dev (empty side).
	m.switchSource(sourceModelsDev)
	if m.source != sourceModelsDev || m.p != m.modelsDevDraft.p {
		t.Fatalf("did not switch to models.dev: source=%v p=%p modelsDev.p=%p", m.source, m.p, m.modelsDevDraft.p)
	}
	m.keyText.Set("md-key")

	// models.dev → Custom: the manual endpoint/key must survive.
	m.switchSource(sourceCustom)
	if m.urlText.Get() != "https://custom.example/v1" {
		t.Fatalf("custom endpoint lost: %q", m.urlText.Get())
	}
	if m.keyText.Get() != "custom-key" {
		t.Fatalf("custom key lost: %q", m.keyText.Get())
	}

	// Custom → models.dev again: the models.dev key must survive too.
	m.switchSource(sourceModelsDev)
	if m.keyText.Get() != "md-key" {
		t.Fatalf("models.dev key lost: %q", m.keyText.Get())
	}
}

// TestModelsDevSourceHasNoEditableEndpoint verifies the models.dev source drops
// the editable Endpoint row (it is read-only metadata) and is ready without a
// probe.
func TestModelsDevSourceHasNoEditableEndpoint(t *testing.T) {
	m := NewAdvancedConfigModel(&provider.Provider{Type: "openai"})
	m.applyModelsDevProvider(testModelsDevProvider())

	if m.source != sourceModelsDev {
		t.Fatalf("applyModelsDevProvider did not switch source: %v", m.source)
	}
	if m.mainRowIndex(rowEndpoint) != -1 {
		t.Fatalf("models.dev source: rowEndpoint index = %d, want -1 (read-only)", m.mainRowIndex(rowEndpoint))
	}
	if m.mainRowIndex(rowProvider) < 0 {
		t.Fatalf("models.dev source: rowProvider absent")
	}
	if !m.connectionReady() {
		t.Fatalf("models.dev source should be connection-ready without a probe")
	}
}

// TestModelsDevSourceNotReadyUntilProviderSelected verifies a freshly switched
// models.dev source (no provider picked, no key) is NOT connection-ready and
// cannot be saved — the "✓ Connected" status and the Model Mapping/Runtime
// sections must wait until a provider is picked (which loads the pool).
func TestModelsDevSourceNotReadyUntilProviderSelected(t *testing.T) {
	m := NewAdvancedConfigModel(&provider.Provider{Type: "openai"})
	m.switchSource(sourceModelsDev)

	if m.connectionReady() {
		t.Fatalf("models.dev source with no provider picked should not be connection-ready")
	}
	if m.canSave() {
		t.Fatalf("models.dev source with no provider picked should not be savable")
	}

	m.applyModelsDevProvider(testModelsDevProvider())
	if !m.connectionReady() {
		t.Fatalf("models.dev source should be ready after picking a provider")
	}
}

// TestProtocolMovedOutOfRuntime verifies the fine-grained Protocol row now lives
// in the Connection section (before Auto Configure) and no longer renders in the
// Runtime section.
func TestProtocolMovedOutOfRuntime(t *testing.T) {
	m := NewAdvancedConfigModel(&provider.Provider{Type: "openai", Endpoint: "https://example.test/v1"})
	if m.mainRowIndex(rowProtocol) >= m.mainRowIndex(rowTest) {
		t.Fatalf("Protocol (%d) should precede Auto Configure (%d)", m.mainRowIndex(rowProtocol), m.mainRowIndex(rowTest))
	}
	view := renderView(t, m)
	idx := strings.Index(view, locale.T("运行时", "Runtime"))
	if idx < 0 {
		t.Fatalf("Runtime heading missing from view")
	}
	if strings.Contains(view[idx:], "Protocol") {
		t.Fatalf("Runtime section still renders a Protocol row:\n%s", view[idx:])
	}
}

// TestProviderRowOpensPicker verifies activating the Provider row (models.dev
// source) opens the full-screen catalog picker and starts the fetch.
func TestProviderRowOpensPicker(t *testing.T) {
	m := NewAdvancedConfigModel(&provider.Provider{Type: "openai"})
	m.applyModelsDevProvider(testModelsDevProvider())
	m.cursor = m.mainRowIndex(rowProvider)

	m.activateRow(rowProvider)
	if !m.modelsDevPicker {
		t.Fatalf("Provider row did not open the models.dev picker")
	}
	if !m.modelsDevLoading {
		t.Fatalf("picker did not start the catalog fetch")
	}
}

// TestSwitchSourceNoopWhileDetecting verifies switching source is refused while
// a connection probe is in flight, so a stale detection result cannot land on
// the wrong side.
func TestSwitchSourceNoopWhileDetecting(t *testing.T) {
	m := NewAdvancedConfigModel(&provider.Provider{Type: "openai", Endpoint: "https://example.test/v1"})
	m.live().detecting = true
	m.switchSource(sourceModelsDev)
	if m.source != sourceCustom {
		t.Fatalf("switchSource changed source during detection: %v", m.source)
	}
}

// TestEditExistingModelsDevProviderOpensModelsDevSource verifies that editing a
// provider already persisted as models.dev opens on the models.dev source rather
// than Custom. Opening as Custom used to let Init's auto-probe overwrite Type
// ("modelsdev") with a single detected protocol and drop the per-model routing
// table.
func TestEditExistingModelsDevProviderOpensModelsDevSource(t *testing.T) {
	p := provider.Provider{
		Name:     "test-gw",
		Type:     "modelsdev",
		Endpoint: "https://gw.example/v1",
		APIKey:   "sk-existing",
		Model:    "model-a,model-b",
		ModelProtocols: map[string]string{
			"model-a": "anthropic",
			"model-b": "openai_responses",
		},
	}
	m := NewAdvancedConfigModel(&p)

	if m.source != sourceModelsDev {
		t.Fatalf("editing a modelsdev provider opened source %v, want models.dev", m.source)
	}
	if m.p == nil || m.p.Type != "modelsdev" {
		t.Fatalf("Type = %q, want preserved as modelsdev", m.p.Type)
	}
	if len(m.p.ModelProtocols) != 2 {
		t.Fatalf("ModelProtocols lost: got %v, want 2 entries", m.p.ModelProtocols)
	}
	if m.live().autoDetectOnOpen {
		t.Fatalf("models.dev provider must not auto-probe on open (would overwrite Type)")
	}
	if !m.live().modelPoolFromDiscovery {
		t.Fatalf("models.dev provider pool should count as discovered")
	}
	if len(m.live().modelPool) != 2 {
		t.Fatalf("modelPool = %v, want 2 entries", m.live().modelPool)
	}

	// The Custom side must be a clean empty draft (same name only), so switching
	// source still offers a blank form.
	if m.customDraft.p == m.p {
		t.Fatalf("custom draft aliases the models.dev provider")
	}
	if m.customDraft.p.Type != "" {
		t.Fatalf("custom draft Type = %q, want empty", m.customDraft.p.Type)
	}
	if m.customDraft.p.Name != "test-gw" {
		t.Fatalf("custom draft Name = %q, want test-gw", m.customDraft.p.Name)
	}
}

// TestEditKeyClearsVerificationAndSyncsProbeKey verifies that editing the API
// key after a successful verification invalidates the cached "connected" status
// and re-syncs the probe key to the newly typed value, so any later verification
// request is keyed off the current input rather than a stale one.
func TestEditKeyClearsVerificationAndSyncsProbeKey(t *testing.T) {
	p := provider.Provider{
		Name:     "test-gw",
		Type:     "modelsdev",
		Endpoint: "https://gw.example/v1",
		APIKey:   "old-key",
		Model:    "model-a",
		ModelProtocols: map[string]string{
			"model-a": "anthropic",
		},
	}
	m := NewAdvancedConfigModel(&p)

	// A prior verification succeeded.
	m.live().keyVerified = true
	m.live().probeAPIKey = "old-key"
	m.live().detectionError = nil

	// Edit the key.
	m.cursor = m.mainRowIndex(rowAPIKey)
	m.handleKey(keyPress('X'))

	if m.keyText.Get() == "old-key" {
		t.Fatalf("key edit did not change the key value")
	}
	if m.live().keyVerified {
		t.Fatalf("editing the key must clear keyVerified")
	}
	if m.live().probeAPIKey != m.keyText.Get() {
		t.Fatalf("probeAPIKey = %q, want %q (the newly typed key)", m.live().probeAPIKey, m.keyText.Get())
	}

	// A late result for the old key must be ignored: the key it verified no
	// longer matches the current input.
	m.handleVerifyDone(keyVerifyDoneMsg{endpoint: "https://gw.example/v1", apiKey: "old-key", err: nil})
	if m.live().keyVerified {
		t.Fatalf("stale verification result resurrected keyVerified=true for an unverified key")
	}
}
