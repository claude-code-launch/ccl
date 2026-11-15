package cmd

import (
	"fmt"
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
// pool, discovered flag set, and the cursor on Context & Compact.
func enterDetectedReview(m *AdvancedConfigModel, models ...string) *AdvancedConfigModel {
	m.live().modelPool = append([]string(nil), models...)
	m.live().modelPoolFromDiscovery = true
	m.p.Model = strings.Join(models, ",")
	if len(models) > 0 {
		if idx := m.mainRowIndex(rowContext); idx >= 0 {
			m.cursor = idx
		}
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
	// Check / Context & Compact / Save / Cancel are reachable without the keyboard.
	p := provider.Provider{Type: "openai_responses", OAuthProvider: "gpt", Endpoint: "oauth://codex"}
	m := readyOAuthPage(NewAdvancedConfigModel(&p))
	enterDetectedReview(m, "gpt-5.6-sol", "gpt-5.6-terra")
	m.width = 100
	m.height = 30
	var clicker tui.MouseListener = m
	_ = clicker // the single page is a MouseListener; clicks are routed by the app
	lines := strings.Split(renderView(t, m), "\n")
	checkLabel := locale.T("检查", "Check")
	checkLine := -1
	for i, line := range lines {
		if strings.Contains(line, checkLabel) {
			checkLine = i
			break
		}
	}
	if checkLine < 0 {
		t.Fatal("Check row not rendered")
	}
	row, ok := rowAtLine(lines, checkLine)
	if !ok || row != rowTestModels {
		t.Fatalf("click on Check resolved to (%v, %t), want rowTestModels", row, ok)
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

func TestModelsSectionShowsModelIDWithCatalogBadgesWithoutDisplayAlias(t *testing.T) {
	p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1", APIKey: "k"}
	rate := 0.5
	flashRate := 0.1
	metadata := indexModelInfos([]protocol.ModelInfo{
		{
			ID: "qmodel_38max", DisplayName: "Qwen3.8-Max", ContextWindow: 1_000_000, RateMultiplier: &rate,
			IsNew: true, PromotionAvailable: true,
		},
		{ID: "dfmodel", DisplayName: "DeepSeek-V4-Flash", ContextWindow: 1_000_000, RateMultiplier: &flashRate, IsNew: true},
	})
	m := NewAdvancedConfigModel(&p)
	enterDetectedReview(m, "qmodel_38max")
	m.live().modelDisplayMetadata = metadata
	m.width = 160

	view := renderView(t, m)
	for _, want := range []string{"qmodel_38max", "0.5x", "new", "off-peak discount"} {
		if !strings.Contains(view, want) {
			t.Fatalf("models section missing %q: %q", want, view)
		}
	}
	if strings.Contains(view, "Qwen3.8-Max") {
		t.Fatalf("main page still shows display aliases: %q", view)
	}
	if got := m.modelDisplayLabel("dfmodel"); !strings.Contains(got, "dfmodel") || !strings.Contains(got, "0.1x") || strings.Contains(got, "DeepSeek-V4-Flash") {
		t.Fatalf("dfmodel label = %q", got)
	}
	if p.Model != "qmodel_38max" {
		t.Fatalf("persisted Model = %q, want internal ID", p.Model)
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
		{"oauth provider", provider.Provider{Type: "openai", OAuthProvider: "codex"}, rowTestModels},
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

func TestModelsDevConnectionDoesNotShowManualVerification(t *testing.T) {
	p := provider.Provider{
		Type: "modelsdev", Endpoint: "https://example.test/v1", APIKey: "test-key",
		Model: "provider/model", ModelProtocols: map[string]string{"provider/model": "openai"},
	}
	m := NewAdvancedConfigModel(&p)
	if m.mainRowIndex(rowTest) != -1 {
		t.Fatal("models.dev connection should not offer a manual Test Connection action")
	}
	m.activateRow(rowTestModels)
	if m.modelTesting {
		t.Fatal("model availability must wait for automatic key verification")
	}
	m.cursor = m.mainRowIndex(rowAPIKey)
	m.live().detecting = true
	m.live().probeAPIKey = p.APIKey
	m.handleVerifyDone(keyVerifyDoneMsg{endpoint: p.Endpoint, apiKey: p.APIKey})
	if m.mainRowIndex(rowTest) != -1 {
		t.Fatal("verified connection should not offer Test Connection")
	}
	if m.currentRow() != rowAPIKey {
		t.Fatalf("focus after verification = %v, want API Key", m.currentRow())
	}
	m.keyText.Set("changed-key")
	m.invalidateModelsDevKeyIfChanged()
	if m.mainRowIndex(rowTest) != -1 || !m.live().autoVerifyPending {
		t.Fatal("changed key should schedule automatic verification without a button")
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

// TestModelsDevRefreshMergesCatalog verifies the one-shot refresh that runs
// after key verification: models added to the catalog appear, models the
// catalog dropped leave the pool, and surviving models keep their [1m] markers.
func TestModelsDevRefreshMergesCatalog(t *testing.T) {
	p := provider.Provider{
		Type: "modelsdev", Name: "test-gw", Endpoint: "https://example.test/v1", APIKey: "key",
		Model: "kept[1m],dropped",
		ModelProtocols: map[string]string{
			"kept": "openai", "dropped": "openai", "fresh": "openai",
		},
	}
	m := NewAdvancedConfigModel(&p)
	if !m.live().modelsDevRefreshPending {
		t.Fatal("opening a persisted models.dev provider should arm the catalog refresh")
	}
	// Simulate a verified key, which is what fires the refresh.
	m.live().keyVerified = true
	m.live().modelsDevRefreshPending = false

	catalog := testModelsDevProvider()
	catalog.Models["kept"] = modelsdev.Model{ID: "kept", Name: "Kept"}
	catalog.Models["fresh"] = modelsdev.Model{ID: "fresh", Name: "Fresh"}
	delete(catalog.Models, "test-model")
	m.handleModelsDevRefreshDone(modelsDevFetchDoneMsg{
		providers:  []modelsdev.Provider{catalog},
		refreshFor: "test-gw",
	})

	got := strings.Join(m.live().modelPool, ",")
	want := "kept,fresh"
	if got != want {
		t.Fatalf("merged pool = %q, want %q (saved models kept, dropped cleared, fresh appended)", got, want)
	}
	if p.Model != "kept[1m],fresh" {
		t.Fatalf("persisted Model = %q, want %q", p.Model, "kept[1m],fresh")
	}
}

// TestModelsDevRefreshAppendsCatalogFromAnotherNamespace covers a gateway whose
// live /models catalog (and therefore the saved pool) uses different IDs than
// its curated models.dev entry: the refresh keeps the working pool verbatim and
// appends the catalog's IDs — with their wire protocols and display metadata.
func TestModelsDevRefreshAppendsCatalogFromAnotherNamespace(t *testing.T) {
	p := provider.Provider{
		Type: "modelsdev", Name: "test-gw", Endpoint: "https://example.test/v1", APIKey: "key",
		Model: "anthropic/claude-sonnet-5.5,typesafe/jev-router",
		ModelProtocols: map[string]string{
			"test-model": "openai", // catalog-side entry, absent from the pool
		},
	}
	m := NewAdvancedConfigModel(&p)
	m.live().modelsDevRefreshPending = false

	catalog := testModelsDevProvider() // id "test-gw", advertises "test-model"
	catalog.Models["other-model"] = modelsdev.Model{ID: "other-model", Name: "Other"}
	m.handleModelsDevRefreshDone(modelsDevFetchDoneMsg{
		providers:  []modelsdev.Provider{catalog},
		refreshFor: "test-gw",
	})

	if got := strings.Join(m.live().modelPool, ","); got != "anthropic/claude-sonnet-5.5,typesafe/jev-router,other-model,test-model" {
		t.Fatalf("different-namespace pool was not appended verbatim: %q", got)
	}
	if p.ModelProtocols["other-model"] != "openai" || p.ModelProtocols["test-model"] != "openai" {
		t.Fatalf("appended catalog models lack wire protocols: %v", p.ModelProtocols)
	}
	for _, want := range []string{"other-model", "test-model"} {
		if _, ok := m.live().modelDisplayMetadata[want]; !ok {
			t.Fatalf("appended catalog model %q has no display metadata", want)
		}
	}
}

// TestModelsDevRefreshIgnoredForOtherProvider verifies a refresh result for a
// different provider id does not touch the current pool.
func TestModelsDevRefreshIgnoredForOtherProvider(t *testing.T) {
	p := provider.Provider{
		Type: "modelsdev", Name: "mine", Endpoint: "https://example.test/v1", APIKey: "key",
		Model: "a,b", ModelProtocols: map[string]string{"a": "openai", "b": "openai"},
	}
	m := NewAdvancedConfigModel(&p)
	m.live().modelsDevRefreshPending = false
	before := strings.Join(m.live().modelPool, ",")

	catalog := testModelsDevProvider() // id "test-gw"
	m.handleModelsDevRefreshDone(modelsDevFetchDoneMsg{
		providers:  []modelsdev.Provider{catalog},
		refreshFor: "test-gw",
	})
	if got := strings.Join(m.live().modelPool, ","); got != before {
		t.Fatalf("pool changed on a foreign refresh: %q -> %q", before, got)
	}
}

// TestModelsDevRefreshKeepsPoolOnFetchError verifies a failed catalog fetch
// leaves the saved pool intact.
func TestModelsDevRefreshKeepsPoolOnFetchError(t *testing.T) {
	p := provider.Provider{
		Type: "modelsdev", Name: "mine", Endpoint: "https://example.test/v1", APIKey: "key",
		Model: "a,b", ModelProtocols: map[string]string{"a": "openai", "b": "openai"},
	}
	m := NewAdvancedConfigModel(&p)
	before := strings.Join(m.live().modelPool, ",")
	m.handleModelsDevRefreshDone(modelsDevFetchDoneMsg{err: fmt.Errorf("offline"), refreshFor: "mine"})
	if got := strings.Join(m.live().modelPool, ","); got != before {
		t.Fatalf("pool changed on failed refresh: %q -> %q", before, got)
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

func TestDefaultModelStepperAndPickerFlow(t *testing.T) {
	saved := locale.Current()
	t.Cleanup(func() { locale.SetLanguage(saved) })
	locale.SetLanguage("en")

	p := providerFrom("gw", "https://example.com/v1", "openai")
	m := NewAdvancedConfigModel(&p)
	enterDetectedReview(m, "alpha-1", "beta-2", "gamma-3")

	// Stepper cycling with Left/Right on rowDefaultModel.
	m.cursor = m.mainRowIndex(rowDefaultModel)
	m.handleKey(tui.KeyEvent{Key: tui.KeyRight})
	if m.p.DefaultModel != "alpha-1" {
		t.Fatalf("after Right, DefaultModel = %q, want alpha-1", m.p.DefaultModel)
	}
	m.handleKey(tui.KeyEvent{Key: tui.KeyRight})
	if m.p.DefaultModel != "beta-2" {
		t.Fatalf("after second Right, DefaultModel = %q, want beta-2", m.p.DefaultModel)
	}
	m.handleKey(tui.KeyEvent{Key: tui.KeyLeft})
	if m.p.DefaultModel != "alpha-1" {
		t.Fatalf("after Left, DefaultModel = %q, want alpha-1", m.p.DefaultModel)
	}
	m.handleKey(tui.KeyEvent{Key: tui.KeyLeft})
	if m.p.DefaultModel != "" {
		t.Fatalf("after wrapping Left, DefaultModel = %q, want empty (auto)", m.p.DefaultModel)
	}

	// Open picker overlay via Enter, filter, Tab toggle [1m], Enter confirm.
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if !m.defaultModelPicker {
		t.Fatal("Enter on rowDefaultModel did not open defaultModelPicker")
	}
	for _, r := range "beta" {
		m.handleKey(keyPress(r))
	}
	if len(m.defaultModelFiltered) != 1 || m.defaultModelFiltered[0] != "beta-2" {
		t.Fatalf("filtered = %v, want [beta-2]", m.defaultModelFiltered)
	}
	m.handleKey(tui.KeyEvent{Key: tui.KeyTab})
	if !m.poolModelHasOneM("beta-2") {
		t.Fatal("Tab inside defaultModelPicker did not toggle [1m]")
	}
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if m.defaultModelPicker {
		t.Fatal("Enter inside defaultModelPicker did not close picker")
	}
	if m.p.DefaultModel != "beta-2[1m]" {
		t.Fatalf("confirmed DefaultModel = %q, want beta-2[1m]", m.p.DefaultModel)
	}

	// Reset to Auto inside picker.
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if m.defaultModelCursor == 0 {
		t.Fatal("expected picker cursor to open on beta-2")
	}
	m.defaultModelCursor = 0
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if m.p.DefaultModel != "" {
		t.Fatalf("reset to Auto DefaultModel = %q, want empty", m.p.DefaultModel)
	}

	// Re-detection preserves surviving DefaultModel (with [1m]) and clears dropped ones.
	m.p.DefaultModel = "beta-2"
	m.applyModelDetectionResult("openai", "alpha-1,beta-2", p.Endpoint, nil)
	if m.p.DefaultModel != "beta-2[1m]" {
		t.Fatalf("surviving DefaultModel = %q, want beta-2[1m]", m.p.DefaultModel)
	}
	m.applyModelDetectionResult("openai", "alpha-1,delta-4", p.Endpoint, nil)
	if m.p.DefaultModel != "" {
		t.Fatalf("dropped DefaultModel = %q, want cleared empty", m.p.DefaultModel)
	}
}
