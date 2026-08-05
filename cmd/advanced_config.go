package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"
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
	rowTest // Test & Auto Configure
	rowProtocol
	rowFast
	rowOpus
	rowSonnet
	rowHaiku
	rowCustom
	rowSubagent
	rowContext // Context & Compact entry
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

	modelPoolFromDiscovery bool
	clearStaleSlots        bool
	hadLocalModelPool      bool
	// connectionDirty records that the user edited Endpoint or API Key after the
	// last successful detection. A dirty connection must be re-tested before
	// saving (except for OAuth providers, which never detect over HTTP).
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

	// detectionError is set when protocol detection AND model fetching both fail on Page 0.
	detectionError error
	detecting      bool
	detectProgress int
	detectFrame    int

	// Page 0
	urlInput textinput.Model
	keyInput textinput.Model

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

// pageDetected reports whether a successful detection populated the config.
// OAuth providers skip HTTP detection entirely, so they are always "detected".
func (m *AdvancedConfigModel) pageDetected() bool {
	if m.usesOAuth() {
		return true
	}
	return m.modelPoolFromDiscovery
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

// visibleRows lists the focusable rows of the single configuration page in
// render order. Before a successful detection only the Connection section is
// shown; after it the full mapping/runtime rows appear.
func (m *AdvancedConfigModel) visibleRows() []configRow {
	rows := []configRow{
		{kind: rowEndpoint},
		{kind: rowAPIKey},
		{kind: rowTest},
	}
	if !m.pageDetected() {
		return rows
	}
	rows = append(rows, configRow{kind: rowProtocol, editable: true})
	rows = append(rows, configRow{kind: rowFast})
	rows = append(rows,
		configRow{kind: rowOpus},
		configRow{kind: rowSonnet},
		configRow{kind: rowHaiku},
		configRow{kind: rowCustom},
		configRow{kind: rowSubagent},
		configRow{kind: rowContext},
	)
	rows = append(rows, configRow{kind: rowMaxOutput, editable: true})
	rows = append(rows,
		configRow{kind: rowTools, editable: true},
		configRow{kind: rowToolSearch, editable: true},
		configRow{kind: rowActive, editable: true},
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
	// height minus the fixed header/panel chrome.
	visibleHeight := m.height - 8
	if visibleHeight < 6 {
		visibleHeight = 6
	}
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
// span two lines (label + value); every other row is a single line.
func rowLineHeight(kind configRowKind) int {
	if kind == rowEndpoint || kind == rowAPIKey {
		return 2
	}
	return 1
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

	ki := textinput.New()
	ki.Prompt = ""
	ki.Placeholder = "sk-..."
	ki.EchoMode = textinput.EchoPassword
	ki.EchoCharacter = '*'
	ki.SetWidth(credentialInputWidth)
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

func (m *AdvancedConfigModel) Init() tea.Cmd { return textinput.Blink }

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

func (m *AdvancedConfigModel) showStaleSlotToggle() bool {
	return m.staleSlotCount() > 0
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
func (m *AdvancedConfigModel) canEditFastMode() bool {
	return m.p != nil && supportsFastMode(m.p.OAuthProvider)
}

func (m *AdvancedConfigModel) page4ProtocolOffset() int {
	if m.canToggleOpenAIProtocol() {
		return 1
	}
	return 0
}

func (m *AdvancedConfigModel) page4FastOffset() int {
	if m.canEditFastMode() {
		return 1
	}
	return 0
}

// Context sizing has no row here: it is expressed per slot by [1m] on page 2,
// and ccl writes no session-wide context value that could be edited.
// skipDisabledPage4Cursor moves past rows that are not interactive.
// direction: +1 when moving down/tab, -1 when moving up/shift-tab.
// Runtime option cycles. Index 0 is always "Default" (delete managed env).
var (
	reviewMaxOutOptions  = []string{"", "16000", "32000", "64000", "128000"}
	reviewToolsOptions   = []string{"", "1", "2", "3", "4", "6", "8"}
	reviewSearchOptions  = []string{"", "true", "false"} // Default / On / Off
	reviewCompactOptions = []compactPreset{
		compactPresetDefault,
		compactPresetPreserve,
	}
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

func (m *AdvancedConfigModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.updateInputWidths()
		return m, nil

	case focusCredentialFieldMsg:
		if m.usesOAuth() {
			return m, nil
		}
		switch msg.cursor {
		case 0:
			m.cursor = m.mainRowIndex(rowEndpoint)
			m.keyInput.Blur()
			return m, m.urlInput.Focus()
		case 1:
			m.cursor = m.mainRowIndex(rowAPIKey)
			m.urlInput.Blur()
			return m, m.keyInput.Focus()
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
			switch m.currentRow() {
			case rowContext, rowProtocol, rowFast, rowMaxOutput, rowTools, rowToolSearch, rowActive:
				m.adjustReviewField(-1)
			}

		case "right", "l":
			if m.filterInput.Focused() {
				return m, nil
			}
			switch m.currentRow() {
			case rowContext, rowProtocol, rowFast, rowMaxOutput, rowTools, rowToolSearch, rowActive:
				m.adjustReviewField(1)
			}

		case "space":
			// Toggle the 1M context marker on a model row. Turning it on is refused
			// when the backend window rules 1M out; turning an existing marker off
			// stays possible.
			if m.filterInput.Focused() {
				return m, nil
			}
			row := m.currentRow()
			if row != rowOpus && row != rowSonnet && row != rowHaiku && row != rowCustom && row != rowSubagent {
				return m, nil
			}
			slot := []string{"opus", "sonnet", "haiku", "custom", "subagent"}[slotForRow(row)]
			model := m.slotModelForRow(row)
			if !m.oneMSlots[slot] && m.oneMSlotBlocked(model) {
				setDebugf("1M blocked slot=%s model=%q", slot, model)
				return m, nil
			}
			if slot == "subagent" && !m.oneMSlots[slot] && !m.materializeSubagentModel() {
				return m, nil
			}
			m.oneMSlots[slot] = !m.oneMSlots[slot]
			synced := m.syncOneMForSameModels(slot, m.oneMSlots[slot])
			setDebugf("toggle 1M slot=%s enabled=%t synced=%d summary=%s", slot, m.oneMSlots[slot], synced, reviewOneMSummary(m.oneMSlots))

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
				m.filterInput.Blur()
				m.autoConfigured = false
				setDebugf("slot selected active_slot=%d model=%q slots=%s", m.activeSlot, selectedModel, slotDebugSummary(*m.p))
				return m, nil
			}

			switch m.currentRow() {
			case rowEndpoint:
				m.cursor = m.mainRowIndex(rowAPIKey)
				m.urlInput.Blur()
				m.keyInput.Focus()
				setDebugf("enter endpoint -> api key endpoint=%q", m.urlInput.Value())
			case rowAPIKey:
				m.cursor = m.mainRowIndex(rowTest)
				m.urlInput.Blur()
				m.keyInput.Blur()
				setDebugf("enter api key -> test api_key_len=%d", len(m.keyInput.Value()))
			case rowTest:
				// Start detection with the current input values (OAuth uses the
				// session runtime endpoint/key already injected by configureOAuthRuntime).
				if !m.usesOAuth() {
					m.p.Endpoint = m.urlInput.Value()
					m.p.APIKey = m.keyInput.Value()
					m.probeEndpoint = m.p.Endpoint
					m.probeAPIKey = m.p.APIKey
					m.connectionDirty = m.autoConfigured
				}
				m.urlInput.Blur()
				m.keyInput.Blur()
				m.detectionError = nil
				m.detecting = true
				m.detectProgress = 5
				m.detectFrame = 0
				setDebugf("start detection endpoint=%q api_key_len=%d oauth=%t", m.probeEndpoint, len(m.probeAPIKey), m.usesOAuth())
				return m, tea.Batch(modelFetchCmd(m.probeEndpoint, m.probeAPIKey), modelFetchTickCmd())
			case rowProtocol, rowFast, rowMaxOutput, rowTools, rowToolSearch:
				m.adjustReviewField(1)
			case rowOpus, rowSonnet, rowHaiku, rowCustom, rowSubagent:
				m.activeSlot = slotForRow(m.currentRow())
				m.filterInput.Focus()
				m.filterInput.SetValue("")
				m.slotListCursor = 0
				m.updateFilteredPool()
				setDebugf("open slot picker active_slot=%d filtered_count=%d", m.activeSlot, len(m.filteredPool))
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
		if m.autoConfigured {
			m.connectionDirty = true
		}
	case m.currentRow() == rowAPIKey && !m.usesOAuth():
		if !m.keyInput.Focused() {
			m.urlInput.Blur()
			cmd = m.keyInput.Focus()
		}
		var updateCmd tea.Cmd
		m.keyInput, updateCmd = m.keyInput.Update(msg)
		cmd = tea.Batch(cmd, updateCmd)
		if m.autoConfigured {
			m.connectionDirty = true
		}
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
	const width = 34
	filled := progress * width / 100
	if filled > width {
		filled = width
	}
	spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spin := spinners[frame%len(spinners)]
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	label := locale.T("正在检测协议并获取模型", "Detecting protocol and fetching models")
	hint := locale.T("请稍候，正在验证 BaseURL 和 API Key", "Please wait while BaseURL and API Key are validated")
	if oauth {
		label = locale.T("正在通过 OAuth 获取模型", "Fetching models through OAuth")
		hint = locale.T("请稍候，本地代理正在读取已认证账号的模型", "Please wait while the local proxy loads models for the authenticated account")
	}
	return "\n" +
		selectedStyle.Render(fmt.Sprintf("%s %s", spin, label)) + "\n" +
		cyanText.Render(fmt.Sprintf("[%s] %3d%%", bar, progress)) + "\n" +
		grayText.Render(hint) + "\n"
}

// focusCredentialFieldMsg asks the model to focus one of the credential inputs.
// The mouse handler runs against the last rendered frame (see View), so it
// reports the intent as a message instead of mutating the model from the view.
type focusCredentialFieldMsg struct{ cursor int }

// credentialFields maps the page-0 cursor positions onto their rendered labels,
// in render order.
var credentialFields = []struct {
	cursor int
	label  string
}{
	{cursor: 0, label: "Endpoint URL"},
	{cursor: 1, label: "API Key"},
}

// credentialFieldAtLine resolves a clicked screen row to a credential input.
//
// The row is matched against the rendered frame rather than recomputed from the
// layout: the panel is centered and its height varies with detection state, so
// searching the frame that is actually on screen is both simpler and correct.
// A field occupies its label line plus the value line below it.
func credentialFieldAtLine(lines []string, y int) (int, bool) {
	for _, offset := range []int{0, -1} {
		row := y + offset
		if row < 0 || row >= len(lines) {
			continue
		}
		// The label must start the row (after the cursor prefix and the panel
		// border), so prose that merely mentions "API Key" cannot steal focus.
		text := strings.TrimLeft(ansi.Strip(lines[row]), " │|>")
		text = strings.TrimSpace(text)
		for _, field := range credentialFields {
			if strings.HasPrefix(text, field.label) {
				return field.cursor, true
			}
		}
	}
	return 0, false
}

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
	if !m.pageDetected() {
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

func padDisplay(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
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
		body.WriteString(fmt.Sprintf("  %-12s %s\n", "Auth", purpleText.Render(providerAuthLabel(*m.p))))
		body.WriteString(fmt.Sprintf("  %-12s %s\n", "Local Proxy", availableStyle.Render(locale.T("已就绪（仅本次会话）", "Ready (this session only)"))))
	} else {
		body.WriteString(renderCredentialField("Endpoint URL", m.urlInput.View(), m.cursor == m.mainRowIndex(rowEndpoint)))
		body.WriteString(renderCredentialField("API Key", m.keyInput.View(), m.cursor == m.mainRowIndex(rowAPIKey)))
	}

	// Detection / auto-configure button.
	if m.detecting {
		body.WriteString(renderModelFetchProgress(m.detectProgress, m.detectFrame, m.usesOAuth()))
	} else {
		testLabel := locale.T("Test & Auto Configure", "Test & Auto Configure")
		testStr := "  " + testLabel
		if m.cursor == m.mainRowIndex(rowTest) {
			testStr = selectedStyle.Render("> " + testLabel)
		}
		body.WriteString(testStr + "\n")

		if m.detectionError != nil {
			errorWidth := max(m.panelWidth()-8, 20)
			body.WriteString(errorBoxStyle.Width(errorWidth).Render(locale.T("检测失败，无法继续", "Detection failed; cannot continue")+"\n"+m.detectionError.Error()) + "\n\n")
		} else if m.modelPoolFromDiscovery {
			status := fmt.Sprintf(locale.T("✓ 已连接 · %s · %d 个模型", "✓ Connected · %s · %d models"), provider.ProtocolLabelForProvider(*m.p), len(m.modelPool))
			body.WriteString(availableStyle.Render(status) + "\n")
			if !m.usesOAuth() {
				body.WriteString(fmt.Sprintf("  %-12s %s\n", "Auth", purpleText.Render(providerAuthLabel(*m.p))))
			}
		}
	}

	// ── Model Mapping (only after detection) ──────────────────────────────
	if m.pageDetected() {
		body.WriteString("\n" + titleStyle.Render("Model Mapping") + "\n")
		renderMappingRow := func(kind configRowKind, label, display string, oneM bool) {
			prefix := "  "
			val := purpleText.Render(truncateMiddle(display, 52))
			if m.cursor == m.mainRowIndex(kind) {
				prefix = selectedStyle.Render("> ")
				val = selectedStyle.Render(truncateMiddle(display, 52))
			}
			badge := "    "
			if oneM {
				badge = lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render("[1M]")
			}
			body.WriteString(fmt.Sprintf("%s%-10s %s %s\n", prefix, label, val, badge))
		}
		renderMappingRow(rowOpus, "Opus", m.modelDisplayLabel(m.p.OpusModel), m.oneMSlots["opus"])
		renderMappingRow(rowSonnet, "Sonnet", m.modelDisplayLabel(m.p.SonnetModel), m.oneMSlots["sonnet"])
		renderMappingRow(rowHaiku, "Haiku", m.modelDisplayLabel(m.p.HaikuModel), m.oneMSlots["haiku"])
		renderMappingRow(rowCustom, "Custom", m.modelDisplayLabel(m.p.CustomModelID), m.oneMSlots["custom"])
		renderMappingRow(rowSubagent, "Subagent", m.subagentDisplayLabel(), m.oneMSlots["subagent"])

		// Context & Compact — per-slot [1m] via Space on the rows above; the
		// provider-wide fallback cycles with ←→.
		ctxPrefix := "  "
		ctxVal := purpleText.Render(m.compactSummary())
		if m.cursor == m.mainRowIndex(rowContext) {
			ctxPrefix = selectedStyle.Render("> ")
			ctxVal = selectedStyle.Render(m.compactSummary())
		}
		body.WriteString(fmt.Sprintf("%s%-12s %s\n", ctxPrefix, "Context", ctxVal))

		// ── Runtime ──────────────────────────────────────────────────────
		body.WriteString("\n" + titleStyle.Render("Runtime") + "\n")
		renderEditable := func(kind configRowKind, label, value string) {
			prefix := "  "
			val := purpleText.Render(value)
			if m.cursor == m.mainRowIndex(kind) {
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
			labelStyled = titleStyle.Render(activeLabel)
		}
		body.WriteString(fmt.Sprintf("%s%s %s\n", activePrefix, boxStyled, labelStyled))

		// ── Actions ──────────────────────────────────────────────────────
		applyLabel := locale.T("保存并激活", "Save & Activate")
		if !m.IsActiveChosen {
			applyLabel = locale.T("保存 Provider", "Save Provider")
		}
		cancelLabel := locale.T("取消", "Cancel")
		applyStr := "  " + applyLabel
		cancelStr := "  " + cancelLabel
		if m.cursor == m.mainRowIndex(rowSave) {
			applyStr = selectedStyle.Render("> " + applyLabel)
		}
		if m.cursor == m.mainRowIndex(rowCancel) {
			cancelStr = selectedStyle.Render("> " + cancelLabel)
		}
		body.WriteString("\n" + applyStr + "          " + cancelStr + "\n")

		if m.connectionDirty && !m.usesOAuth() {
			body.WriteString(grayText.Render(locale.T("连接已修改，保存前请重新检测", "Connection changed; re-test before saving")) + "\n")
		}
		body.WriteString(grayText.Render(locale.T(
			"↑↓ 选择 · ←→ 调整 · enter 确认 · 模型行 enter 筛选",
			"↑↓ select · ←→ adjust · enter confirm · enter on a model row to filter",
		)) + "\n")
	} else {
		body.WriteString("\n" + grayText.Render(locale.T(
			"填写 Endpoint 与 API Key 后点击 Test & Auto Configure 自动填充",
			"Enter Endpoint and API Key, then Test & Auto Configure to auto-fill",
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
		// The frame must fit: border (2) + footer (3) leave height-5 for the body.
		maxBody := m.height - 5
		if maxBody < 6 {
			maxBody = 6
		}
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
	// Mouse reporting on the Connection inputs only.
	if !m.usesOAuth() && m.mainRowIndex(rowEndpoint) >= 0 {
		v.MouseMode = tea.MouseModeCellMotion
		lines := strings.Split(finalStr, "\n")
		v.OnMouse = func(msg tea.MouseMsg) tea.Cmd {
			if _, ok := msg.(tea.MouseClickMsg); !ok {
				return nil
			}
			cursor, ok := credentialFieldAtLine(lines, msg.Mouse().Y)
			if !ok {
				return nil
			}
			return func() tea.Msg { return focusCredentialFieldMsg{cursor: cursor} }
		}
	}
	return v
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
