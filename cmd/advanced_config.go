package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

var (
	windowStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("237")).
			Padding(1, 2).
			Width(70)

	titleStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Bold(true)
	badgeStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("141")).Bold(true).MarginLeft(2)
	protoBadgeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("141")).MarginLeft(2)
	cyanText        = lipgloss.NewStyle().Foreground(lipgloss.Color("49"))
	purpleText      = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))
	grayText        = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	errorText       = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	selectedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
	lightning       = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true).Render("⚡1M")
	filterStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	langTipStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("229")).Background(lipgloss.Color("239")).Padding(0, 1).MarginTop(1)
)

const (
	filterViewHeight     = 15 // max visible items in filter list
	credentialInputWidth = 58
)

type AdvancedConfigModel struct {
	p         *provider.Provider
	modelPool []string
	oneMSlots map[string]bool

	modelPoolFromDiscovery bool
	clearStaleSlots        bool
	hadLocalModelPool      bool

	page   int
	cursor int

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

	// Page 3
	effortLevels []string
	effortCursor int

	// Page 4
	IsActiveChosen bool
}

type modelFetchTickMsg struct{}

type modelFetchDoneMsg struct {
	endpoint            string
	apiKey              string
	detectedType        string
	detectedEndpoint    string
	anthropicAuth       string
	discoveredModelsRaw string
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

	m := &AdvancedConfigModel{
		p:               p,
		oneMSlots:       make(map[string]bool),
		page:            0,
		cursor:          0,
		urlInput:        ui,
		keyInput:        ki,
		filterInput:     fi,
		effortLevels:    []string{"", "low", "medium", "high", "xhigh", "max", "ultracode"},
		effortCursor:    0,
		IsActiveChosen:  true,
		clearStaleSlots: true,
	}

	// 从已有配置中读取 EffortLevel 的默认值
	if p.EffortLevel != "" {
		for i, level := range m.effortLevels {
			if level == p.EffortLevel {
				m.effortCursor = i
				break
			}
		}
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

	return m
}

// NewAdvancedConfigModelAtPage1 creates a model starting at page 1 (slot mapping)
// with a pre-populated model pool, skipping the credential page.
func NewAdvancedConfigModelAtPage1(p *provider.Provider, modelPool []string) *AdvancedConfigModel {
	m := NewAdvancedConfigModel(p)
	m.page = 1
	m.cursor = 4 // start at Opus slot
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
		return modelFetchDoneMsg{
			endpoint:            endpoint,
			apiKey:              apiKey,
			detectedType:        result.protocol,
			detectedEndpoint:    result.baseURL,
			anthropicAuth:       result.anthropicAuth,
			discoveredModelsRaw: result.models,
			err:                 result.err,
		}
	}
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

// doAutoConfig auto-fills slot models with the first 4 available from modelPool,
// leaves Claude's own effort setting in control, and clears 1M slots.
func (m *AdvancedConfigModel) doAutoConfig() {
	slots := []*string{&m.p.OpusModel, &m.p.SonnetModel, &m.p.HaikuModel, &m.p.CustomModelID}
	for i, ptr := range slots {
		if i < len(m.modelPool) {
			*ptr = m.modelPool[i]
		} else {
			*ptr = ""
		}
	}
	// Default: no 1M context
	m.oneMSlots = make(map[string]bool)
	// Default: do not override Claude's own effort setting.
	m.p.EffortLevel = ""
	m.effortCursor = 0
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

func (m *AdvancedConfigModel) goBack() {
	oldPage := m.page
	oldCursor := m.cursor
	if m.page == 1 {
		// Go back from slot mapping to config mode choice
		m.page = 5
		m.cursor = 1 // pre-select Manual Config (since user was in manual mode)
	} else if m.page == 5 {
		// Go back from choice to credentials
		m.page = 0
		m.cursor = 2
	} else if m.page > 0 {
		m.page--
		if m.page == 0 {
			m.cursor = 2
		}
		if m.page == 1 {
			m.cursor = 4
		}
		if m.page == 2 {
			m.cursor = 4
		}
		if m.page == 3 {
			m.cursor = m.effortNextCursor()
		}
	}
	setDebugf("goBack old_page=%d old_cursor=%d new_page=%d new_cursor=%d", oldPage, oldCursor, m.page, m.cursor)
}

func (m *AdvancedConfigModel) effortNextCursor() int {
	return len(m.effortLevels)
}

func (m *AdvancedConfigModel) effortBackCursor() int {
	return len(m.effortLevels) + 1
}

func (m *AdvancedConfigModel) effortLabel(level string) string {
	if level == "" {
		return locale.T("默认（跟随 Claude 设置）", "Default (follow Claude setting)")
	}
	return level
}

func reviewOneMSummary(oneMSlots map[string]bool) string {
	var slots []string
	for _, slot := range []string{"opus", "sonnet", "haiku", "custom"} {
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
	if detectedEndpoint != "" {
		m.p.Endpoint = detectedEndpoint
	}
	if detectedType != "" {
		m.p.Type = detectedType
		m.p.AnthropicAuth = ""
	}
	if detectedType == "anthropic" {
		m.p.Endpoint = protocol.NormalizeAnthropicBaseURLForClaude(m.p.Endpoint)
		if anthropicAuth != "" {
			m.p.AnthropicAuth = anthropicAuth
		}
	}

	m.modelPool = []string{}
	m.modelPoolFromDiscovery = false
	if derr == nil && len(discoveredModels) > 0 {
		m.modelPool = discoveredModels
		m.modelPoolFromDiscovery = true
		m.p.Model = strings.Join(discoveredModels, ",")
		setDebugf("applyModelDetectionResult using discovered model pool count=%d", len(m.modelPool))
	}

	if derr != nil {
		m.detectionError = derr
		m.page = 0
		m.cursor = 2
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
		m.cursor = 2
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
		if !m.detecting || msg.endpoint != m.p.Endpoint || msg.apiKey != m.p.APIKey {
			setDebugf(
				"modelFetchDone ignored detecting=%t endpoint_match=%t api_key_match=%t msg_endpoint=%q provider_endpoint=%q",
				m.detecting,
				msg.endpoint == m.p.Endpoint,
				msg.apiKey == m.p.APIKey,
				msg.endpoint,
				m.p.Endpoint,
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
		return m, m.applyModelDetectionResult(msg.detectedType, msg.discoveredModelsRaw, msg.anthropicAuth, msg.detectedEndpoint, msg.err)

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		}

		if m.detecting {
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
				if m.page == 0 && (m.cursor == 2 || m.cursor == 3) {
					m.cursor = 1
				} else if m.page == 1 && (m.cursor == 4 || m.cursor == 5) {
					m.cursor = 3
				} else if m.page == 2 && (m.cursor == 4 || m.cursor == 5) {
					m.cursor = 3
				} else if m.page == 3 && (m.cursor == m.effortNextCursor() || m.cursor == m.effortBackCursor()) {
					m.cursor = len(m.effortLevels) - 1
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
				if m.cursor < 2 {
					m.cursor++
				}
			} else if m.page == 1 {
				if m.cursor < 4 {
					m.cursor++
				}
			} else if m.page == 2 {
				if m.cursor < 4 {
					m.cursor++
				}
			} else if m.page == 3 {
				if m.cursor < m.effortBackCursor() {
					m.cursor++
				}
			} else if m.page == 4 {
				if m.cursor < 2 {
					m.cursor++
				}
			} else if m.page == 5 {
				if m.cursor < m.page5MaxCursor() {
					m.cursor++
				}
			}

		case "left", "h":
			if m.page == 0 && m.cursor == 3 {
				m.cursor = 2
			}
			if m.page == 1 && m.cursor == 5 {
				m.cursor = 4
			}
			if m.page == 2 && m.cursor == 5 {
				m.cursor = 4
			}
			if m.page == 3 && m.cursor == m.effortBackCursor() {
				m.cursor = m.effortNextCursor()
			}
			if m.page == 4 && m.cursor < 2 {
				m.IsActiveChosen = true
				setDebugf("page4 active choice toggled active_chosen=%t", m.IsActiveChosen)
			}
			if m.page == 5 && m.cursor == 2 && m.showStaleSlotToggle() {
				m.clearStaleSlots = true
				setDebugf("page5 stale slot cleanup toggled clear_stale_slots=%t stale_slot_count=%d", m.clearStaleSlots, m.staleSlotCount())
			}

		case "right", "l":
			if m.page == 0 && m.cursor == 2 {
				m.cursor = 3
			}
			if m.page == 1 && m.cursor == 4 {
				m.cursor = 5
			}
			if m.page == 2 && m.cursor == 4 {
				m.cursor = 5
			}
			if m.page == 3 && m.cursor == m.effortNextCursor() {
				m.cursor = m.effortBackCursor()
			}
			if m.page == 4 && m.cursor < 2 {
				m.IsActiveChosen = false
				setDebugf("page4 active choice toggled active_chosen=%t", m.IsActiveChosen)
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
			if m.page == 0 && m.cursor > 3 {
				m.cursor = 0
			} else if m.page == 1 && m.cursor > 5 {
				m.cursor = 0
			} else if m.page == 2 && m.cursor > 5 {
				m.cursor = 0
			} else if m.page == 3 && m.cursor > m.effortBackCursor() {
				m.cursor = 0
			} else if m.page == 4 && m.cursor > 2 {
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
					m.cursor = 3
				} else if m.page == 1 {
					m.cursor = 5
				} else if m.page == 2 {
					m.cursor = 5
				} else if m.page == 3 {
					m.cursor = m.effortBackCursor()
				} else if m.page == 4 {
					m.cursor = 2
				} else if m.page == 5 {
					m.cursor = m.page5MaxCursor()
				}
			}

		case "enter":
			// 如果点击了底部的“上一步”按钮，直接返回
			if (m.page == 0 && m.cursor == 3) || (m.page == 1 && m.cursor == 5) || (m.page == 2 && m.cursor == 5) || (m.page == 3 && m.cursor == m.effortBackCursor()) {
				m.goBack()
				return m, nil
			}

			switch m.page {
			case 0:
				if m.cursor == 0 {
					m.cursor = 1
					m.urlInput.Blur()
					m.keyInput.Focus()
					setDebugf("page0 enter endpoint field complete next_cursor=%d endpoint=%q", m.cursor, m.urlInput.Value())
				} else if m.cursor == 1 {
					m.cursor = 2
					setDebugf("page0 enter api key field complete next_cursor=%d api_key_len=%d", m.cursor, len(m.keyInput.Value()))
				} else if m.cursor == 2 {
					m.p.Endpoint = m.urlInput.Value()
					m.p.APIKey = m.keyInput.Value()
					m.urlInput.Blur()
					m.keyInput.Blur()
					m.detectionError = nil
					m.detecting = true
					m.detectProgress = 5
					m.detectFrame = 0
					setDebugf("page0 start detection endpoint=%q api_key_len=%d", m.p.Endpoint, len(m.p.APIKey))
					return m, tea.Batch(modelFetchCmd(m.p.Endpoint, m.p.APIKey), modelFetchTickCmd())
				}
			case 1:
				if !m.filterInput.Focused() {
					if m.cursor == 4 {
						m.page = 2
						m.cursor = 4 // default to Next button
						setDebugf("page1 next to page2 slots=%s", slotDebugSummary(*m.p))
					} else {
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
					ptr := []*string{&m.p.OpusModel, &m.p.SonnetModel, &m.p.HaikuModel, &m.p.CustomModelID}[m.activeSlot]
					*ptr = selectedModel
					m.filterInput.Blur()
					setDebugf("page1 slot selected active_slot=%d model=%q slots=%s", m.activeSlot, selectedModel, slotDebugSummary(*m.p))
				}
			case 2:
				if m.cursor < 4 {
					// 当光标在槽位上时，按 enter 切换 1M 状态
					slot := []string{"opus", "sonnet", "haiku", "custom"}[m.cursor]
					m.oneMSlots[slot] = !m.oneMSlots[slot]
					setDebugf("page2 toggle one_m slot=%s enabled=%t summary=%s", slot, m.oneMSlots[slot], reviewOneMSummary(m.oneMSlots))
					m.cursor++
				} else if m.cursor == 4 {
					// 当光标在“下一步”按钮上时，按 enter 前进到 Page 3
					m.page = 3
					m.cursor = m.effortNextCursor() // default to Next button
					setDebugf("page2 next to page3 one_m=%s", reviewOneMSummary(m.oneMSlots))
				}
			case 3:
				if m.cursor < len(m.effortLevels) {
					m.effortCursor = m.cursor
					setDebugf("page3 effort selected cursor=%d level=%q label=%q", m.cursor, m.effortLevels[m.effortCursor], m.effortLabel(m.effortLevels[m.effortCursor]))
				} else {
					m.p.EffortLevel = m.effortLevels[m.effortCursor]
					m.page = 4
					m.cursor = 2 // default to Save & Finish
					setDebugf("page3 next to review effort=%q label=%q", m.p.EffortLevel, m.effortLabel(m.p.EffortLevel))
				}
			case 4:
				if m.cursor < 2 {
					m.cursor = 2
					setDebugf("page4 active choice confirmed active_chosen=%t cursor=%d", m.IsActiveChosen, m.cursor)
				} else {
					setDebugf("page4 save requested provider=%q type=%q effort=%q model_count=%d slots=%s one_m=%s active_chosen=%t", m.p.Name, m.p.Type, m.p.EffortLevel, countCSV(m.p.Model), slotDebugSummary(*m.p), reviewOneMSummary(m.oneMSlots), m.IsActiveChosen)
					return m, tea.Quit
				}
			case 5:
				// Page 5: Auto / Manual config choice
				if m.cursor == 0 {
					// Auto Config: auto-fill slots, set effort=max, skip 1M, go to save
					m.applyStaleSlotPolicy()
					m.doAutoConfig()
					m.page = 4
					m.cursor = 2 // focus on save button
					setDebugf("page5 auto config selected next_page=%d cursor=%d slots=%s effort=%q", m.page, m.cursor, slotDebugSummary(*m.p), m.p.EffortLevel)
				} else if m.cursor == 1 {
					// Manual Config: go to slot mapping (old page 1)
					m.applyStaleSlotPolicy()
					m.page = 1
					m.cursor = 4
					setDebugf("page5 manual config selected next_page=%d cursor=%d clear_stale_slots=%t slots=%s", m.page, m.cursor, m.clearStaleSlots, slotDebugSummary(*m.p))
				} else if m.cursor == 2 && m.showStaleSlotToggle() {
					m.clearStaleSlots = !m.clearStaleSlots
					setDebugf("page5 stale slot cleanup toggled clear_stale_slots=%t stale_slot_count=%d", m.clearStaleSlots, m.staleSlotCount())
				}
			}
		}
	}

	if m.page == 0 {
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

func renderModelFetchProgress(progress, frame int) string {
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

func (m *AdvancedConfigModel) View() tea.View {
	var body strings.Builder
	protoLabel := protoBadgeStyle.Render("Protocol: " + m.getProtocol())

	switch m.page {
	case 0:
		// ==================== PAGE 0: 凭据配置 ====================
		body.WriteString(titleStyle.Render(locale.T("基础凭据配置", "Base Credentials")) + badgeStyle.Render("Credentials") + protoLabel + "\n\n")
		body.WriteString(renderCredentialField("Endpoint URL", m.urlInput.View(), m.cursor == 0))
		body.WriteString(renderCredentialField("API Key", m.keyInput.View(), m.cursor == 1))

		if m.detecting {
			body.WriteString(renderModelFetchProgress(m.detectProgress, m.detectFrame))
		} else {
			if m.detectionError != nil {
				body.WriteString(errorText.Render(locale.T("检测失败，无法继续：", "Detection failed; cannot continue:")) + "\n")
				body.WriteString(errorText.Render("  "+m.detectionError.Error()) + "\n\n")
			}
			body.WriteString(renderBottomButtons(m.page, m.cursor, 2, 3))
			body.WriteString(grayText.Render(locale.T("↑↓ 切换焦点 · ←→ 切换按钮 · enter 确认", "↑↓ Switch · ←→ Toggle Buttons · enter confirm")))
		}

	case 1:
		// ==================== PAGE 1: 槽位映射配置 ====================
		if !m.filterInput.Focused() {
			body.WriteString(titleStyle.Render(locale.T("Claude Slot 映射配置", "Claude Slot Mapping")) + badgeStyle.Render("Slot List") + protoLabel + "\n\n")
			renderRow := func(idx int, label, val string) {
				prefix := "  "
				var labelStr string
				if m.cursor == idx {
					prefix = selectedStyle.Render("> ")
					labelStr = selectedStyle.Render(label) + grayText.Render(" ("+locale.T("enter 筛选", "enter to list")+")")
				} else {
					// ✅ 修复：先对纯文本 label 填充至 6 宽，再加颜色
					labelStr = purpleText.Render(fmt.Sprintf("%-6s", label))
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

			body.WriteString(renderBottomButtons(m.page, m.cursor, 4, 5))
			body.WriteString(grayText.Render(locale.T("↑↓ 移动光标 · ←→ 切换按钮 · enter 选择编辑/跳转", "↑↓ Move · ←→ Toggle Buttons · enter select")))
		} else {
			slotName := []string{"Opus", "Sonnet", "Haiku", "Custom"}[m.activeSlot]
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
				if i == m.slotListCursor {
					prefix = selectedStyle.Render(" > ")
					line = selectedStyle.Render(mod)
				}
				body.WriteString(prefix + line + "\n")
			}
			if end < len(m.filteredPool) {
				body.WriteString(grayText.Render(fmt.Sprintf("   ↓ ... %d more below ...", len(m.filteredPool)-end)) + "\n")
			}
			body.WriteString(selectedStyle.Render(fmt.Sprintf("  %d/%d", m.slotListCursor+1, len(m.filteredPool))) + "\n\n" + grayText.Render(locale.T("键盘输入任意字符快速过滤 · ↑↓ 选择 · enter 锁定 · esc 取消", "Type to filter · ↑↓ Scroll · enter lock · esc cancel")) + "\n")
		}

	case 2:
		// ==================== PAGE 2: 1M 上下文配置页 ====================
		body.WriteString(titleStyle.Render(locale.T("1M 上下文配置", "1M Context")) + badgeStyle.Render("MultiSelect") + protoLabel + "\n")
		body.WriteString(grayText.Render(locale.T("enter 切换开关状态", "enter Toggle Slot Status")) + "\n\n")

		renderMultiRow := func(idx int, label, modelVal string) {
			box := "[ ]"
			slotKey := []string{"opus", "sonnet", "haiku", "custom"}[idx]
			if m.oneMSlots[slotKey] {
				box = "[x]"
			}

			// ✅ 修复：统一包装前缀，确保选中与未选中时完美对齐
			var indicator string
			if m.cursor == idx {
				indicator = selectedStyle.Render("> " + box)
			} else {
				indicator = "  " + box
			}

			// ✅ 核心修复：先对纯文本进行 14 位填充对齐，然后再渲染样式！
			paddedLabel := fmt.Sprintf("%-14s", label)
			itemText := grayText.Render(paddedLabel)
			if m.cursor == idx {
				itemText = titleStyle.Render(paddedLabel)
			}

			modelStr := cyanText.Render(modelVal)
			if modelVal == "" {
				modelStr = grayText.Render(locale.T("(未设置)", "(unset)"))
			}

			suffix := ""
			if m.oneMSlots[slotKey] {
				suffix = " " + lightning
			}

			// ✅ 修复：这里直接拼接 %s，不再使用破绽百出的 %-14s
			body.WriteString(fmt.Sprintf("%s  %s – %s%s\n", indicator, itemText, modelStr, suffix))
		}

		renderMultiRow(0, "Opus Config", m.p.OpusModel)
		renderMultiRow(1, "Sonnet Config", m.p.SonnetModel)
		renderMultiRow(2, "Haiku Config", m.p.HaikuModel)
		renderMultiRow(3, "Custom Config", m.p.CustomModelID)

		body.WriteString(renderBottomButtons(m.page, m.cursor, 4, 5))
		body.WriteString(grayText.Render(locale.T("enter 切换 · ↑↓ 移动 · ←→ 切换按钮", "enter Toggle · ↑↓ Move · ←→ Toggle Buttons")))

	case 3:
		// ==================== PAGE 3: Reasoning Effort 思考流配置 ====================
		body.WriteString(titleStyle.Render(locale.T("Reasoning Effort 思考流配置", "Reasoning Effort")) + badgeStyle.Render("Effort") + protoLabel + "\n\n")
		for i, level := range m.effortLevels {
			label := m.effortLabel(level)
			prefix := "  "
			if m.cursor == i {
				prefix = selectedStyle.Render("> ")
			}
			radio := "( )"
			if m.effortCursor == i {
				radio = purpleText.Render("(●)")
			}
			itemText := grayText.Render(label)
			if m.cursor == i {
				itemText = titleStyle.Render(label)
			}
			body.WriteString(fmt.Sprintf("%s%s %s\n", prefix, radio, itemText))
		}

		body.WriteString(renderBottomButtons(m.page, m.cursor, m.effortNextCursor(), m.effortBackCursor()))
		body.WriteString(grayText.Render(locale.T("↑↓ 移动选择级别 · ←→ 切换按钮 · enter 前进/后退", "↑↓ Move · ←→ Toggle Buttons · enter next/back")))

	case 4:
		// ==================== PAGE 4: 核对并应用此配置 ====================
		body.WriteString(titleStyle.Render(locale.T("核对并应用此 Provider 配置", "Review & Apply")) + badgeStyle.Render("Confirm") + "\n\n")
		body.WriteString(fmt.Sprintf("  • %-12s: %s\n", "Endpoint", cyanText.Render(m.p.Endpoint)))
		body.WriteString(fmt.Sprintf("  • %-12s: %s\n", "Protocol", purpleText.Render(m.getProtocol())))
		body.WriteString(fmt.Sprintf("  • %-12s: %s\n", "Auth", purpleText.Render(providerAuthLabel(*m.p))))
		body.WriteString(fmt.Sprintf("  • %-12s: %s\n", "Effort Level", purpleText.Render(m.effortLabel(m.effortLevels[m.effortCursor]))))
		body.WriteString(fmt.Sprintf("  • %-12s: %s\n", "1M Context", purpleText.Render(reviewOneMSummary(m.oneMSlots))))

		body.WriteString("\n  " + locale.T("是否将该 Provider 设为当前激活配置？", "Set as active provider right now?") + "\n\n")

		yesStr, noStr := " "+locale.T("是", "Yes")+" ", " "+locale.T("否", "No")+" "
		if m.IsActiveChosen {
			yesStr = selectedStyle.Render(" > " + locale.T("是", "Yes") + " <")
			noStr = grayText.Render("  " + locale.T("否", "No") + " ")
		} else {
			yesStr = grayText.Render("  " + locale.T("是", "Yes") + " ")
			noStr = selectedStyle.Render(" > " + locale.T("否", "No") + " <")
		}
		if m.cursor == 0 || m.cursor == 1 {
			body.WriteString("  " + yesStr + "  " + noStr + "\n")
		} else {
			body.WriteString("    " + strings.TrimSpace(yesStr) + "    " + strings.TrimSpace(noStr) + "\n")
		}

		saveStr := "  " + locale.T("完成并保存", "Save & Finish")
		if m.cursor == 2 {
			saveStr = selectedStyle.Render("> " + locale.T("完成并保存", "Save & Finish"))
		}
		body.WriteString("\n" + saveStr + "\n\n")
		body.WriteString(grayText.Render(locale.T("←→ 切换激活选项 · ↑↓ 移动 · enter 保存", "←→ Toggle · ↑↓ Move · enter save")))

	case 5:
		// ==================== PAGE 5: 配置模式选择 ====================
		body.WriteString(titleStyle.Render(locale.T("配置模式选择", "Config Mode")) + badgeStyle.Render("Choice") + protoLabel + "\n\n")
		body.WriteString(grayText.Render(fmt.Sprintf(locale.T("已从接口获取 %d 个模型，请选择配置方式：", "Fetched %d models from provider API. Choose config mode:"), len(m.modelPool))) + "\n")
		if m.modelPoolFromDiscovery && m.hadLocalModelPool {
			body.WriteString(grayText.Render(locale.T("旧本地模型池将用本次接口结果刷新。", "The local model pool will be refreshed with this API result.")) + "\n")
		}
		body.WriteString("\n")

		autoPrefix := "  "
		autoLabel := grayText.Render(locale.T("🔄 自动配置 (推荐)", "🔄 Auto Config (recommended)"))
		autoDesc := grayText.Render("    " + locale.T("自动填入前 4 个可用模型，Effort = Default，跳过 1M 配置", "Auto-fill first 4 models, Effort=Default, skip 1M"))
		if m.cursor == 0 {
			autoPrefix = selectedStyle.Render("> ")
			autoLabel = selectedStyle.Render(locale.T("🔄 自动配置 (推荐)", "🔄 Auto Config (recommended)"))
		}
		body.WriteString(autoPrefix + autoLabel + "\n")
		body.WriteString(autoDesc + "\n\n")

		manualPrefix := "  "
		manualLabel := grayText.Render(locale.T("🛠 手动配置", "🛠 Manual Config"))
		manualDesc := grayText.Render("    " + locale.T("手动选择每个槽位的模型、1M 上下文开关、推理力度", "Manually set slot models, 1M context, effort level"))
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

	// 指示器
	dots := []string{grayText.Render("⚫"), grayText.Render("⚫"), grayText.Render("⚫"), grayText.Render("⚫"), grayText.Render("⚫"), grayText.Render("⚫")}
	activeColors := []string{"🔵", "🟣", "🟢", "🟡", "🔴", "🟠"}
	dots[m.page] = activeColors[m.page]
	pager := fmt.Sprintf("\n\n%s", lipgloss.NewStyle().Width(70).Align(lipgloss.Center).Render(strings.Join(dots, "   ")))

	langTipMsg := locale.T(
		"💡 提示: 想要更改终端显示语言？使用 `ccl lang` 即可轻松修改",
		"💡 Tip: Want to change TUI display language? Use `ccl lang` to modify it",
	)
	langTipBanner := "\n" + lipgloss.NewStyle().Width(70).Align(lipgloss.Center).Render(langTipStyle.Render(langTipMsg))

	finalStr := windowStyle.Render(body.String()) + pager + langTipBanner
	v := tea.NewView(finalStr)
	v.AltScreen = true
	return v
}
