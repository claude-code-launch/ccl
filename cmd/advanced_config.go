package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

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

const (
	filterViewHeight      = 15 // max visible items in filter list
	preferredPanelWidth   = 82
	minimumPanelWidth     = 54
	minimumTerminalMargin = 4
	// Availability checks are optional and quota-consuming. Keep the burst small
	// so gateway rate limits do not make healthy models look unavailable.
	slotTestConcurrency = 2
	lowCostProbeModel   = "gpt-5.4-mini"
	// Cold-start gateways routinely answer a first inference request in well
	// over ten seconds; the timeout must outlast that without hanging the test.
	modelProbeTimeout = 20 * time.Second
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
	rowAuth
	rowFast
	rowOpus
	rowSonnet
	rowHaiku
	rowFable
	rowCustom
	rowSubagent
	rowTestModels // Test model availability (optional, costs quota)
	rowContext    // Context & Compact entry
	rowStatusline // Status Line on/off pin
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
	// A models.dev key is verified automatically after editing settles or the
	// input loses focus. Clearing this before dispatch prevents repeated probes
	// after a failed attempt; editing the key schedules a fresh check.
	autoVerifyPending bool
	lastKeyEditAt     time.Time
	verifyGeneration  uint64
	// modelsDevRefreshPending arms a one-shot catalog refresh that fires after
	// the key verifies: an already-saved provider opens on its persisted pool,
	// and only a verified connection proves the refresh would not run against a
	// stale endpoint/key. Cleared once the refresh result is consumed.
	modelsDevRefreshPending bool

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
	// providerName is the name the page was opened for. NameGenerated marks a
	// placeholder from randomProviderName; only then does picking a models.dev
	// provider rename the draft to the catalog ID. A name the user typed, or an
	// existing provider being edited, is kept.
	providerName  string
	NameGenerated bool

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
	oauthDone  chan oauthRuntimeDoneMsg

	// The OAuth runtime is started in the background so the panel opens at once.
	// oauthStartDone closes when that goroutine finishes, which is also the
	// happens-before edge that makes runtimeCleanup safe to read from
	// stopOAuthRuntime.
	oauthStartDone chan struct{}
	oauthCancel    context.CancelFunc
	runtimeCleanup func()
	runtimeLoading bool
	runtimeReady   bool
	runtimeErr     error
	runtimeFrame   int
	// runtimeCatalogFallback means the runtime came up but could not fetch the
	// account's catalog, so it is serving a built-in compatibility list. The page
	// keeps the models it already had and says why, instead of adopting the guess.
	runtimeCatalogFallback bool
}

type modelAvailability uint8

const (
	modelAvailabilityUnknown modelAvailability = iota
	modelAvailabilityAvailable
	modelAvailabilityUnavailable
	modelAvailabilityInconclusive
)

type modelAvailabilityDoneMsg struct {
	testID   uint64
	statuses map[string]modelAvailability
}

// oauthRuntimeDoneMsg reports the loopback runtime a subscription needs. The
// panel is already on screen by the time it arrives: the fields are everything
// configureOAuthRuntime needs to adopt the runtime.
type oauthRuntimeDoneMsg struct {
	endpoint string
	apiKey   string
	models   []string
	names    map[string]string
	// catalogFallback reports that models is a compatibility list the runtime
	// fell back to, not the account's catalog.
	catalogFallback bool
	err             error
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
	endpoint   string
	apiKey     string
	generation uint64
	err        error
}

// keyVerifyTimeout bounds the single authenticated inference request used to
// verify a models.dev API key. It matches modelProbeTimeout: cold-start
// gateways routinely answer a first inference request in well over ten
// seconds, and a shorter verify budget would block saving a key the
// availability probe itself could reach.
const keyVerifyTimeout = 20 * time.Second

// modelsDevFetchDoneMsg carries the models.dev catalog (or its fetch error) back
// to the picker overlay. A refresh of an already-picked provider is flagged by
// refreshFor so the picker handler ignores it.
type modelsDevFetchDoneMsg struct {
	providers  []modelsdev.Provider
	err        error
	refreshFor string
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
// Auto Configure run (a model pool discovered). An OAuth subscription owns its
// credential from the start, but its catalog arrives with the loopback runtime
// that is started in the background, so it is ready only once that lands.
func (m *AdvancedConfigModel) connectionReady() bool {
	if m.usesOAuth() {
		return m.runtimeReady
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
		// no Auto Configure (a subscription has no connection to probe), and
		// Protocol stays in the Runtime section (its Connection block is
		// read-only subscription metadata).
		rows = append(rows,
			configRow{kind: rowAPIKey},
		)
		ready := m.connectionReady()
		rows = append(rows,
			configRow{kind: rowOpus, editable: ready},
			configRow{kind: rowSonnet, editable: ready},
			configRow{kind: rowHaiku, editable: ready},
			configRow{kind: rowFable, editable: ready},
			configRow{kind: rowCustom, editable: ready},
			configRow{kind: rowSubagent, editable: ready},
			configRow{kind: rowTestModels, editable: ready},
			configRow{kind: rowContext, editable: ready},
			configRow{kind: rowProtocol, editable: ready},
			configRow{kind: rowFast, editable: ready},
			configRow{kind: rowStatusline, editable: ready},
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
		)
		if m.canSelectCustomProtocol() && provider.IsAnthropicType(m.p.Type) {
			rows = append(rows, configRow{kind: rowAuth})
		}
		rows = append(rows, configRow{kind: rowTest})
	} else {
		// models.dev: endpoint and protocol come from metadata (read-only); the
		// Provider row opens the catalog picker and only the API key is editable.
		// Protocol stays rendered inline (like Endpoint) but is not a navigable
		// stop — there is nothing to toggle on a mixed-protocol gateway. The
		// entered key is checked automatically, without a separate action row.
		rows = append(rows,
			configRow{kind: rowProvider},
			configRow{kind: rowAPIKey},
		)
	}
	ready := m.connectionReady()
	modelTestReady := ready && (!m.usesModelsDev() || m.live().keyVerified)
	// Render order matches View: Connection → Model Mapping → Context →
	// Runtime (Fast/Status Line) → Active → actions.
	rows = append(rows,
		configRow{kind: rowOpus, editable: ready},
		configRow{kind: rowSonnet, editable: ready},
		configRow{kind: rowHaiku, editable: ready},
		configRow{kind: rowFable, editable: ready},
		configRow{kind: rowCustom, editable: ready},
		configRow{kind: rowSubagent, editable: ready},
		configRow{kind: rowTestModels, editable: modelTestReady},
		configRow{kind: rowContext, editable: ready},
		configRow{kind: rowFast, editable: ready},
		configRow{kind: rowStatusline, editable: ready},
		configRow{kind: rowActive, editable: ready},
		configRow{kind: rowSave},
		configRow{kind: rowCancel},
	)
	return rows
}

// focusDetectionAction moves the cursor onto Auto Configure, or onto the first
// model slot when the page has no manual connection action (OAuth/models.dev).
func (m *AdvancedConfigModel) focusDetectionAction() {
	if index := m.mainRowIndex(rowTest); index >= 0 {
		m.cursor = index
		return
	}
	m.cursor = m.mainRowIndex(rowOpus)
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

// isModelRow reports whether a row kind is one of the model slots.
func (m *AdvancedConfigModel) isModelRow(kind configRowKind) bool {
	switch kind {
	case rowOpus, rowSonnet, rowHaiku, rowFable, rowCustom, rowSubagent:
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
	slot := modelSlotKeys[slotForRow(row)]
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

// modelSlotKeys names the model slots in advancedSlotRefs order, so row kinds,
// the [1m] state map and the slot picker all index the same list. Keep it in
// step with advancedSlotRefs.
var modelSlotKeys = []string{"opus", "sonnet", "haiku", "fable", "custom", "subagent"}

// slotForRow maps a model row kind back to its advancedSlotRefs index.
func slotForRow(kind configRowKind) int {
	switch kind {
	case rowOpus:
		return 0
	case rowSonnet:
		return 1
	case rowHaiku:
		return 2
	case rowFable:
		return 3
	case rowCustom:
		return 4
	case rowSubagent:
		return 5
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
	case rowFable:
		return m.p.FableModel
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
		// Nothing to probe, but the page must not be saved before the runtime
		// that owns the catalog has answered — the pool and the slot mapping it
		// fills are what gets persisted.
		return m.runtimeReady
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
		providerName:      p.Name,
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
		oauthDone:         make(chan oauthRuntimeDoneMsg, 2),
		oauthStartDone:    make(chan struct{}),
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
	cleanAndPopulate(&m.p.FableModel, "fable")
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
	//
	// OAuth subscriptions load the pool without any of that: there is no probe to
	// wait for, so the persisted catalog is available immediately.
	pool := uniqueModels(parseModelList(m.p.Model))
	if len(pool) > 0 {
		m.live().modelPool = pool
		switch {
		case isModelsDev:
			m.live().modelPoolFromDiscovery = true
			m.live().autoDetectOnOpen = false
			m.live().connectionDirty = false
			m.live().detectedInputEndpoint = m.p.Endpoint
			m.live().probeEndpoint = m.p.Endpoint
			m.live().inputEndpoint = m.p.Endpoint
			m.live().inputAPIKey = m.p.APIKey
			m.live().autoVerifyPending = strings.TrimSpace(m.p.APIKey) != ""
			// The persisted pool is a snapshot from the last save. Arm a one-shot
			// catalog refresh that runs once the key re-verifies.
			m.live().modelsDevRefreshPending = true
		case m.usesOAuth():
			// A subscription has no probe to run — connectionReady is true from
			// the start — so the persisted pool is the whole catalog: AutoClaw's
			// built-in list, or whatever `ccl oauth` discovered. Skipping it here
			// left the model picker and the availability test with nothing to
			// show. configureOAuthRuntime refreshes it from the live runtime.
		default:
			m.live().autoDetectOnOpen = true
		}
	}

	return m
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

func (m *AdvancedConfigModel) availabilitySmokeTestModel() string {
	if m.p == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(m.p.OAuthProvider)) {
	case "gpt":
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
			searchable := strings.ToLower(mod + " " + m.modelSearchLabel(mod))
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

// modelDisplayLabel renders a mapping or picker row as the plain model ID plus
// any catalog badges. The provider's display alias (for example AutoClaw's
// "Auto" for zai_auto) is deliberately left out: the identifier is what gets
// persisted, what the request carries, and what every other ccl surface prints.
func (m *AdvancedConfigModel) modelDisplayLabel(id string) string {
	return modelBadgeLabel(stripOneMSuffix(id), m.live().modelDisplayMetadata)
}

// modelSearchLabel widens the picker's filter text with the provider's display
// alias, so a model stays findable by the name its catalog advertises even
// though the row itself shows the ID.
func (m *AdvancedConfigModel) modelSearchLabel(id string) string {
	return modelReportLabel(stripOneMSuffix(id), m.live().modelDisplayMetadata)
}

func (m *AdvancedConfigModel) setRuntimeModelNames(names map[string]string) {
	if m.live().modelDisplayMetadata == nil {
		m.live().modelDisplayMetadata = make(map[string]protocol.ModelInfo)
	}
	for id, name := range names {
		id, name = strings.TrimSpace(id), strings.TrimSpace(name)
		if id == "" || name == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(id))
		info := m.live().modelDisplayMetadata[key]
		info.ID = id
		info.DisplayName = name
		m.live().modelDisplayMetadata[key] = info
	}
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
	case modelAvailabilityInconclusive:
		return span(locale.T("? 未确认", "? inconclusive"), stGray)
	default:
		return span(locale.T("? 未测试", "? not tested"), stGray)
	}
}

func (m *AdvancedConfigModel) availabilityCounts() (available, unavailable, inconclusive int) {
	for _, model := range m.live().modelPool {
		switch m.availabilityFor(model) {
		case modelAvailabilityAvailable:
			available++
		case modelAvailabilityUnavailable:
			unavailable++
		case modelAvailabilityInconclusive:
			inconclusive++
		}
	}
	return available, unavailable, inconclusive
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

// advancedSlotRef names one model slot of a provider and points at its field.
// The key order here is the slot order used by modelSlotKeys, slotForRow and
// the slot picker; all of them index the same list.
type advancedSlotRef struct {
	key string
	ptr *string
}

func advancedSlotRefs(p *provider.Provider) []advancedSlotRef {
	return []advancedSlotRef{
		{key: "opus", ptr: &p.OpusModel},
		{key: "sonnet", ptr: &p.SonnetModel},
		{key: "haiku", ptr: &p.HaikuModel},
		{key: "fable", ptr: &p.FableModel},
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

// 实时获取/检测协议名称
func (m *AdvancedConfigModel) getProtocol() string {
	if m.p.Type != "" {
		return provider.ProtocolLabelForProvider(*m.p)
	}
	if strings.Contains(strings.ToLower(m.urlText.Get()), "anthropic") {
		return "anthropic-messages"
	}
	return "openai-chat"
}

func (m *AdvancedConfigModel) getProtocolFamily() string {
	if m.p != nil {
		switch {
		case provider.IsAnthropicType(m.p.Type):
			return "Anthropic"
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
// whose data plane can be selected by the user. OAuth and models.dev remain
// fixed, so the manual protocol selector never appears for them.
func (m *AdvancedConfigModel) canSelectCustomProtocol() bool {
	if m.p == nil || m.source != sourceCustom || strings.TrimSpace(m.p.OAuthProvider) != "" {
		return false
	}
	return strings.TrimSpace(m.p.Type) == "" || provider.IsOpenAICompatibleType(m.p.Type) || provider.IsAnthropicType(m.p.Type)
}

func customProtocolLabel(providerType string) string {
	if strings.TrimSpace(providerType) == "" {
		return "Auto"
	}
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
	if strings.TrimSpace(m.p.Type) == "" {
		// An undetected provider starts one step before the first protocol, so the
		// first forward step selects Chat rather than skipping it.
		if delta > 0 {
			index = -1
		} else {
			index = 0
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

func formatEditableValue(label string, isDefault bool) string {
	if isDefault {
		return "‹ Default · " + label + " ›"
	}
	return "‹ " + label + " ›"
}

// formatStatuslineLabel renders the ccl status-line opt-out. The stored field
// is negative (statuslineDisabled), so On is the zero value.
func formatStatuslineLabel(disabled bool) string {
	if disabled {
		return formatEditableValue("Off", false)
	}
	return formatEditableValue("On", false)
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
	if m.currentRow() == rowProtocol && m.canSelectCustomProtocol() {
		m.cycleCustomProtocol(delta)
		return
	}
	if m.currentRow() == rowAuth && m.canSelectCustomProtocol() {
		if provider.IsAnthropicType(m.p.Type) {
			values := []string{"", anthropicAuthXAPIKey, anthropicAuthBearer}
			index := 0
			for i, value := range values {
				if value == m.p.AnthropicAuth {
					index = i
				}
			}
			m.p.AnthropicAuth = values[(index+delta%len(values)+len(values))%len(values)]
			m.live().anthropicAuth = m.p.AnthropicAuth
		}
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
	case rowStatusline:
		// Toggle like Fast: left/right/enter all flip the pin.
		m.p.StatuslineDisabled = !m.p.StatuslineDisabled
		setDebugf("statusline toggled disabled=%t", m.p.StatuslineDisabled)
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
	for _, slot := range modelSlotKeys {
		if oneMSlots[slot] {
			slots = append(slots, slot)
		}
	}
	if len(slots) == 0 {
		return "off"
	}
	return strings.Join(slots, ",")
}

// currentWithOneMMarkers returns the provider with each configured slot's [1m]
// marker re-attached. Construction strips the markers into live().oneMSlots, but
// the recommendation engine reads them back off the slots to tell a window the
// user opened from one they never touched — so the markers have to be on the
// value it sees.
func (m *AdvancedConfigModel) currentWithOneMMarkers() provider.Provider {
	current := *m.p
	for _, slot := range advancedSlotRefs(&current) {
		if *slot.ptr != "" && m.live().oneMSlots[slot.key] {
			*slot.ptr += "[1m]"
		}
	}
	return current
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
