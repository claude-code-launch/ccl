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
	stepStyle        = lipgloss.NewStyle().Faint(true).MarginLeft(1)
	dividerStyle     = lipgloss.NewStyle().Faint(true)
	selectedStyle    = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	lightning        = lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render("⚡1M")
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
	slotMappingCount      = 5
	slotTestCursor        = slotMappingCount
	slotNextCursor        = slotTestCursor + 1
	slotBackCursor        = slotNextCursor + 1
	// Page 2: slots 0..4, compact radios 5..9, Next/Back after.
	compactRadioCount     = 5
	oneMCompactStart      = slotMappingCount
	oneMNextCursor        = oneMCompactStart + compactRadioCount
	oneMBackCursor        = oneMNextCursor + 1
	slotTestConcurrency   = 50
	lowCostProbeModel     = "gpt-5.4-mini"
)

// compactRadioOrder matches the product mockup Auto Compact radio list.
var compactRadioOrder = []compactPreset{
	compactPresetDefault,
	compactPreset200K,
	compactPreset500K,
	compactPreset1M,
	compactPresetPreserve, // Custom
}

type AdvancedConfigModel struct {
	p             *provider.Provider
	modelPool     []string
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

	page   int
	cursor int
	width  int
	height int

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

	// Page 3

	// Page 4
	IsActiveChosen bool
	manualConfig   bool
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
	err                 error
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
		p:                   p,
		oneMSlots:           make(map[string]bool),
		modelContextWindows: make(map[string]int),
		compactPreset:       compactState.preset,
		compactState:        compactState,
		probeEndpoint:       p.Endpoint,
		probeAPIKey:         p.APIKey,
		page:                0,
		cursor:              0,
		urlInput:            ui,
		keyInput:            ki,
		filterInput:         fi,
		IsActiveChosen:      true,
		clearStaleSlots:     true,
		modelAvailability:   make(map[string]modelAvailability),
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
	m.cursor = m.page0NextCursor()
	m.urlInput.Blur()
	m.keyInput.Blur()
}

func (m *AdvancedConfigModel) usesOAuth() bool {
	return m.p != nil && strings.TrimSpace(m.p.OAuthProvider) != ""
}

func (m *AdvancedConfigModel) page0NextCursor() int {
	if m.usesOAuth() {
		return 0
	}
	return 2
}

func (m *AdvancedConfigModel) page0BackCursor() int {
	if m.usesOAuth() {
		return 1
	}
	return 3
}

func (m *AdvancedConfigModel) page0MaxCursor() int {
	return m.page0BackCursor()
}

// NewAdvancedConfigModelAtPage1 creates a model starting at page 1 (slot mapping)
// with a pre-populated model pool, skipping the credential page.
func NewAdvancedConfigModelAtPage1(p *provider.Provider, modelPool []string) *AdvancedConfigModel {
	m := NewAdvancedConfigModel(p)
	m.page = 1
	m.cursor = slotNextCursor
	m.modelPool = modelPool
	m.urlInput.Blur()
	m.keyInput.Blur()
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
			if infos, err := protocol.GetOpenAIModelInfos(result.baseURL, apiKey); err == nil {
				for _, info := range infos {
					if info.ContextWindow > 0 {
						windows[info.ID] = info.ContextWindow
					}
				}
			}
		}
		return modelFetchDoneMsg{
			endpoint:            endpoint,
			apiKey:              apiKey,
			detectedType:        result.protocol,
			detectedEndpoint:    result.baseURL,
			anthropicAuth:       result.anthropicAuth,
			discoveredModelsRaw: result.models,
			contextWindows:      windows,
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
	case "chatgpt", "codex":
		return lowCostProbeModel
	default:
		return ""
	}
}

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
			if strings.Contains(strings.ToLower(mod), q) {
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
func (m *AdvancedConfigModel) doAutoConfig() {
	slots := []*string{&m.p.OpusModel, &m.p.SonnetModel, &m.p.HaikuModel, &m.p.CustomModelID}
	for i, ptr := range slots {
		if i < len(m.modelPool) {
			*ptr = m.modelPool[i]
		} else {
			*ptr = ""
		}
	}
	m.p.SubagentModel = ""
	// Auto mode recommends 1M only for the strict allowlist. Otherwise it
	// preserves custom/200K settings, but clears stale per-slot 1M state.
	hadOneMSlots := len(m.oneMSlots) > 0
	m.oneMSlots = make(map[string]bool)
	m.compactPreset = m.compactState.preset
	if hadOneMSlots {
		m.compactPreset = compactPresetDefault
		m.compactState = compactConfigState{preset: compactPresetDefault}
	}
	if allConfiguredModelsRecommendOneM(*m.p) {
		for _, slot := range advancedSlotRefs(m.p) {
			if strings.TrimSpace(*slot.ptr) != "" {
				m.oneMSlots[slot.key] = true
			}
		}
		// Confirmed 1M models: enable Extended Context and Balanced compact
		// (500K/80%). Users can deepen to Maximum 1M/90% manually.
		m.compactPreset = compactPreset500K
		m.compactState = compactConfigState{preset: compactPreset500K, window: compactWindow500K, pct: compactPct500K}
	}
	// Default: do not override Claude's own effort setting.
	m.p.EffortLevel = ""
	setDebugf("doAutoConfig model_pool_count=%d slots=%s effort=%q one_m=%s", len(m.modelPool), slotDebugSummary(*m.p), m.p.EffortLevel, reviewOneMSummary(m.oneMSlots))
}

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

func uniqueModels(models []string) []string {
	var out []string
	for _, mod := range models {
		mod = strings.TrimSpace(mod)
		if mod != "" && !stringInSlice(mod, out) {
			out = append(out, mod)
		}
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

func (m *AdvancedConfigModel) page5MaxCursor() int {
	if m.showStaleSlotToggle() {
		return 2
	}
	return 1
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
		return provider.ProtocolLabel(m.p.Type)
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

// canToggleOpenAIProtocol is true when the provider is OpenAI-compatible, so the
// review page can switch between Chat Completions and Responses.
func (m *AdvancedConfigModel) canToggleOpenAIProtocol() bool {
	return m.p != nil && provider.IsOpenAICompatibleType(m.p.Type)
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

// Page 4 cursor model (compact review):
//   0 protocol (only when OpenAI-compatible)
//   active toggle
//   apply
//   back
func (m *AdvancedConfigModel) page4ProtocolCursor() int {
	if m.canToggleOpenAIProtocol() {
		return 0
	}
	return -1
}

func (m *AdvancedConfigModel) page4ActiveCursor() int {
	if m.canToggleOpenAIProtocol() {
		return 1
	}
	return 0
}

func (m *AdvancedConfigModel) page4SaveCursor() int {
	if m.canToggleOpenAIProtocol() {
		return 2
	}
	return 1
}

func (m *AdvancedConfigModel) page4BackCursor() int {
	if m.canToggleOpenAIProtocol() {
		return 3
	}
	return 2
}

func (m *AdvancedConfigModel) page4MaxCursor() int {
	return m.page4BackCursor()
}

func (m *AdvancedConfigModel) page4InitialCursor() int {
	return m.page4SaveCursor()
}

func (m *AdvancedConfigModel) goBack() {
	oldPage := m.page
	oldCursor := m.cursor
	if m.page == 1 {
		// Go back from slot mapping to config mode choice
		m.page = 5
		m.cursor = 1 // pre-select Manual Config (since user was in manual mode)
		m.manualConfig = true
	} else if m.page == 5 {
		// Go back from choice to credentials
		m.page = 0
		m.cursor = m.page0NextCursor()
	} else if m.page == 4 {
		// Skip removed Effort page; return to Context & Compact.
		m.page = 2
		m.cursor = oneMNextCursor
	} else if m.page > 0 {
		m.page--
		if m.page == 0 {
			m.cursor = m.page0NextCursor()
		}
		if m.page == 1 {
			m.cursor = slotNextCursor
		}
		if m.page == 2 {
			m.cursor = oneMNextCursor
		}
	}
	setDebugf("goBack old_page=%d old_cursor=%d new_page=%d new_cursor=%d", oldPage, oldCursor, m.page, m.cursor)
}


// workflowStep keeps the visible flow independent from the internal page IDs.
// Page 5 is the config-mode choice shown immediately after credentials.
// Reasoning Effort (old page 3) is no longer part of ccl set — Claude Code
// manages effort natively via /effort, --effort, and settings.
func (m *AdvancedConfigModel) workflowStep() int {
	switch m.page {
	case 0:
		return 1
	case 5:
		return 2
	case 1:
		return 3
	case 2:
		return 4
	case 4:
		return 5
	default:
		return 1
	}
}

// selectedCompactRadioIndex maps the current compact state onto the radio list.
// Custom/legacy/unknown states land on the Custom radio.
func (m *AdvancedConfigModel) selectedCompactRadioIndex() int {
	preset := m.compactPreset
	if m.compactState.custom || m.compactState.legacy {
		preset = compactPresetPreserve
	}
	for i, p := range compactRadioOrder {
		if p == preset {
			return i
		}
	}
	return len(compactRadioOrder) - 1
}

// selectCompactPreset sets the provider-wide compact budget from a radio index.
// Per-slot [1m] markers are independent and never cleared here.
func (m *AdvancedConfigModel) selectCompactPreset(radioIdx int) {
	if radioIdx < 0 || radioIdx >= len(compactRadioOrder) {
		return
	}
	m.compactPreset = compactRadioOrder[radioIdx]
	m.compactState = compactConfigState{preset: m.compactPreset}
}

func compactRadioLabel(preset compactPreset) string {
	switch preset {
	case compactPresetDefault:
		return "Claude default"
	case compactPreset200K:
		return "Switch-safe     200K / 70%"
	case compactPreset500K:
		return "Balanced        500K / 80%"
	case compactPreset1M:
		return "Maximum depth     1M / 90%"
	default:
		return "Custom"
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
	return compactStateSummary(state, m.oneMSlots)
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
		m.page = 0
		m.cursor = m.page0NextCursor()
		setDebugf("applyModelDetectionResult detection failed detection_error=%v model_count=%d", m.detectionError, len(m.modelPool))
		return nil
	}

	// 本次 set 必须以接口返回的模型为准；不再用旧的本地模型池兜底。
	if len(m.modelPool) == 0 {
		if derr != nil {
			m.detectionError = derr
		} else {
			m.detectionError = fmt.Errorf("%s", locale.T(
				"未从接口获取到任何可用模型，未使用本地旧模型池",
				"no models were fetched from the provider API; local cached models were not used",
			))
		}
		m.page = 0
		m.cursor = m.page0NextCursor()
		setDebugf("applyModelDetectionResult no models detection_error=%v", m.detectionError)
		return nil
	}

	sort.Strings(m.modelPool)
	m.page = 5
	m.cursor = 0
	setDebugf(
		"applyModelDetectionResult success provider_type=%q endpoint=%q anthropic_auth=%q model_count=%d stale_slot_count=%d clear_stale_slots=%t page=%d cursor=%d",
		m.p.Type,
		m.p.Endpoint,
		m.p.AnthropicAuth,
		len(m.modelPool),
		m.staleSlotCount(),
		m.clearStaleSlots,
		m.page,
		m.cursor,
	)
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
		switch msg.String() {
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
				m.cursor = slotTestCursor
				setDebugf("model availability test canceled test_id=%d", m.modelTestID)
			}
			return m, nil
		}
		switch msg.String() {
		case "esc":
			if m.page == 1 && m.filterInput.Focused() {
				m.filterInput.Blur()
				setDebugf("esc closed slot picker active_slot=%d page=%d cursor=%d", m.activeSlot, m.page, m.cursor)
			} else {
				if m.page == 0 {
					setDebugf("esc quit from credentials page cursor=%d endpoint_set=%t api_key_len=%d", m.cursor, strings.TrimSpace(m.urlInput.Value()) != "", len(m.keyInput.Value()))
					return m, tea.Quit
				}
				setDebugf("esc goBack from page=%d cursor=%d", m.page, m.cursor)
				m.goBack()
			}
			return m, nil

		case "up", "k":
			if m.page == 1 && m.filterInput.Focused() {
				if m.slotListCursor > 0 {
					m.slotListCursor--
					// Scroll window up if cursor goes above visible area
					if m.slotListCursor < m.filterWindowStart {
						m.filterWindowStart = m.slotListCursor
					}
				}
				return m, nil
			} else {
				if m.page == 0 && !m.usesOAuth() && (m.cursor == 2 || m.cursor == 3) {
					m.cursor = 1
				} else if m.page == 1 && (m.cursor == slotNextCursor || m.cursor == slotBackCursor) {
					m.cursor = slotTestCursor
				} else if m.page == 2 && (m.cursor == oneMNextCursor || m.cursor == oneMBackCursor) {
					m.cursor = oneMCompactStart + compactRadioCount - 1
				} else if m.page == 5 && m.cursor > 0 {
					m.cursor--
				} else if m.cursor > 0 {
					m.cursor--
				}
			}

		case "down", "j":
			if m.page == 1 && m.filterInput.Focused() {
				if m.slotListCursor < len(m.filteredPool)-1 {
					m.slotListCursor++
					// Scroll window down if cursor goes below visible area
					if m.slotListCursor >= m.filterWindowStart+filterViewHeight {
						m.filterWindowStart = m.slotListCursor - filterViewHeight + 1
					}
				}
				return m, nil
			}

			if m.page == 0 {
				if m.usesOAuth() {
					if m.cursor < m.page0MaxCursor() {
						m.cursor++
					}
				} else if m.cursor < 2 {
					m.cursor++
				}
			} else if m.page == 1 {
				if m.cursor < slotNextCursor {
					m.cursor++
				}
			} else if m.page == 2 {
				if m.cursor < oneMNextCursor {
					m.cursor++
				}
			} else if m.page == 4 {
				if m.cursor < m.page4MaxCursor() {
					m.cursor++
				}
			} else if m.page == 5 {
				if m.cursor < m.page5MaxCursor() {
					m.cursor++
				}
			}

		case "left", "h":
			if m.page == 0 && m.cursor == m.page0BackCursor() {
				m.cursor = m.page0NextCursor()
			}
			if m.page == 1 && m.cursor == slotBackCursor {
				m.cursor = slotNextCursor
			}
			if m.page == 2 && m.cursor == oneMBackCursor {
				m.cursor = oneMNextCursor
			}
			if m.page == 4 {
				if m.cursor == m.page4ProtocolCursor() {
					m.toggleOpenAIProtocol()
				} else if m.cursor == m.page4ActiveCursor() {
					m.IsActiveChosen = true
					setDebugf("page4 active choice toggled active_chosen=%t", m.IsActiveChosen)
				}
			}
			if m.page == 5 && m.cursor == 2 && m.showStaleSlotToggle() {
				m.clearStaleSlots = true
				setDebugf("page5 stale slot cleanup toggled clear_stale_slots=%t stale_slot_count=%d", m.clearStaleSlots, m.staleSlotCount())
			}

		case "right", "l":
			if m.page == 0 && m.cursor == m.page0NextCursor() {
				m.cursor = m.page0BackCursor()
			}
			if m.page == 1 && m.cursor == slotNextCursor {
				m.cursor = slotBackCursor
			}
			if m.page == 2 && m.cursor == oneMNextCursor {
				m.cursor = oneMBackCursor
			}
			if m.page == 4 {
				if m.cursor == m.page4ProtocolCursor() {
					m.toggleOpenAIProtocol()
				} else if m.cursor == m.page4ActiveCursor() {
					m.IsActiveChosen = false
					setDebugf("page4 active choice toggled active_chosen=%t", m.IsActiveChosen)
				}
			}
			if m.page == 5 && m.cursor == 2 && m.showStaleSlotToggle() {
				m.clearStaleSlots = false
				setDebugf("page5 stale slot cleanup toggled clear_stale_slots=%t stale_slot_count=%d", m.clearStaleSlots, m.staleSlotCount())
			}

		case "tab":
			// Tab → 下一项（同 ↓）
			if m.page == 1 && m.filterInput.Focused() {
				if m.slotListCursor < len(m.filteredPool)-1 {
					m.slotListCursor++
					// Scroll window down if cursor goes below visible area
					if m.slotListCursor >= m.filterWindowStart+filterViewHeight {
						m.filterWindowStart = m.slotListCursor - filterViewHeight + 1
					}
				}
				return m, nil
			}
			m.cursor++
			if m.page == 0 && m.cursor > m.page0MaxCursor() {
				m.cursor = 0
			} else if m.page == 1 && m.cursor > slotBackCursor {
				m.cursor = 0
			} else if m.page == 2 && m.cursor > oneMBackCursor {
				m.cursor = 0
			} else if m.page == 4 && m.cursor > m.page4MaxCursor() {
				m.cursor = 0
			} else if m.page == 5 && m.cursor > m.page5MaxCursor() {
				m.cursor = 0
			}

		// case "space":
		// 	// 空格键：Page 2 切换槽位的 1M 上下文开关
		// 	if m.page == 2 && m.cursor < 4 {
		// 		slot := []string{"opus", "sonnet", "haiku", "custom"}[m.cursor]
		// 		m.oneMSlots[slot] = !m.oneMSlots[slot]
		// 	}

		case "shift+tab":
			// Shift+Tab → 上一项（同 ↑）
			m.cursor--
			if m.cursor < 0 {
				if m.page == 0 {
					m.cursor = m.page0MaxCursor()
				} else if m.page == 1 {
					m.cursor = slotBackCursor
				} else if m.page == 2 {
					m.cursor = oneMBackCursor
				} else if m.page == 4 {
					m.cursor = m.page4MaxCursor()
				} else if m.page == 5 {
					m.cursor = m.page5MaxCursor()
				}
			}

		case "enter":
			// 如果点击了底部的“上一步”按钮，直接返回
			if (m.page == 0 && m.cursor == m.page0BackCursor()) || (m.page == 1 && m.cursor == slotBackCursor) || (m.page == 2 && m.cursor == oneMBackCursor) {
				m.goBack()
				return m, nil
			}

			switch m.page {
			case 0:
				if m.usesOAuth() && m.cursor == m.page0NextCursor() {
					m.detectionError = nil
					m.detecting = true
					m.detectProgress = 5
					m.detectFrame = 0
					setDebugf("page0 start OAuth detection provider=%q endpoint=%q", m.p.OAuthProvider, m.probeEndpoint)
					return m, tea.Batch(modelFetchCmd(m.probeEndpoint, m.probeAPIKey), modelFetchTickCmd())
				} else if !m.usesOAuth() && m.cursor == 0 {
					m.cursor = 1
					m.urlInput.Blur()
					m.keyInput.Focus()
					setDebugf("page0 enter endpoint field complete next_cursor=%d endpoint=%q", m.cursor, m.urlInput.Value())
				} else if !m.usesOAuth() && m.cursor == 1 {
					m.cursor = 2
					setDebugf("page0 enter api key field complete next_cursor=%d api_key_len=%d", m.cursor, len(m.keyInput.Value()))
				} else if !m.usesOAuth() && m.cursor == m.page0NextCursor() {
					m.p.Endpoint = m.urlInput.Value()
					m.p.APIKey = m.keyInput.Value()
					m.probeEndpoint = m.p.Endpoint
					m.probeAPIKey = m.p.APIKey
					m.urlInput.Blur()
					m.keyInput.Blur()
					m.detectionError = nil
					m.detecting = true
					m.detectProgress = 5
					m.detectFrame = 0
					setDebugf("page0 start detection endpoint=%q api_key_len=%d", m.p.Endpoint, len(m.p.APIKey))
					return m, tea.Batch(modelFetchCmd(m.probeEndpoint, m.probeAPIKey), modelFetchTickCmd())
				}
			case 1:
				if !m.filterInput.Focused() {
					if m.cursor == slotTestCursor {
						m.modelTestID++
						testID := m.modelTestID
						ctx, cancel := context.WithCancel(context.Background())
						m.modelTesting = true
						m.modelTestCancel = cancel
						m.modelTestFrame = 0
						m.modelTestCanceled = false
						setDebugf("model availability test started model_count=%d", len(m.modelPool))
						return m, tea.Batch(
							modelAvailabilityTestCmd(ctx, testID, m.modelPool, m.probeEndpoint, m.probeAPIKey, m.p.Type, m.p.AnthropicAuth, m.availabilitySmokeTestModel()),
							modelAvailabilityTickCmd(testID),
						)
					} else if m.cursor == slotNextCursor {
						m.page = 2
						m.cursor = 0
						setDebugf("page1 next to page2 slots=%s", slotDebugSummary(*m.p))
					} else if m.cursor >= 0 && m.cursor < slotTestCursor {
						m.activeSlot = m.cursor
						m.filterInput.Focus()
						m.filterInput.SetValue("")
						m.slotListCursor = 0
						m.updateFilteredPool()
						setDebugf("page1 open slot picker active_slot=%d filtered_count=%d", m.activeSlot, len(m.filteredPool))
					}
				} else {
					// Safety: clamp cursor to valid range before accessing filteredPool
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
					setDebugf("page1 slot selected active_slot=%d model=%q slots=%s", m.activeSlot, selectedModel, slotDebugSummary(*m.p))
				}
			case 2:
				if m.cursor < slotMappingCount {
					// Extended context only — compact preset is independent.
					slot := []string{"opus", "sonnet", "haiku", "custom", "subagent"}[m.cursor]
					if slot == "subagent" && !m.oneMSlots[slot] && !m.materializeSubagentModel() {
						return m, nil
					}
					m.oneMSlots[slot] = !m.oneMSlots[slot]
					synced := m.syncOneMForSameModels(slot, m.oneMSlots[slot])
					setDebugf("page2 toggle one_m slot=%s enabled=%t synced=%d summary=%s", slot, m.oneMSlots[slot], synced, reviewOneMSummary(m.oneMSlots))
					m.cursor++
				} else if m.cursor >= oneMCompactStart && m.cursor < oneMNextCursor {
					m.selectCompactPreset(m.cursor - oneMCompactStart)
					setDebugf("page2 select compact radio=%d preset=%v summary=%s", m.cursor-oneMCompactStart, m.compactPreset, m.compactSummary())
				} else if m.cursor == oneMNextCursor {
					// Context page is the last configuration step before review.
					m.page = 4
					m.cursor = m.page4InitialCursor()
					setDebugf("page2 next to review one_m=%s compact=%s", reviewOneMSummary(m.oneMSlots), m.compactSummary())
				}
			case 4:
				if m.cursor == m.page4ProtocolCursor() {
					m.toggleOpenAIProtocol()
				} else if m.cursor == m.page4ActiveCursor() {
					m.IsActiveChosen = !m.IsActiveChosen
					setDebugf("page4 active choice toggled active_chosen=%t", m.IsActiveChosen)
				} else if m.cursor == m.page4BackCursor() {
					m.goBack()
				} else if m.cursor == m.page4SaveCursor() {
					m.saveConfirmed = true
					setDebugf("page4 save requested provider=%q type=%q effort=%q model_count=%d slots=%s one_m=%s active_chosen=%t", m.p.Name, m.p.Type, m.p.EffortLevel, countCSV(m.p.Model), slotDebugSummary(*m.p), reviewOneMSummary(m.oneMSlots), m.IsActiveChosen)
					return m, tea.Quit
				}
			case 5:
				// Page 5: Auto / Manual config choice
				if m.cursor == 0 {
					// Auto Config: auto-fill slots, set effort=max, skip 1M, go to save
					m.manualConfig = false
					m.applyStaleSlotPolicy()
					m.doAutoConfig()
					m.page = 4
					m.cursor = m.page4InitialCursor()
					setDebugf("page5 auto config selected next_page=%d cursor=%d slots=%s effort=%q", m.page, m.cursor, slotDebugSummary(*m.p), m.p.EffortLevel)
				} else if m.cursor == 1 {
					// Manual Config: go to slot mapping (old page 1)
					m.manualConfig = true
					m.applyStaleSlotPolicy()
					m.page = 1
					m.cursor = slotNextCursor
					setDebugf("page5 manual config selected next_page=%d cursor=%d clear_stale_slots=%t slots=%s", m.page, m.cursor, m.clearStaleSlots, slotDebugSummary(*m.p))
				} else if m.cursor == 2 && m.showStaleSlotToggle() {
					m.clearStaleSlots = !m.clearStaleSlots
					setDebugf("page5 stale slot cleanup toggled clear_stale_slots=%t stale_slot_count=%d", m.clearStaleSlots, m.staleSlotCount())
				}
			}
		}
	}

	if m.page == 0 && !m.usesOAuth() {
		// 让光标位置与输入框焦点保持同步：只有获得焦点的输入框才会处理
		// 按键和粘贴（textinput.Update 在未聚焦时会直接返回）。
		var focusCmd, updateCmd tea.Cmd
		switch m.cursor {
		case 0:
			if !m.urlInput.Focused() {
				m.keyInput.Blur()
				focusCmd = m.urlInput.Focus()
			}
			m.urlInput, updateCmd = m.urlInput.Update(msg)
		case 1:
			if !m.keyInput.Focused() {
				m.urlInput.Blur()
				focusCmd = m.keyInput.Focus()
			}
			m.keyInput, updateCmd = m.keyInput.Update(msg)
		default:
			// 光标在按钮上时，取消两个输入框的焦点
			m.urlInput.Blur()
			m.keyInput.Blur()
		}
		cmd = tea.Batch(focusCmd, updateCmd)
	} else if m.page == 1 && m.filterInput.Focused() {
		m.filterInput, cmd = m.filterInput.Update(msg)
		m.updateFilteredPool()
	}

	return m, cmd
}

func renderBottomButtons(page int, currentCursor int, nextIdx, backIdx int) string {
	nextStr := locale.T("下一步", "Next")
	backStr := locale.T("上一步", "Back")

	if currentCursor == nextIdx {
		nextStr = selectedStyle.Render("> " + nextStr)
	} else {
		nextStr = "  " + nextStr
	}

	if page == 0 {
		backStr = grayText.Render(backStr)
	} else {
		if currentCursor == backIdx {
			backStr = selectedStyle.Render("> " + backStr)
		} else {
			backStr = "  " + backStr
		}
	}

	return fmt.Sprintf("\n%s    %s\n\n", nextStr, backStr)
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
	if m.page != 4 {
		line += protoBadgeStyle.Render("Protocol: " + m.getProtocolFamily())
	}
	step := fmt.Sprintf(locale.T("步骤 %d/5", "Step %d/5"), m.workflowStep())
	line += stepStyle.Render(step)
	dividerWidth := max(m.panelWidth()-6, 16)
	return line + "\n" + dividerStyle.Render(strings.Repeat("─", dividerWidth)) + "\n\n"
}

func (m *AdvancedConfigModel) renderStepProgress() string {
	const totalSteps = 5
	dots := make([]string, 0, totalSteps)
	for step := 1; step <= totalSteps; step++ {
		if step == m.workflowStep() {
			dots = append(dots, cyanText.Render("●"))
			continue
		}
		dots = append(dots, grayText.Render("○"))
	}
	return strings.Join(dots, "  ")
}

func renderReviewModelMapping(label, model string, oneM bool) string {
	display := stripOneMSuffix(model)
	if strings.TrimSpace(display) == "" {
		// Keep auto subagent labels intact.
		display = strings.TrimSpace(model)
	}
	value := cyanText.Render(truncateMiddle(display, 36))
	if strings.TrimSpace(display) == "" {
		value = grayText.Render(locale.T("(未设置)", "(unset)"))
	}
	badge := "    "
	if oneM {
		badge = lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render("[1M]")
	}
	return fmt.Sprintf("  %-10s %-36s %s\n", label, value, badge)
}

// truncateMiddle keeps endpoint/model names on one line for the review page.
func truncateMiddle(s string, max int) string {
	s = strings.TrimSpace(s)
	if max < 8 || len(s) <= max {
		return s
	}
	keep := (max - 1) / 2
	return s[:keep] + "…" + s[len(s)-(max-keep-1):]
}

func (m *AdvancedConfigModel) View() tea.View {
	var body strings.Builder

	switch m.page {
	case 0:
		// ==================== PAGE 0: 凭据配置 ====================
		if m.usesOAuth() {
			body.WriteString(m.renderPageHeader(locale.T("OAuth 认证配置", "OAuth Credentials"), "OAuth"))
			body.WriteString(fmt.Sprintf("  • %-12s: %s\n", "Provider", cyanText.Render(m.p.OAuthProvider)))
			body.WriteString(fmt.Sprintf("  • %-12s: %s\n", "Auth", purpleText.Render(providerAuthLabel(*m.p))))
			body.WriteString(fmt.Sprintf("  • %-12s: %s\n", "Local Proxy", availableStyle.Render(locale.T("已就绪（仅本次会话）", "Ready (this session only)"))))
			body.WriteString("\n" + grayText.Render(locale.T(
				"模型发现和可用性测试将通过本地 OAuth 代理完成，不会保存临时 API Key。",
				"Model discovery and availability tests use the local OAuth proxy; its temporary API key is never saved.",
			)) + "\n")
		} else {
			body.WriteString(m.renderPageHeader(locale.T("基础凭据配置", "Base Credentials"), "Credentials"))
			body.WriteString(renderCredentialField("Endpoint URL", m.urlInput.View(), m.cursor == 0))
			body.WriteString(renderCredentialField("API Key", m.keyInput.View(), m.cursor == 1))
		}

		if m.detecting {
			body.WriteString(renderModelFetchProgress(m.detectProgress, m.detectFrame, m.usesOAuth()))
		} else {
			if m.detectionError != nil {
				errorWidth := max(m.panelWidth()-8, 20)
				body.WriteString(errorBoxStyle.Width(errorWidth).Render(locale.T("检测失败，无法继续", "Detection failed; cannot continue")+"\n"+m.detectionError.Error()) + "\n\n")
			}
			body.WriteString(renderBottomButtons(m.page, m.cursor, m.page0NextCursor(), m.page0BackCursor()))
			if m.usesOAuth() {
				body.WriteString(grayText.Render(locale.T("←→ 切换按钮 · enter 获取模型", "←→ Toggle Buttons · enter fetch models")))
			} else {
				body.WriteString(grayText.Render(locale.T("↑↓ 切换焦点 · ←→ 切换按钮 · enter 确认", "↑↓ Switch · ←→ Toggle Buttons · enter confirm")))
			}
		}

	case 1:
		// ==================== PAGE 1: 槽位映射配置 ====================
		if !m.filterInput.Focused() {
			body.WriteString(m.renderPageHeader(locale.T("Claude Slot 映射配置", "Claude Slot Mapping"), "Slot List"))
			renderRow := func(idx int, label, val string) {
				prefix := "  "
				var labelStr string
				if m.cursor == idx {
					prefix = selectedStyle.Render("> ")
					labelStr = selectedStyle.Render(label) + grayText.Render(" ("+locale.T("enter 筛选", "enter to list")+")")
				} else {
					// ✅ 修复：先对纯文本 label 填充至 6 宽，再加颜色
					labelStr = purpleText.Render(fmt.Sprintf("%-9s", label))
				}
				modelStr := cyanText.Render(val)
				if val == "" {
					modelStr = grayText.Render(locale.T("(未设置)", "(unset)"))
				}
				body.WriteString(fmt.Sprintf("%s%s – %s\n", prefix, labelStr, modelStr))
			}
			renderRow(0, "Opus", m.p.OpusModel)
			renderRow(1, "Sonnet", m.p.SonnetModel)
			renderRow(2, "Haiku", m.p.HaikuModel)
			renderRow(3, "Custom", m.p.CustomModelID)
			renderRow(4, "Subagent", subagentMappingDisplay(*m.p))

			testPrefix := "  "
			testLabel := locale.T("测试模型可用性", "Test model availability")
			testDetail := locale.T("将为每个模型发送一次最小请求", "Sends one minimal request per model")
			if smokeModel := m.availabilitySmokeTestModel(); smokeModel != "" {
				testDetail = fmt.Sprintf(locale.T("仅使用 %s 发送一次低成本测试请求", "Uses one low-cost test request with %s"), smokeModel)
			}
			if m.modelTesting {
				spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
				spin := spinners[m.modelTestFrame%len(spinners)]
				testLabel = fmt.Sprintf("%s %s", spin, locale.T("正在测试模型可用性...", "Testing model availability..."))
				if smokeModel := m.availabilitySmokeTestModel(); smokeModel != "" {
					testDetail = fmt.Sprintf(locale.T("正在使用 %s 测试 · 按 esc 取消", "Testing with %s · press esc to cancel"), smokeModel)
				} else {
					testDetail = locale.T("测试进行中 · 按 esc 取消", "Test in progress · press esc to cancel")
				}
			} else if len(m.modelAvailability) > 0 {
				available, unavailable := m.availabilityCounts()
				testDetail = fmt.Sprintf(locale.T("%d 个可用 · %d 个不可用 · 可用模型已前置", "%d available · %d unavailable · available models first"), available, unavailable)
			} else if m.modelTestCanceled {
				testDetail = locale.T("测试已取消，结果未应用", "Test canceled; results were not applied")
			}
			if m.cursor == slotTestCursor {
				testPrefix = selectedStyle.Render("> ")
				testLabel = selectedStyle.Render(testLabel)
			} else {
				testLabel = purpleText.Render(testLabel)
			}
			body.WriteString("\n" + testPrefix + testLabel + "\n")
			body.WriteString(grayText.Render("    "+testDetail) + "\n")

			body.WriteString(renderBottomButtons(m.page, m.cursor, slotNextCursor, slotBackCursor))
			body.WriteString(grayText.Render(locale.T("↑↓ 移动光标 · ←→ 切换按钮 · enter 选择、测试或跳转", "↑↓ Move · ←→ Toggle Buttons · enter select, test, or continue")))
		} else {
			slotName := []string{"Opus", "Sonnet", "Haiku", "Custom", "Subagent"}[m.activeSlot]
			body.WriteString(titleStyle.Render(fmt.Sprintf(locale.T("配置槽位 [%s] 模型筛选", "Select Model for Slot [%s]"), slotName)))
			body.WriteString("\n" + filterStyle.Render(locale.T("🔍 过滤模型: ", "🔍 Filter model: ")) + m.filterInput.View() + "\n")
			// Virtual scrolling: only render visible window of filteredPool
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
				line := grayText.Render(mod)
				status := ""
				if stringInSlice(mod, m.modelPool) {
					status = "  " + m.availabilityLabel(mod)
				}
				if i == m.slotListCursor {
					prefix = selectedStyle.Render(" > ")
					line = selectedStyle.Render(mod)
				}
				body.WriteString(prefix + line + status + "\n")
			}
			if end < len(m.filteredPool) {
				body.WriteString(grayText.Render(fmt.Sprintf("   ↓ ... %d more below ...", len(m.filteredPool)-end)) + "\n")
			}
			body.WriteString(selectedStyle.Render(fmt.Sprintf("  %d/%d", m.slotListCursor+1, len(m.filteredPool))) + "\n\n" + grayText.Render(locale.T("状态来自可用性测试 · 键盘输入过滤 · ↑↓ 选择 · enter 锁定 · esc 取消", "Status comes from availability test · type to filter · ↑↓ scroll · enter lock · esc cancel")) + "\n")
		}

	case 2:
		// ==================== PAGE 2: Extended Context + Auto Compact ====================
		// Layout matches the product mockup: checkbox matrix + radio list.
		body.WriteString(m.renderPageHeader(locale.T("上下文与自动压缩", "Context & Compact"), "Context"))
		body.WriteString(grayText.Render(locale.T(
			"[1m] 按槽位；压缩为 Provider 全局 · 同名模型切换会同步",
			"[1m] per-slot; compact is provider-wide · same model syncs",
		)) + "\n\n")

		body.WriteString(titleStyle.Render("Extended Context") + "\n")

		renderContextRow := func(idx int, label, modelVal string) {
			slotKey := []string{"opus", "sonnet", "haiku", "custom", "subagent"}[idx]
			box := "[ ]"
			if m.oneMSlots[slotKey] {
				box = "[x]"
			}

			prefix := "  "
			labelStyle := grayText
			if m.cursor == idx {
				prefix = selectedStyle.Render("> ")
				labelStyle = titleStyle
			}

			displayModel := stripOneMSuffix(modelVal)
			if displayModel == "" && slotKey == "subagent" {
				displayModel = subagentMappingDisplay(*m.p)
			}
			modelPart := cyanText.Render(displayModel)
			if strings.TrimSpace(displayModel) == "" {
				modelPart = grayText.Render(locale.T("(未设置)", "(unset)"))
			}

			capLabel := grayText.Render("Standard/unknown")
			if m.oneMSlots[slotKey] {
				capLabel = lightning
			} else if recommendedOneMModel(modelVal) {
				capLabel = availableStyle.Render(locale.T("建议 1M", "1M recommended"))
			} else if window, ok := m.modelContextWindows[stripOneMSuffix(modelVal)]; ok && protocol.ContextWindowSuggests1M(window) {
				capLabel = availableStyle.Render("1M reported")
			}

			// Columns: [x] Label   model   capacity
			body.WriteString(fmt.Sprintf("%s%s %-10s %-28s %s\n",
				prefix, box, labelStyle.Render(label), modelPart, capLabel))
		}

		renderContextRow(0, "Opus", m.p.OpusModel)
		renderContextRow(1, "Sonnet", m.p.SonnetModel)
		renderContextRow(2, "Haiku", m.p.HaikuModel)
		renderContextRow(3, "Custom", m.p.CustomModelID)
		renderContextRow(4, "Subagent", m.p.SubagentModel)

		body.WriteString("\n" + titleStyle.Render("Auto Compact") + "\n")
		selectedRadio := m.selectedCompactRadioIndex()
		for i, preset := range compactRadioOrder {
			cursorIdx := oneMCompactStart + i
			radio := "( )"
			if i == selectedRadio {
				radio = purpleText.Render("(●)")
			}
			prefix := "  "
			label := grayText.Render(compactRadioLabel(preset))
			if m.cursor == cursorIdx {
				prefix = selectedStyle.Render("> ")
				label = titleStyle.Render(compactRadioLabel(preset))
			}
			body.WriteString(fmt.Sprintf("%s%s %s\n", prefix, radio, label))
		}

		body.WriteString(renderBottomButtons(m.page, m.cursor, oneMNextCursor, oneMBackCursor))
		body.WriteString(grayText.Render(locale.T("enter 切换 · ↑↓ 移动 · ←→ 按钮", "enter toggle · ↑↓ move · ←→ buttons")))

	case 3:
		// Effort configuration was removed from ccl set. Jump to review if reached.
		m.page = 4
		m.cursor = m.page4InitialCursor()
		body.WriteString(grayText.Render(locale.T(
			"Reasoning Effort 已改由 Claude Code 管理（/effort、--effort）…",
			"Reasoning Effort is managed by Claude Code (/effort, --effort)…",
		)) + "\n")

	case 4:
		// ==================== PAGE 4: compact configuration summary ====================
		body.WriteString(m.renderPageHeader(locale.T("核对并应用", "Review & Apply"), "Confirm"))

		// Connection
		body.WriteString(titleStyle.Render("Connection") + "\n")
		body.WriteString(fmt.Sprintf("  %-11s %s\n", "Endpoint", cyanText.Render(truncateMiddle(m.p.Endpoint, 52))))
		if m.canToggleOpenAIProtocol() {
			prefix := "  "
			if m.cursor == m.page4ProtocolCursor() {
				prefix = selectedStyle.Render("> ")
			}
			chat := "( ) Chat"
			responses := "( ) Responses"
			if provider.IsOpenAIResponsesType(m.p.Type) {
				responses = purpleText.Render("(●) Responses")
			} else {
				chat = purpleText.Render("(●) Chat")
			}
			body.WriteString(fmt.Sprintf("%s%-11s %s   %s\n", prefix, "Protocol", chat, responses))
		} else {
			body.WriteString(fmt.Sprintf("  %-11s %s\n", "Protocol", purpleText.Render(m.getProtocol())))
		}
		body.WriteString(fmt.Sprintf("  %-11s %s\n", "Auth", purpleText.Render(providerAuthLabel(*m.p))))

		// Model Mapping (includes [1M] badges; no separate Context line)
		body.WriteString("\n" + titleStyle.Render("Model Mapping") + "\n")
		body.WriteString(renderReviewModelMapping("Opus", m.p.OpusModel, m.oneMSlots["opus"]))
		body.WriteString(renderReviewModelMapping("Sonnet", m.p.SonnetModel, m.oneMSlots["sonnet"]))
		body.WriteString(renderReviewModelMapping("Haiku", m.p.HaikuModel, m.oneMSlots["haiku"]))
		body.WriteString(renderReviewModelMapping("Custom", m.p.CustomModelID, m.oneMSlots["custom"]))
		body.WriteString(renderReviewModelMapping("Subagent", subagentMappingDisplay(*m.p), m.oneMSlots["subagent"]))

		// Runtime
		body.WriteString("\n" + titleStyle.Render("Runtime") + "\n")
		body.WriteString(fmt.Sprintf("  %-11s %s\n", "Effort", purpleText.Render(locale.T("由 Claude Code 管理", "Claude Code managed"))))
		if strings.TrimSpace(m.p.EffortLevel) != "" {
			body.WriteString(grayText.Render(fmt.Sprintf(
				locale.T("  提示：配置中仍有 effortLevel=%s，会覆盖 Claude Code；保存后将清除", "  note: saved effortLevel=%s overrides Claude Code; cleared on save"),
				m.p.EffortLevel,
			)) + "\n")
		}
		compactHint := ""
		switch m.compactPreset {
		case compactPreset200K:
			compactHint = "  ~140K"
		case compactPreset500K:
			compactHint = "  ~400K"
		case compactPreset1M:
			compactHint = "  ~900K"
		}
		body.WriteString(fmt.Sprintf("  %-11s %s%s\n", "Compact", purpleText.Render(m.compactSummary()), grayText.Render(compactHint)))
		if m.manualConfig {
			runtimeSettings := claude.ResolveRuntimeSettings(*m.p)
			maxOut := runtimeSettings.MaxOutputTokens
			if maxOut == "32000" {
				maxOut = "32K"
			}
			body.WriteString(fmt.Sprintf("  %-11s %s\n", "Max Output", purpleText.Render(maxOut)))
			body.WriteString(fmt.Sprintf("  %-11s %s concurrent\n", "Tools", purpleText.Render(runtimeSettings.ToolUseConcurrency)))
			toolSearch := runtimeSettings.ToolSearch
			if toolSearch == "false" {
				toolSearch = "Off"
			} else if toolSearch == "true" {
				toolSearch = "On"
			}
			body.WriteString(fmt.Sprintf("  %-11s %s\n", "Tool Search", purpleText.Render(toolSearch)))
		}

		// Active checkbox
		body.WriteString("\n")
		activePrefix := "  "
		if m.cursor == m.page4ActiveCursor() {
			activePrefix = selectedStyle.Render("> ")
		}
		activeBox := "[ ]"
		if m.IsActiveChosen {
			activeBox = "[x]"
		}
		activeLabel := locale.T("设为当前激活 Provider", "Set as active provider")
		if m.cursor == m.page4ActiveCursor() {
			body.WriteString(fmt.Sprintf("%s%s %s\n", activePrefix, selectedStyle.Render(activeBox), titleStyle.Render(activeLabel)))
		} else {
			body.WriteString(fmt.Sprintf("%s%s %s\n", activePrefix, activeBox, grayText.Render(activeLabel)))
		}

		// Actions
		applyLabel := locale.T("应用并完成", "Apply & Finish")
		backLabel := locale.T("返回", "Back")
		applyStr := "  " + applyLabel
		backStr := "  " + backLabel
		if m.cursor == m.page4SaveCursor() {
			applyStr = selectedStyle.Render("> " + applyLabel)
		}
		if m.cursor == m.page4BackCursor() {
			backStr = selectedStyle.Render("> " + backLabel)
		}
		body.WriteString("\n" + applyStr + "             " + backStr + "\n\n")
		if m.canToggleOpenAIProtocol() {
			body.WriteString(grayText.Render(locale.T("↑↓ 移动 · ←→ 改协议 · enter 选择/应用", "↑↓ Move · ←→ Change protocol · enter Select/Apply")))
		} else {
			body.WriteString(grayText.Render(locale.T("↑↓ 移动 · enter 选择/应用", "↑↓ Move · enter Select/Apply")))
		}

	case 5:
		// ==================== PAGE 5: 配置模式选择 ====================
		body.WriteString(m.renderPageHeader(locale.T("配置模式选择", "Config Mode"), "Choice"))
		body.WriteString(grayText.Render(fmt.Sprintf(locale.T("已从接口获取 %d 个模型，请选择配置方式：", "Fetched %d models from provider API. Choose config mode:"), len(m.modelPool))) + "\n")
		if m.modelPoolFromDiscovery && m.hadLocalModelPool {
			body.WriteString(grayText.Render(locale.T("旧本地模型池将用本次接口结果刷新。", "The local model pool will be refreshed with this API result.")) + "\n")
		}
		body.WriteString("\n")

		autoPrefix := "  "
		autoLabel := grayText.Render(locale.T("🔄 自动配置 (推荐)", "🔄 Auto Config (recommended)"))
		autoDesc := grayText.Render("    " + locale.T("自动填入前 4 个可用模型，跳过手动 1M 配置", "Auto-fill first 4 models; skip manual 1M config"))
		if m.cursor == 0 {
			autoPrefix = selectedStyle.Render("> ")
			autoLabel = selectedStyle.Render(locale.T("🔄 自动配置 (推荐)", "🔄 Auto Config (recommended)"))
		}
		body.WriteString(autoPrefix + autoLabel + "\n")
		body.WriteString(autoDesc + "\n\n")

		manualPrefix := "  "
		manualLabel := grayText.Render(locale.T("🛠 手动配置", "🛠 Manual Config"))
		manualDesc := grayText.Render("    " + locale.T("手动选择每个槽位的模型与 1M 上下文开关", "Manually set slot models and 1M context"))
		if m.cursor == 1 {
			manualPrefix = selectedStyle.Render("> ")
			manualLabel = selectedStyle.Render(locale.T("🛠 手动配置", "🛠 Manual Config"))
		}
		body.WriteString(manualPrefix + manualLabel + "\n")
		body.WriteString(manualDesc + "\n\n")

		if m.showStaleSlotToggle() {
			cleanupPrefix := "  "
			cleanupLabel := grayText.Render(locale.T("手动配置时清理旧槽位", "Clean stale slots for manual config"))
			if m.cursor == 2 {
				cleanupPrefix = selectedStyle.Render("> ")
				cleanupLabel = selectedStyle.Render(locale.T("手动配置时清理旧槽位", "Clean stale slots for manual config"))
			}
			box := "[ ]"
			state := locale.T("否", "No")
			if m.clearStaleSlots {
				box = "[x]"
				state = locale.T("是", "Yes")
			}
			cleanupDesc := grayText.Render(fmt.Sprintf("    %s %s · %s",
				box,
				state,
				fmt.Sprintf(locale.T("将清空 %d 个不在接口模型列表中的旧槽位", "Clear %d old slot(s) not present in the API model list"), m.staleSlotCount()),
			))
			body.WriteString(cleanupPrefix + cleanupLabel + "\n")
			body.WriteString(cleanupDesc + "\n\n")
		}

		body.WriteString(grayText.Render(locale.T("↑↓ 选择 · ←→ 切换清理选项 · enter 确认 · esc 返回", "↑↓ Select · ←→ Toggle cleanup · enter confirm · esc back")))
	}

	panelStyle := windowStyle.Width(m.panelWidth())
	if m.page == 4 {
		panelStyle = panelStyle.Padding(0, 2)
	}
	panel := panelStyle.Render(body.String())
	langTipMsg := locale.T(
		"💡 提示: 使用 `ccl lang` 更改终端显示语言",
		"💡 Tip: Change the TUI display language with `ccl lang`",
	)
	content := panel
	if m.page != 4 {
		footer := m.renderStepProgress() + "\n" + grayText.Render(langTipMsg)
		content += "\n\n" + footer
	}
	finalStr := content
	if m.width > 0 && m.height > 0 {
		finalStr = lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, content)
	}
	v := tea.NewView(finalStr)
	v.AltScreen = true
	return v
}
