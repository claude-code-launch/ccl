package cmd

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/atotto/clipboard"
	tui "github.com/grindlemire/go-tui"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// The single dark-terminal palette carried over from the previous adaptive
// (light/dark) color set. go-tui has no background detection, so the dark
// variants — the ones this panel was designed around — are the single source.
var (
	colorAccent    = tui.RGBColor(0x65, 0xB7, 0xFF)
	colorSecondary = tui.RGBColor(0xB7, 0x9C, 0xFF)
	colorData      = tui.RGBColor(0x41, 0xD7, 0xC8)
	colorWarning   = tui.RGBColor(0xF0, 0xB8, 0x4D)
	colorError     = tui.RGBColor(0xFF, 0x8A, 0x80)
)

// Text styles used across the panel. A tui.Style is a value; the styled form of
// a string is a tui.TextSpan produced by span().
var (
	stTitle       = tui.NewStyle().Bold()
	stBadge       = tui.NewStyle().Foreground(colorAccent).Bold()
	stProtoBadge  = tui.NewStyle().Foreground(colorSecondary)
	stCyan        = tui.NewStyle().Foreground(colorData)
	stPurple      = tui.NewStyle().Foreground(colorSecondary)
	stGray        = tui.NewStyle().Dim()
	stDivider     = tui.NewStyle().Dim()
	stSelected    = tui.NewStyle().Foreground(colorAccent).Bold()
	stFilter      = tui.NewStyle().Foreground(colorAccent)
	stAvailable   = tui.NewStyle().Foreground(colorData).Bold()
	stUnavailable = tui.NewStyle().Foreground(colorError)
	stOneM        = tui.NewStyle().Foreground(colorWarning).Bold()
)

// span wraps text with a style for tui.WithRichText rows.
func span(text string, st tui.Style) tui.TextSpan {
	return tui.TextSpan{Text: text, Style: st}
}

// line builds one no-wrap row element from styled spans. Rows never wrap so
// the line number of every label stays stable for the mouse hit test.
func line(spans ...tui.TextSpan) *tui.Element {
	return tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Row), tui.WithWrap(false), tui.WithRichText(spans...))
}

// plainLine is line() for unstyled text.
func plainLine(text string) *tui.Element {
	return line(span(text, tui.NewStyle()))
}

const (
	filterViewHeight      = 15 // max visible items in filter list
	preferredPanelWidth   = 82
	minimumPanelWidth     = 54
	minimumTerminalMargin = 4
	// 8 workers, not 50: a concurrent burst against one gateway trips per-key
	// rate limits and marks healthy models unavailable.
	slotTestConcurrency = 8
	lowCostProbeModel   = "gpt-5.4-mini"
)

// configRowKind enumerates the focusable rows of the single configuration page.
// The page cursor indexes into visibleRows(); adding or hiding a row never
// requires renumbering offsets.
type configRowKind uint8

const (
	rowSource configRowKind = iota // Connection source stepper (Custom ↔ models.dev)
	rowEndpoint
	rowAPIKey
	rowProvider // open the models.dev provider picker (models.dev source only)
	rowCopyKey  // click the key value to copy the full key
	rowCopyURL  // click the endpoint value to copy the URL
	rowTest     // Auto Configure
	rowProtocol
	rowFast
	rowOpus
	rowSonnet
	rowHaiku
	rowCustom
	rowSubagent
	rowTestModels // Test model availability (optional, costs quota)
	rowContext    // Context & Compact entry
	rowTools
	rowToolSearch
	rowActive
	rowSave
	rowCancel
)

type configRow struct {
	kind configRowKind
	// editable marks rows adjusted with ←→ (Protocol/Fast/Runtime) or enter
	// (model rows / save). Endpoint and API Key are always text-editable.
	editable bool
}

// connectionSource selects which Connection mode the page is editing: a manual
// Custom gateway (endpoint/key/protocol typed by hand) or a models.dev provider
// (endpoint and per-model protocol table derived from catalog metadata).
type connectionSource uint8

const (
	sourceCustom connectionSource = iota
	sourceModelsDev
)

// connDraft holds every piece of connection-scoped state for one Source side.
// The model owns two instances (customDraft / modelsDevDraft) so switching
// between Custom and models.dev preserves each side's endpoint, key, model pool,
// and detection state. switchSource rebinds m.p to the live draft's provider.
type connDraft struct {
	p *provider.Provider

	modelPool []string
	// modelDisplayMetadata is keyed by lower-case model ID. It affects search
	// and rendering only; slot values and provider.Model always retain the
	// upstream ID required for requests.
	modelDisplayMetadata map[string]protocol.ModelInfo
	// modelContextWindows stores advisory context_window values from /models
	// catalogs (keyed by model id). Zero/missing means unknown — never treat as
	// a hard guarantee of 1M support.
	modelContextWindows map[string]int
	oneMSlots           map[string]bool
	compactPreset       compactPreset
	// anthropicAuth remembers the direct Anthropic authentication style while the
	// custom protocol selector is temporarily on Chat or Responses. The provider
	// field itself stays empty for non-Anthropic protocols.
	anthropicAuth string

	probeEndpoint string
	probeAPIKey   string

	// detectedInputEndpoint / detectedInputKey hold the raw Endpoint/API Key the
	// last successful detection ran against (before any normalization). A
	// connection is dirty when the current inputs differ from these, so reverting
	// an edit or cancelling a re-detection un-dirties the page naturally.
	detectedInputEndpoint string
	detectedInputKey      string

	// inputEndpoint / inputAPIKey mirror the text-input widget values so
	// switchSource can save and restore what the user typed without leaking the
	// normalized endpoint back into the input box on the way back.
	inputEndpoint string
	inputAPIKey   string

	modelPoolFromDiscovery bool
	clearStaleSlots        bool
	hadLocalModelPool      bool
	// connectionDirty reports that the current Endpoint/API Key inputs differ
	// from the last successful detection. A dirty connection must be re-tested
	// before saving (except for OAuth providers, which never detect over HTTP).
	// It is derived on demand rather than a sticky flag, so reverting an edit
	// or cancelling a re-detection clears it.
	connectionDirty bool
	// autoConfigured is set after a successful detection filled the slots. It is
	// cleared when the user edits a field so a later re-render cannot silently
	// overwrite their choice.
	autoConfigured bool
	// autoDetectOnOpen marks an existing provider whose connection should be
	// re-verified automatically when the page opens (Init). Until that check
	// succeeds, connectionReady is false and Model Mapping / Runtime stay greyed.
	autoDetectOnOpen bool

	// detectionError is set when protocol detection and model fetching both fail.
	detectionError error
	detecting      bool
	detectProgress int
	detectFrame    int

	// keyVerified records that a models.dev API key was actually verified against
	// the provider endpoint (a real /models probe, not just a non-empty string).
	// It is cleared whenever the key input is edited or the provider changes.
	keyVerified bool

	// saveGuardPending marks that the user pressed Save on the Custom source
	// while the models.dev side still holds the same-named provider, and the
	// overwrite warning is awaiting a second confirmation.
	saveGuardPending bool
}

type AdvancedConfigModel struct {
	source         connectionSource
	customDraft    *connDraft
	modelsDevDraft *connDraft
	// p always aliases the live draft's provider (m.live().p). It is rebound on
	// switchSource so every m.p.XXX access below resolves to the active source
	// without per-site edits.
	p *provider.Provider

	cursor int
	width  int
	height int
	// scrollOffset is the number of rendered lines scrolled off the top of the
	// single page when the content exceeds the terminal height. The cursor row is
	// kept visible: scrolling happens in the key handlers, never in Render.
	scrollOffset int
	// keyCopied / urlCopied show a brief "copied" hint after the value is copied.
	keyCopied bool
	urlCopied bool
	// lastCopyClickAt / lastCopyClickRow detect a double-click on a value row:
	// the second click within the window copies instead of focusing.
	lastCopyClickAt  time.Time
	lastCopyClickRow configRowKind
	// lastKeyCopyAt / lastUrlCopyAt timestamp the copy so the shared 2s timer
	// watcher (handleFetchTick) can clear the hint.
	lastKeyCopyAt time.Time
	lastUrlCopyAt time.Time

	// The four text inputs (endpoint, API key, slot filter, models.dev filter)
	// are hand-rolled line editors: each owns a State[string] plus a focused
	// flag. The API key editor is multi-line (Enter inserts a newline).
	urlText    *tui.State[string]
	keyText    *tui.State[string]
	urlFocused bool
	keyFocused bool

	// Input box widths, derived from panelWidth in updateInputWidths (and
	// only used by Render; the hand-rolled editors carry no width of their own).
	urlInputWidth    int
	keyInputWidth    int
	filterInputWidth int

	// Model mapping
	activeSlot        int
	filterText        *tui.State[string]
	filterFocused     bool
	filteredPool      []string
	slotListCursor    int
	filterWindowStart int // first visible index in filter list
	modelAvailability map[string]modelAvailability
	modelTesting      bool
	modelTestCancel   context.CancelFunc
	modelTestFrame    int
	modelTestID       uint64
	modelTestCanceled bool

	// models.dev picker overlay, opened from the Connection section. It lets the
	// user pick a models.dev provider instead of typing an endpoint URL.
	modelsDevPicker   bool
	modelsDevText     *tui.State[string]
	modelsDevFiltered []modelsdev.Provider
	modelsDevItems    []modelsdev.Provider
	modelsDevCursor   int
	modelsDevWindow   int
	modelsDevLoading  bool
	modelsDevError    error

	// Save state
	IsActiveChosen bool
	saveConfirmed  bool
	// quitRequested records that a key/click asked the session to end (app.Stop
	// when wired to a live app). Tests assert on it without an App.
	quitRequested bool

	// app is bound by the framework before the first render (AppBinder). The
	// async channels below deliver their results onto the main loop through it.
	app        *tui.App
	fetchDone  chan modelFetchDoneMsg
	verifyDone chan keyVerifyDoneMsg
	mdDone     chan modelsDevFetchDoneMsg
	availDone  chan modelAvailabilityDoneMsg
}

type modelAvailability uint8

const (
	modelAvailabilityUnknown modelAvailability = iota
	modelAvailabilityAvailable
	modelAvailabilityUnavailable
)

type modelAvailabilityDoneMsg struct {
	testID   uint64
	statuses map[string]modelAvailability
}

type modelFetchDoneMsg struct {
	endpoint            string
	apiKey              string
	detectedType        string
	detectedEndpoint    string
	anthropicAuth       string
	discoveredModelsRaw string
	contextWindows      map[string]int
	modelInfos          []protocol.ModelInfo
	err                 error
}

// keyVerifyDoneMsg reports the result of verifying a models.dev API key against
// the provider endpoint. Unlike modelFetchDoneMsg it never mutates provider
// config — the metadata-derived Type, endpoint, and per-model protocol table are
// left untouched; only the key's validity (keyVerified) is recorded.
type keyVerifyDoneMsg struct {
	endpoint string
	apiKey   string
	err      error
}

// keyVerifyTimeout bounds the single authenticated inference request used to
// verify a models.dev API key.
const keyVerifyTimeout = 10 * time.Second

// keyVerifyAsync sends one minimal, authenticated inference request for the
// given model and protocol, and reports only whether the key was accepted at
// the auth layer. Unlike fetchModelsAsync it never mutates provider config —
// the metadata-derived Type, endpoint, and per-model protocol table are left
// untouched; only the key's validity (keyVerified) is recorded. The result is
// delivered on the verify channel consumed by Watchers().
func keyVerifyAsync(done chan<- keyVerifyDoneMsg, endpoint, apiKey, model, proto string) {
	go func() {
		err := verifyProviderAPIKey(context.Background(), model, endpoint, apiKey, proto, keyVerifyTimeout)
		done <- keyVerifyDoneMsg{endpoint: endpoint, apiKey: apiKey, err: err}
	}()
}

// verifyProviderAPIKey sends a minimal authenticated inference request for one
// model in the provider's protocol table and reports whether the key is
// rejected. A 401/403 means the key itself is invalid; a 2xx or any other 4xx
// (bad model/params, rate limit) proves the key passed auth. Transport errors
// and 5xx are reported as "cannot verify" rather than "verified".
func verifyProviderAPIKey(parent context.Context, model, endpoint, apiKey, proto string, timeout time.Duration) error {
	var status int
	var err error
	switch proto {
	case "anthropic":
		status, err = probeModelStatus(parent, buildAnthropicMessagesURL(endpoint), map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		}, map[string]string{"anthropic-version": "2023-06-01", "x-api-key": apiKey}, timeout)
	case "openai_responses":
		status, err = protocol.ProbeOpenAIResponsesStatusContext(parent, endpoint, apiKey, model, timeout)
	default: // "openai" (Chat Completions)
		status, err = probeModelStatus(parent, buildChatURL(endpoint), map[string]any{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
			"max_tokens": 1,
		}, map[string]string{"Authorization": "Bearer " + apiKey}, timeout)
	}
	if err != nil {
		return err
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf(locale.T("API key 无效（HTTP %d）", "invalid API key (HTTP %d)"), status)
	}
	if status >= 200 && status < 300 {
		return nil
	}
	if status >= 400 && status < 500 {
		// Auth already passed; the model/params/rate-limit rejected the request.
		return nil
	}
	return fmt.Errorf(locale.T("验证请求失败（HTTP %d）", "verification request failed (HTTP %d)"), status)
}

// firstRoutableModel returns the first model in the pool that has a known
// protocol in the provider's per-model table, used to send a real key-verification
// request. ok is false when the pool is empty or no model has a protocol.
func (m *AdvancedConfigModel) firstRoutableModel() (model, proto string, ok bool) {
	for _, name := range m.live().modelPool {
		if proto := m.p.ModelProtocols[strings.ToLower(strings.TrimSpace(name))]; proto != "" {
			return name, proto, true
		}
	}
	return "", "", false
}

// modelsDevFetchDoneMsg carries the models.dev catalog (or its fetch error) back
// to the picker overlay.
type modelsDevFetchDoneMsg struct {
	providers []modelsdev.Provider
	err       error
}

// fetchModelsDevAsync fetches the models.dev catalog off the UI thread and
// delivers the result on the models.dev channel.
func fetchModelsDevAsync(done chan<- modelsDevFetchDoneMsg) {
	go func() {
		providers, err := modelsDevProviders(context.Background())
		done <- modelsDevFetchDoneMsg{providers: providers, err: err}
	}()
}

// fetchModelsAsync runs protocol detection + model fetching off the UI thread
// and delivers the result on the fetch channel.
func fetchModelsAsync(done chan<- modelFetchDoneMsg, endpoint, apiKey string) {
	go func() {
		setDebugf("modelFetch start endpoint=%q api_key_len=%d", endpoint, len(apiKey))
		result := detectProtocolAndModelsDetailed(endpoint, apiKey)
		setDebugf(
			"modelFetch done endpoint=%q detected_endpoint=%q protocol=%q anthropic_auth=%q model_count=%d err=%v",
			endpoint,
			result.baseURL,
			result.protocol,
			result.anthropicAuth,
			countCSV(result.models),
			result.err,
		)
		// Best-effort: pull context_window metadata for OpenAI-family catalogs.
		// Failures are ignored — IDs still come from detection.
		windows := map[string]int{}
		if result.err == nil && result.protocol != "" && !provider.IsAnthropicType(result.protocol) && !provider.IsCommandCodeType(result.protocol) {
			// Subscription runtimes only expose windows through the Codex catalog,
			// which AdvertisedContextWindows tries before the plain OpenAI list.
			advertised, source := claude.AdvertisedContextWindows(result.baseURL, apiKey)
			for id, window := range advertised {
				windows[id] = window
			}
			setDebugf("modelFetch context windows catalog=%q count=%d", source, len(windows))
		}
		done <- modelFetchDoneMsg{
			endpoint:            endpoint,
			apiKey:              apiKey,
			detectedType:        result.protocol,
			detectedEndpoint:    result.baseURL,
			anthropicAuth:       result.anthropicAuth,
			discoveredModelsRaw: result.models,
			contextWindows:      windows,
			modelInfos:          result.modelInfos,
			err:                 result.err,
		}
	}()
}

// testModelsAsync probes every model in the pool concurrently and delivers the
// statuses on the availability channel. 8 workers, not 50: a concurrent burst
// against one gateway trips per-key rate limits and marks healthy models
// unavailable.
func testModelsAsync(done chan<- modelAvailabilityDoneMsg, ctx context.Context, testID uint64, models []string, endpoint, apiKey, providerType, anthropicAuth string, protocols map[string]string, smokeTestModel string) {
	models = append([]string(nil), models...)
	go func() {
		statuses := make(map[string]modelAvailability, len(models))
		if len(models) == 0 {
			done <- modelAvailabilityDoneMsg{testID: testID, statuses: statuses}
			return
		}
		if smokeTestModel != "" {
			status := modelAvailabilityUnavailable
			if testSingleModelWithProtocolsContext(ctx, smokeTestModel, endpoint, apiKey, providerType, anthropicAuth, protocols, 10*time.Second) {
				status = modelAvailabilityAvailable
			}
			if ctx.Err() == nil {
				for _, model := range models {
					statuses[model] = status
				}
			}
			done <- modelAvailabilityDoneMsg{testID: testID, statuses: statuses}
			return
		}

		jobs := make(chan string, len(models))
		for _, model := range models {
			jobs <- model
		}
		close(jobs)

		var wg sync.WaitGroup
		var mu sync.Mutex
		workers := min(slotTestConcurrency, len(models))
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case model, ok := <-jobs:
						if !ok {
							return
						}
						status := modelAvailabilityUnavailable
						if testSingleModelWithProtocolsContext(ctx, model, endpoint, apiKey, providerType, anthropicAuth, protocols, 10*time.Second) {
							status = modelAvailabilityAvailable
						}
						if ctx.Err() != nil {
							return
						}
						mu.Lock()
						statuses[model] = status
						mu.Unlock()
					}
				}
			}()
		}

		wg.Wait()
		done <- modelAvailabilityDoneMsg{testID: testID, statuses: statuses}
	}()
}

// live returns the connection draft for the currently active source. Every
// connection-scoped field is reached through it so switchSource swaps the whole
// set at once.
func (m *AdvancedConfigModel) live() *connDraft {
	if m.source == sourceModelsDev {
		return m.modelsDevDraft
	}
	return m.customDraft
}

// currentRow returns the configRowKind the page cursor sits on.
func (m *AdvancedConfigModel) currentRow() configRowKind {
	rows := m.visibleRows()
	if m.cursor < 0 || m.cursor >= len(rows) {
		return rowCancel
	}
	return rows[m.cursor].kind
}

// connectionReady reports whether the Model Mapping and Runtime sections are
// interactive. For a new provider the Endpoint/API Key must be filled and
// Auto Configure run (a model pool discovered); OAuth providers are always
// ready (their credentials are already live).
func (m *AdvancedConfigModel) connectionReady() bool {
	if m.usesOAuth() {
		return true
	}
	// models.dev is ready only after a provider has been picked (which loads the
	// model pool via metadata) — same gate as a detected Custom connection.
	return m.live().modelPoolFromDiscovery
}

// visibleRows lists the focusable rows of the single configuration page in
// render order. Connection rows are always present; before a successful
// detection the Model Mapping / Runtime rows still render but are marked
// non-editable (greyed out) until the connection is ready.
func (m *AdvancedConfigModel) visibleRows() []configRow {
	rows := make([]configRow, 0, 4)
	if m.usesOAuth() {
		// OAuth keeps its original row set: no Source stepper, no endpoint input,
		// and Protocol stays in the Runtime section (its Connection block is
		// read-only subscription metadata).
		rows = append(rows,
			configRow{kind: rowAPIKey},
			configRow{kind: rowTest},
		)
		ready := m.connectionReady()
		rows = append(rows,
			configRow{kind: rowOpus, editable: ready},
			configRow{kind: rowSonnet, editable: ready},
			configRow{kind: rowHaiku, editable: ready},
			configRow{kind: rowCustom, editable: ready},
			configRow{kind: rowSubagent, editable: ready},
			configRow{kind: rowTestModels, editable: ready},
			configRow{kind: rowContext, editable: ready},
			configRow{kind: rowProtocol, editable: ready},
			configRow{kind: rowFast, editable: ready},
			configRow{kind: rowTools, editable: ready},
			configRow{kind: rowToolSearch, editable: ready},
			configRow{kind: rowActive, editable: ready},
			configRow{kind: rowSave},
			configRow{kind: rowCancel},
		)
		return rows
	}

	// Non-OAuth: the Source stepper leads, then the per-source Connection rows.
	rows = append(rows, configRow{kind: rowSource})
	if m.source == sourceCustom {
		rows = append(rows,
			configRow{kind: rowEndpoint},
			configRow{kind: rowAPIKey},
			configRow{kind: rowProtocol},
			configRow{kind: rowTest},
		)
	} else {
		// models.dev: endpoint and protocol come from metadata (read-only); the
		// Provider row opens the catalog picker and only the API key is editable.
		// Protocol stays rendered inline (like Endpoint) but is not a navigable
		// stop — there is nothing to toggle on a mixed-protocol gateway. The Test
		// Connection row verifies the entered key against the real endpoint.
		rows = append(rows,
			configRow{kind: rowProvider},
			configRow{kind: rowAPIKey},
			configRow{kind: rowTest},
		)
	}
	ready := m.connectionReady()
	// Render order matches View: Connection → Model Mapping → Context →
	// Runtime (Fast/Tools/...) → Active → actions.
	rows = append(rows,
		configRow{kind: rowOpus, editable: ready},
		configRow{kind: rowSonnet, editable: ready},
		configRow{kind: rowHaiku, editable: ready},
		configRow{kind: rowCustom, editable: ready},
		configRow{kind: rowSubagent, editable: ready},
		configRow{kind: rowTestModels, editable: ready},
		configRow{kind: rowContext, editable: ready},
		configRow{kind: rowFast, editable: ready},
		configRow{kind: rowTools, editable: ready},
		configRow{kind: rowToolSearch, editable: ready},
		configRow{kind: rowActive, editable: ready},
		configRow{kind: rowSave},
		configRow{kind: rowCancel},
	)
	return rows
}

// mainRowIndex maps a kind onto its index in visibleRows, or -1 when the row is
// not currently shown.
func (m *AdvancedConfigModel) mainRowIndex(kind configRowKind) int {
	for i, r := range m.visibleRows() {
		if r.kind == kind {
			return i
		}
	}
	return -1
}

// scrollBodyBudget returns how many body lines fit under the fixed page chrome
// (title bar, connection block gap, detection status, panel border, footer tip).
// Both keepCursorVisible (Update) and View use this so the cursor row the update
// keeps visible is exactly the window View slices, never a line taller than the
// renderer's actual body lines.
func scrollBodyBudget(height int) int {
	budget := height - 8
	if budget < 6 {
		budget = 6
	}
	return budget
}

// keepCursorVisible clamps scrollOffset so the cursor row stays inside the
// visible region. It runs after cursor movement in Update; View never mutates
// scroll state.
//
// scrollWindow slices the body at the *element* index; the previous
// rowLineHeight estimate counted only focusable rows and ignored structural
// lines (header, section titles, blank separators, footer), so the cursor
// drifted off-screen until it happened to hit the tail anchor. Locating the
// cursor by its rendered label keeps the two in agreement.
func (m *AdvancedConfigModel) keepCursorVisible() {
	if m.height <= 0 {
		return
	}
	rows := m.bodyRows()
	visibleHeight := scrollBodyBudget(m.height)
	if len(rows) <= visibleHeight {
		m.scrollOffset = 0
		return
	}
	cursorLine := m.cursorBodyLine(rows)

	if cursorLine < m.scrollOffset {
		m.scrollOffset = cursorLine
	}
	if cursorLine+1 > m.scrollOffset+visibleHeight {
		m.scrollOffset = cursorLine + 1 - visibleHeight
	}
	maxOffset := len(rows) - visibleHeight
	if m.scrollOffset > maxOffset {
		m.scrollOffset = maxOffset
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
}

// cursorBodyLine returns the 0-based body-line index of the cursor row, located
// by matching the row's rendered label prefix in the sprinted body text. Every
// body element renders as exactly one line (a no-wrap flex row), so this text
// line index equals the element index scrollWindow slices at.
func (m *AdvancedConfigModel) cursorBodyLine(rows []*tui.Element) int {
	vrows := m.visibleRows()
	if m.cursor < 0 || m.cursor >= len(vrows) {
		return 0
	}
	kind := vrows[m.cursor].kind
	// Cancel shares the Save & Activate line; match Save's labels so the row
	// locates instead of resolving to 0 (which would snap the scroll to the top).
	if kind == rowCancel {
		kind = rowSave
	}
	prefixes := rowClickLabelPrefixes(kind)
	if len(prefixes) == 0 {
		return 0
	}
	container := tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Column), tui.WithWrap(false))
	for _, r := range rows {
		container.AddChild(r)
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	for i, line := range strings.Split(tui.Sprint(container, tui.WithPrintWidth(width)), "\n") {
		// Sprint emits ANSI SGR codes ahead of every styled span; strip them
		// before the label match, exactly like the mouse hit-test does.
		trimmed := strings.TrimLeft(stripANSI(line), " >│")
		// The Active row leads with a "[x] "/"[ ] " checkbox; skip it so the
		// label matches.
		if strings.HasPrefix(trimmed, "[") {
			if idx := strings.Index(trimmed, "] "); idx >= 0 {
				trimmed = trimmed[idx+2:]
			}
		}
		for _, p := range prefixes {
			if strings.HasPrefix(trimmed, p) {
				return i
			}
		}
	}
	return 0
}

// isModelRow reports whether a row kind is one of the five model slots.
func (m *AdvancedConfigModel) isModelRow(kind configRowKind) bool {
	switch kind {
	case rowOpus, rowSonnet, rowHaiku, rowCustom, rowSubagent:
		return true
	}
	return false
}

// toggleOneMAtRow flips the 1M context marker for the model row the cursor is
// on. Turning it on is refused when the backend window rules 1M out; turning an
// existing marker off stays possible. Returns true when the marker changed.
func (m *AdvancedConfigModel) toggleOneMAtRow(row configRowKind) bool {
	if !m.isModelRow(row) {
		return false
	}
	slot := []string{"opus", "sonnet", "haiku", "custom", "subagent"}[slotForRow(row)]
	model := m.slotModelForRow(row)
	if !m.live().oneMSlots[slot] && m.oneMSlotBlocked(model) {
		setDebugf("1M blocked slot=%s model=%q", slot, model)
		return false
	}
	if slot == "subagent" && !m.live().oneMSlots[slot] && !m.materializeSubagentModel() {
		return false
	}
	m.live().oneMSlots[slot] = !m.live().oneMSlots[slot]
	synced := m.syncOneMForSameModels(slot, m.live().oneMSlots[slot])
	setDebugf("toggle 1M slot=%s enabled=%t synced=%d summary=%s", slot, m.live().oneMSlots[slot], synced, reviewOneMSummary(m.live().oneMSlots))
	return true
}

// slotForRow maps a model row kind back to its advancedSlotRefs index.
func slotForRow(kind configRowKind) int {
	switch kind {
	case rowOpus:
		return 0
	case rowSonnet:
		return 1
	case rowHaiku:
		return 2
	case rowCustom:
		return 3
	case rowSubagent:
		return 4
	}
	return 0
}

// slotModelForRow returns the model currently bound to a model row kind.
func (m *AdvancedConfigModel) slotModelForRow(kind configRowKind) string {
	switch kind {
	case rowOpus:
		return m.p.OpusModel
	case rowSonnet:
		return m.p.SonnetModel
	case rowHaiku:
		return m.p.HaikuModel
	case rowCustom:
		return m.p.CustomModelID
	case rowSubagent:
		return m.p.SubagentModel
	}
	return ""
}

// canSave reports whether the current draft may be persisted. A new provider, or
// one whose Connection was edited since the last detection, must have a
// successful detection first. OAuth providers never detect over HTTP.
func (m *AdvancedConfigModel) canSave() bool {
	if m.usesOAuth() {
		return true
	}
	if m.live().connectionDirty {
		return false
	}
	if m.live().modelPoolFromDiscovery {
		// models.dev loads its model pool from metadata without requiring the API
		// key, so a "connected" pool can exist while the key is still empty. The
		// key is mandatory to persist (providerConfigurationComplete), but a mere
		// non-empty string proves nothing — it must have been verified against the
		// endpoint before the page allows saving.
		if m.usesModelsDev() {
			return m.live().keyVerified
		}
		return true
	}
	// No detection yet: allow saving an existing provider whose connection inputs
	// still match the persisted ones (e.g. editing only a model mapping).
	return m.live().autoConfigured && m.connectionUnchanged()
}

// saveWouldReplaceModelsDev reports whether saving right now would silently
// replace a configured models.dev provider: the active source is Custom, the
// Custom draft shares its name with the models.dev side, and that side carries
// the per-model protocol table that would be lost.
func (m *AdvancedConfigModel) saveWouldReplaceModelsDev() bool {
	if m.source != sourceCustom || m.modelsDevDraft == nil || m.modelsDevDraft.p == nil {
		return false
	}
	other := m.modelsDevDraft.p
	return strings.EqualFold(strings.TrimSpace(other.Name), strings.TrimSpace(m.p.Name)) &&
		provider.IsModelsDevType(other.Type) && len(other.ModelProtocols) > 0
}

// requestSave runs the shared save gate: it blocks an unconfirmed save, arms
// the overwrite warning when saving the Custom side would replace the
// same-named models.dev provider, and otherwise confirms the save. It returns
// true when the model should quit for persistence.
func (m *AdvancedConfigModel) requestSave() bool {
	if !m.canSave() {
		setDebugf("save blocked: connection dirty or undetected")
		return false
	}
	if m.saveWouldReplaceModelsDev() && !m.customDraft.saveGuardPending {
		m.customDraft.saveGuardPending = true
		setDebugf("save guarded: saving custom source would replace models.dev provider %q", m.p.Name)
		return false
	}
	m.saveConfirmed = true
	setDebugf("save requested provider=%q type=%q model_count=%d slots=%s one_m=%s compact=%s active_chosen=%t fast_mode=%t", m.p.Name, m.p.Type, countCSV(m.p.Model), slotDebugSummary(*m.p), reviewOneMSummary(m.live().oneMSlots), m.compactSummary(), m.IsActiveChosen, m.p.FastMode)
	return true
}

// refreshConnectionDirty re-derives the dirty state from the current inputs
// against the last successful detection's raw inputs. It is called whenever an
// input changes, so reverting an edit or cancelling a re-detection naturally
// clears the flag without a sticky manual reset.
func (m *AdvancedConfigModel) refreshConnectionDirty() {
	if m.usesModelsDev() {
		m.live().connectionDirty = false
		return
	}
	m.live().connectionDirty = !m.connectionMatchesDetected()
}

// connectionMatchesDetected reports whether the current Endpoint/API Key inputs
// match the raw inputs the last successful detection ran against. Before any
// successful detection the persisted provider values are the baseline.
func (m *AdvancedConfigModel) connectionMatchesDetected() bool {
	if m.p == nil {
		return false
	}
	baselineEndpoint := m.live().detectedInputEndpoint
	baselineKey := m.live().detectedInputKey
	hasDetected := m.live().detectedInputEndpoint != "" || m.live().detectedInputKey != ""
	if !hasDetected {
		baselineEndpoint = strings.TrimSpace(m.p.Endpoint)
		baselineKey = m.p.APIKey
	}
	return strings.TrimSpace(m.urlText.Get()) == strings.TrimSpace(baselineEndpoint) &&
		m.keyText.Get() == baselineKey
}

// syncConnectionInputs copies the current Endpoint/API Key inputs into the
// provider draft. The normal flow does this inside Auto Configure's detection
// step, but models.dev providers bypass the probe (their endpoint and protocol
// come from metadata), so the entered API key would otherwise never reach the
// persisted provider.
func (m *AdvancedConfigModel) syncConnectionInputs() {
	if m.usesOAuth() {
		return
	}
	m.p.Endpoint = strings.TrimSpace(m.urlText.Get())
	m.p.APIKey = m.keyText.Get()
}

// connectionUnchanged reports whether the endpoint/api-key inputs still match
// the provider that was last detected (or the persisted one).
func (m *AdvancedConfigModel) connectionUnchanged() bool {
	if m.p == nil {
		return false
	}
	endpointMatch := strings.TrimSpace(m.urlText.Get()) == strings.TrimSpace(m.p.Endpoint)
	keyMatch := m.keyText.Get() == m.p.APIKey
	return endpointMatch && keyMatch
}

func NewAdvancedConfigModel(p *provider.Provider) *AdvancedConfigModel {
	// An existing models.dev provider must open on the models.dev source, not
	// Custom: treating it as Custom would let the auto-probe overwrite Type
	// ("modelsdev") with a single detected protocol and drop the per-model
	// routing table. When models.dev, the Custom draft starts empty (same name)
	// so the user can still switch to a clean Custom form.
	isModelsDev := p != nil && provider.IsModelsDevType(p.Type)
	customP := p
	modelsDevP := &provider.Provider{}
	if isModelsDev {
		customP = &provider.Provider{Name: p.Name}
		modelsDevP = p
	}

	customDraft := &connDraft{
		p:                    customP,
		oneMSlots:            make(map[string]bool),
		modelContextWindows:  make(map[string]int),
		modelDisplayMetadata: make(map[string]protocol.ModelInfo),
		compactPreset:        compactPresetFromProvider(*customP),
		anthropicAuth:        customP.AnthropicAuth,
		probeEndpoint:        customP.Endpoint,
		probeAPIKey:          customP.APIKey,
		inputEndpoint:        customP.Endpoint,
		inputAPIKey:          customP.APIKey,
		clearStaleSlots:      true,
	}
	modelsDevDraft := &connDraft{
		p:                    modelsDevP,
		oneMSlots:            make(map[string]bool),
		modelContextWindows:  make(map[string]int),
		modelDisplayMetadata: make(map[string]protocol.ModelInfo),
	}

	source := sourceCustom
	active := customDraft
	if isModelsDev {
		source = sourceModelsDev
		active = modelsDevDraft
	}

	m := &AdvancedConfigModel{
		source:            source,
		customDraft:       customDraft,
		modelsDevDraft:    modelsDevDraft,
		p:                 active.p,
		cursor:            0,
		urlText:           tui.NewState(p.Endpoint),
		keyText:           tui.NewState(p.APIKey),
		filterText:        tui.NewState(""),
		modelsDevText:     tui.NewState(""),
		IsActiveChosen:    true,
		modelAvailability: make(map[string]modelAvailability),
		fetchDone:         make(chan modelFetchDoneMsg, 8),
		verifyDone:        make(chan keyVerifyDoneMsg, 8),
		mdDone:            make(chan modelsDevFetchDoneMsg, 2),
		availDone:         make(chan modelAvailabilityDoneMsg, 2),
	}

	cleanAndPopulate := func(modelStr *string, slotKey string) {
		if hasOneMSuffix(*modelStr) {
			m.live().oneMSlots[slotKey] = true
			*modelStr = stripOneMSuffix(*modelStr)
		}
	}
	cleanAndPopulate(&m.p.OpusModel, "opus")
	cleanAndPopulate(&m.p.SonnetModel, "sonnet")
	cleanAndPopulate(&m.p.HaikuModel, "haiku")
	cleanAndPopulate(&m.p.CustomModelID, "custom")
	cleanAndPopulate(&m.p.SubagentModel, "subagent")

	// An existing provider already carries a model pool (p.Model) and a
	// connection. Load the pool for display but do NOT mark it discovered: the
	// connection is re-verified automatically on open (Init), and until that
	// check succeeds the Model Mapping / Runtime sections stay greyed out.
	//
	// models.dev is the exception: its pool and routing are already complete, so
	// it counts as discovered, and it must NOT trigger Init's auto-probe (which
	// would overwrite Type). Its connection mirrors are seeded from the persisted
	// values, but the key still needs re-verification (keyVerified stays false).
	if !m.usesOAuth() {
		pool := uniqueModels(parseModelList(m.p.Model))
		if len(pool) > 0 {
			m.live().modelPool = pool
			if isModelsDev {
				m.live().modelPoolFromDiscovery = true
				m.live().autoDetectOnOpen = false
				m.live().connectionDirty = false
				m.live().detectedInputEndpoint = m.p.Endpoint
				m.live().probeEndpoint = m.p.Endpoint
				m.live().inputEndpoint = m.p.Endpoint
				m.live().inputAPIKey = m.p.APIKey
			} else {
				m.live().autoDetectOnOpen = true
			}
		}
	}

	return m
}

func (m *AdvancedConfigModel) configureOAuthRuntime(endpoint, apiKey string) {
	m.live().probeEndpoint = endpoint
	m.live().probeAPIKey = apiKey
	m.live().connectionDirty = false
	m.cursor = m.mainRowIndex(rowTest)
	m.urlFocused = false
	m.keyFocused = false
}

func (m *AdvancedConfigModel) usesOAuth() bool {
	return m.p != nil && strings.TrimSpace(m.p.OAuthProvider) != ""
}

// usesModelsDev reports whether the active Connection source is the models.dev
// picker. Its endpoint and per-model protocols come from metadata, not from an
// HTTP probe, so connection readiness and dirty tracking are bypassed.
func (m *AdvancedConfigModel) usesModelsDev() bool {
	return m.source == sourceModelsDev
}

// openModelsDevPicker opens the models.dev provider overlay and starts the
// catalog fetch.
func (m *AdvancedConfigModel) openModelsDevPicker() {
	m.urlFocused = false
	m.keyFocused = false
	m.live().detecting = false
	m.modelsDevPicker = true
	m.modelsDevLoading = true
	m.modelsDevError = nil
	m.modelsDevItems = nil
	m.modelsDevFiltered = nil
	m.modelsDevCursor = 0
	m.modelsDevWindow = 0
	m.modelsDevText.Set("")
}

func (m *AdvancedConfigModel) closeModelsDevPicker() {
	m.modelsDevPicker = false
}

// updateModelsDevFilter recomputes the filtered provider list from the filter
// input and clamps cursor/window. Filtering matches name and id case-insensitively.
func (m *AdvancedConfigModel) updateModelsDevFilter() {
	q := strings.ToLower(strings.TrimSpace(m.modelsDevText.Get()))
	if q == "" {
		m.modelsDevFiltered = m.modelsDevItems
	} else {
		m.modelsDevFiltered = make([]modelsdev.Provider, 0, len(m.modelsDevItems))
		for _, p := range m.modelsDevItems {
			if strings.Contains(strings.ToLower(p.Name), q) || strings.Contains(strings.ToLower(p.ID), q) {
				m.modelsDevFiltered = append(m.modelsDevFiltered, p)
			}
		}
	}
	if m.modelsDevCursor >= len(m.modelsDevFiltered) {
		m.modelsDevCursor = len(m.modelsDevFiltered) - 1
	}
	if m.modelsDevCursor < 0 {
		m.modelsDevCursor = 0
	}
	m.modelsDevWindow = 0
}

// saveInputsToDraft copies the shared text-widget values into the current
// source's input mirror, so switching away and back preserves what the user
// typed. It does not touch p.Endpoint/APIKey: those hold the normalized or
// metadata values, while the mirror keeps the raw text.
func (m *AdvancedConfigModel) saveInputsToDraft() {
	if m.usesOAuth() {
		return
	}
	m.live().inputEndpoint = m.urlText.Get()
	m.live().inputAPIKey = m.keyText.Get()
}

// otherSource returns the source to toggle to (Custom ↔ models.dev).
func (m *AdvancedConfigModel) otherSource() connectionSource {
	if m.source == sourceModelsDev {
		return sourceCustom
	}
	return sourceModelsDev
}

// switchSource flips the Connection source, preserving each side's draft. The
// current side's text inputs are saved to its mirror, the pointer rebinds to the
// target draft, and the shared widget values refresh from the target mirror.
func (m *AdvancedConfigModel) switchSource(target connectionSource) {
	if m.source == target || m.live().detecting {
		return
	}
	// Abort an in-flight availability test: its results belong to the old pool.
	if m.modelTesting && m.modelTestCancel != nil {
		m.modelTestCancel()
	}
	m.modelTesting = false
	m.modelTestCancel = nil
	m.modelTestCanceled = false
	m.modelAvailability = make(map[string]modelAvailability)

	m.saveInputsToDraft()
	m.source = target
	m.p = m.live().p
	// Leaving the Custom side abandons any pending overwrite warning.
	m.customDraft.saveGuardPending = false

	m.urlText.Set(m.live().inputEndpoint)
	m.keyText.Set(m.live().inputAPIKey)
	m.urlFocused = false
	m.keyFocused = false
	m.filterFocused = false
	m.updateFilteredPool()
	m.cursor = m.mainRowIndex(rowSource)
	m.keepCursorVisible()
}

// applyModelsDevProvider fills the models.dev draft with the chosen provider and
// switches to it. Endpoint, per-model protocol table, model pool, and slot
// recommendation all come from metadata; only the API key remains to be entered.
// The Custom draft is left untouched so the user can switch back.
func (m *AdvancedConfigModel) applyModelsDevProvider(p modelsdev.Provider) {
	draft, metadata := modelsDevProviderToDraft(p)
	d := m.modelsDevDraft
	*d.p = draft
	d.modelPool = uniqueModels(parseModelList(draft.Model))
	d.modelPoolFromDiscovery = true
	d.modelDisplayMetadata = copyModelInfoIndex(metadata)
	d.modelContextWindows = contextWindowsFromModelInfos(d.modelDisplayMetadata)
	d.detectedInputEndpoint = draft.Endpoint
	d.detectedInputKey = ""
	d.connectionDirty = false
	d.autoDetectOnOpen = false
	d.detectionError = nil
	d.keyVerified = false
	d.probeEndpoint = draft.Endpoint
	d.probeAPIKey = ""
	d.inputEndpoint = draft.Endpoint
	d.inputAPIKey = ""

	if m.source != sourceModelsDev {
		m.saveInputsToDraft()
		m.source = sourceModelsDev
		m.p = d.p
		// The overwrite warning belonged to the Custom side being left behind.
		m.customDraft.saveGuardPending = false
	}
	m.applyRecommendation()
	m.urlText.Set(d.inputEndpoint)
	m.keyText.Set(d.inputAPIKey)
	m.urlFocused = false
	m.keyFocused = true
	m.cursor = m.mainRowIndex(rowAPIKey)
	setDebugf("models.dev applied provider=%q model_count=%d protocols=%d", draft.Name, len(d.modelPool), len(draft.ModelProtocols))
}

// handleModelsDevPickerKey handles a key press while the models.dev overlay is
// open. Enter applies the selected provider and closes the overlay; esc/ctrl+c
// cancel; ↑↓ move the cursor; any other printable key filters the list.
func (m *AdvancedConfigModel) handleModelsDevPickerKey(ke tui.KeyEvent) {
	if ke.Key == tui.KeyEscape || ke.Mod == tui.ModCtrl && ke.Rune == 'c' {
		m.closeModelsDevPicker()
		return
	}
	switch ke.Key {
	case tui.KeyUp:
		if m.modelsDevCursor > 0 {
			m.modelsDevCursor--
		}
		if m.modelsDevCursor < m.modelsDevWindow {
			m.modelsDevWindow = m.modelsDevCursor
		}
	case tui.KeyDown:
		if m.modelsDevCursor < len(m.modelsDevFiltered)-1 {
			m.modelsDevCursor++
		}
		if m.modelsDevCursor >= m.modelsDevWindow+selectViewHeight {
			m.modelsDevWindow = m.modelsDevCursor - selectViewHeight + 1
		}
	case tui.KeyEnter:
		if len(m.modelsDevFiltered) > 0 && m.modelsDevCursor >= 0 && m.modelsDevCursor < len(m.modelsDevFiltered) {
			m.applyModelsDevProvider(m.modelsDevFiltered[m.modelsDevCursor])
			m.closeModelsDevPicker()
		}
	case tui.KeyBackspace:
		t := m.modelsDevText.Get()
		if t != "" {
			m.modelsDevText.Set(t[:len(t)-1])
		}
		m.updateModelsDevFilter()
	default:
		if ke.IsRune() && ke.Rune != 0 {
			m.modelsDevText.Set(m.modelsDevText.Get() + string(ke.Rune))
			m.updateModelsDevFilter()
		}
	}
}

// textInputHasKeyboard 表示当前按键会被某个文本输入框消费。条件与 KeyMap 的
// 输入路由保持一致：主页面光标停在 Endpoint/API Key 上，或模型筛选框聚焦。
// OAuth provider 没有可编辑的端点字段，因此不算。
func (m *AdvancedConfigModel) textInputHasKeyboard() bool {
	if m.usesOAuth() {
		return m.filterFocused
	}
	row := m.currentRow()
	return row == rowEndpoint || row == rowAPIKey || m.filterFocused
}

// NewAdvancedMappingModel opens the slot mapper with a pre-populated catalog.
// Model IDs remain the selectable and persisted values; metadata supplies only
// display labels and context hints.
func NewAdvancedMappingModel(p *provider.Provider, modelPool []string, metadata map[string]protocol.ModelInfo) *AdvancedConfigModel {
	m := NewAdvancedConfigModel(p)
	m.live().modelPool = modelPool
	m.live().modelPoolFromDiscovery = true
	m.live().modelDisplayMetadata = copyModelInfoIndex(metadata)
	m.live().modelContextWindows = contextWindowsFromModelInfos(m.live().modelDisplayMetadata)
	m.urlFocused = false
	m.keyFocused = false
	m.cursor = m.mainRowIndex(rowOpus)
	return m
}

// startAutoDetect re-verifies an existing provider's connection on open. Until
// the check succeeds the sections below Connection stay greyed out; OAuth
// providers are always ready so they skip this. Called once from Watchers()
// startup (go-tui has no Init() on the SetRootComponent path).
func (m *AdvancedConfigModel) startAutoDetect() {
	if m.live().autoDetectOnOpen && !m.usesOAuth() && !m.usesModelsDev() && strings.TrimSpace(m.live().probeEndpoint) != "" {
		m.live().detecting = true
		m.live().detectProgress = 5
		m.live().detectFrame = 0
		fetchModelsAsync(m.fetchDone, m.live().probeEndpoint, m.live().probeAPIKey)
	}
}

func (m *AdvancedConfigModel) availabilitySmokeTestModel() string {
	if m.p == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(m.p.OAuthProvider)) {
	case "gpt", "chatgpt", "codex":
		return lowCostProbeModel
	default:
		return ""
	}
}

// materializeSubagentModel fills the Subagent slot with the runtime default when
// it is unset, so a [1m] marker can be attached. Returns false when no default
// exists (nothing to materialize).
func (m *AdvancedConfigModel) materializeSubagentModel() bool {
	if m.p == nil {
		return false
	}
	if strings.TrimSpace(m.p.SubagentModel) != "" {
		return true
	}
	model := stripOneMSuffix(claude.ResolveRuntimeSettings(*m.p).SubagentModel)
	if model == "" {
		return false
	}
	m.p.SubagentModel = model
	if m.p.Env != nil {
		delete(m.p.Env, claude.SubagentModelEnv)
	}
	return true
}

func (m *AdvancedConfigModel) updateFilteredPool() {
	q := strings.ToLower(m.filterText.Get())
	if q == "" {
		m.filteredPool = append([]string{locale.T("(设置为未设置/清空)", "(clear/unset)")}, m.live().modelPool...)
	} else {
		m.filteredPool = []string{}
		for _, mod := range m.live().modelPool {
			searchable := strings.ToLower(mod + " " + m.modelDisplayLabel(mod))
			if strings.Contains(searchable, q) {
				m.filteredPool = append(m.filteredPool, mod)
			}
		}
		if len(m.filteredPool) == 0 {
			m.filteredPool = []string{locale.T("(无匹配模型)", "(no match)")}
		}
	}
	// Clamp cursor to new filtered pool bounds and reset scroll window
	m.filterWindowStart = 0
	if len(m.filteredPool) > 0 && m.slotListCursor >= len(m.filteredPool) {
		m.slotListCursor = len(m.filteredPool) - 1
	}
}

func copyModelInfoIndex(metadata map[string]protocol.ModelInfo) map[string]protocol.ModelInfo {
	copied := make(map[string]protocol.ModelInfo, len(metadata))
	for id, info := range metadata {
		copied[strings.ToLower(strings.TrimSpace(id))] = info
	}
	return copied
}

func contextWindowsFromModelInfos(metadata map[string]protocol.ModelInfo) map[string]int {
	windows := make(map[string]int, len(metadata))
	for id, info := range metadata {
		if info.ContextWindow > 0 {
			windows[strings.ToLower(strings.TrimSpace(id))] = info.ContextWindow
		}
	}
	return windows
}

func (m *AdvancedConfigModel) modelDisplayLabel(id string) string {
	return modelReportLabel(stripOneMSuffix(id), m.live().modelDisplayMetadata)
}

func (m *AdvancedConfigModel) subagentDisplayLabel() string {
	if m.p == nil {
		return ""
	}
	if model := strings.TrimSpace(m.p.SubagentModel); model != "" {
		return m.modelDisplayLabel(model)
	}
	if model, ok := m.p.Env[claude.SubagentModelEnv]; ok && strings.TrimSpace(model) != "" {
		return fmt.Sprintf("(env: %s)", m.modelDisplayLabel(strings.TrimSpace(model)))
	}
	effective := strings.TrimSpace(claude.ResolveRuntimeSettings(*m.p).SubagentModel)
	if effective == "" {
		return "(auto)"
	}
	return fmt.Sprintf("(auto: %s)", m.modelDisplayLabel(effective))
}

func reorderModelsByAvailability(models []string, statuses map[string]modelAvailability) []string {
	available := make([]string, 0, len(models))
	unknown := make([]string, 0, len(models))
	unavailable := make([]string, 0, len(models))
	for _, model := range models {
		switch statuses[model] {
		case modelAvailabilityAvailable:
			available = append(available, model)
		case modelAvailabilityUnavailable:
			unavailable = append(unavailable, model)
		default:
			unknown = append(unknown, model)
		}
	}
	return append(append(available, unknown...), unavailable...)
}

func (m *AdvancedConfigModel) availabilityFor(model string) modelAvailability {
	if status, ok := m.modelAvailability[model]; ok {
		return status
	}
	return modelAvailabilityUnknown
}

// availabilitySpan renders the per-model availability badge for the picker.
func (m *AdvancedConfigModel) availabilitySpan(model string) tui.TextSpan {
	switch m.availabilityFor(model) {
	case modelAvailabilityAvailable:
		return span(locale.T("✓ 可用", "✓ available"), stAvailable)
	case modelAvailabilityUnavailable:
		return span(locale.T("✗ 不可用", "✗ unavailable"), stUnavailable)
	default:
		return span(locale.T("? 未测试", "? not tested"), stGray)
	}
}

func (m *AdvancedConfigModel) availabilityCounts() (available, unavailable int) {
	for _, model := range m.live().modelPool {
		switch m.availabilityFor(model) {
		case modelAvailabilityAvailable:
			available++
		case modelAvailabilityUnavailable:
			unavailable++
		}
	}
	return available, unavailable
}

// syncTerminalSize refreshes m.width/m.height from the app's terminal each
// render. Scrolling (keepCursorVisible / scrollWindow) and the mouse
// hit-test both need the live size; without it a real session never scrolls
// and the panel is clipped at the terminal bottom, hiding Save/Cancel.
// A nil app (offline tests) keeps the fields as-is.
func (m *AdvancedConfigModel) syncTerminalSize(app *tui.App) {
	if app == nil {
		return
	}
	w, h := app.Size()
	if w > 0 {
		m.width = w
	}
	if h > 0 {
		m.height = h
		m.updateInputWidths()
	}
}

func (m *AdvancedConfigModel) panelWidth() int {
	if m.width <= 0 {
		return 70
	}
	available := m.width - minimumTerminalMargin
	if available < minimumPanelWidth {
		return max(available, 1)
	}
	return min(available, preferredPanelWidth)
}

func (m *AdvancedConfigModel) updateInputWidths() {
	inputWidth := max(m.panelWidth()-8, 20)
	m.urlInputWidth = inputWidth
	m.keyInputWidth = inputWidth
	m.filterInputWidth = inputWidth
}

// doAutoConfig auto-fills the four Claude model slots, leaves subagents on
// automatic model selection, and clears explicit effort and 1M settings.
type advancedSlotRef struct {
	key string
	ptr *string
}

func advancedSlotRefs(p *provider.Provider) []advancedSlotRef {
	return []advancedSlotRef{
		{key: "opus", ptr: &p.OpusModel},
		{key: "sonnet", ptr: &p.SonnetModel},
		{key: "haiku", ptr: &p.HaikuModel},
		{key: "custom", ptr: &p.CustomModelID},
		{key: "subagent", ptr: &p.SubagentModel},
	}
}

// uniqueModels drops blanks and duplicates while preserving order. A seen-set
// keeps it linear; the model pool can hold hundreds of entries.
func uniqueModels(models []string) []string {
	out := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, mod := range models {
		mod = strings.TrimSpace(mod)
		if mod == "" {
			continue
		}
		if _, ok := seen[mod]; ok {
			continue
		}
		seen[mod] = struct{}{}
		out = append(out, mod)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (m *AdvancedConfigModel) staleSlotCount() int {
	if !m.live().modelPoolFromDiscovery {
		return 0
	}
	count := 0
	for _, slot := range advancedSlotRefs(m.p) {
		model := strings.TrimSpace(*slot.ptr)
		if model != "" && !stringInSlice(model, m.live().modelPool) {
			count++
		}
	}
	return count
}

func (m *AdvancedConfigModel) applyStaleSlotPolicy() {
	if !m.live().clearStaleSlots || !m.live().modelPoolFromDiscovery {
		return
	}

	cleared := 0
	for _, slot := range advancedSlotRefs(m.p) {
		model := strings.TrimSpace(*slot.ptr)
		if model == "" || stringInSlice(model, m.live().modelPool) {
			continue
		}
		*slot.ptr = ""
		delete(m.live().oneMSlots, slot.key)
		cleared++
	}
	if cleared > 0 {
		setDebugf("applyStaleSlotPolicy cleared=%d slots=%s one_m=%s", cleared, slotDebugSummary(*m.p), reviewOneMSummary(m.live().oneMSlots))
	}
}

// 实时获取/检测协议名称
func (m *AdvancedConfigModel) getProtocol() string {
	if m.p.Type != "" {
		if provider.IsCommandCodeType(m.p.Type) {
			return "Command Code"
		}
		return provider.ProtocolLabelForProvider(*m.p)
	}
	if strings.Contains(strings.ToLower(m.urlText.Get()), "anthropic") {
		return "anthropic"
	}
	return "openai(chat)"
}

func (m *AdvancedConfigModel) getProtocolFamily() string {
	if m.p != nil {
		switch {
		case provider.IsAnthropicType(m.p.Type):
			return "Anthropic"
		case provider.IsCommandCodeType(m.p.Type):
			return "Command Code"
		case provider.IsOpenAICompatibleType(m.p.Type):
			return "OpenAI"
		}
	}
	if strings.Contains(strings.ToLower(m.urlText.Get()), "anthropic") {
		return "Anthropic"
	}
	return "OpenAI"
}

// canSelectCustomProtocol is true only for a manual Custom API-key provider
// whose data plane can be selected by the user. OAuth runtimes, models.dev, and
// Command Code have fixed or per-model routing and remain read-only.
func (m *AdvancedConfigModel) canSelectCustomProtocol() bool {
	if m.p == nil || m.source != sourceCustom || strings.TrimSpace(m.p.OAuthProvider) != "" {
		return false
	}
	return provider.IsOpenAICompatibleType(m.p.Type) || provider.IsAnthropicType(m.p.Type)
}

func customProtocolLabel(providerType string) string {
	switch {
	case provider.IsOpenAIResponsesType(providerType):
		return "Responses"
	case provider.IsAnthropicType(providerType):
		return "Anthropic"
	default:
		return "Chat"
	}
}

// cycleCustomProtocol moves a manual gateway through Chat, Responses, and native
// Anthropic Messages. It reinterprets the last verified raw endpoint for the
// selected protocol without changing the text the user entered.
func (m *AdvancedConfigModel) cycleCustomProtocol(delta int) {
	if delta == 0 || !m.canSelectCustomProtocol() {
		return
	}
	protocols := []string{"openai", "openai_responses", "anthropic"}
	index := 0
	for i, protocolType := range protocols {
		if (provider.IsOpenAIResponsesType(m.p.Type) && protocolType == "openai_responses") ||
			(provider.IsAnthropicType(m.p.Type) && protocolType == "anthropic") ||
			(!provider.IsOpenAIResponsesType(m.p.Type) && !provider.IsAnthropicType(m.p.Type) && protocolType == "openai") {
			index = i
			break
		}
	}
	if provider.IsAnthropicType(m.p.Type) {
		m.live().anthropicAuth = m.p.AnthropicAuth
	}
	index = (index + delta) % len(protocols)
	if index < 0 {
		index += len(protocols)
	}
	m.p.Type = protocols[index]
	if provider.IsAnthropicType(m.p.Type) {
		m.p.AnthropicAuth = m.live().anthropicAuth
	} else {
		m.p.AnthropicAuth = ""
	}

	rawEndpoint := strings.TrimSpace(m.live().detectedInputEndpoint)
	if rawEndpoint == "" {
		rawEndpoint = strings.TrimSpace(m.urlText.Get())
	}
	if rawEndpoint != "" {
		if provider.IsAnthropicType(m.p.Type) {
			m.p.Endpoint = protocol.NormalizeAnthropicBaseURLForClaude(rawEndpoint)
		} else {
			m.p.Endpoint = normalizeModelBaseURL(rawEndpoint)
		}
		m.live().probeEndpoint = m.p.Endpoint
	}
	setDebugf("protocol selected type=%q label=%q anthropic_auth=%q endpoint=%q", m.p.Type, customProtocolLabel(m.p.Type), m.p.AnthropicAuth, m.p.Endpoint)
}

// Single-page cursor model.
//
// Color + control language on this page:
//
//	cyan/green  = read-only facts (endpoint, auth, model mapping)
//	purple      = editable values, always wrapped as ‹ value ›
//	blue        = current focus / primary action
//	yellow      = [1M] badges
//
// oneMSlotBlocked reports that the backend advertises a window well below 1M for
// this slot's model, so the 1M variant must not be offered: it would only make
// Claude Code size the session (and its compaction) for a window the backend
// does not have, and the request is rejected before compaction runs.
//
// Unknown windows stay editable — the catalog is advisory and often absent.
func (m *AdvancedConfigModel) oneMSlotBlocked(modelVal string) bool {
	window, ok := m.advertisedWindow(modelVal)
	return ok && !protocol.ContextWindowSuggests1M(window)
}

// advertisedWindow looks up the window the backend reports for a slot model.
// The catalog is keyed by lowercased model id, so gateways serving mixed-case ids
// (GLM-4.6, Qwen3-Coder) must not fall through this check.
func (m *AdvancedConfigModel) advertisedWindow(modelVal string) (int, bool) {
	window, ok := m.live().modelContextWindows[strings.ToLower(stripOneMSuffix(modelVal))]
	if !ok || window <= 0 {
		return 0, false
	}
	return window, true
}

// Runtime option cycles. Index 0 is always "Default" (delete managed env).
var (
	reviewToolsOptions  = []string{"", "1", "2", "3", "4", "6", "8"}
	reviewSearchOptions = []string{"", "true", "false"} // Default / On / Off
)

func ensureProviderEnvMap(p *provider.Provider) {
	if p.Env == nil {
		p.Env = make(map[string]string)
	}
}

func deleteProviderEnvKey(p *provider.Provider, key string) {
	if p.Env == nil {
		return
	}
	delete(p.Env, key)
	if len(p.Env) == 0 {
		p.Env = nil
	}
}

func setProviderEnvValue(p *provider.Provider, key, value string) {
	if value == "" {
		deleteProviderEnvKey(p, key)
		return
	}
	ensureProviderEnvMap(p)
	p.Env[key] = value
}

func cycleStringOption(current string, options []string, delta int) string {
	idx := 0
	for i, opt := range options {
		if opt == current {
			idx = i
			break
		}
	}
	n := len(options)
	idx = (idx + delta) % n
	if idx < 0 {
		idx += n
	}
	return options[idx]
}

func (m *AdvancedConfigModel) reviewToolsValue() string {
	if m.p.Env == nil {
		return ""
	}
	v := strings.TrimSpace(m.p.Env[claude.ToolUseConcurrencyEnv])
	return v
}

func (m *AdvancedConfigModel) reviewSearchValue() string {
	if m.p.Env == nil {
		return ""
	}
	v := strings.ToLower(strings.TrimSpace(m.p.Env[claude.ToolSearchEnv]))
	switch v {
	case "true", "1", "on", "yes":
		return "true"
	case "false", "0", "off", "no":
		return "false"
	default:
		return v
	}
}

func formatEditableValue(label string, isDefault bool) string {
	if isDefault {
		return "‹ Default · " + label + " ›"
	}
	return "‹ " + label + " ›"
}

func formatToolsLabel(value string) string {
	if value == "" {
		return formatEditableValue("3", true)
	}
	return formatEditableValue(value, false)
}

func formatSearchLabel(value string) string {
	switch value {
	case "":
		return formatEditableValue("Off", true)
	case "true":
		return formatEditableValue("On", false)
	case "false":
		return formatEditableValue("Off", false)
	default:
		return formatEditableValue(value, false)
	}
}

func formatFastLabel(on bool) string {
	if on {
		return formatEditableValue("On", false)
	}
	return formatEditableValue("Off", false)
}

func (m *AdvancedConfigModel) adjustReviewField(delta int) {
	// The Source stepper must cycle before a connection is established: it is how
	// the user picks Custom vs models.dev in the first place. Gating it on
	// connectionReady would lock a fresh provider out of the models.dev path.
	if m.currentRow() == rowSource {
		m.switchSource(m.otherSource())
		return
	}
	if !m.connectionReady() {
		return
	}
	switch m.currentRow() {
	case rowContext:
		m.cycleCompactPreset(delta)
	case rowProtocol:
		if !m.usesModelsDev() {
			m.cycleCustomProtocol(delta)
		}
	case rowFast:
		// Toggle like Protocol; left/right/enter all flip the pin.
		m.p.FastMode = !m.p.FastMode
		setDebugf("fast toggled fast_mode=%t", m.p.FastMode)
	case rowTools:
		cur := m.reviewToolsValue()
		known := false
		for _, opt := range reviewToolsOptions {
			if opt == cur {
				known = true
				break
			}
		}
		if !known {
			cur = ""
		}
		next := cycleStringOption(cur, reviewToolsOptions, delta)
		setProviderEnvValue(m.p, claude.ToolUseConcurrencyEnv, next)
	case rowToolSearch:
		cur := m.reviewSearchValue()
		known := false
		for _, opt := range reviewSearchOptions {
			if opt == cur {
				known = true
				break
			}
		}
		if !known {
			cur = ""
		}
		next := cycleStringOption(cur, reviewSearchOptions, delta)
		setProviderEnvValue(m.p, claude.ToolSearchEnv, next)
	case rowActive:
		if delta < 0 {
			m.IsActiveChosen = true
		} else {
			m.IsActiveChosen = false
		}
	}
}

// cycleCompactPreset moves the provider-wide compact budget through Default,
// Balanced 500K, and Balanced 800K. Per-slot [1m] markers remain independent.
func (m *AdvancedConfigModel) cycleCompactPreset(delta int) {
	if delta == 0 {
		return
	}
	presets := []compactPreset{
		compactPresetDefault,
		compactPresetBalanced500K,
		compactPresetBalanced800K,
	}
	index := 0
	for i, preset := range presets {
		if m.live().compactPreset == preset {
			index = i
			break
		}
	}
	index = (index + delta) % len(presets)
	if index < 0 {
		index += len(presets)
	}
	m.live().compactPreset = presets[index]
	setDebugf("cycle compact preset delta=%d preset=%v summary=%s", delta, m.live().compactPreset, m.compactSummary())
}

// syncOneMForSameModels prompts-free: when a slot toggles [1m], apply the same
// marker to every other configured slot that maps to the identical base model.
// Claude Code reads [1m] per model env var, so Sonnet does not inherit from Custom.
func (m *AdvancedConfigModel) syncOneMForSameModels(sourceSlot string, enabled bool) int {
	var sourceModel string
	for _, slot := range advancedSlotRefs(m.p) {
		if slot.key == sourceSlot {
			sourceModel = strings.ToLower(stripOneMSuffix(*slot.ptr))
			break
		}
	}
	if sourceModel == "" {
		return 0
	}
	synced := 0
	for _, slot := range advancedSlotRefs(m.p) {
		if slot.key == sourceSlot {
			continue
		}
		base := strings.ToLower(stripOneMSuffix(*slot.ptr))
		if base == "" || base != sourceModel {
			continue
		}
		if m.live().oneMSlots[slot.key] != enabled {
			m.live().oneMSlots[slot.key] = enabled
			synced++
		}
	}
	return synced
}

func (m *AdvancedConfigModel) compactSummary() string {
	return compactPresetLabel(m.live().compactPreset)
}

func reviewOneMSummary(oneMSlots map[string]bool) string {
	var slots []string
	for _, slot := range []string{"opus", "sonnet", "haiku", "custom", "subagent"} {
		if oneMSlots[slot] {
			slots = append(slots, slot)
		}
	}
	if len(slots) == 0 {
		return "off"
	}
	return strings.Join(slots, ",")
}

func (m *AdvancedConfigModel) applyModelDetectionResult(detectedType, discoveredModelsRaw, anthropicAuth, detectedEndpoint string, derr error) {
	discoveredModels := uniqueModels(parseModelList(discoveredModelsRaw))
	m.live().hadLocalModelPool = countCSV(m.p.Model) > 0
	// Capture the raw detection inputs before probeEndpoint is normalized below,
	// so the successful-detection baseline is what the user actually typed.
	detectedInputEndpoint := m.live().probeEndpoint
	detectedInputKey := m.live().probeAPIKey
	setDebugf(
		"applyModelDetectionResult start detected_type=%q detected_endpoint=%q anthropic_auth=%q discovered_model_count=%d existing_model_count=%d err=%v",
		detectedType,
		detectedEndpoint,
		anthropicAuth,
		len(discoveredModels),
		countCSV(m.p.Model),
		derr,
	)
	if !m.usesOAuth() && detectedEndpoint != "" {
		m.p.Endpoint = detectedEndpoint
		m.live().probeEndpoint = detectedEndpoint
	}
	if !m.usesOAuth() && detectedType != "" {
		// The probe can only tell apart Anthropic / Command Code / the OpenAI
		// family — it cannot distinguish OpenAI's Chat Completions from its
		// Responses API (both are reached through the same GET /v1/models list).
		// Reopening an existing openai_responses provider must not let the
		// auto-probe downgrade its stored Type back to "openai" (chat); preserve
		// the user's Responses choice when the probe still lands in the OpenAI
		// family, and only overwrite the Type when the probe family disagrees.
		if !(provider.IsOpenAIResponsesType(m.p.Type) && provider.IsOpenAICompatibleType(detectedType)) {
			m.p.Type = detectedType
			m.p.AnthropicAuth = ""
		}
	}
	if !m.usesOAuth() && detectedType == "anthropic" {
		m.p.Endpoint = protocol.NormalizeAnthropicBaseURLForClaude(m.p.Endpoint)
		if anthropicAuth != "" {
			m.p.AnthropicAuth = anthropicAuth
		}
		m.live().anthropicAuth = m.p.AnthropicAuth
	} else if !m.usesOAuth() && detectedType != "" {
		m.live().anthropicAuth = ""
	}

	m.live().modelPool = []string{}
	m.live().modelPoolFromDiscovery = false
	m.modelAvailability = make(map[string]modelAvailability)
	m.modelTesting = false
	m.modelTestCancel = nil
	m.modelTestCanceled = false
	if derr == nil && len(discoveredModels) > 0 {
		m.live().modelPool = discoveredModels
		m.live().modelPoolFromDiscovery = true
		m.p.Model = strings.Join(discoveredModels, ",")
		setDebugf("applyModelDetectionResult using discovered model pool count=%d", len(m.live().modelPool))
	}

	if derr != nil {
		m.live().detectionError = derr
		m.cursor = m.mainRowIndex(rowTest)
		setDebugf("applyModelDetectionResult detection failed detection_error=%v model_count=%d", m.live().detectionError, len(m.live().modelPool))
		return
	}

	// 本次 set 必须以接口返回的模型为准；不再用旧的本地模型池兜底。
	if len(m.live().modelPool) == 0 {
		m.live().detectionError = fmt.Errorf("%s", locale.T(
			"未从接口获取到任何可用模型，未使用本地旧模型池",
			"no models were fetched from the provider API; local cached models were not used",
		))
		m.cursor = m.mainRowIndex(rowTest)
		setDebugf("applyModelDetectionResult no models detection_error=%v", m.live().detectionError)
		return
	}

	sort.Strings(m.live().modelPool)
	// Single page: detection success auto-configures the slots and stays on the
	// page. Connection is now the detected endpoint, so it is no longer dirty.
	m.live().connectionDirty = false
	// Record the inputs this successful detection ran against (the raw values
	// before endpoint normalization) as the save baseline. Reverting an edit or
	// cancelling a re-detection naturally returns the page to this state.
	m.live().detectedInputEndpoint = detectedInputEndpoint
	m.live().detectedInputKey = detectedInputKey
	m.applyRecommendation()
	m.cursor = m.mainRowIndex(rowOpus)
	setDebugf(
		"applyModelDetectionResult success provider_type=%q endpoint=%q anthropic_auth=%q model_count=%d stale_slot_count=%d clear_stale_slots=%t cursor=%d",
		m.p.Type,
		m.p.Endpoint,
		m.p.AnthropicAuth,
		len(m.live().modelPool),
		m.staleSlotCount(),
		m.live().clearStaleSlots,
		m.cursor,
	)
}

// applyRecommendation fills empty slots from the auto recommendation engine and
// records that the config was auto-configured. User-edited fields (identified by
// not matching the recommendation) are left alone.
func (m *AdvancedConfigModel) applyRecommendation() {
	rec := RecommendModels(*m.p, m.live().modelPool, m.live().modelDisplayMetadata)
	// Only fill slots the user has not already pinned in this session. Detecting
	// again must not overwrite a manual choice made after the first detection.
	if strings.TrimSpace(m.p.OpusModel) == "" {
		m.p.OpusModel = rec.Opus
	}
	if strings.TrimSpace(m.p.SonnetModel) == "" {
		m.p.SonnetModel = rec.Sonnet
	}
	if strings.TrimSpace(m.p.HaikuModel) == "" {
		m.p.HaikuModel = rec.Haiku
	}
	if strings.TrimSpace(m.p.CustomModelID) == "" {
		m.p.CustomModelID = rec.Custom
	}
	if strings.TrimSpace(m.p.SubagentModel) == "" {
		m.p.SubagentModel = rec.Subagent
	}
	for key, on := range rec.OneMSlots {
		if m.live().oneMSlots[key] == on {
			continue
		}
		m.live().oneMSlots[key] = on
	}
	m.live().autoConfigured = true
	m.live().detectionError = nil
	m.live().connectionDirty = false
	m.live().hadLocalModelPool = countCSV(m.p.Model) > 0
	setDebugf("applyRecommendation slots=%s one_m=%s", slotDebugSummary(*m.p), reviewOneMSummary(m.live().oneMSlots))
}

// activateRow fires the action for a button row on click or Enter: Auto
// Configure starts the connection check, Test Model Availability starts the
// per-model probes. Async work is delivered through the model's channels and
// consumed on the main loop by the Watchers below.
func (m *AdvancedConfigModel) activateRow(kind configRowKind) {
	switch kind {
	case rowProvider:
		m.openModelsDevPicker()
		fetchModelsDevAsync(m.mdDone)
	case rowSource:
		m.switchSource(m.otherSource())
	case rowTest:
		// models.dev providers are pre-configured from metadata: the endpoint,
		// model pool, and per-model protocol table are already populated. The one
		// thing metadata cannot prove is the API key, so this row sends a real
		// authenticated inference request instead of re-running full detection —
		// which would overwrite Type ("modelsdev") and drop routing.
		if m.usesModelsDev() {
			key := strings.TrimSpace(m.keyText.Get())
			if key == "" {
				return
			}
			model, proto, ok := m.firstRoutableModel()
			if !ok {
				// No model with a known protocol means the provider has no usable
				// model pool (its AI SDK package is unrecognized), so there is nothing
				// to verify a key against. Report that instead of claiming a connection.
				m.live().probeEndpoint = m.p.Endpoint
				m.live().probeAPIKey = key
				m.live().detectionError = fmt.Errorf("%s", locale.T(
					"该 Provider 没有可用模型（AI SDK 包暂不支持），无法验证 key",
					"this provider has no usable models (unsupported AI SDK package); cannot verify the key",
				))
				m.live().keyVerified = false
				setDebugf("models.dev key verify aborted: no routable model endpoint=%q", m.p.Endpoint)
				return
			}
			m.live().probeEndpoint = m.p.Endpoint
			m.live().probeAPIKey = key
			m.live().detectionError = nil
			m.live().keyVerified = false
			m.live().detecting = true
			m.live().detectProgress = 5
			m.live().detectFrame = 0
			m.keyFocused = false
			setDebugf("start models.dev key verify endpoint=%q api_key_len=%d model=%q proto=%q", m.live().probeEndpoint, len(key), model, proto)
			keyVerifyAsync(m.verifyDone, m.live().probeEndpoint, key, model, proto)
			return
		}
		// Start detection with the current input values (OAuth uses the session
		// runtime endpoint/key already injected by configureOAuthRuntime).
		if !m.usesOAuth() {
			m.p.Endpoint = m.urlText.Get()
			m.p.APIKey = m.keyText.Get()
			m.live().probeEndpoint = m.p.Endpoint
			m.live().probeAPIKey = m.p.APIKey
			// The inputs being detected become the new baseline only if the
			// detection succeeds; until then keep the dirty state honest.
			m.refreshConnectionDirty()
		}
		m.urlFocused = false
		m.keyFocused = false
		m.live().detectionError = nil
		m.live().detecting = true
		m.live().detectProgress = 5
		m.live().detectFrame = 0
		setDebugf("start detection endpoint=%q api_key_len=%d oauth=%t", m.live().probeEndpoint, len(m.live().probeAPIKey), m.usesOAuth())
		fetchModelsAsync(m.fetchDone, m.live().probeEndpoint, m.live().probeAPIKey)
	case rowTestModels:
		if !m.connectionReady() {
			return
		}
		if len(m.live().modelPool) == 0 {
			setDebugf("model availability test skipped: empty pool")
			return
		}
		m.modelTestID++
		testID := m.modelTestID
		ctx, cancel := context.WithCancel(context.Background())
		m.modelTesting = true
		m.modelTestCancel = cancel
		m.modelTestFrame = 0
		m.modelTestCanceled = false
		setDebugf("model availability test started model_count=%d", len(m.live().modelPool))
		testModelsAsync(m.availDone, ctx, testID, m.live().modelPool, m.live().probeEndpoint, m.live().probeAPIKey, m.p.Type, m.p.AnthropicAuth, m.p.ModelProtocols, m.availabilitySmokeTestModel())
	}
}

func (m *AdvancedConfigModel) markDirty() {
	if m.app != nil {
		m.app.MarkDirty()
	}
}

func (m *AdvancedConfigModel) quit() {
	m.quitRequested = true
	if m.app != nil {
		m.app.Stop()
	}
}

// handleFocusRow carries out the click semantics of a configuration row: the
// first click selects it (moves the cursor); a second click on the
// already-selected row performs its action. Endpoint and API Key focus their
// text inputs on first click so typing lands there.
func (m *AdvancedConfigModel) handleFocusRow(row configRowKind) {
	if row == rowCopyKey || row == rowCopyURL {
		// A single click on a value row focuses its input; a double-click
		// (second click on the same row within the window) copies the value.
		now := time.Now()
		double := row == m.lastCopyClickRow && now.Sub(m.lastCopyClickAt) < 500*time.Millisecond
		m.lastCopyClickRow = row
		m.lastCopyClickAt = now
		if !double {
			focus := rowAPIKey
			if row == rowCopyURL {
				focus = rowEndpoint
			}
			m.cursor = m.mainRowIndex(focus)
			m.urlFocused = false
			m.keyFocused = false
			m.markDirty()
			return
		}
		if row == rowCopyKey {
			m.keyCopied = true
			m.lastKeyCopyAt = now
			if err := clipboard.WriteAll(m.keyText.Get()); err != nil {
				setDebugf("copy key to clipboard failed: %v", err)
			}
			setDebugf("key copied to clipboard")
			m.markDirty()
			return
		}
		m.urlCopied = true
		m.lastUrlCopyAt = now
		if err := clipboard.WriteAll(m.urlText.Get()); err != nil {
			setDebugf("copy url to clipboard failed: %v", err)
		}
		setDebugf("url copied to clipboard")
		m.markDirty()
		return
	}
	idx := m.mainRowIndex(row)
	if idx < 0 {
		return
	}
	alreadySelected := m.cursor == idx && !m.textInputHasKeyboard()
	m.cursor = idx
	m.keepCursorVisible()
	m.urlFocused = false
	m.keyFocused = false
	switch row {
	case rowEndpoint:
		m.urlFocused = true
		m.refreshConnectionDirty()
	case rowAPIKey:
		m.keyFocused = true
		m.refreshConnectionDirty()
	case rowTest, rowTestModels, rowProvider, rowSource:
		if alreadySelected {
			m.activateRow(row)
		}
	case rowSave:
		if alreadySelected && m.requestSave() {
			m.quit()
		}
	case rowCancel:
		if alreadySelected {
			setDebugf("click cancel requested")
			m.quit()
		}
	default:
		m.filterFocused = false
	}
	m.markDirty()
}

// handleFetchDone applies a completed connection check. Late results from a
// superseded probe are dropped: the model only accepts the result when the
// probe endpoint/key still match the live inputs.
func (m *AdvancedConfigModel) handleFetchDone(msg modelFetchDoneMsg) {
	if !m.live().detecting || msg.endpoint != m.live().probeEndpoint || msg.apiKey != m.live().probeAPIKey {
		setDebugf(
			"modelFetchDone ignored detecting=%t endpoint_match=%t api_key_match=%t msg_endpoint=%q probe_endpoint=%q",
			m.live().detecting,
			msg.endpoint == m.live().probeEndpoint,
			msg.apiKey == m.live().probeAPIKey,
			msg.endpoint,
			m.live().probeEndpoint,
		)
		return
	}
	m.live().detectProgress = 100
	m.live().detecting = false
	setDebugf(
		"modelFetchDone accepted detected_type=%q detected_endpoint=%q anthropic_auth=%q model_count=%d err=%v",
		msg.detectedType,
		msg.detectedEndpoint,
		msg.anthropicAuth,
		countCSV(msg.discoveredModelsRaw),
		msg.err,
	)
	if msg.contextWindows != nil {
		m.live().modelContextWindows = msg.contextWindows
	}
	m.live().modelDisplayMetadata = indexModelInfos(msg.modelInfos)
	for id, window := range contextWindowsFromModelInfos(m.live().modelDisplayMetadata) {
		if _, exists := m.live().modelContextWindows[id]; !exists {
			m.live().modelContextWindows[id] = window
		}
	}
	m.applyModelDetectionResult(msg.detectedType, msg.discoveredModelsRaw, msg.anthropicAuth, msg.detectedEndpoint, msg.err)
	m.markDirty()
}

// handleVerifyDone applies a models.dev key verification result, guarded the
// same way as handleFetchDone.
func (m *AdvancedConfigModel) handleVerifyDone(msg keyVerifyDoneMsg) {
	if !m.usesModelsDev() || !m.live().detecting || msg.endpoint != m.live().probeEndpoint || msg.apiKey != m.live().probeAPIKey {
		setDebugf(
			"keyVerifyDone ignored modelsdev=%t detecting=%t endpoint_match=%t api_key_match=%t",
			m.usesModelsDev(),
			m.live().detecting,
			msg.endpoint == m.live().probeEndpoint,
			msg.apiKey == m.live().probeAPIKey,
		)
		return
	}
	m.live().detectProgress = 100
	m.live().detecting = false
	m.live().detectionError = msg.err
	m.live().keyVerified = msg.err == nil
	setDebugf("keyVerifyDone verified=%t err=%v", m.live().keyVerified, msg.err)
	m.markDirty()
}

func (m *AdvancedConfigModel) handleModelsDevDone(msg modelsDevFetchDoneMsg) {
	if !m.modelsDevPicker {
		return
	}
	m.modelsDevLoading = false
	m.modelsDevError = msg.err
	if msg.err == nil {
		m.modelsDevItems = msg.providers
		m.updateModelsDevFilter()
	}
	m.markDirty()
}

// handleAvailabilityDone applies a finished model availability test. A result
// whose testID no longer matches the current run is dropped.
func (m *AdvancedConfigModel) handleAvailabilityDone(msg modelAvailabilityDoneMsg) {
	if !m.modelTesting || msg.testID != m.modelTestID {
		return
	}
	m.modelTesting = false
	m.modelTestCancel = nil
	m.modelTestCanceled = false
	m.modelAvailability = msg.statuses
	m.live().modelPool = reorderModelsByAvailability(m.live().modelPool, m.modelAvailability)
	m.p.Model = strings.Join(m.live().modelPool, ",")
	m.updateFilteredPool()
	available, unavailable := m.availabilityCounts()
	setDebugf("model availability test finished model_count=%d available=%d unavailable=%d", len(m.live().modelPool), available, unavailable)
	m.markDirty()
}

// handleFetchTick advances the connection-check spinner frames while a probe
// is in flight, and clears the copied-value hints two seconds after a copy.
// A single timer watcher drives both cadences.
func (m *AdvancedConfigModel) handleFetchTick() {
	now := time.Now()
	if m.keyCopied && now.Sub(m.lastKeyCopyAt) >= 2*time.Second {
		m.keyCopied = false
	}
	if m.urlCopied && now.Sub(m.lastUrlCopyAt) >= 2*time.Second {
		m.urlCopied = false
	}
	if !m.live().detecting {
		return
	}
	m.live().detectFrame++
	if m.live().detectProgress < 95 {
		m.live().detectProgress += 3
		if m.live().detectProgress > 95 {
			m.live().detectProgress = 95
		}
	}
	m.markDirty()
}

// handleAvailabilityTick advances the model-test spinner frames while the
// current test run is still in flight.
func (m *AdvancedConfigModel) handleAvailabilityTick() {
	if !m.modelTesting {
		return
	}
	m.modelTestFrame++
	m.markDirty()
}

// handleKey routes one key press. The models.dev picker and the two modal
// states (connection check, model test) own the keyboard while active; after
// that the slot picker filter and the endpoint/key inputs take printable
// keys, and the remaining keys move the page cursor.
func (m *AdvancedConfigModel) handleKey(ke tui.KeyEvent) {
	if m.modelsDevPicker {
		m.handleModelsDevPickerKey(ke)
		m.markDirty()
		return
	}

	if ke.Mod == tui.ModCtrl && ke.Rune == 'c' {
		m.quit()
		return
	}

	// q 是 quit 的单字母别名，仅当没有文本输入框持有键盘时生效（q 也是
	// 合法的输入字符，见下方的 runeAlias 说明）。
	if ke.IsRune() && ke.Mod == 0 && ke.Rune == 'q' && !m.textInputHasKeyboard() {
		m.quit()
		return
	}

	// 模态：连接检查/模型测试进行中。esc 取消操作（或退出等待），其余按键
	// 等待结束。ctrl+c 已在上面处理。
	if m.live().detecting {
		if ke.Key == tui.KeyEscape {
			m.live().detecting = false
			m.live().detectionError = fmt.Errorf("%s", locale.T("已取消连接检查", "connection check canceled"))
			m.cursor = m.mainRowIndex(rowTest)
			setDebugf("connection check canceled by user")
			m.markDirty()
		}
		return
	}
	if m.modelTesting {
		if ke.Key == tui.KeyEscape {
			if m.modelTestCancel != nil {
				m.modelTestCancel()
			}
			m.modelTesting = false
			m.modelTestCancel = nil
			m.modelTestCanceled = true
			setDebugf("model availability test canceled test_id=%d", m.modelTestID)
			m.markDirty()
		}
		return
	}

	// 文本输入框拥有键盘时，单字母导航别名让位给输入本身：q/h/j/k/l 是合法
	// 的输入字符。方向键没有这个歧义。
	runeAlias := ke.IsRune() && ke.Mod == 0 && strings.ContainsRune("q hjkl", ke.Rune) && ke.Rune != ' '
	inputHasKeyboard := m.textInputHasKeyboard()

	// vim 单字母别名 h/j/k/l：无输入焦点时映射到方向键。
	if m.handleNavAlias(ke, inputHasKeyboard) {
		return
	}

	switch ke.Key {
	case tui.KeyEscape:
		if m.filterFocused {
			m.filterFocused = false
			setDebugf("esc closed slot picker active_slot=%d cursor=%d", m.activeSlot, m.cursor)
			m.markDirty()
			return
		}
		setDebugf("esc quit cursor=%d endpoint_set=%t api_key_len=%d", m.cursor, strings.TrimSpace(m.urlText.Get()) != "", len(m.keyText.Get()))
		m.quit()
		return

	case tui.KeyUp:
		if inputHasKeyboard && ke.IsRune() {
			return // navigation alias yielded to text input
		}
		if m.filterFocused {
			if m.slotListCursor > 0 {
				m.slotListCursor--
				if m.slotListCursor < m.filterWindowStart {
					m.filterWindowStart = m.slotListCursor
				}
			}
			m.markDirty()
			return
		}
		rows := m.visibleRows()
		if len(rows) == 0 {
			return
		}
		if m.cursor > 0 {
			m.cursor--
		} else {
			m.cursor = len(rows) - 1
		}
		m.keepCursorVisible()
		m.markDirty()
		return

	case tui.KeyDown:
		if inputHasKeyboard && ke.IsRune() {
			return
		}
		if m.filterFocused {
			if m.slotListCursor < len(m.filteredPool)-1 {
				m.slotListCursor++
				if m.slotListCursor >= m.filterWindowStart+filterViewHeight {
					m.filterWindowStart = m.slotListCursor - filterViewHeight + 1
				}
			}
			m.markDirty()
			return
		}
		rows := m.visibleRows()
		if len(rows) == 0 {
			return
		}
		if m.cursor < len(rows)-1 {
			m.cursor++
		} else {
			m.cursor = 0
		}
		m.keepCursorVisible()
		m.markDirty()
		return

	case tui.KeyLeft:
		if inputHasKeyboard && ke.IsRune() {
			return
		}
		if m.filterFocused {
			return
		}
		if m.isModelRow(m.currentRow()) {
			m.toggleOneMAtRow(m.currentRow())
		} else {
			switch m.currentRow() {
			case rowSource, rowContext, rowProtocol, rowFast, rowTools, rowToolSearch, rowActive:
				m.adjustReviewField(-1)
			}
		}
		m.markDirty()
		return

	case tui.KeyRight:
		if inputHasKeyboard && ke.IsRune() {
			return
		}
		if m.filterFocused {
			return
		}
		if m.isModelRow(m.currentRow()) {
			m.toggleOneMAtRow(m.currentRow())
		} else {
			switch m.currentRow() {
			case rowSource, rowContext, rowProtocol, rowFast, rowTools, rowToolSearch, rowActive:
				m.adjustReviewField(1)
			}
		}
		m.markDirty()
		return

	case tui.KeyEnter:
		// The API key textarea inserts newlines with Enter, so while it is
		// focused the key must fall through to the text routing below rather
		// than advance the page cursor.
		if !m.keyFocused {
			m.handleEnter()
			m.markDirty()
			return
		}

	case tui.KeyTab:
		if inputHasKeyboard && ke.IsRune() {
			return
		}
		if m.filterFocused {
			if m.slotListCursor < len(m.filteredPool)-1 {
				m.slotListCursor++
				if m.slotListCursor >= m.filterWindowStart+filterViewHeight {
					m.filterWindowStart = m.slotListCursor - filterViewHeight + 1
				}
			}
			m.markDirty()
			return
		}
		rows := m.visibleRows()
		if len(rows) == 0 {
			return
		}
		m.cursor = (m.cursor + 1) % len(rows)
		m.keepCursorVisible()
		m.markDirty()
		return
	}

	// shift+tab：主页面反向移动光标（filter 打开时不响应）。
	if ke.Key == tui.KeyTab && ke.Mod == tui.ModShift {
		if m.filterFocused || inputHasKeyboard && ke.IsRune() {
			return
		}
		rows := m.visibleRows()
		if len(rows) == 0 {
			return
		}
		m.cursor--
		if m.cursor < 0 {
			m.cursor = len(rows) - 1
		}
		m.keepCursorVisible()
		m.markDirty()
		return
	}

	// space：模型行上切换 1M 标记（输入框聚焦时作为空格字符输入）。
	if ke.Key == tui.KeyRune && ke.Rune == ' ' && ke.Mod == 0 {
		if m.filterFocused || inputHasKeyboard && runeAlias {
			// falls through to text routing below
		} else {
			m.toggleOneMAtRow(m.currentRow())
			m.markDirty()
			return
		}
	}

	// 文本输入路由：与旧实现的输入框焦点同步规则一致——光标在 Endpoint/
	// API Key 行或 filter 聚焦时，可打印字符进入对应文本。
	if ke.IsRune() && ke.Mod == 0 && ke.Rune != 0 {
		ch := string(ke.Rune)
		switch {
		case m.filterFocused:
			m.filterText.Set(m.filterText.Get() + ch)
			m.updateFilteredPool()
		case m.currentRow() == rowEndpoint && !m.usesOAuth():
			m.urlFocused = true
			m.keyFocused = false
			m.urlText.Set(m.urlText.Get() + ch)
			m.refreshConnectionDirty()
		case m.currentRow() == rowAPIKey && !m.usesOAuth():
			m.urlFocused = false
			m.keyFocused = true
			m.keyText.Set(m.keyText.Get() + ch)
			m.refreshConnectionDirty()
			if m.usesModelsDev() {
				m.invalidateModelsDevKeyIfChanged()
			}
		default:
			// 光标在按钮或只读行上时，取消两个输入框的焦点。
			if !inputHasKeyboard {
				m.urlFocused = false
				m.keyFocused = false
			}
		}
		m.markDirty()
		return
	}

	if ke.Key == tui.KeyBackspace {
		switch {
		case m.filterFocused:
			t := m.filterText.Get()
			if t != "" {
				m.filterText.Set(t[:len(t)-1])
			}
			m.updateFilteredPool()
		case m.currentRow() == rowEndpoint && !m.usesOAuth():
			t := m.urlText.Get()
			if t != "" {
				m.urlText.Set(t[:len(t)-1])
			}
			m.refreshConnectionDirty()
		case m.currentRow() == rowAPIKey && !m.usesOAuth():
			before := m.keyText.Get()
			t := before
			if t != "" {
				m.keyText.Set(t[:len(t)-1])
			}
			m.refreshConnectionDirty()
			if m.usesModelsDev() && m.keyText.Get() != before {
				m.invalidateModelsDevKeyIfChanged()
			}
		}
		m.markDirty()
		return
	}

	// 其余按键（非打印导航在上方处理过）：确保输入框焦点不残留。
	if !inputHasKeyboard {
		m.urlFocused = false
		m.keyFocused = false
		m.markDirty()
	}
}

// handleNavAlias maps the vim single-letter navigation aliases (h/j/k/l) onto
// the same key events the arrow keys produce. It runs before the text-input
// routing but only when no text input owns the keyboard, so the aliases stay
// typeable inside the endpoint/API key/filter inputs.
func (m *AdvancedConfigModel) handleNavAlias(ke tui.KeyEvent, inputHasKeyboard bool) bool {
	if inputHasKeyboard || !ke.IsRune() || ke.Mod != 0 {
		return false
	}
	switch ke.Rune {
	case 'k':
		m.handleKey(tui.KeyEvent{Key: tui.KeyUp})
	case 'j':
		m.handleKey(tui.KeyEvent{Key: tui.KeyDown})
	case 'h':
		m.handleKey(tui.KeyEvent{Key: tui.KeyLeft})
	case 'l':
		m.handleKey(tui.KeyEvent{Key: tui.KeyRight})
	default:
		return false
	}
	return true
}

// handleEnter performs the Enter action of the row under the cursor. It runs
// only when no text input owns the keyboard (the key textarea inserts
// newlines instead).
func (m *AdvancedConfigModel) handleEnter() {
	if m.filterFocused {
		// Model picker selection.
		if len(m.filteredPool) == 0 {
			return
		}
		if m.slotListCursor < 0 || m.slotListCursor >= len(m.filteredPool) {
			m.slotListCursor = 0
		}
		selectedModel := m.filteredPool[m.slotListCursor]
		if selectedModel == locale.T("(设置为未设置/清空)", "(clear/unset)") || selectedModel == locale.T("(无匹配模型)", "(no match)") {
			selectedModel = ""
		}
		ptr := []*string{&m.p.OpusModel, &m.p.SonnetModel, &m.p.HaikuModel, &m.p.CustomModelID, &m.p.SubagentModel}[m.activeSlot]
		*ptr = selectedModel
		if m.activeSlot == 4 && m.p.Env != nil {
			delete(m.p.Env, claude.SubagentModelEnv)
		}
		// A slot whose model was just changed must not keep a [1m] marker
		// the backend rules out for the new model — toggleOneMAtRow refuses
		// to enable one there, so leaving an enabled marker would be
		// inconsistent and would send a non-1M model with the [1m] suffix.
		slotKey := []string{"opus", "sonnet", "haiku", "custom", "subagent"}[m.activeSlot]
		if m.live().oneMSlots[slotKey] && m.oneMSlotBlocked(selectedModel) {
			m.live().oneMSlots[slotKey] = false
			setDebugf("slot model changed to a non-1M model; cleared 1M marker slot=%s model=%q", slotKey, selectedModel)
		}
		m.filterFocused = false
		m.live().autoConfigured = false
		setDebugf("slot selected active_slot=%d model=%q slots=%s", m.activeSlot, selectedModel, slotDebugSummary(*m.p))
		return
	}

	switch m.currentRow() {
	case rowSource:
		m.switchSource(m.otherSource())
	case rowEndpoint:
		m.cursor = m.mainRowIndex(rowAPIKey)
		m.urlFocused = false
		m.keyFocused = true
		setDebugf("enter endpoint -> api key endpoint=%q", m.urlText.Get())
	case rowAPIKey:
		// Custom advances to Auto Configure; models.dev has no test step, so
		// move to the first model slot instead.
		if m.usesModelsDev() {
			m.cursor = m.mainRowIndex(rowOpus)
		} else {
			m.cursor = m.mainRowIndex(rowTest)
		}
		m.urlFocused = false
		m.keyFocused = false
		setDebugf("enter api key -> next api_key_len=%d", len(m.keyText.Get()))
	case rowProvider:
		m.activateRow(rowProvider)
	case rowTest:
		m.activateRow(rowTest)
	case rowProtocol, rowFast, rowTools, rowToolSearch:
		m.adjustReviewField(1)
	case rowOpus, rowSonnet, rowHaiku, rowCustom, rowSubagent:
		if !m.connectionReady() {
			return
		}
		m.activeSlot = slotForRow(m.currentRow())
		m.filterFocused = true
		m.filterText.Set("")
		m.slotListCursor = 0
		m.updateFilteredPool()
		setDebugf("open slot picker active_slot=%d filtered_count=%d", m.activeSlot, len(m.filteredPool))
	case rowTestModels:
		m.activateRow(rowTestModels)
	case rowContext:
		// Context & Compact is edited inline; nothing to open yet.
		setDebugf("context row selected")
	case rowActive:
		m.IsActiveChosen = !m.IsActiveChosen
		setDebugf("active choice toggled active_chosen=%t", m.IsActiveChosen)
	case rowSave:
		if m.requestSave() {
			m.quit()
		}
	case rowCancel:
		setDebugf("cancel requested")
		m.quit()
	}
	m.keepCursorVisible()
}

// invalidateModelsDevKeyIfChanged drops a stale key verification once the key
// text changes: keyVerified only proves the key that was verified at the
// time, and content changes void it. If a verify request is still in flight
// the probe baseline is resynced too, so a late keyVerifyDoneMsg cannot slip
// past the guard and mark the unverified new key as connected.
func (m *AdvancedConfigModel) invalidateModelsDevKeyIfChanged() {
	m.live().keyVerified = false
	m.live().detectionError = nil
	m.live().detecting = false
	m.live().probeAPIKey = m.keyText.Get()
}

// renderModelFetchProgress builds the connection-check in-progress block: a
// spinner frame, the label, and the hint line.
func renderModelFetchProgress(progress, frame int, oauth bool) []*tui.Element {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spin := spinners[frame%len(spinners)]
	label := locale.T("正在连接...", "Connecting...")
	if oauth {
		label = locale.T("正在通过 OAuth 连接...", "Connecting via OAuth...")
	}
	return []*tui.Element{
		plainLine(""),
		line(span(fmt.Sprintf("%s %s", spin, label), stSelected)),
		spanLine(locale.T("请稍候，正在验证连接", "Please wait while the connection is verified"), stGray),
	}
}

// credentialField renders one credential row: the label line and the value
// line(s). The API key can span multiple lines (its editor accepts Enter);
// each line is its own element so the mouse hit-test rows stay stable. A
// focused field swaps the label for the accent style and the "> " cursor
// prefix, matching the stepper rows below.
func credentialField(label, value string, focused bool) []*tui.Element {
	prefix := "  "
	labelStyle := stPurple
	valueStyle := stGray
	if focused {
		prefix = "> "
		labelStyle = stSelected
		valueStyle = stSelected
	}
	rows := []*tui.Element{
		line(span(prefix, labelStyle), span(label, labelStyle)),
	}
	for _, v := range strings.Split(value, "\n") {
		rows = append(rows, line(span("  ", tui.NewStyle()), span(v, valueStyle)))
	}
	rows = append(rows, plainLine(""))
	return rows
}

// renderPageHeader renders the page title row(s): title + badge (+ protocol
// family until a detection pins it) and a divider rule.
func (m *AdvancedConfigModel) renderPageHeader(title, badge string) []*tui.Element {
	// Leading spaces keep the lipgloss MarginLeft(1) look; go-tui styles have
	// no margin, so the separation lives in the span text itself.
	spans := []tui.TextSpan{span(title, stTitle), span(" "+badge, stBadge)}
	if !m.live().modelPoolFromDiscovery && !m.usesOAuth() {
		// Must be a third span, not a later AddChild: block children appended to
		// a flex Row mis-layout and the text overlaps (see CLAUDE.md go-tui notes).
		spans = append(spans, span(locale.T(" 协议: ", " Protocol: ")+m.getProtocolFamily(), stProtoBadge))
	}
	head := line(spans...)
	dividerWidth := max(m.panelWidth()-6, 16)
	return []*tui.Element{
		head,
		spanLine(strings.Repeat("─", dividerWidth), stDivider),
		plainLine(""),
	}
}

// truncateMiddle keeps endpoint/model names on one line for the review page.
// Width is measured in terminal cells (ANSI-aware, Unicode-aware) via
// tui.StringWidth.
func truncateMiddle(s string, max int) string {
	s = strings.TrimSpace(s)
	if max < 8 || tui.StringWidth(s) <= max {
		return s
	}
	runess := []rune(s)
	ellipsis := "…"
	budget := max - tui.StringWidth(ellipsis)
	if budget < 2 {
		return ellipsis
	}
	leftBudget := budget / 2
	rightBudget := budget - leftBudget

	var left string
	for _, r := range runess {
		cand := left + string(r)
		if tui.StringWidth(cand) > leftBudget {
			break
		}
		left = cand
	}
	var right string
	for i := len(runess) - 1; i >= 0; i-- {
		cand := string(runess[i]) + right
		if tui.StringWidth(cand) > rightBudget {
			break
		}
		right = cand
	}
	return left + ellipsis + right
}

// Render builds the whole page as a go-tui element tree. Three exits: the
// slot picker overlay (filter focused), the models.dev picker overlay, and
// the main configuration page. A nil app is tolerated so tests can render
// offline.
func (m *AdvancedConfigModel) Render(app *tui.App) *tui.Element {
	m.syncTerminalSize(app)
	// Model picker overlay: when the filter input has focus, render only the
	// filtered model list (search + availability) instead of the main page.
	if m.filterFocused {
		return m.viewModelPicker()
	}
	// models.dev picker overlay: render only the provider catalog instead of the
	// main page.
	if m.modelsDevPicker {
		return m.viewModelsDevPicker()
	}
	return m.viewMainPage()
}

// bodyRows builds the full page body (header through the action bar) as an
// ordered list of single-line elements. viewMainPage wraps it in the panel;
// keepCursorVisible reuses it to locate the cursor row's precise body line for
// scrolling. Section renderers are read-only — they only build elements.
func (m *AdvancedConfigModel) bodyRows() []*tui.Element {
	rows := m.renderPageHeader(locale.T("Provider 配置", "Provider Configuration"), m.pageBadge())
	rows = append(rows, m.viewConnectionSection()...)
	rows = append(rows, m.viewDetectionSection()...)
	rows = append(rows, m.viewMappingSection()...)
	rows = append(rows, m.viewRuntimeSection()...)
	rows = append(rows, m.viewActionSection()...)
	return rows
}

func (m *AdvancedConfigModel) viewMainPage() *tui.Element {
	return m.wrapMainPanel(m.bodyRows())
}

// pageBadge labels the header: the connection family this page edits.
func (m *AdvancedConfigModel) pageBadge() string {
	if m.usesOAuth() {
		return "OAuth"
	}
	return "Config"
}

// viewConnectionSection renders the Connection block: subscription metadata
// for OAuth providers, or the Source stepper plus endpoint/key/protocol rows
// for Custom and models.dev sources.
func (m *AdvancedConfigModel) viewConnectionSection() []*tui.Element {
	rows := []*tui.Element{m.connectionTitleRow()}
	if m.usesOAuth() {
		// OAuth rows are subscription metadata; the model-fetch action sits
		// directly under Auth like the non-OAuth detection buttons.
		return append(rows,
			kvRow("Provider", span(m.p.OAuthProvider, stCyan)),
			kvRow("Fast", span(providerFastSummary(*m.p), stCyan)),
			kvRow(locale.T("鉴权", "Auth"), span(providerAuthLabel(*m.p), stAvailable)),
			plainLine(""),
			m.actionButtonRow(locale.T("Auto Configure", "Auto Configure"), rowTest),
			kvRow(locale.T("本地代理", "Local Proxy"), span(locale.T("已就绪（仅本次会话）", "Ready (this session only)"), stAvailable)),
		)
	}

	copiedHint := ""
	if m.keyCopied {
		copiedHint = "  " + locale.T("✓ 已复制", "✓ copied")
	}
	urlCopiedHint := ""
	if m.urlCopied {
		urlCopiedHint = "  " + locale.T("✓ 已复制", "✓ copied")
	}
	// Endpoint and API Key render identically: unfocused they are grey text
	// (so the two fields match), and only when focused do they show the live
	// input view with its cursor. Double-clicking a value row copies the full
	// value. Trailing blank lines from the textarea's fixed height are
	// trimmed so the field does not consume extra rows in the panel.
	const idleWidth = 60

	// Source stepper: single-value ‹ › toggle between Custom and models.dev,
	// styled like the other steppers below (purple when idle, accent when
	// focused, cycling with ←→).
	sourceVal := "models.dev"
	if m.source == sourceCustom {
		sourceVal = locale.T("自定义", "Custom")
	}
	rows = append(rows, stepperRow(locale.T("来源", "Source"), "‹ "+sourceVal+" ›", m.cursor == m.mainRowIndex(rowSource)))

	if m.usesModelsDev() {
		// models.dev: endpoint/protocol come from metadata (read-only). The
		// Provider row opens the catalog picker; only the API key is editable.
		rows = append(rows, stepperRow("Provider", truncateMiddle(m.p.Name, idleWidth), m.cursor == m.mainRowIndex(rowProvider)))
		rows = append(rows, kvRow(locale.T("端点", "Endpoint"), span(truncateMiddle(m.p.Endpoint, idleWidth), stCyan)))
	} else {
		urlValue := truncateMiddle(m.urlText.Get(), idleWidth) + urlCopiedHint
		rows = append(rows, credentialField(locale.T("端点 URL", "Endpoint URL"), urlValue, m.urlFocused || m.cursor == m.mainRowIndex(rowEndpoint))...)
	}

	keyValue := truncateMiddle(m.keyText.Get(), idleWidth) + copiedHint
	rows = append(rows, credentialField("API Key", keyValue, m.keyFocused || m.cursor == m.mainRowIndex(rowAPIKey))...)

	// Protocol moved up from Runtime: Chat/Responses/Anthropic is selectable
	// for a manual Custom gateway; fixed and per-model runtimes stay read-only.
	if m.usesModelsDev() {
		rows = append(rows, kvRow(locale.T("协议", "Protocol"), span("auto/mixed", stAvailable)))
	} else if m.canSelectCustomProtocol() {
		value := customProtocolLabel(m.p.Type)
		rows = append(rows, stepperRow(locale.T("协议", "Protocol"), "‹ "+value+" ›", m.cursor == m.mainRowIndex(rowProtocol)))
	} else {
		rows = append(rows, kvRow(locale.T("协议", "Protocol"), span(m.getProtocol(), stAvailable)))
	}
	// Auth row: how the upstream verifies requests (API key / OAuth binding).
	// For OAuth providers the Connection block is subscription metadata, and
	// the Auth row above already carries this information.
	rows = append(rows, kvRow(locale.T("鉴权", "Auth"), span(providerAuthLabel(*m.p), stAvailable)))

	// Auto Configure / Test Connection row under Auth, separated by a blank
	// line so the action reads as its own group of one.
	if m.usesModelsDev() {
		rows = append(rows, plainLine(""), m.actionButtonRow(locale.T("验证连接", "Test Connection"), rowTest))
	} else {
		rows = append(rows, plainLine(""), m.actionButtonRow(locale.T("Auto Configure", "Auto Configure"), rowTest))
	}
	return rows
}

// connectionTitleRow renders the section heading with the live connection
// status on the same line: "Connection  ✓ Connected · Chat · 3 models".
// The status appears only once a probe has actually succeeded; while dirty,
// detecting, or failed it stays hidden so the header never shows stale state.
// models.dev needs a verified key on top of the metadata pool: the pool is
// populated at construction, so on its own it proves nothing about access.
func (m *AdvancedConfigModel) connectionTitleRow() *tui.Element {
	title := span(locale.T("连接", "Connection"), stTitle)
	if m.live().detecting {
		return line(title)
	}
	if m.usesModelsDev() && (!m.live().modelPoolFromDiscovery || !m.live().keyVerified) {
		return line(title)
	}
	if m.live().detectionError != nil {
		return line(title)
	}
	if m.live().modelPoolFromDiscovery {
		status := fmt.Sprintf(locale.T("  ✓ 已连接 · %s · %d 个模型", "  ✓ Connected · %s · %d models"), provider.ProtocolLabelForProvider(*m.p), len(m.live().modelPool))
		return line(title, span(status, stAvailable))
	}
	return line(title)
}

// viewDetectionSection renders the connection-check feedback: the in-flight
// spinner and error status lines. The Auto Configure / Test Connection button
// itself lives at the end of the Connection section, directly under Auth.
func (m *AdvancedConfigModel) viewDetectionSection() []*tui.Element {
	if m.live().detecting {
		return renderModelFetchProgress(m.live().detectProgress, m.live().detectFrame, m.usesOAuth())
	}

	// models.dev providers are pre-configured from metadata (endpoint, model
	// pool, and per-model protocol table are already in place), so the only
	// missing piece is the API key. It must be verified against the real
	// endpoint (Test Connection) before the page reports a live connection —
	// a non-empty string is not proof of validity.
	if m.usesModelsDev() {
		if !m.live().modelPoolFromDiscovery {
			return []*tui.Element{spanLine(locale.T("尚未选择 Provider", "No provider selected yet"), stGray)}
		}
		switch {
		case strings.TrimSpace(m.keyText.Get()) == "":
			return []*tui.Element{spanLine(locale.T("已选择 Provider · 请输入 API Key", "Provider selected · enter your API key"), stGray)}
		case m.live().detectionError != nil:
			return []*tui.Element{
				spanLine(locale.T("验证失败，无法连接", "Verification failed; cannot connect"), stUnavailable),
				spanLine(m.live().detectionError.Error(), stUnavailable),
				plainLine(""),
			}
		case m.live().keyVerified:
			// The header line already carries the connected status.
			return nil
		default:
			return []*tui.Element{spanLine(locale.T("Key 尚未验证", "Key not verified yet"), stGray)}
		}
	}

	if m.live().detectionError != nil {
		return []*tui.Element{
			spanLine(locale.T("检测失败，无法继续", "Detection failed; cannot continue"), stUnavailable),
			spanLine(m.live().detectionError.Error(), stUnavailable),
			plainLine(""),
		}
	}
	// Connected: the header line carries the status; nothing extra here.
	return nil
}

// actionButtonRow renders one left-aligned action row (Auto Configure / Test
// Connection) with the same plain-text affordance as Test Model Availability:
// purple when idle, "> " + accent when selected. No background button box —
// the two action rows must read as one visual family.
func (m *AdvancedConfigModel) actionButtonRow(label string, kind configRowKind) *tui.Element {
	prefix := "  "
	prefixStyle := tui.NewStyle()
	style := stPurple
	if m.cursor == m.mainRowIndex(kind) {
		prefix = "> "
		prefixStyle = stSelected
		style = stSelected
	}
	return line(
		span(prefix, prefixStyle),
		span(label, style),
	)
}

// viewMappingSection renders Model Mapping: the five model slots with their
// [1M] / availability badges, the optional availability test, and the
// provider-wide Context & Compact stepper. Rows grey out until the
// connection is ready.
func (m *AdvancedConfigModel) viewMappingSection() []*tui.Element {
	rows := []*tui.Element{
		plainLine(""),
		spanLine(locale.T("模型映射", "Model Mapping"), stTitle),
	}
	ready := m.connectionReady()
	renderMappingRow := func(kind configRowKind, label, display, modelID string, oneM bool) {
		val := span(truncateMiddle(display, 52), stPurple)
		if !ready {
			// Connection not ready: grey out the row, no focus affordance.
			val = span(truncateMiddle(display, 52), stGray)
		} else if m.cursor == m.mainRowIndex(kind) {
			val = span(truncateMiddle(display, 52), stSelected)
		}
		prefix := "  "
		prefixStyle := tui.NewStyle()
		if ready && m.cursor == m.mainRowIndex(kind) {
			prefix = "> "
			prefixStyle = stSelected
		}
		// Availability badge, shown only after the optional test ran. The
		// badge is a span inside the row's single RichText — adding it as a
		// second flex child makes the row compress and overlap the label.
		badgeText := "    "
		badgeStyle := tui.NewStyle()
		if status, ok := m.modelAvailability[modelID]; ok && status != modelAvailabilityUnknown {
			switch status {
			case modelAvailabilityAvailable:
				badgeText = "✓ "
				badgeStyle = stAvailable
			case modelAvailabilityUnavailable:
				badgeText = "✗ "
				badgeStyle = stUnavailable
			}
		} else if oneM && ready {
			badgeText = "[1M]"
			badgeStyle = stOneM
		}
		row := line(
			span(prefix, prefixStyle),
			span(fmt.Sprintf("%-10s ", label), tui.NewStyle()),
			span(truncateMiddle(display, 52), styleOf(val)),
			span(" "+badgeText, badgeStyle),
		)
		rows = append(rows, row)
	}
	renderMappingRow(rowOpus, "Opus", m.modelDisplayLabel(m.p.OpusModel), m.p.OpusModel, m.live().oneMSlots["opus"])
	renderMappingRow(rowSonnet, "Sonnet", m.modelDisplayLabel(m.p.SonnetModel), m.p.SonnetModel, m.live().oneMSlots["sonnet"])
	renderMappingRow(rowHaiku, "Haiku", m.modelDisplayLabel(m.p.HaikuModel), m.p.HaikuModel, m.live().oneMSlots["haiku"])
	renderMappingRow(rowCustom, "Custom", m.modelDisplayLabel(m.p.CustomModelID), m.p.CustomModelID, m.live().oneMSlots["custom"])
	renderMappingRow(rowSubagent, "Subagent", m.subagentDisplayLabel(), m.p.SubagentModel, m.live().oneMSlots["subagent"])

	// Test Model Availability — optional; each probe consumes quota, so the
	// user opts in explicitly. Results are shown next to the model rows above.
	// A blank line separates it from the model slots, matching the action rows.
	rows = append(rows, plainLine(""))
	testPrefix := "  "
	testPrefixStyle := tui.NewStyle()
	testLabel := locale.T("Test Model Availability", "Test Model Availability")
	testStyle := stPurple
	if !ready {
		testStyle = stGray
	} else if m.modelTesting {
		spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		spin := spinners[m.modelTestFrame%len(spinners)]
		testLabel = fmt.Sprintf("%s %s", spin, locale.T("正在测试模型可用性...", "Testing model availability..."))
	} else if m.cursor == m.mainRowIndex(rowTestModels) {
		testPrefix = "> "
		testPrefixStyle = stSelected
		testStyle = stSelected
	}
	rows = append(rows, line(
		span(testPrefix, testPrefixStyle),
		span(testLabel, testStyle),
	))
	if m.modelTesting {
		rows = append(rows, spanLine("    "+locale.T("测试进行中 · 按 esc 取消", "Testing in progress · press esc to cancel"), stGray))
	} else if len(m.modelAvailability) > 0 {
		available, unavailable := m.availabilityCounts()
		rows = append(rows, spanLine(fmt.Sprintf("    "+locale.T("%d 个可用 · %d 个不可用", "%d available · %d unavailable"), available, unavailable), stGray))
	} else if m.cursor == m.mainRowIndex(rowTestModels) {
		// go-tui has no tooltip API: the quota warning behaves like a hover hint
		// and only renders while the row is selected.
		rows = append(rows, spanLine(locale.T("    ⚠ 会为每个模型发送一次最小请求，消耗额度", "    ⚠ sends one minimal request per model; consumes quota"), stGray))
	}

	return rows
}

// viewRuntimeSection renders the Runtime block: the OAuth read-only protocol
// display (when applicable), the Context & Compact stepper, the
// Fast/Tools/Tool Search steppers, plus the active-provider checkbox.
func (m *AdvancedConfigModel) viewRuntimeSection() []*tui.Element {
	rows := []*tui.Element{
		plainLine(""),
		spanLine(locale.T("运行时", "Runtime"), stTitle),
	}
	ready := m.connectionReady()
	renderEditable := func(kind configRowKind, label, value string) {
		rows = append(rows, stepperRow(label, value, ready && m.cursor == m.mainRowIndex(kind)))
	}
	if m.usesOAuth() {
		// OAuth subscriptions keep the read-only protocol display here: their
		// Connection block is subscription metadata with no protocol concept.
		rows = append(rows, kvRow(locale.T("协议", "Protocol"), span(m.getProtocol(), stAvailable)))
	}
	// Context & Compact — per-slot [1m] via Space on the model rows above; the
	// provider-wide fallback cycles with ←→ (shown as ‹ › like other editable
	// values).
	renderEditable(rowContext, locale.T("上下文与压缩", "Context & Compact"), "‹ "+m.compactSummary()+" ›")
	renderEditable(rowFast, "Fast", formatFastLabel(m.p.FastMode))
	renderEditable(rowTools, locale.T("工具", "Tools"), formatToolsLabel(m.reviewToolsValue()))
	renderEditable(rowToolSearch, locale.T("工具搜索", "Tool Search"), formatSearchLabel(m.reviewSearchValue()))

	// Active checkbox.
	activeBox := "[ ]"
	if m.IsActiveChosen {
		activeBox = "[x]"
	}
	activeLabel := locale.T("设为当前激活 Provider", "Set as active provider")
	activeSelected := m.cursor == m.mainRowIndex(rowActive)
	boxStyle := stPurple
	labelStyle := stPurple
	prefix := "  "
	prefixStyle := tui.NewStyle()
	if activeSelected {
		prefix = "> "
		prefixStyle = stSelected
		boxStyle = stSelected
		labelStyle = stSelected
	}
	return append(rows, line(
		span(prefix, prefixStyle),
		span(activeBox+" ", boxStyle),
		span(activeLabel, labelStyle),
	))
}

// viewActionSection renders the Save/Cancel action bar, the save-gating
// warnings, and the key hint footer.
func (m *AdvancedConfigModel) viewActionSection() []*tui.Element {
	applyLabel := locale.T("保存并激活", "Save & Activate")
	if !m.IsActiveChosen {
		applyLabel = locale.T("保存 Provider", "Save Provider")
	}
	cancelLabel := locale.T("取消", "Cancel")
	applyDisabled := !m.canSave()
	applyPrefix := "  "
	applyPrefixStyle := tui.NewStyle()
	applyStyle := stPurple
	if applyDisabled {
		// Not connected (or a dirty connection not yet re-tested): the button
		// is greyed out and not focusable.
		applyStyle = stGray
	} else if m.cursor == m.mainRowIndex(rowSave) {
		applyPrefix = "> "
		applyPrefixStyle = stSelected
		applyStyle = stSelected
	}
	cancelPrefix := "  "
	cancelPrefixStyle := tui.NewStyle()
	cancelStyle := stPurple
	if m.cursor == m.mainRowIndex(rowCancel) {
		cancelPrefix = "> "
		cancelPrefixStyle = stSelected
		cancelStyle = stSelected
	}
	rows := []*tui.Element{
		plainLine(""),
		line(
			span(applyPrefix, applyPrefixStyle),
			span(applyLabel, applyStyle),
			span("     ", tui.NewStyle()),
			span(cancelPrefix, cancelPrefixStyle),
			span(cancelLabel, cancelStyle),
		),
	}
	if m.live().connectionDirty && !m.usesOAuth() {
		rows = append(rows, spanLine(locale.T("连接已修改，保存前请重新检测", "Connection changed; re-test before saving"), stGray))
	}
	if m.customDraft != nil && m.customDraft.saveGuardPending {
		rows = append(rows, spanLine(locale.T(
			"保存将覆盖同名 models.dev Provider（逐模型协议表会丢失）；再次点击保存确认，或切回 models.dev",
			"Saving replaces the same-named models.dev provider (its per-model protocol table is lost); press Save again to confirm, or switch back to models.dev",
		), stUnavailable))
	}
	return append(rows, spanLine(locale.T(
		"↑↓ 选择 · ←→ 调整 · enter 确认 · 模型行 enter 筛选",
		"↑↓ select · ←→ adjust · enter confirm · enter on a model row to filter",
	), stGray))
}

// wrapMainPanel wraps the page body in the rounded, scrollable panel plus the
// outer content chrome (scroll indicator and language tip).
func (m *AdvancedConfigModel) wrapMainPanel(rows []*tui.Element) *tui.Element {
	panel := tui.New(
		tui.WithDisplay(tui.DisplayFlex),
		tui.WithDirection(tui.Column),
		tui.WithWrap(false),
		tui.WithBorder(tui.BorderRounded),
		tui.WithPaddingTRBL(0, 2, 0, 2),
	)
	for _, r := range m.scrollWindow(rows) {
		panel.AddChild(r)
	}

	content := tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Column), tui.WithWrap(false), tui.WithPaddingTRBL(1, 0, 1, 0))
	// The indicator mirrors scrollWindow's actual offset: both derive from
	// scrollBodyBudget, so the indicator appears exactly when the body was cut.
	if m.height > 0 && len(rows) > scrollBodyBudget(m.height) && m.scrollWindowOffset(rows) > 0 {
		content.AddChild(spanLine(locale.T("▲ 上滚 · ↑ 查看", "▲ scrolled up · ↑ to view"), stGray))
	}
	content.AddChild(panel)
	content.AddChild(plainLine(""))
	content.AddChild(spanLine(locale.T(
		"💡 提示: 使用 `ccl lang` 更改终端显示语言",
		"💡 Tip: Change the TUI display language with `ccl lang`",
	), stGray))
	return content
}

// scrollWindowOffset reports the body offset scrollWindow would slice at: it is
// the model's scrollOffset, anchored to the tail once the cursor reaches the
// action bar. The indicator logic uses it so the two can never disagree.
func (m *AdvancedConfigModel) scrollWindowOffset(rows []*tui.Element) int {
	if m.height <= 0 {
		return 0
	}
	maxBody := scrollBodyBudget(m.height)
	if len(rows) <= maxBody {
		return 0
	}
	offset := m.scrollOffset
	if offset < 0 {
		offset = 0
	}
	if m.cursor >= m.mainRowIndex(rowSave) {
		offset = len(rows) - maxBody
	}
	if offset+maxBody > len(rows) {
		offset = len(rows) - maxBody
	}
	if offset < 0 {
		offset = 0
	}
	return offset
}

// scrollWindow slices the page body rows to the visible window when the body
// exceeds the terminal height. The offset mirrors keepCursorVisible (the model
// field), anchored to the tail when the action bar would fall off.
func (m *AdvancedConfigModel) scrollWindow(rows []*tui.Element) []*tui.Element {
	// No terminal size yet (offline render / first frame): the min-clamped
	// budget below would slice the body to 6 rows, so return everything.
	if m.height <= 0 {
		return rows
	}
	maxBody := scrollBodyBudget(m.height)
	offset := m.scrollWindowOffset(rows)
	if offset == 0 && len(rows) <= maxBody {
		return rows
	}
	window := rows[offset : offset+maxBody]
	if len(window) == 0 {
		return []*tui.Element{plainLine("")}
	}
	return window
}

// kvRow renders a read-only key/value line ("  Key  Value") with styled value.
// The label column shares stepperLabelPad with stepperRow so read-only and
// editable rows stay aligned.
func kvRow(key string, value tui.TextSpan) *tui.Element {
	labelText := key + strings.Repeat(" ", max(stepperLabelPad(key)-tui.StringWidth(key), 0)) + " "
	return line(
		span("  ", tui.NewStyle()),
		span(labelText, tui.NewStyle()),
		value,
	)
}

// stepperRow renders an editable value row: a label plus a ‹ value › style value
// that highlights when the cursor is on the row. The label is padded to a fixed
// column measured in terminal cells (tui.StringWidth, not bytes) so CJK labels
// like "Context & Compact" keep the value column aligned with kvRow.
func stepperRow(label, value string, selected bool) *tui.Element {
	labelPad := stepperLabelPad(label)
	valueStyle := stPurple
	prefix := "  "
	prefixStyle := tui.NewStyle()
	if selected {
		valueStyle = stSelected
		prefix = "> "
		prefixStyle = stSelected
	}
	labelText := label + strings.Repeat(" ", max(labelPad-tui.StringWidth(label), 0)) + " "
	return line(
		span(prefix, prefixStyle),
		span(labelText, tui.NewStyle()),
		span(value, valueStyle),
	)
}

// stepperLabelPad returns the label column width shared by stepperRow and the
// other field rows: wide enough for the longest label ("Tool Search") at 12
// cells, or the label itself plus two cells of breathing room when it is
// longer. Measured with tui.StringWidth so wide runes count correctly.
func stepperLabelPad(label string) int {
	const base = 12
	if w := tui.StringWidth(label); w+2 > base {
		return w + 2
	}
	return base
}

// spanLine renders one single-style text line (no wrap so hit-test rows stay
// stable).
func spanLine(text string, st tui.Style) *tui.Element {
	return tui.New(tui.WithText(text), tui.WithTextStyle(st), tui.WithWrap(false))
}

// styleOf is a tiny helper keeping mapping-row construction readable.
func styleOf(s tui.TextSpan) tui.Style { return s.Style }

// rowAtLine resolves a clicked screen row to a configuration row kind by
// matching the rendered labels. A click on the value row directly below a
// label (Endpoint/API Key inputs) resolves to the same row as clicking the
// label. Rows that are not focusable return ok=false.
func rowAtLine(lines []string, y int) (configRowKind, bool) {
	return rowAtLineAt(lines, y, -1)
}

// rowAtLineAt resolves a clicked screen row to a configuration row kind. x is
// the column offset (-1 to ignore). The X column disambiguates multiple labels
// on one rendered line (Save & Activate vs Cancel): the click maps to whichever
// label's character range contains it.
func rowAtLineAt(lines []string, y, x int) (configRowKind, bool) {
	if y < 0 || y >= len(lines) {
		return rowCancel, false
	}
	for _, off := range []int{0, -1} {
		row := y + off
		if row < 0 || row >= len(lines) {
			continue
		}
		// Strip ANSI only; keep the leading border/space columns so label
		// offsets line up with the click's X column.
		text := stripANSI(lines[row])
		// Copy rows (key / URL value) are only hit when clicked directly on their
		// own row; the off-by-one fallback (a value-row click resolving to its
		// label row) must not trigger them.
		kind, ok := matchRowLabel(text, x, off == 0)
		if ok {
			return kind, true
		}
	}
	return rowCancel, false
}

// stripANSI removes SGR/CSI escape sequences from a rendered line, leaving the
// printable text. go-tui renders through its own escape builder, so the panel
// code owns a minimal stripper instead of pulling in x/ansi.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) {
			// CSI sequences: ESC [ ... letter
			if s[i+1] == '[' {
				j := i + 2
				for j < len(s) && !isANSIFinalByte(s[j]) {
					j++
				}
				if j < len(s) {
					j++ // include the final byte
				}
				i = j
				continue
			}
			// Two-byte escapes (ESC c, ESC \, ...)
			i += 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// isANSIFinalByte reports whether c terminates a CSI sequence (ANSI "final"
// byte range 0x40-0x7E).
func isANSIFinalByte(c byte) bool {
	return c >= 0x40 && c <= 0x7e
}

// matchRowLabel matches a rendered line against the clickable labels. A label
// matches only when it begins a field — the line, after stripping the leading
// border/cursor/space/checkbox prefix, starts with the label. This keeps prose
// like "detection uses the API Key you entered" from matching. The label's
// start column is kept so a click's X can disambiguate Save vs Cancel, which
// share one line.
func matchRowLabel(text string, x int, allowButton bool) (configRowKind, bool) {
	// Strip leading border, cursor arrow, and whitespace to find the first field.
	trimmed := strings.TrimLeft(text, " │|>")
	lead := len(text) - len(trimmed)
	// Account for a checkbox "[x] "/"[ ] " before the label.
	rest := trimmed
	if strings.HasPrefix(rest, "[") {
		if idx := strings.Index(rest, "] "); idx >= 0 {
			rest = rest[idx+2:]
			lead = len(text) - len(rest)
		}
	}
	// Find every label that starts the trimmed field. Both the English and the
	// Chinese rendering of a label can lead the field, so both count as the row.
	var matched configRowKind
	var matchedIdx int
	hasMatch := false
	for kind := range rowClickLabels {
		for _, label := range rowClickLabelPrefixes(kind) {
			if strings.HasPrefix(rest, label) {
				idx := lead
				if !hasMatch || idx < matchedIdx {
					matched = kind
					matchedIdx = idx
					hasMatch = true
				}
				break
			}
		}
	}
	if !hasMatch {
		// No leading label: a value row. The endpoint value copies the URL; the
		// API key value (now plaintext and possibly spanning multiple lines)
		// copies the key. Both only respond to a direct click (allowButton), not
		// the off-by-one label fallback. The leading border/space is already
		// stripped in rest. Prose and hints must never resolve to a copy row,
		// so only URL prefixes and dense credential tokens (a key, after the
		// textarea's trailing padding is trimmed) count as value rows.
		if !allowButton {
			return rowCancel, false
		}
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			return rowCopyURL, true
		}
		key := strings.TrimSpace(rest)
		if key != "" && (strings.HasPrefix(key, "sk-") || strings.HasPrefix(key, "-----BEGIN") || !strings.ContainsAny(key, " \t")) {
			return rowCopyKey, true
		}
		return rowCancel, false
	}
	// A single label at the field start is unambiguous.
	if x < 0 {
		return matched, true
	}
	// With column info, Save and Cancel on the same line are distinct. The click
	// belongs to the label whose field start is at or before x (Save then Cancel).
	best := matched
	bestIdx := matchedIdx
	for kind := range rowClickLabels {
		for _, label := range rowClickLabelPrefixes(kind) {
			if kind == matched {
				break
			}
			idx := strings.Index(rest, label)
			if idx < 0 {
				continue
			}
			absIdx := lead + idx
			if absIdx <= x && (bestIdx > x || absIdx > bestIdx) {
				best = kind
				bestIdx = absIdx
			}
			break
		}
	}
	return best, true
}

// rowClickLabels maps a configuration row to the label prefixes a click (or
// the cursor-line lookup) must match on its rendered line. Both the English
// and the Chinese rendering of each label are listed: the page renders labels
// through locale.T, so in a Chinese session only the zh form appears on
// screen and a single-language table would silently stop matching. Only rows
// that make sense to click are listed.
type rowClickLabel struct {
	en, zh string
}

var rowClickLabels = map[configRowKind]rowClickLabel{
	rowSource:     {en: "Source", zh: "来源"},
	rowEndpoint:   {en: "Endpoint URL", zh: "端点 URL"},
	rowAPIKey:     {en: "API Key"},
	rowProvider:   {en: "Provider"},
	rowTest:       {en: "Auto Configure"},
	rowProtocol:   {en: "Protocol", zh: "协议"},
	rowFast:       {en: "Fast"},
	rowOpus:       {en: "Opus"},
	rowSonnet:     {en: "Sonnet"},
	rowHaiku:      {en: "Haiku"},
	rowCustom:     {en: "Custom"},
	rowSubagent:   {en: "Subagent"},
	rowTestModels: {en: "Test Model Availability"},
	rowContext:    {en: "Context & Compact", zh: "上下文与压缩"},
	rowTools:      {en: "Tools", zh: "工具"},
	rowToolSearch: {en: "Tool Search", zh: "工具搜索"},
	rowActive:     {en: "Set as active provider", zh: "设为当前激活 Provider"},
	// The Save button also renders as "Save Provider" when activation is not
	// chosen; matchRowLabel matches prefixes, so the shorter shared prefix of
	// both variants ("Save ") is what must stay clickable.
	rowSave:   {en: "Save & Activate", zh: "保存并激活"},
	rowCancel: {en: "Cancel", zh: "取消"},
}

// rowClickLabelPrefixes returns every rendered label variant for a row kind.
func rowClickLabelPrefixes(kind configRowKind) []string {
	label, ok := rowClickLabels[kind]
	if !ok {
		return nil
	}
	prefixes := make([]string, 0, 3)
	if label.en != "" {
		prefixes = append(prefixes, label.en)
	}
	if label.zh != "" {
		prefixes = append(prefixes, label.zh)
	}
	// "Save Provider" is the other rendering of the Save button.
	if kind == rowSave {
		prefixes = append(prefixes, "Save Provider", "保存 Provider")
	}
	return prefixes
}

// viewModelPicker renders the filtered model selection overlay. It is shown
// whenever the filter input owns the keyboard; selecting a model (enter) or
// pressing esc returns to the main configuration page.
func (m *AdvancedConfigModel) viewModelPicker() *tui.Element {
	slotName := []string{"Opus", "Sonnet", "Haiku", "Custom", "Subagent"}[m.activeSlot]
	root := tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Column), tui.WithWrap(false), tui.WithPadding(1))
	root.AddChild(tui.New(
		tui.WithRichText(span(fmt.Sprintf(locale.T("配置槽位 [%s] 模型筛选", "Select Model for Slot [%s]"), slotName), stTitle)),
	))
	filterText := m.filterText.Get()
	root.AddChild(line(
		span(locale.T("🔍 过滤模型: ", "🔍 Filter model: "), stFilter),
		span(filterText, tui.NewStyle()),
	))

	start := m.filterWindowStart
	end := start + filterViewHeight
	if end > len(m.filteredPool) {
		end = len(m.filteredPool)
	}
	if start > 0 {
		root.AddChild(spanLine(fmt.Sprintf("   ↑ ... %d more above ...", start), stGray))
	}
	for i := start; i < end; i++ {
		mod := m.filteredPool[i]
		display := mod
		if stringInSlice(mod, m.live().modelPool) {
			display = m.modelDisplayLabel(mod)
		}
		status := ""
		if stringInSlice(mod, m.live().modelPool) {
			badge := m.availabilitySpan(mod)
			status = "  " + badge.Text
		}
		rowStyle := stGray
		prefix := "   "
		prefixStyle := tui.NewStyle()
		if i == m.slotListCursor {
			rowStyle = stSelected
			prefix = " > "
			prefixStyle = stSelected
		}
		if status != "" {
			root.AddChild(line(span(prefix, prefixStyle), span(display, rowStyle), span(status, stGray)))
		} else {
			root.AddChild(line(span(prefix, prefixStyle), span(display, rowStyle)))
		}
	}
	if end < len(m.filteredPool) {
		root.AddChild(spanLine(fmt.Sprintf("   ↓ ... %d more below ...", len(m.filteredPool)-end), stGray))
	}
	root.AddChild(spanLine(fmt.Sprintf("  %d/%d", m.slotListCursor+1, len(m.filteredPool)), stSelected))
	root.AddChild(plainLine(""))
	root.AddChild(spanLine(locale.T("状态来自可用性测试 · 键盘输入过滤 · ↑↓ 选择 · enter 锁定 · esc 取消", "Status comes from availability test · type to filter · ↑↓ scroll · enter lock · esc cancel"), stGray))
	return panelWrap(root, m.panelWidth())
}

// viewModelsDevPicker renders the models.dev provider overlay opened from the
// Connection section. It is filterable and scrolls like the slot picker.
func (m *AdvancedConfigModel) viewModelsDevPicker() *tui.Element {
	root := tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Column), tui.WithWrap(false), tui.WithPadding(1))
	root.AddChild(tui.New(tui.WithText(locale.T("从 models.dev 选择 Provider", "Choose a provider from models.dev")), tui.WithTextStyle(stTitle)))
	root.AddChild(plainLine(""))
	root.AddChild(line(
		span(locale.T("🔍 过滤: ", "🔍 Filter: "), stFilter),
		span(m.modelsDevText.Get(), tui.NewStyle()),
	))
	root.AddChild(plainLine(""))

	if m.modelsDevLoading {
		root.AddChild(spanLine(locale.T("正在加载 models.dev 目录...", "Loading the models.dev catalog..."), stSelected))
	} else if m.modelsDevError != nil {
		root.AddChild(spanLine(locale.T("拉取失败", "Fetch failed"), stUnavailable))
		root.AddChild(spanLine(m.modelsDevError.Error(), stUnavailable))
	} else if len(m.modelsDevFiltered) == 0 {
		root.AddChild(spanLine(locale.T("(无匹配)", "(no match)"), stGray))
	} else {
		start := m.modelsDevWindow
		end := start + selectViewHeight
		if end > len(m.modelsDevFiltered) {
			end = len(m.modelsDevFiltered)
		}
		if start > 0 {
			root.AddChild(spanLine(fmt.Sprintf("   ↑ ... %d more above ...", start), stGray))
		}
		for i := start; i < end; i++ {
			p := m.modelsDevFiltered[i]
			display := p.Name
			if p.ID != "" && p.ID != p.Name {
				display = fmt.Sprintf("%s  (%s)", p.Name, p.ID)
			}
			prefix := "  "
			prefixStyle := tui.NewStyle()
			rowStyle := tui.NewStyle()
			if i == m.modelsDevCursor {
				prefix = "▸ "
				prefixStyle = stSelected
				rowStyle = stSelected
			}
			root.AddChild(line(span(prefix, prefixStyle), span(display, rowStyle)))
		}
		if end < len(m.modelsDevFiltered) {
			root.AddChild(spanLine(fmt.Sprintf("   ↓ ... %d more below ...", len(m.modelsDevFiltered)-end), stGray))
		}
	}

	root.AddChild(plainLine(""))
	root.AddChild(spanLine(locale.T("输入过滤 · ↑↓ 选择 · enter 确认 · esc 取消", "type to filter · ↑↓ choose · enter confirm · esc cancel"), stGray))
	return panelWrap(root, m.panelWidth())
}

// panelWrap wraps an overlay body in the shared rounded panel.
func panelWrap(body *tui.Element, width int) *tui.Element {
	panel := tui.New(
		tui.WithDisplay(tui.DisplayFlex),
		tui.WithDirection(tui.Column),
		tui.WithWrap(false),
		tui.WithBorder(tui.BorderRounded),
		tui.WithPaddingTRBL(0, 2, 0, 2),
		tui.WithMaxWidth(width),
	)
	panel.AddChild(body)
	return panel
}

// KeyMap routes every key through handleKey. go-tui matches one binding per
// event, so a single AnyKey stop binding covers the whole page (the picker,
// the modals, and the inputs are disambiguated inside handleKey).
func (m *AdvancedConfigModel) KeyMap() tui.KeyMap {
	return tui.KeyMap{
		tui.OnStop(tui.AnyKey, func(ke tui.KeyEvent) {
			m.handleKey(ke)
			m.markDirty()
		}),
	}
}

// HandleMouse maps a click to the configuration row under the pointer. The
// rendered frame is re-rendered offline (tui.Sprint at the current terminal
// width) so the hit test works on exactly what the user sees. Only left-button
// presses are handled; the wheel is deliberately ignored (the page anchors its
// scroll window to the cursor instead).
func (m *AdvancedConfigModel) HandleMouse(me tui.MouseEvent) bool {
	if m.modelsDevPicker || m.filterFocused {
		return false
	}
	if me.Button != tui.MouseLeft || me.Action != tui.MousePress {
		return false
	}
	lines := strings.Split(tui.Sprint(m.Render(nil), tui.WithPrintWidth(max(m.width, 1))), "\n")
	row, ok := rowAtLineAt(lines, me.Y, me.X)
	if !ok {
		return false
	}
	m.handleFocusRow(row)
	return true
}

// Watchers bridges the async goroutines onto the main loop: the four result
// channels, the two spinner/cleanup timers, and the one-shot auto-detect that
// used to live in Init(). go-tui calls Watchers() after the first render, so
// the channels are guaranteed to be watched before any fetch can complete.
func (m *AdvancedConfigModel) Watchers() []tui.Watcher {
	m.startAutoDetect()
	return []tui.Watcher{
		tui.Watch(m.fetchDone, m.handleFetchDone),
		tui.Watch(m.verifyDone, m.handleVerifyDone),
		tui.Watch(m.mdDone, m.handleModelsDevDone),
		tui.Watch(m.availDone, m.handleAvailabilityDone),
		tui.OnTimer(120*time.Millisecond, m.handleFetchTick),
		tui.OnTimer(120*time.Millisecond, m.handleAvailabilityTick),
	}
}

// BindApp stores the app reference used by markDirty/quit and wires the four
// input States so their Set() calls mark the frame dirty on their own. The
// framework calls it before the first render.
func (m *AdvancedConfigModel) BindApp(app *tui.App) {
	m.app = app
	m.urlText.BindApp(app)
	m.keyText.BindApp(app)
	m.filterText.BindApp(app)
	m.modelsDevText.BindApp(app)
}

// UnbindApp clears the app reference (symmetric cleanup for tests).
func (m *AdvancedConfigModel) UnbindApp() { m.app = nil }
