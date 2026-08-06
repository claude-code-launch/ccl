package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

var (
	// Each semantic color has a high-contrast light and dark terminal variant.
	colorBorder    = compat.AdaptiveColor{Light: lipgloss.Color("#8A93A0"), Dark: lipgloss.Color("#687386")}
	colorAccent    = compat.AdaptiveColor{Light: lipgloss.Color("#0B72E7"), Dark: lipgloss.Color("#65B7FF")}
	colorSecondary = compat.AdaptiveColor{Light: lipgloss.Color("#6E4BB6"), Dark: lipgloss.Color("#B79CFF")}
	colorData      = compat.AdaptiveColor{Light: lipgloss.Color("#007C7C"), Dark: lipgloss.Color("#41D7C8")}
	colorWarning   = compat.AdaptiveColor{Light: lipgloss.Color("#9A6700"), Dark: lipgloss.Color("#F0B84D")}
	colorError     = compat.AdaptiveColor{Light: lipgloss.Color("#B42318"), Dark: lipgloss.Color("#FF8A80")}

	windowStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Padding(1, 2).
			Width(70)

	titleStyle       = lipgloss.NewStyle().Bold(true)
	badgeStyle       = lipgloss.NewStyle().Foreground(colorAccent).Bold(true).MarginLeft(1)
	protoBadgeStyle  = lipgloss.NewStyle().Foreground(colorSecondary).MarginLeft(1)
	cyanText         = lipgloss.NewStyle().Foreground(colorData)
	purpleText       = lipgloss.NewStyle().Foreground(colorSecondary)
	grayText         = lipgloss.NewStyle().Faint(true)
	errorBoxStyle    = lipgloss.NewStyle().Foreground(colorError).Border(lipgloss.NormalBorder()).BorderForeground(colorError).Padding(0, 1).Width(62)
	dividerStyle     = lipgloss.NewStyle().Faint(true)
	selectedStyle    = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	filterStyle      = lipgloss.NewStyle().Foreground(colorAccent)
	availableStyle   = lipgloss.NewStyle().Foreground(colorData).Bold(true)
	unavailableStyle = lipgloss.NewStyle().Foreground(colorError)
	// Buttons always carry a background so they read as selectable even when the
	// cursor is elsewhere. The unselected state is a dimmer version of the
	// selected accent so focus is obvious at a glance.
	buttonStyle       = lipgloss.NewStyle().Foreground(compat.AdaptiveColor{Light: lipgloss.Color("#6B7A93"), Dark: lipgloss.Color("#9AA7BE")}).Background(compat.AdaptiveColor{Light: lipgloss.Color("#C8D2E4"), Dark: lipgloss.Color("#2E3A50")}).Padding(0, 1)
	buttonActiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Background(colorAccent).Bold(true).Padding(0, 1)
)

const (
	filterViewHeight      = 15 // max visible items in filter list
	credentialInputWidth  = 58
	preferredPanelWidth   = 82
	minimumPanelWidth     = 54
	minimumTerminalMargin = 4
	slotTestConcurrency   = 50
	lowCostProbeModel     = "gpt-5.4-mini"
)

// configRowKind enumerates the focusable rows of the single configuration page.
// The page cursor indexes into visibleRows(); adding or hiding a row never
// requires renumbering offsets.
type configRowKind uint8

const (
	rowEndpoint configRowKind = iota
	rowAPIKey
	rowCopyKey // click the key value to copy the full key
	rowCopyURL // click the endpoint value to copy the URL
	rowTest    // Auto Configure
	rowProtocol
	rowFast
	rowOpus
	rowSonnet
	rowHaiku
	rowCustom
	rowSubagent
	rowTestModels // Test model availability (optional, costs quota)
	rowContext    // Context & Compact entry
	rowMaxOutput
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

// compactRadioOrder is the context sizing choice offered to the user.
//
// Claude Code has one default window and a per-slot 1M variant, and it scales its
// own compaction to whichever a slot uses, so ccl no longer offers intermediate
// context presets: pick the 1M variant per slot with Extended Context, or keep
// manual env values.
var compactRadioOrder = []compactPreset{
	compactPresetDefault,
	compactPresetPreserve, // Custom (manual env)
}

type AdvancedConfigModel struct {
	p         *provider.Provider
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
	compactState        compactConfigState

	probeEndpoint string
	probeAPIKey   string

	// detectedInputEndpoint / detectedInputKey hold the raw Endpoint/API Key the
	// last successful detection ran against (before any normalization). A
	// connection is dirty when the current inputs differ from these, so reverting
	// an edit or cancelling a re-detection un-dirties the page naturally.
	detectedInputEndpoint string
	detectedInputKey      string

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

	page   int
	cursor int
	width  int
	height int
	// scrollOffset is the number of rendered lines scrolled off the top of the
	// single page when the content exceeds the terminal height. The cursor row is
	// kept visible: scrolling happens in Update, never in View (which is pure).
	scrollOffset int
	// autoDetectOnOpen marks an existing provider whose connection should be
	// re-verified automatically when the page opens (Init). Until that check
	// succeeds, connectionReady is false and Model Mapping / Runtime stay greyed.
	autoDetectOnOpen bool
	// keyCopied / urlCopied show a brief "copied" hint after the value is copied.
	keyCopied bool
	urlCopied bool
	// lastCopyClickAt / lastCopyClickRow detect a double-click on a value row:
	// the second click within the window copies instead of focusing.
	lastCopyClickAt  time.Time
	lastCopyClickRow configRowKind

	// detectionError is set when protocol detection AND model fetching both fail on Page 0.
	detectionError error
	detecting      bool
	detectProgress int
	detectFrame    int

	// Page 0
	urlInput textinput.Model
	keyInput textarea.Model

	// Page 1
	activeSlot        int
	filterInput       textinput.Model
	filteredPool      []string
	slotListCursor    int
	filterWindowStart int // first visible index in filter list
	modelAvailability map[string]modelAvailability
	modelTesting      bool
	modelTestCancel   context.CancelFunc
	modelTestFrame    int
	modelTestID       uint64
	modelTestCanceled bool

	// Page 4
	IsActiveChosen bool
	saveConfirmed  bool
}

type modelFetchTickMsg struct{}

// copiedClearMsg fires shortly after a key is copied, clearing the hint.
type copiedClearMsg struct{}

// urlCopiedClearMsg fires shortly after the endpoint URL is copied.
type urlCopiedClearMsg struct{}

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

type modelAvailabilityTickMsg struct {
	testID uint64
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

// pageDetected reports whether the full single page (Connection + Model Mapping
// + Runtime) is shown. It is always true: the config page is one page for both
// new and existing providers. Before detection the Model Mapping rows render
// empty with an Auto Configure hint; after it they show the discovered models.
func (m *AdvancedConfigModel) pageDetected() bool {
	return true
}

// currentRow returns the configRowKind the page cursor sits on. It is only
// meaningful when page == 4 (the single configuration page).
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
	return m.modelPoolFromDiscovery
}

// visibleRows lists the focusable rows of the single configuration page in
// render order. Connection rows are always present; before a successful
// detection the Model Mapping / Runtime rows still render but are marked
// non-editable (greyed out) until the connection is ready.
func (m *AdvancedConfigModel) visibleRows() []configRow {
	rows := []configRow{
		{kind: rowEndpoint},
		{kind: rowAPIKey},
		{kind: rowTest},
	}
	ready := m.connectionReady()
	// Render order matches View: Connection → Model Mapping → Context →
	// Runtime (Protocol/Fast/...) → Active → actions.
	rows = append(rows,
		configRow{kind: rowOpus, editable: ready},
		configRow{kind: rowSonnet, editable: ready},
		configRow{kind: rowHaiku, editable: ready},
		configRow{kind: rowCustom, editable: ready},
		configRow{kind: rowSubagent, editable: ready},
		configRow{kind: rowTestModels, editable: ready},
		configRow{kind: rowContext, editable: ready},
	)
	rows = append(rows, configRow{kind: rowProtocol, editable: ready})
	rows = append(rows, configRow{kind: rowFast, editable: ready})
	rows = append(rows, configRow{kind: rowMaxOutput, editable: ready})
	rows = append(rows,
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

// renderedCursorLine returns the 0-based line index within a rendered body where
// the cursor row's label starts. The body is searched line by line for the row's
// clickable label (matching rowAtLine's label set), so the window slicing in View
// lines up with the row the cursor is actually on — even when structural blank
// lines between sections shift rows from the rowLineHeight estimate. Rows that
// have no rendered label (or a cursor beyond the visible rows) resolve to 0.
func renderedCursorLine(body string, cursor int, rows []configRow) int {
	if cursor < 0 || cursor >= len(rows) {
		return 0
	}
	kind := rows[cursor].kind
	label, ok := rowClickLabels[kind]
	if !ok || label == "" {
		return 0
	}
	for i, line := range strings.Split(body, "\n") {
		if strings.Contains(line, label) {
			return i
		}
	}
	return 0
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
func (m *AdvancedConfigModel) keepCursorVisible() {
	// Scroll is only meaningful once the terminal height is known and the page
	// content can exceed it. The view reports its overflow through the same
	// height; without a height there is nothing to keep visible.
	if m.height <= 0 {
		return
	}
	// Estimate the row height of the cursor: connection rows take two lines
	// (label + value); everything else one. The scroll budget is the terminal
	// height minus the fixed header/panel chrome (must match View).
	visibleHeight := scrollBodyBudget(m.height)
	rows := m.visibleRows()
	if len(rows) == 0 {
		return
	}
	cursorLine := 0
	for i := 0; i < m.cursor && i < len(rows); i++ {
		cursorLine += rowLineHeight(rows[i].kind)
	}
	rowH := rowLineHeight(rows[m.cursor].kind)

	if cursorLine < m.scrollOffset {
		m.scrollOffset = cursorLine
	}
	if cursorLine+rowH > m.scrollOffset+visibleHeight {
		m.scrollOffset = cursorLine + rowH - visibleHeight
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
}

// rowLineHeight reports how many rendered lines a row occupies. Connection rows
// span label + value; the API key value is a 2-line textarea so its row is one
// line taller than the endpoint. Every other row is a single line.
func rowLineHeight(kind configRowKind) int {
	switch kind {
	case rowEndpoint:
		return 2
	case rowAPIKey:
		return 3
	}
	return 1
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
	if !m.oneMSlots[slot] && m.oneMSlotBlocked(model) {
		setDebugf("1M blocked slot=%s model=%q", slot, model)
		return false
	}
	if slot == "subagent" && !m.oneMSlots[slot] && !m.materializeSubagentModel() {
		return false
	}
	m.oneMSlots[slot] = !m.oneMSlots[slot]
	synced := m.syncOneMForSameModels(slot, m.oneMSlots[slot])
	setDebugf("toggle 1M slot=%s enabled=%t synced=%d summary=%s", slot, m.oneMSlots[slot], synced, reviewOneMSummary(m.oneMSlots))
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
	if m.connectionDirty {
		return false
	}
	if m.modelPoolFromDiscovery {
		return true
	}
	// No detection yet: allow saving an existing provider whose connection inputs
	// still match the persisted ones (e.g. editing only a model mapping).
	return m.autoConfigured && m.connectionUnchanged()
}

// refreshConnectionDirty re-derives the dirty state from the current inputs
// against the last successful detection's raw inputs. It is called whenever an
// input changes, so reverting an edit or cancelling a re-detection naturally
// clears the flag without a sticky manual reset.
func (m *AdvancedConfigModel) refreshConnectionDirty() {
	m.connectionDirty = !m.connectionMatchesDetected()
}

// connectionMatchesDetected reports whether the current Endpoint/API Key inputs
// match the raw inputs the last successful detection ran against. Before any
// successful detection the persisted provider values are the baseline.
func (m *AdvancedConfigModel) connectionMatchesDetected() bool {
	if m.p == nil {
		return false
	}
	baselineEndpoint := m.detectedInputEndpoint
	baselineKey := m.detectedInputKey
	hasDetected := m.detectedInputEndpoint != "" || m.detectedInputKey != ""
	if !hasDetected {
		baselineEndpoint = strings.TrimSpace(m.p.Endpoint)
		baselineKey = m.p.APIKey
	}
	return strings.TrimSpace(m.urlInput.Value()) == strings.TrimSpace(baselineEndpoint) &&
		m.keyInput.Value() == baselineKey
}

// connectionUnchanged reports whether the endpoint/api-key inputs still match
// the provider that was last detected (or the persisted one).
func (m *AdvancedConfigModel) connectionUnchanged() bool {
	if m.p == nil {
		return false
	}
	endpointMatch := strings.TrimSpace(m.urlInput.Value()) == strings.TrimSpace(m.p.Endpoint)
	keyMatch := m.keyInput.Value() == m.p.APIKey
	return endpointMatch && keyMatch
}

func NewAdvancedConfigModel(p *provider.Provider) *AdvancedConfigModel {
	ui := textinput.New()
	ui.Prompt = ""
	ui.Placeholder = "https://api.openai.com/v1"
	ui.SetWidth(credentialInputWidth)
	ui.Focus()
	ui.SetValue(p.Endpoint)

	// The API key is a multi-line plaintext textarea (no password masking): the
	// user may paste or type a multi-line credential and see it as-is. Endpoint
	// stays a single-line textinput below.
	ki := textarea.New()
	ki.Prompt = ""
	ki.Placeholder = "sk-..."
	ki.ShowLineNumbers = false
	ki.SetWidth(credentialInputWidth)
	ki.SetHeight(2)
	ki.SetValue(p.APIKey)

	fi := textinput.New()
	fi.Placeholder = ""

	compactState := compactStateFromProvider(*p)
	m := &AdvancedConfigModel{
		p:                    p,
		oneMSlots:            make(map[string]bool),
		modelContextWindows:  make(map[string]int),
		modelDisplayMetadata: make(map[string]protocol.ModelInfo),
		compactPreset:        compactState.preset,
		compactState:         compactState,
		probeEndpoint:        p.Endpoint,
		probeAPIKey:          p.APIKey,
		page:                 4,
		cursor:               0,
		urlInput:             ui,
		keyInput:             ki,
		filterInput:          fi,
		IsActiveChosen:       true,
		clearStaleSlots:      true,
		connectionDirty:      false,
		modelAvailability:    make(map[string]modelAvailability),
	}

	cleanAndPopulate := func(modelStr *string, slotKey string) {
		if hasOneMSuffix(*modelStr) {
			m.oneMSlots[slotKey] = true
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
	if !m.usesOAuth() {
		pool := uniqueModels(parseModelList(m.p.Model))
		if len(pool) > 0 {
			m.modelPool = pool
			m.autoDetectOnOpen = true
		}
	}

	return m
}

func (m *AdvancedConfigModel) configureOAuthRuntime(endpoint, apiKey string) {
	m.probeEndpoint = endpoint
	m.probeAPIKey = apiKey
	m.connectionDirty = false
	m.cursor = m.mainRowIndex(rowTest)
	m.urlInput.Blur()
	m.keyInput.Blur()
}

func (m *AdvancedConfigModel) usesOAuth() bool {
	return m.p != nil && strings.TrimSpace(m.p.OAuthProvider) != ""
}

// textInputHasKeyboard 表示当前按键会被某个文本输入框消费。条件与本文件末尾
// 的输入路由保持一致：主页面光标停在 Endpoint/API Key 上，或模型筛选框聚焦。
// OAuth provider 没有可编辑的端点字段，因此不算。
func (m *AdvancedConfigModel) textInputHasKeyboard() bool {
	if m.usesOAuth() {
		return m.filterInput.Focused()
	}
	row := m.currentRow()
	return row == rowEndpoint || row == rowAPIKey || m.filterInput.Focused()
}

// vimNavAliases 是导航键的单字母别名。它们同时是合法的输入字符，因此在文本
// 输入框拥有键盘时必须让位，否则用户打不出这些字母。方向键没有这个歧义。
var vimNavAliases = map[string]bool{"q": true, "h": true, "j": true, "k": true, "l": true}

// NewAdvancedConfigModelAtPage1 creates a model starting at page 1 (slot mapping)
// with a pre-populated model pool, skipping the credential page.
func NewAdvancedConfigModelAtPage1(p *provider.Provider, modelPool []string) *AdvancedConfigModel {
	return NewAdvancedConfigModelAtPage1WithMetadata(p, modelPool, nil)
}

// NewAdvancedConfigModelAtPage1WithMetadata opens the slot mapper with rich
// catalog labels while keeping modelPool IDs as the selectable and persisted
// values.
func NewAdvancedConfigModelAtPage1WithMetadata(p *provider.Provider, modelPool []string, metadata map[string]protocol.ModelInfo) *AdvancedConfigModel {
	m := NewAdvancedConfigModel(p)
	m.page = 4
	m.modelPool = modelPool
	m.modelPoolFromDiscovery = true
	m.modelDisplayMetadata = copyModelInfoIndex(metadata)
	m.modelContextWindows = contextWindowsFromModelInfos(m.modelDisplayMetadata)
	m.urlInput.Blur()
	m.keyInput.Blur()
	m.cursor = m.mainRowIndex(rowOpus)
	return m
}

func (m *AdvancedConfigModel) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, textarea.Blink}
	// Re-verify an existing provider's connection on open. Until the check
	// succeeds the sections below Connection stay greyed out; OAuth providers
	// are always ready so they skip this.
	if m.autoDetectOnOpen && !m.usesOAuth() && strings.TrimSpace(m.probeEndpoint) != "" {
		m.detecting = true
		m.detectProgress = 5
		m.detectFrame = 0
		cmds = append(cmds, tea.Batch(modelFetchCmd(m.probeEndpoint, m.probeAPIKey), modelFetchTickCmd()))
	}
	return tea.Batch(cmds...)
}

func modelFetchTickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg {
		return modelFetchTickMsg{}
	})
}

func modelFetchCmd(endpoint, apiKey string) tea.Cmd {
	return func() tea.Msg {
		setDebugf("modelFetchCmd start endpoint=%q api_key_len=%d", endpoint, len(apiKey))
		result := detectProtocolAndModelsDetailed(endpoint, apiKey)
		setDebugf(
			"modelFetchCmd done endpoint=%q detected_endpoint=%q protocol=%q anthropic_auth=%q model_count=%d err=%v",
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
		if result.err == nil && result.protocol != "" && !provider.IsAnthropicType(result.protocol) {
			// Subscription runtimes only expose windows through the Codex catalog,
			// which AdvertisedContextWindows tries before the plain OpenAI list.
			advertised, source := claude.AdvertisedContextWindows(result.baseURL, apiKey)
			for id, window := range advertised {
				windows[id] = window
			}
			setDebugf("modelFetchCmd context windows catalog=%q count=%d", source, len(windows))
		}
		return modelFetchDoneMsg{
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
	}
}

func modelAvailabilityTickCmd(testID uint64) tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg {
		return modelAvailabilityTickMsg{testID: testID}
	})
}

func modelAvailabilityTestCmd(ctx context.Context, testID uint64, models []string, endpoint, apiKey, providerType, anthropicAuth, smokeTestModel string) tea.Cmd {
	models = append([]string(nil), models...)
	return func() tea.Msg {
		statuses := make(map[string]modelAvailability, len(models))
		if len(models) == 0 {
			return modelAvailabilityDoneMsg{testID: testID, statuses: statuses}
		}
		if smokeTestModel != "" {
			status := modelAvailabilityUnavailable
			if testSingleModelContext(ctx, smokeTestModel, endpoint, apiKey, providerType, anthropicAuth, 10*time.Second) {
				status = modelAvailabilityAvailable
			}
			if ctx.Err() == nil {
				for _, model := range models {
					statuses[model] = status
				}
			}
			return modelAvailabilityDoneMsg{testID: testID, statuses: statuses}
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
						if testSingleModelContext(ctx, model, endpoint, apiKey, providerType, anthropicAuth, 10*time.Second) {
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
		return modelAvailabilityDoneMsg{testID: testID, statuses: statuses}
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
	q := strings.ToLower(m.filterInput.Value())
	if q == "" {
		m.filteredPool = append([]string{locale.T("(设置为未设置/清空)", "(clear/unset)")}, m.modelPool...)
	} else {
		m.filteredPool = []string{}
		for _, mod := range m.modelPool {
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
	return modelReportLabel(stripOneMSuffix(id), m.modelDisplayMetadata)
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

// availabilityLabel renders the per-model availability badge for the picker.
func (m *AdvancedConfigModel) availabilityLabel(model string) string {
	switch m.availabilityFor(model) {
	case modelAvailabilityAvailable:
		return availableStyle.Render(locale.T("✓ 可用", "✓ available"))
	case modelAvailabilityUnavailable:
		return unavailableStyle.Render(locale.T("✗ 不可用", "✗ unavailable"))
	default:
		return grayText.Render(locale.T("? 未测试", "? not tested"))
	}
}

func (m *AdvancedConfigModel) availabilityCounts() (available, unavailable int) {
	for _, model := range m.modelPool {
		switch m.availabilityFor(model) {
		case modelAvailabilityAvailable:
			available++
		case modelAvailabilityUnavailable:
			unavailable++
		}
	}
	return available, unavailable
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
	m.urlInput.SetWidth(inputWidth)
	m.keyInput.SetWidth(inputWidth)
	m.filterInput.SetWidth(inputWidth)
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
	if !m.modelPoolFromDiscovery {
		return 0
	}
	count := 0
	for _, slot := range advancedSlotRefs(m.p) {
		model := strings.TrimSpace(*slot.ptr)
		if model != "" && !stringInSlice(model, m.modelPool) {
			count++
		}
	}
	return count
}

func (m *AdvancedConfigModel) applyStaleSlotPolicy() {
	if !m.clearStaleSlots || !m.modelPoolFromDiscovery {
		return
	}

	cleared := 0
	for _, slot := range advancedSlotRefs(m.p) {
		model := strings.TrimSpace(*slot.ptr)
		if model == "" || stringInSlice(model, m.modelPool) {
			continue
		}
		*slot.ptr = ""
		delete(m.oneMSlots, slot.key)
		cleared++
	}
	if cleared > 0 {
		setDebugf("applyStaleSlotPolicy cleared=%d slots=%s one_m=%s", cleared, slotDebugSummary(*m.p), reviewOneMSummary(m.oneMSlots))
	}
}

// 实时获取/检测协议名称
func (m *AdvancedConfigModel) getProtocol() string {
	if m.p.Type != "" {
		return provider.ProtocolLabelForProvider(*m.p)
	}
	if strings.Contains(strings.ToLower(m.urlInput.Value()), "anthropic") {
		return "anthropic"
	}
	return "openai(chat)"
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
	if strings.Contains(strings.ToLower(m.urlInput.Value()), "anthropic") {
		return "Anthropic"
	}
	return "OpenAI"
}

// canToggleOpenAIProtocol is true when the provider is a manual OpenAI-compatible
// API-key endpoint. OAuth backends ignore options.Protocol in StartProvider and
// always use their fixed Chat/Responses path, so the review page must not offer
// a toggle that only changes a label.
func (m *AdvancedConfigModel) canToggleOpenAIProtocol() bool {
	if m.p == nil || !provider.IsOpenAICompatibleType(m.p.Type) {
		return false
	}
	return strings.TrimSpace(m.p.OAuthProvider) == ""
}

// maxOutputUpstreamManaged is true when the selected path has no supported
// upstream output-limit field. ChatGPT OAuth remains editable because the value
// still configures Claude Code's client limit; Kiro's Amazon Q request schema
// has no equivalent for Anthropic max_tokens.
func (m *AdvancedConfigModel) maxOutputUpstreamManaged() bool {
	if m.p == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(m.p.OAuthProvider)) {
	case "codex", "copilot", "kiro":
		return true
	}
	// Gemini OAuth (antigravity) maps Claude max_tokens → generationConfig.maxOutputTokens.
	if provider.IsOpenAIResponsesType(m.p.Type) && protocol.IsCodexBaseEndpoint(m.p.Endpoint) {
		return true
	}
	return false
}

func (m *AdvancedConfigModel) toggleOpenAIProtocol() {
	if m.p == nil || !m.canToggleOpenAIProtocol() {
		return
	}
	if provider.IsOpenAIResponsesType(m.p.Type) {
		m.p.Type = "openai"
	} else {
		m.p.Type = "openai_responses"
	}
	setDebugf("page4 protocol toggled type=%q label=%q", m.p.Type, provider.ProtocolLabel(m.p.Type))
}

// Page 4 cursor model (editable summary):
// optional protocol, optional Fast, then Compact / MaxOutput / Tools / ToolSearch,
// active checkbox, Apply, Back.
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
	window, ok := m.modelContextWindows[strings.ToLower(stripOneMSuffix(modelVal))]
	if !ok || window <= 0 {
		return 0, false
	}
	return window, true
}

// slotModelForIndex returns the model configured in the page-2 row order.
// canEditFastMode reports whether the Fast (Claude Code /fast) pin can be
// toggled. It is a settings.json fastMode flag that every provider can carry; it
// only has a speed effect for the GPT/Codex OAuth backend, but setting it for
// others is harmless.
func (m *AdvancedConfigModel) canEditFastMode() bool {
	return m.p != nil
}

// Context sizing has no row here: it is expressed per slot by [1m] on page 2,
// and ccl writes no session-wide context value that could be edited.
// skipDisabledPage4Cursor moves past rows that are not interactive.
// direction: +1 when moving down/tab, -1 when moving up/shift-tab.
// Runtime option cycles. Index 0 is always "Default" (delete managed env).
var (
	reviewMaxOutOptions = []string{"", "16000", "32000", "64000", "128000"}
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

func (m *AdvancedConfigModel) reviewMaxOutValue() string {
	if m.p.Env == nil {
		return ""
	}
	if v, err := claude.NormalizeMaxOutputTokens(m.p.Env[claude.MaxOutputTokensEnv]); err == nil {
		// Only treat known cycle values as selected; custom stays as-is via display.
		for _, opt := range reviewMaxOutOptions {
			if opt == v {
				return v
			}
		}
		return v
	}
	return ""
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

func formatMaxOutLabel(value string) string {
	switch value {
	case "":
		return formatEditableValue("32K", true)
	case "16000":
		return formatEditableValue("16K", false)
	case "32000":
		return formatEditableValue("32K", false)
	case "64000":
		return formatEditableValue("64K", false)
	case "128000":
		return formatEditableValue("128K", false)
	default:
		return formatEditableValue(value, false)
	}
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
	if !m.connectionReady() {
		return
	}
	switch m.currentRow() {
	case rowContext:
		m.cycleCompactPreset(delta)
	case rowProtocol:
		m.toggleOpenAIProtocol()
	case rowFast:
		if !m.canEditFastMode() {
			return
		}
		// Toggle like Protocol; left/right/enter all flip the pin.
		m.p.FastMode = !m.p.FastMode
		setDebugf("fast toggled fast_mode=%t", m.p.FastMode)
	case rowMaxOutput:
		if m.maxOutputUpstreamManaged() {
			return
		}
		cur := m.reviewMaxOutValue()
		// Snap unknown custom values into the cycle at Default.
		known := false
		for _, opt := range reviewMaxOutOptions {
			if opt == cur {
				known = true
				break
			}
		}
		if !known {
			cur = ""
		}
		next := cycleStringOption(cur, reviewMaxOutOptions, delta)
		setProviderEnvValue(m.p, claude.MaxOutputTokensEnv, next)
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

// workflowStep keeps the visible flow independent from the internal page IDs.
// Page 5 is the config-mode choice shown immediately after credentials.
// Reasoning Effort (old page 3) is no longer part of ccl set — Claude Code
// manages effort natively via /effort, --effort, and settings.
// selectedCompactRadioIndex maps the current compact state onto the radio list.
// Custom/legacy/unknown states land on the Custom radio.
// selectCompactPreset sets the provider-wide compact budget from a radio index.
// Per-slot [1m] markers are independent and never cleared here.
// cycleCompactPreset moves the provider-wide compact budget one step through the
// radio order (Claude default ↔ Custom). Used by the Context row on the single
// page; per-slot [1m] markers are independent and never cleared here.
func (m *AdvancedConfigModel) cycleCompactPreset(delta int) {
	if delta == 0 {
		return
	}
	current := m.compactPreset
	if m.compactState.custom || m.compactState.legacy {
		current = compactPresetPreserve
	}
	idx := -1
	for i, p := range compactRadioOrder {
		if p == current {
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = 0
	}
	next := (idx + delta) % len(compactRadioOrder)
	if next < 0 {
		next += len(compactRadioOrder)
	}
	m.selectCompactPreset(next)
	setDebugf("cycle compact preset delta=%d preset=%v summary=%s", delta, m.compactPreset, m.compactSummary())
}

func (m *AdvancedConfigModel) selectCompactPreset(radioIdx int) {
	if radioIdx < 0 || radioIdx >= len(compactRadioOrder) {
		return
	}
	m.compactPreset = compactRadioOrder[radioIdx]
	m.compactState = compactConfigState{preset: m.compactPreset}
	// Custom promises the hand-set context env survives; the launcher only honors
	// that promise when the provider opts out of ccl's context policy.
	if m.p == nil {
		return
	}
	if m.compactPreset == compactPresetPreserve {
		ensureProviderEnvMap(m.p)
		m.p.Env[provider.EnvContextBudgetMode] = provider.ContextBudgetManual
		return
	}
	if m.p.Env != nil {
		delete(m.p.Env, provider.EnvContextBudgetMode)
		if len(m.p.Env) == 0 {
			m.p.Env = nil
		}
	}
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
		if m.oneMSlots[slot.key] != enabled {
			m.oneMSlots[slot.key] = enabled
			synced++
		}
	}
	return synced
}

func (m *AdvancedConfigModel) compactSummary() string {
	state := m.compactState
	state.preset = m.compactPreset
	if m.compactPreset != compactPresetPreserve {
		state.legacy = false
		state.custom = false
	}
	summary := compactStateSummary(state, m.oneMSlots)
	if m.p != nil {
		summary += backendManagedContextNote(*m.p)
	}
	return summary
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

func (m *AdvancedConfigModel) applyModelDetectionResult(detectedType, discoveredModelsRaw, anthropicAuth, detectedEndpoint string, derr error) tea.Cmd {
	discoveredModels := uniqueModels(parseModelList(discoveredModelsRaw))
	m.hadLocalModelPool = countCSV(m.p.Model) > 0
	// Capture the raw detection inputs before probeEndpoint is normalized below,
	// so the successful-detection baseline is what the user actually typed.
	detectedInputEndpoint := m.probeEndpoint
	detectedInputKey := m.probeAPIKey
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
		m.probeEndpoint = detectedEndpoint
	}
	if !m.usesOAuth() && detectedType != "" {
		m.p.Type = detectedType
		if detectedType == "openai" && protocol.IsCodexBaseEndpoint(m.p.Endpoint) {
			m.p.Type = "openai_responses"
		}
		m.p.AnthropicAuth = ""
	}
	if !m.usesOAuth() && detectedType == "anthropic" {
		m.p.Endpoint = protocol.NormalizeAnthropicBaseURLForClaude(m.p.Endpoint)
		if anthropicAuth != "" {
			m.p.AnthropicAuth = anthropicAuth
		}
	}

	m.modelPool = []string{}
	m.modelPoolFromDiscovery = false
	m.modelAvailability = make(map[string]modelAvailability)
	m.modelTesting = false
	m.modelTestCancel = nil
	m.modelTestCanceled = false
	if derr == nil && len(discoveredModels) > 0 {
		m.modelPool = discoveredModels
		m.modelPoolFromDiscovery = true
		m.p.Model = strings.Join(discoveredModels, ",")
		setDebugf("applyModelDetectionResult using discovered model pool count=%d", len(m.modelPool))
	}

	if derr != nil {
		m.detectionError = derr
		m.cursor = m.mainRowIndex(rowTest)
		setDebugf("applyModelDetectionResult detection failed detection_error=%v model_count=%d", m.detectionError, len(m.modelPool))
		return nil
	}

	// 本次 set 必须以接口返回的模型为准；不再用旧的本地模型池兜底。
	if len(m.modelPool) == 0 {
		m.detectionError = fmt.Errorf("%s", locale.T(
			"未从接口获取到任何可用模型，未使用本地旧模型池",
			"no models were fetched from the provider API; local cached models were not used",
		))
		m.cursor = m.mainRowIndex(rowTest)
		setDebugf("applyModelDetectionResult no models detection_error=%v", m.detectionError)
		return nil
	}

	sort.Strings(m.modelPool)
	// Single page: detection success auto-configures the slots and stays on the
	// page. Connection is now the detected endpoint, so it is no longer dirty.
	m.connectionDirty = false
	// Record the inputs this successful detection ran against (the raw values
	// before endpoint normalization) as the save baseline. Reverting an edit or
	// cancelling a re-detection naturally returns the page to this state.
	m.detectedInputEndpoint = detectedInputEndpoint
	m.detectedInputKey = detectedInputKey
	m.applyRecommendation()
	m.cursor = m.mainRowIndex(rowOpus)
	setDebugf(
		"applyModelDetectionResult success provider_type=%q endpoint=%q anthropic_auth=%q model_count=%d stale_slot_count=%d clear_stale_slots=%t cursor=%d",
		m.p.Type,
		m.p.Endpoint,
		m.p.AnthropicAuth,
		len(m.modelPool),
		m.staleSlotCount(),
		m.clearStaleSlots,
		m.cursor,
	)
	return nil
}

// applyRecommendation fills empty slots from the auto recommendation engine and
// records that the config was auto-configured. User-edited fields (identified by
// not matching the recommendation) are left alone.
func (m *AdvancedConfigModel) applyRecommendation() {
	rec := RecommendModels(*m.p, m.modelPool, m.modelDisplayMetadata)
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
		if m.oneMSlots[key] == on {
			continue
		}
		m.oneMSlots[key] = on
	}
	m.autoConfigured = true
	m.detectionError = nil
	m.connectionDirty = false
	m.hadLocalModelPool = countCSV(m.p.Model) > 0
	setDebugf("applyRecommendation slots=%s one_m=%s", slotDebugSummary(*m.p), reviewOneMSummary(m.oneMSlots))
}

// activateRow fires the action for a button row on click or Enter: Auto
// Configure starts the connection check, Test Model Availability starts the
// per-model probes. Returns the tea.Cmd to run, or nil for no-op.
func (m *AdvancedConfigModel) activateRow(kind configRowKind) tea.Cmd {
	switch kind {
	case rowTest:
		// Start detection with the current input values (OAuth uses the session
		// runtime endpoint/key already injected by configureOAuthRuntime).
		if !m.usesOAuth() {
			m.p.Endpoint = m.urlInput.Value()
			m.p.APIKey = m.keyInput.Value()
			m.probeEndpoint = m.p.Endpoint
			m.probeAPIKey = m.p.APIKey
			// The inputs being detected become the new baseline only if the
			// detection succeeds; until then keep the dirty state honest.
			m.refreshConnectionDirty()
		}
		m.urlInput.Blur()
		m.keyInput.Blur()
		m.detectionError = nil
		m.detecting = true
		m.detectProgress = 5
		m.detectFrame = 0
		setDebugf("start detection endpoint=%q api_key_len=%d oauth=%t", m.probeEndpoint, len(m.probeAPIKey), m.usesOAuth())
		return tea.Batch(modelFetchCmd(m.probeEndpoint, m.probeAPIKey), modelFetchTickCmd())
	case rowTestModels:
		if !m.connectionReady() {
			return nil
		}
		if len(m.modelPool) == 0 {
			setDebugf("model availability test skipped: empty pool")
			return nil
		}
		m.modelTestID++
		testID := m.modelTestID
		ctx, cancel := context.WithCancel(context.Background())
		m.modelTesting = true
		m.modelTestCancel = cancel
		m.modelTestFrame = 0
		m.modelTestCanceled = false
		setDebugf("model availability test started model_count=%d", len(m.modelPool))
		return tea.Batch(
			modelAvailabilityTestCmd(ctx, testID, m.modelPool, m.probeEndpoint, m.probeAPIKey, m.p.Type, m.p.AnthropicAuth, m.availabilitySmokeTestModel()),
			modelAvailabilityTickCmd(testID),
		)
	}
	return nil
}

func (m *AdvancedConfigModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.updateInputWidths()
		return m, nil

	case copiedClearMsg:
		m.keyCopied = false
		return m, nil

	case urlCopiedClearMsg:
		m.urlCopied = false
		return m, nil

	case focusRowMsg:
		// Click semantics: the first click on a row selects it (moves the cursor);
		// a second click on the already-selected row performs its action. Endpoint
		// and API Key focus their text inputs on first click so typing lands there.
		if msg.row == rowCopyKey || msg.row == rowCopyURL {
			// A single click on a value row focuses its input; a double-click
			// (second click on the same row within the window) copies the value.
			now := time.Now()
			double := msg.row == m.lastCopyClickRow && now.Sub(m.lastCopyClickAt) < 500*time.Millisecond
			m.lastCopyClickRow = msg.row
			m.lastCopyClickAt = now
			if !double {
				focus := rowAPIKey
				if msg.row == rowCopyURL {
					focus = rowEndpoint
				}
				m.cursor = m.mainRowIndex(focus)
				m.urlInput.Blur()
				m.keyInput.Blur()
				return m, nil
			}
			if msg.row == rowCopyKey {
				m.keyCopied = true
				if err := clipboard.WriteAll(m.keyInput.Value()); err != nil {
					setDebugf("copy key to clipboard failed: %v", err)
				}
				setDebugf("key copied to clipboard")
				return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return copiedClearMsg{} })
			}
			m.urlCopied = true
			if err := clipboard.WriteAll(m.urlInput.Value()); err != nil {
				setDebugf("copy url to clipboard failed: %v", err)
			}
			setDebugf("url copied to clipboard")
			return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return urlCopiedClearMsg{} })
		}
		idx := m.mainRowIndex(msg.row)
		if idx < 0 {
			return m, nil
		}
		alreadySelected := m.cursor == idx && !m.textInputHasKeyboard()
		m.cursor = idx
		m.keepCursorVisible()
		switch msg.row {
		case rowEndpoint:
			m.keyInput.Blur()
			return m, m.urlInput.Focus()
		case rowAPIKey:
			m.urlInput.Blur()
			return m, m.keyInput.Focus()
		case rowTest, rowTestModels:
			m.urlInput.Blur()
			m.keyInput.Blur()
			if alreadySelected {
				return m, m.activateRow(msg.row)
			}
			return m, nil
		case rowSave:
			m.urlInput.Blur()
			m.keyInput.Blur()
			if alreadySelected {
				if !m.canSave() {
					setDebugf("click save blocked: connection dirty or undetected")
					return m, nil
				}
				m.compactState = compactConfigState{preset: m.compactPreset}
				m.saveConfirmed = true
				setDebugf("click save requested provider=%q", m.p.Name)
				return m, tea.Quit
			}
			return m, nil
		case rowCancel:
			m.urlInput.Blur()
			m.keyInput.Blur()
			if alreadySelected {
				setDebugf("click cancel requested")
				return m, tea.Quit
			}
			return m, nil
		default:
			m.urlInput.Blur()
			m.keyInput.Blur()
			m.filterInput.Blur()
		}
		return m, nil

	case modelFetchTickMsg:
		if !m.detecting {
			return m, nil
		}
		m.detectFrame++
		if m.detectProgress < 95 {
			m.detectProgress += 3
			if m.detectProgress > 95 {
				m.detectProgress = 95
			}
		}
		return m, modelFetchTickCmd()

	case modelFetchDoneMsg:
		if !m.detecting || msg.endpoint != m.probeEndpoint || msg.apiKey != m.probeAPIKey {
			setDebugf(
				"modelFetchDone ignored detecting=%t endpoint_match=%t api_key_match=%t msg_endpoint=%q probe_endpoint=%q",
				m.detecting,
				msg.endpoint == m.probeEndpoint,
				msg.apiKey == m.probeAPIKey,
				msg.endpoint,
				m.probeEndpoint,
			)
			return m, nil
		}
		m.detectProgress = 100
		m.detecting = false
		setDebugf(
			"modelFetchDone accepted detected_type=%q detected_endpoint=%q anthropic_auth=%q model_count=%d err=%v",
			msg.detectedType,
			msg.detectedEndpoint,
			msg.anthropicAuth,
			countCSV(msg.discoveredModelsRaw),
			msg.err,
		)
		if msg.contextWindows != nil {
			m.modelContextWindows = msg.contextWindows
		}
		m.modelDisplayMetadata = indexModelInfos(msg.modelInfos)
		for id, window := range contextWindowsFromModelInfos(m.modelDisplayMetadata) {
			if _, exists := m.modelContextWindows[id]; !exists {
				m.modelContextWindows[id] = window
			}
		}
		return m, m.applyModelDetectionResult(msg.detectedType, msg.discoveredModelsRaw, msg.anthropicAuth, msg.detectedEndpoint, msg.err)

	case modelAvailabilityDoneMsg:
		if !m.modelTesting || msg.testID != m.modelTestID {
			return m, nil
		}
		m.modelTesting = false
		m.modelTestCancel = nil
		m.modelTestCanceled = false
		m.modelAvailability = msg.statuses
		m.modelPool = reorderModelsByAvailability(m.modelPool, m.modelAvailability)
		m.p.Model = strings.Join(m.modelPool, ",")
		m.updateFilteredPool()
		available, unavailable := m.availabilityCounts()
		setDebugf("model availability test finished model_count=%d available=%d unavailable=%d", len(m.modelPool), available, unavailable)
		return m, nil

	case modelAvailabilityTickMsg:
		if !m.modelTesting || msg.testID != m.modelTestID {
			return m, nil
		}
		m.modelTestFrame++
		return m, modelAvailabilityTickCmd(msg.testID)

	case tea.KeyMsg:
		// 文本输入框拥有键盘时，单字母导航别名让位给输入本身：把它们清空，
		// 下面两个 switch 都不会匹配，按键最终落到文件末尾的输入路由。
		// ctrl+c 与方向键不受影响，esc 仍由各页自行处理。
		key := msg.String()
		if vimNavAliases[key] && m.textInputHasKeyboard() {
			key = ""
		}

		switch key {
		case "ctrl+c", "q":
			return m, tea.Quit
		}

		if m.detecting {
			// Allow esc to abort a connection check so the user is never frozen
			// out of the page; ctrl+c/q still quit above.
			if key == "esc" {
				m.detecting = false
				m.detectionError = fmt.Errorf("%s", locale.T("已取消连接检查", "connection check canceled"))
				m.cursor = m.mainRowIndex(rowTest)
				setDebugf("connection check canceled by user")
				return m, nil
			}
			return m, nil
		}
		if m.modelTesting {
			if msg.String() == "esc" {
				if m.modelTestCancel != nil {
					m.modelTestCancel()
				}
				m.modelTesting = false
				m.modelTestCancel = nil
				m.modelTestCanceled = true
				setDebugf("model availability test canceled test_id=%d", m.modelTestID)
			}
			return m, nil
		}
		switch key {
		case "esc":
			// Single page: esc quits (unless the model picker overlay is open).
			if m.filterInput.Focused() {
				m.filterInput.Blur()
				setDebugf("esc closed slot picker active_slot=%d cursor=%d", m.activeSlot, m.cursor)
				return m, nil
			}
			setDebugf("esc quit cursor=%d endpoint_set=%t api_key_len=%d", m.cursor, strings.TrimSpace(m.urlInput.Value()) != "", len(m.keyInput.Value()))
			return m, tea.Quit

		case "up", "k":
			if m.filterInput.Focused() {
				if m.slotListCursor > 0 {
					m.slotListCursor--
					if m.slotListCursor < m.filterWindowStart {
						m.filterWindowStart = m.slotListCursor
					}
				}
				return m, nil
			}
			rows := m.visibleRows()
			if len(rows) == 0 {
				return m, nil
			}
			if m.cursor > 0 {
				m.cursor--
			} else {
				m.cursor = len(rows) - 1
			}
			m.keepCursorVisible()

		case "down", "j":
			if m.filterInput.Focused() {
				if m.slotListCursor < len(m.filteredPool)-1 {
					m.slotListCursor++
					if m.slotListCursor >= m.filterWindowStart+filterViewHeight {
						m.filterWindowStart = m.slotListCursor - filterViewHeight + 1
					}
				}
				return m, nil
			}
			rows := m.visibleRows()
			if len(rows) == 0 {
				return m, nil
			}
			if m.cursor < len(rows)-1 {
				m.cursor++
			} else {
				m.cursor = 0
			}
			m.keepCursorVisible()

		case "left", "h":
			if m.filterInput.Focused() {
				return m, nil
			}
			if m.isModelRow(m.currentRow()) {
				m.toggleOneMAtRow(m.currentRow())
			} else {
				switch m.currentRow() {
				case rowContext, rowProtocol, rowFast, rowMaxOutput, rowTools, rowToolSearch, rowActive:
					m.adjustReviewField(-1)
				}
			}

		case "right", "l":
			if m.filterInput.Focused() {
				return m, nil
			}
			if m.isModelRow(m.currentRow()) {
				m.toggleOneMAtRow(m.currentRow())
			} else {
				switch m.currentRow() {
				case rowContext, rowProtocol, rowFast, rowMaxOutput, rowTools, rowToolSearch, rowActive:
					m.adjustReviewField(1)
				}
			}

		case "space":
			// Toggle the 1M context marker on a model row. Turning it on is refused
			// when the backend window rules 1M out; turning an existing marker off
			// stays possible.
			if m.filterInput.Focused() {
				return m, nil
			}
			m.toggleOneMAtRow(m.currentRow())

		case "tab":
			if m.filterInput.Focused() {
				if m.slotListCursor < len(m.filteredPool)-1 {
					m.slotListCursor++
					if m.slotListCursor >= m.filterWindowStart+filterViewHeight {
						m.filterWindowStart = m.slotListCursor - filterViewHeight + 1
					}
				}
				return m, nil
			}
			rows := m.visibleRows()
			if len(rows) == 0 {
				return m, nil
			}
			m.cursor = (m.cursor + 1) % len(rows)
			m.keepCursorVisible()

		case "shift+tab":
			if m.filterInput.Focused() {
				return m, nil
			}
			rows := m.visibleRows()
			if len(rows) == 0 {
				return m, nil
			}
			m.cursor--
			if m.cursor < 0 {
				m.cursor = len(rows) - 1
			}

		case "enter":
			if m.filterInput.Focused() {
				// Model picker selection.
				if len(m.filteredPool) == 0 {
					return m, nil
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
				if m.oneMSlots[slotKey] && m.oneMSlotBlocked(selectedModel) {
					m.oneMSlots[slotKey] = false
					setDebugf("slot model changed to a non-1M model; cleared 1M marker slot=%s model=%q", slotKey, selectedModel)
				}
				m.filterInput.Blur()
				m.autoConfigured = false
				setDebugf("slot selected active_slot=%d model=%q slots=%s", m.activeSlot, selectedModel, slotDebugSummary(*m.p))
				return m, nil
			}

			// The API key textarea inserts newlines with Enter, so while it is
			// focused the key must fall through to the input routing below rather
			// than advance the page cursor.
			if m.keyInput.Focused() {
				break
			}

			switch m.currentRow() {
			case rowEndpoint:
				m.cursor = m.mainRowIndex(rowAPIKey)
				m.urlInput.Blur()
				m.keyInput.Focus()
				setDebugf("enter endpoint -> api key endpoint=%q", m.urlInput.Value())
				// Return so the Enter that moved focus here is not re-delivered to
				// the freshly focused textarea (which would insert a newline).
				return m, nil
			case rowAPIKey:
				m.cursor = m.mainRowIndex(rowTest)
				m.urlInput.Blur()
				m.keyInput.Blur()
				setDebugf("enter api key -> test api_key_len=%d", len(m.keyInput.Value()))
			case rowTest:
				return m, m.activateRow(rowTest)
			case rowProtocol, rowFast, rowMaxOutput, rowTools, rowToolSearch:
				m.adjustReviewField(1)
			case rowOpus, rowSonnet, rowHaiku, rowCustom, rowSubagent:
				if !m.connectionReady() {
					return m, nil
				}
				m.activeSlot = slotForRow(m.currentRow())
				m.filterInput.Focus()
				m.filterInput.SetValue("")
				m.slotListCursor = 0
				m.updateFilteredPool()
				setDebugf("open slot picker active_slot=%d filtered_count=%d", m.activeSlot, len(m.filteredPool))
			case rowTestModels:
				return m, m.activateRow(rowTestModels)
			case rowContext:
				// Context & Compact is edited inline; nothing to open yet.
				setDebugf("context row selected")
			case rowActive:
				m.IsActiveChosen = !m.IsActiveChosen
				setDebugf("active choice toggled active_chosen=%t", m.IsActiveChosen)
			case rowSave:
				if !m.canSave() {
					setDebugf("save blocked: connection dirty or undetected")
					return m, nil
				}
				m.compactState = compactConfigState{preset: m.compactPreset}
				m.saveConfirmed = true
				setDebugf("save requested provider=%q type=%q model_count=%d slots=%s one_m=%s compact=%s active_chosen=%t fast_mode=%t", m.p.Name, m.p.Type, countCSV(m.p.Model), slotDebugSummary(*m.p), reviewOneMSummary(m.oneMSlots), m.compactSummary(), m.IsActiveChosen, m.p.FastMode)
				return m, tea.Quit
			case rowCancel:
				setDebugf("cancel requested")
				return m, tea.Quit
			}
			m.keepCursorVisible()
		}
	}

	// 让光标位置与输入框焦点保持同步：只有获得焦点的输入框才会处理
	// 按键和粘贴（textinput.Update 在未聚焦时会直接返回）。
	switch {
	case m.filterInput.Focused():
		m.filterInput, cmd = m.filterInput.Update(msg)
		m.updateFilteredPool()
	case m.currentRow() == rowEndpoint && !m.usesOAuth():
		if !m.urlInput.Focused() {
			m.keyInput.Blur()
			cmd = m.urlInput.Focus()
		}
		var updateCmd tea.Cmd
		m.urlInput, updateCmd = m.urlInput.Update(msg)
		cmd = tea.Batch(cmd, updateCmd)
		m.refreshConnectionDirty()
	case m.currentRow() == rowAPIKey && !m.usesOAuth():
		if !m.keyInput.Focused() {
			m.urlInput.Blur()
			cmd = m.keyInput.Focus()
		}
		var updateCmd tea.Cmd
		m.keyInput, updateCmd = m.keyInput.Update(msg)
		cmd = tea.Batch(cmd, updateCmd)
		m.refreshConnectionDirty()
	default:
		// 光标在按钮或只读行上时，取消两个输入框的焦点
		m.urlInput.Blur()
		m.keyInput.Blur()
	}

	return m, cmd
}

func renderModelFetchProgress(progress, frame int, oauth bool) string {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spin := spinners[frame%len(spinners)]
	label := locale.T("Connecting...", "Connecting...")
	if oauth {
		label = locale.T("Connecting via OAuth...", "Connecting via OAuth...")
	}
	return "\n" +
		selectedStyle.Render(fmt.Sprintf("%s %s", spin, label)) + "\n" +
		grayText.Render(locale.T("请稍候，正在验证连接", "Please wait while the connection is verified")) + "\n"
}

// focusCredentialFieldMsg asks the model to focus one of the credential inputs.
// focusRowMsg asks the model to move the cursor to a specific configuration row,
// clicked in the rendered frame. The mouse handler reports intent as a message
// instead of mutating the model from the view.
type focusRowMsg struct{ row configRowKind }

func renderCredentialField(label, value string, focused bool) string {
	prefix := "  "
	labelText := purpleText.Render(label)
	if focused {
		prefix = selectedStyle.Render("> ")
		labelText = selectedStyle.Render(label)
	}
	return fmt.Sprintf("%s%s\n  %s\n\n", prefix, labelText, value)
}

func (m *AdvancedConfigModel) renderPageHeader(title, badge string) string {
	line := titleStyle.Render(title) + badgeStyle.Render(badge)
	// Show the protocol family in the header until a detection has pinned it.
	if !m.modelPoolFromDiscovery && !m.usesOAuth() {
		line += protoBadgeStyle.Render("Protocol: " + m.getProtocolFamily())
	}
	dividerWidth := max(m.panelWidth()-6, 16)
	return line + "\n" + dividerStyle.Render(strings.Repeat("─", dividerWidth)) + "\n\n"
}

// truncateMiddle keeps endpoint/model names on one line for the review page.
// Width is measured in terminal cells via lipgloss (ANSI-aware, Unicode-aware).
func truncateMiddle(s string, max int) string {
	s = strings.TrimSpace(s)
	if max < 8 || lipgloss.Width(s) <= max {
		return s
	}
	runess := []rune(s)
	ellipsis := "…"
	budget := max - lipgloss.Width(ellipsis)
	if budget < 2 {
		return ellipsis
	}
	leftBudget := budget / 2
	rightBudget := budget - leftBudget

	var left string
	for _, r := range runess {
		cand := left + string(r)
		if lipgloss.Width(cand) > leftBudget {
			break
		}
		left = cand
	}
	var right string
	for i := len(runess) - 1; i >= 0; i-- {
		cand := string(runess[i]) + right
		if lipgloss.Width(cand) > rightBudget {
			break
		}
		right = cand
	}
	return left + ellipsis + right
}

func (m *AdvancedConfigModel) View() tea.View {
	// Model picker overlay: when the filter input has focus, render only the
	// filtered model list (search + availability) instead of the main page.
	if m.filterInput.Focused() {
		return m.viewModelPicker()
	}

	var body strings.Builder

	// ── Title bar ──────────────────────────────────────────────────────────
	title := locale.T("Provider 配置", "Provider Configuration")
	badge := "Config"
	if m.usesOAuth() {
		badge = "OAuth"
	}
	body.WriteString(m.renderPageHeader(title, badge))

	// ── Connection ─────────────────────────────────────────────────────────
	body.WriteString(titleStyle.Render("Connection") + "\n")
	if m.usesOAuth() {
		body.WriteString(fmt.Sprintf("  %-12s %s\n", "Provider", cyanText.Render(m.p.OAuthProvider)))
		if m.p.AuthGroup != "" {
			body.WriteString(fmt.Sprintf("  %-12s %s\n", "Group", cyanText.Render(m.p.AuthGroup)))
			body.WriteString(fmt.Sprintf("  %-12s %s\n", "Accounts", cyanText.Render(fmt.Sprintf("%d", len(m.p.OAuthAccountCredentials)))))
		}
		body.WriteString(fmt.Sprintf("  %-12s %s\n", "Fast", cyanText.Render(providerFastSummary(*m.p))))
		body.WriteString(fmt.Sprintf("  %-12s %s\n", "Auth", availableStyle.Render(providerAuthLabel(*m.p))))
		body.WriteString(fmt.Sprintf("  %-12s %s\n", "Local Proxy", availableStyle.Render(locale.T("已就绪（仅本次会话）", "Ready (this session only)"))))
	} else {
		copiedHint := ""
		if m.keyCopied {
			copiedHint = "  " + availableStyle.Render(locale.T("✓ 已复制", "✓ copied"))
		}
		urlCopiedHint := ""
		if m.urlCopied {
			urlCopiedHint = "  " + availableStyle.Render(locale.T("✓ 已复制", "✓ copied"))
		}
		// Endpoint is a single-line text input; the API key is a multi-line
		// plaintext textarea (no masking). Each renders its value directly, and
		// double-clicking a value row copies the full value. Trailing blank lines
		// from the textarea's fixed height are trimmed so the field does not
		// consume extra rows in the panel.
		body.WriteString(renderCredentialField("Endpoint URL", m.urlInput.View()+urlCopiedHint, false))
		body.WriteString(renderCredentialField("API Key", strings.TrimRight(m.keyInput.View(), "\n")+copiedHint, false))
	}

	// Detection / auto-configure button.
	if m.detecting {
		body.WriteString(renderModelFetchProgress(m.detectProgress, m.detectFrame, m.usesOAuth()))
	} else {
		testLabel := locale.T("Auto Configure", "Auto Configure")
		testStr := buttonStyle.Render(testLabel)
		if m.cursor == m.mainRowIndex(rowTest) {
			testStr = buttonActiveStyle.Render(testLabel)
		}
		body.WriteString("  " + testStr + "\n")

		if m.detectionError != nil {
			errorWidth := max(m.panelWidth()-8, 20)
			body.WriteString(errorBoxStyle.Width(errorWidth).Render(locale.T("检测失败，无法继续", "Detection failed; cannot continue")+"\n"+m.detectionError.Error()) + "\n\n")
		} else if m.modelPoolFromDiscovery {
			status := fmt.Sprintf(locale.T("✓ 已连接 · %s · %d 个模型", "✓ Connected · %s · %d models"), provider.ProtocolLabelForProvider(*m.p), len(m.modelPool))
			body.WriteString(availableStyle.Render(status) + "\n")
			if !m.usesOAuth() {
				body.WriteString(fmt.Sprintf("  %-12s %s\n", "Auth", availableStyle.Render(providerAuthLabel(*m.p))))
			}
		}
	}

	// ── Model Mapping (only after detection) ──────────────────────────────
	{
		body.WriteString("\n" + titleStyle.Render("Model Mapping") + "\n")
		// (the block always renders; rows grey out until connectionReady)
		ready := m.connectionReady()
		renderMappingRow := func(kind configRowKind, label, display, modelID string, oneM bool) {
			prefix := "  "
			val := purpleText.Render(truncateMiddle(display, 52))
			if !ready {
				// Connection not ready: grey out the row, no focus affordance.
				val = grayText.Render(truncateMiddle(display, 52))
			} else if m.cursor == m.mainRowIndex(kind) {
				prefix = selectedStyle.Render("> ")
				val = selectedStyle.Render(truncateMiddle(display, 52))
			}
			badge := "    "
			if oneM {
				badge = lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render("[1M]")
			}
			if !ready {
				badge = grayText.Render("  ")
			}
			// Availability badge, shown only after the optional test ran.
			if status, ok := m.modelAvailability[modelID]; ok && status != modelAvailabilityUnknown {
				switch status {
				case modelAvailabilityAvailable:
					badge = availableStyle.Render("✓") + " "
				case modelAvailabilityUnavailable:
					badge = unavailableStyle.Render("✗") + " "
				}
			}
			body.WriteString(fmt.Sprintf("%s%-10s %s %s\n", prefix, label, val, badge))
		}
		renderMappingRow(rowOpus, "Opus", m.modelDisplayLabel(m.p.OpusModel), m.p.OpusModel, m.oneMSlots["opus"])
		renderMappingRow(rowSonnet, "Sonnet", m.modelDisplayLabel(m.p.SonnetModel), m.p.SonnetModel, m.oneMSlots["sonnet"])
		renderMappingRow(rowHaiku, "Haiku", m.modelDisplayLabel(m.p.HaikuModel), m.p.HaikuModel, m.oneMSlots["haiku"])
		renderMappingRow(rowCustom, "Custom", m.modelDisplayLabel(m.p.CustomModelID), m.p.CustomModelID, m.oneMSlots["custom"])
		renderMappingRow(rowSubagent, "Subagent", m.subagentDisplayLabel(), m.p.SubagentModel, m.oneMSlots["subagent"])

		// Test Model Availability — optional; each probe consumes quota, so the
		// user opts in explicitly. Results are shown next to the model rows above.
		testPrefix := "  "
		testLabel := locale.T("Test Model Availability", "Test Model Availability")
		if !ready {
			testLabel = grayText.Render(testLabel)
		} else if m.modelTesting {
			spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
			spin := spinners[m.modelTestFrame%len(spinners)]
			testLabel = fmt.Sprintf("%s %s", spin, locale.T("正在测试模型可用性...", "Testing model availability..."))
		} else if m.cursor == m.mainRowIndex(rowTestModels) {
			testPrefix = selectedStyle.Render("> ")
			testLabel = selectedStyle.Render(testLabel)
		} else {
			testLabel = purpleText.Render(testLabel)
		}
		body.WriteString(testPrefix + testLabel + "\n")
		if m.modelTesting {
			body.WriteString(grayText.Render("    "+locale.T("测试进行中 · 按 esc 取消", "Testing in progress · press esc to cancel")) + "\n")
		} else if len(m.modelAvailability) > 0 {
			available, unavailable := m.availabilityCounts()
			body.WriteString(grayText.Render(fmt.Sprintf("    "+locale.T("%d 个可用 · %d 个不可用", "%d available · %d unavailable"), available, unavailable)) + "\n")
		} else {
			body.WriteString(grayText.Render(locale.T("    ⚠ 会为每个模型发送一次最小请求，消耗额度", "    ⚠ sends one minimal request per model; consumes quota")) + "\n")
		}

		// Context & Compact — per-slot [1m] via Space on the rows above; the
		// provider-wide fallback cycles with ←→ (shown as ‹ › like other editable
		// values).
		ctxPrefix := "  "
		ctxVal := purpleText.Render("‹ " + m.compactSummary() + " ›")
		if !ready {
			ctxVal = grayText.Render("‹ " + m.compactSummary() + " ›")
		} else if m.cursor == m.mainRowIndex(rowContext) {
			ctxPrefix = selectedStyle.Render("> ")
			ctxVal = selectedStyle.Render("‹ " + m.compactSummary() + " ›")
		}
		body.WriteString(fmt.Sprintf("%s%-12s %s\n", ctxPrefix, "Context", ctxVal))

		// ── Runtime ──────────────────────────────────────────────────────
		body.WriteString("\n" + titleStyle.Render("Runtime") + "\n")
		renderEditable := func(kind configRowKind, label, value string) {
			prefix := "  "
			val := purpleText.Render(value)
			if !ready {
				val = grayText.Render(value)
			} else if m.cursor == m.mainRowIndex(kind) {
				prefix = selectedStyle.Render("> ")
				val = selectedStyle.Render(value)
			}
			body.WriteString(fmt.Sprintf("%s%-12s %s\n", prefix, label, val))
		}
		if m.canToggleOpenAIProtocol() {
			value := "Chat"
			if provider.IsOpenAIResponsesType(m.p.Type) {
				value = "Responses"
			}
			renderEditable(rowProtocol, "Protocol", "‹ "+value+" ›")
		} else {
			body.WriteString(fmt.Sprintf("  %-12s %s\n", "Protocol", availableStyle.Render(m.getProtocol())))
		}
		renderEditable(rowFast, "Fast", formatFastLabel(m.p.FastMode))
		if m.maxOutputUpstreamManaged() {
			body.WriteString(fmt.Sprintf("  %-12s %s\n", "Max Output", availableStyle.Render(locale.T("上游管理", "Upstream managed"))))
		} else {
			renderEditable(rowMaxOutput, "Max Output", formatMaxOutLabel(m.reviewMaxOutValue()))
		}
		renderEditable(rowTools, "Tools", formatToolsLabel(m.reviewToolsValue()))
		renderEditable(rowToolSearch, "Tool Search", formatSearchLabel(m.reviewSearchValue()))

		// ── Active checkbox ──────────────────────────────────────────────
		body.WriteString("\n")
		activePrefix := "  "
		activeBox := "[ ]"
		if m.IsActiveChosen {
			activeBox = "[x]"
		}
		activeLabel := locale.T("设为当前激活 Provider", "Set as active provider")
		boxStyled := purpleText.Render(activeBox)
		labelStyled := purpleText.Render(activeLabel)
		if m.cursor == m.mainRowIndex(rowActive) {
			activePrefix = selectedStyle.Render("> ")
			boxStyled = selectedStyle.Render(activeBox)
			labelStyled = selectedStyle.Render(activeLabel)
		}
		body.WriteString(fmt.Sprintf("%s%s %s\n", activePrefix, boxStyled, labelStyled))

		// ── Actions ──────────────────────────────────────────────────────
		applyLabel := locale.T("保存并激活", "Save & Activate")
		if !m.IsActiveChosen {
			applyLabel = locale.T("保存 Provider", "Save Provider")
		}
		cancelLabel := locale.T("取消", "Cancel")
		applyDisabled := !m.canSave()
		applyStr := buttonStyle.Render(applyLabel)
		if applyDisabled {
			// Not connected (or a dirty connection not yet re-tested): the button
			// is greyed out and not focusable.
			applyStr = grayText.Render("  " + applyLabel + "  ")
		} else if m.cursor == m.mainRowIndex(rowSave) {
			applyStr = buttonActiveStyle.Render(applyLabel)
		}
		cancelStr := buttonStyle.Render(cancelLabel)
		if m.cursor == m.mainRowIndex(rowCancel) {
			cancelStr = buttonActiveStyle.Render(cancelLabel)
		}
		body.WriteString("\n  " + applyStr + "          " + cancelStr + "\n")

		if m.connectionDirty && !m.usesOAuth() {
			body.WriteString(grayText.Render(locale.T("连接已修改，保存前请重新检测", "Connection changed; re-test before saving")) + "\n")
		}
		body.WriteString(grayText.Render(locale.T(
			"↑↓ 选择 · ←→ 调整 · enter 确认 · 模型行 enter 筛选",
			"↑↓ select · ←→ adjust · enter confirm · enter on a model row to filter",
		)) + "\n")
	}

	panelStyle := windowStyle.Width(m.panelWidth())
	if m.page == 4 {
		panelStyle = panelStyle.Padding(0, 2)
	}
	// Scroll the page body when it exceeds the terminal. The offset is derived
	// from the cursor row so the focused row stays visible; View does not mutate
	// model state. Fixed chrome (panel border + footer tip) leaves the rest.
	bodyLines := strings.Split(body.String(), "\n")
	scrollOffset := m.scrollOffset
	if m.height > 0 && len(bodyLines) > m.height {
		// The frame must fit: title bar, detection block, panel border, footer tip.
		// The budget must match keepCursorVisible so a cursor it keeps visible is
		// exactly the window sliced here.
		maxBody := scrollBodyBudget(m.height)
		// Anchor the scroll window to the cursor row using the persisted offset
		// (maintained by keepCursorVisible on cursor movement). The Save/Cancel
		// action bar is the most important thing to keep reachable, so when the
		// persisted offset would hide it the window is anchored to the tail.
		offset := m.scrollOffset
		if offset < 0 {
			offset = 0
		}
		// If the cursor is near the action bar (Save/Cancel), or the offset would
		// leave it off-screen, clamp to the tail so the actions stay visible.
		if m.cursor >= m.mainRowIndex(rowSave) {
			offset = len(bodyLines) - maxBody
			if offset < 0 {
				offset = 0
			}
		} else if offset+maxBody > len(bodyLines) {
			offset = len(bodyLines) - maxBody
			if offset < 0 {
				offset = 0
			}
		}
		// Keep the cursor's own rendered line inside the window. keepCursorVisible
		// already did this for the cursor-line model, but the structural blank
		// lines in the body can push the rendered line a few rows later than the
		// cursor-line estimate; clamp here so the focused row is never sliced off.
		if m.cursor >= 0 && m.cursor < len(m.visibleRows()) {
			cursorLine := renderedCursorLine(body.String(), m.cursor, m.visibleRows())
			if cursorLine < offset {
				offset = cursorLine
			} else if cursorLine >= offset+maxBody {
				offset = cursorLine - maxBody + 1
			}
			if offset < 0 {
				offset = 0
			}
			if offset+maxBody > len(bodyLines) {
				offset = len(bodyLines) - maxBody
				if offset < 0 {
					offset = 0
				}
			}
		}
		bodyLines = bodyLines[offset : offset+maxBody]
		if len(bodyLines) == 0 {
			bodyLines = []string{""}
		}
	}
	panel := panelStyle.Render(strings.Join(bodyLines, "\n"))
	langTipMsg := locale.T(
		"💡 提示: 使用 `ccl lang` 更改终端显示语言",
		"💡 Tip: Change the TUI display language with `ccl lang`",
	)
	content := panel
	if scrollOffset > 0 {
		content = grayText.Render(locale.T("▲ 上滚 · ↑ 查看", "▲ scrolled up · ↑ to view")) + "\n" + content
	}
	content += "\n\n" + grayText.Render(langTipMsg)

	finalStr := content
	if m.width > 0 && m.height > 0 {
		finalStr = lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, content)
	}
	v := tea.NewView(finalStr)
	v.AltScreen = true
	// Mouse reporting: clicking a configuration row focuses it, and the wheel
	// scrolls the single page when content exceeds the terminal.
	if !m.filterInput.Focused() {
		// AllMotion so trackpad two-finger scroll and plain wheel both report
		// motion/wheel events even when no button is held.
		v.MouseMode = tea.MouseModeAllMotion
		lines := strings.Split(finalStr, "\n")
		v.OnMouse = func(msg tea.MouseMsg) tea.Cmd {
			// Only clicks are handled; the mouse wheel and trackpad two-finger
			// scroll are deliberately ignored so they do not scroll the page.
			if _, ok := msg.(tea.MouseClickMsg); !ok {
				return nil
			}
			mouse := msg.Mouse()
			row, ok := rowAtLineAt(lines, mouse.Y, mouse.X)
			if !ok {
				return nil
			}
			return func() tea.Msg { return focusRowMsg{row: row} }
		}
	}
	return v
}

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
		text := ansi.Strip(lines[row])
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
	// Find every label that starts the trimmed field.
	var matched configRowKind
	var matchedIdx int
	hasMatch := false
	for kind, label := range rowClickLabels {
		if strings.HasPrefix(rest, label) {
			idx := lead
			if !hasMatch || idx < matchedIdx {
				matched = kind
				matchedIdx = idx
				hasMatch = true
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
	for kind, label := range rowClickLabels {
		if kind == matched {
			continue
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
	}
	return best, true
}

// rowClickLabels maps a configuration row to the label prefix a click must
// match on its rendered line. Only rows that make sense to click are listed.
var rowClickLabels = map[configRowKind]string{
	rowEndpoint:   "Endpoint URL",
	rowAPIKey:     "API Key",
	rowTest:       "Auto Configure",
	rowProtocol:   "Protocol",
	rowFast:       "Fast",
	rowOpus:       "Opus",
	rowSonnet:     "Sonnet",
	rowHaiku:      "Haiku",
	rowCustom:     "Custom",
	rowSubagent:   "Subagent",
	rowTestModels: "Test Model Availability",
	rowContext:    "Context",
	rowMaxOutput:  "Max Output",
	rowTools:      "Tools",
	rowToolSearch: "Tool Search",
	rowActive:     "Set as active provider",
	rowSave:       "Save & Activate",
	rowCancel:     "Cancel",
}

// viewModelPicker renders the filtered model selection overlay. It is shown
// whenever the filter input owns the keyboard; selecting a model (enter) or
// pressing esc returns to the main configuration page.
func (m *AdvancedConfigModel) viewModelPicker() tea.View {
	var body strings.Builder
	slotName := []string{"Opus", "Sonnet", "Haiku", "Custom", "Subagent"}[m.activeSlot]
	body.WriteString(titleStyle.Render(fmt.Sprintf(locale.T("配置槽位 [%s] 模型筛选", "Select Model for Slot [%s]"), slotName)))
	body.WriteString("\n" + filterStyle.Render(locale.T("🔍 过滤模型: ", "🔍 Filter model: ")) + m.filterInput.View() + "\n")

	start := m.filterWindowStart
	end := start + filterViewHeight
	if end > len(m.filteredPool) {
		end = len(m.filteredPool)
	}
	if start > 0 {
		body.WriteString(grayText.Render(fmt.Sprintf("   ↑ ... %d more above ...", start)) + "\n")
	}
	for i := start; i < end; i++ {
		mod := m.filteredPool[i]
		prefix := "   "
		display := mod
		if stringInSlice(mod, m.modelPool) {
			display = m.modelDisplayLabel(mod)
		}
		line := grayText.Render(display)
		status := ""
		if stringInSlice(mod, m.modelPool) {
			status = "  " + m.availabilityLabel(mod)
		}
		if i == m.slotListCursor {
			prefix = selectedStyle.Render(" > ")
			line = selectedStyle.Render(display)
		}
		body.WriteString(prefix + line + status + "\n")
	}
	if end < len(m.filteredPool) {
		body.WriteString(grayText.Render(fmt.Sprintf("   ↓ ... %d more below ...", len(m.filteredPool)-end)) + "\n")
	}
	body.WriteString(selectedStyle.Render(fmt.Sprintf("  %d/%d", m.slotListCursor+1, len(m.filteredPool))) + "\n\n" + grayText.Render(locale.T("状态来自可用性测试 · 键盘输入过滤 · ↑↓ 选择 · enter 锁定 · esc 取消", "Status comes from availability test · type to filter · ↑↓ scroll · enter lock · esc cancel")) + "\n")

	panel := windowStyle.Width(m.panelWidth()).Render(body.String())
	finalStr := panel
	if m.width > 0 && m.height > 0 {
		finalStr = lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, panel)
	}
	v := tea.NewView(finalStr)
	v.AltScreen = true
	return v
}
