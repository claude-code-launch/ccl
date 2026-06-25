package cmd

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
)

var (
	windowStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("237")).
			Padding(1, 2).
			Width(70)

	titleStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Bold(true)
	badgeStyle      = lipgloss.NewStyle().Background(lipgloss.Color("99")).Foreground(lipgloss.Color("255")).Padding(0, 1).MarginLeft(2)
	protoBadgeStyle = lipgloss.NewStyle().Background(lipgloss.Color("141")).Foreground(lipgloss.Color("255")).Padding(0, 1).MarginLeft(2)
	cyanText        = lipgloss.NewStyle().Foreground(lipgloss.Color("49"))
	purpleText      = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))
	grayText        = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	selectedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
	lightning       = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true).Render("⚡1M")
	filterStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	langTipStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("229")).Background(lipgloss.Color("239")).Padding(0, 1).MarginTop(1)
)

type AdvancedConfigModel struct {
	p         *provider.Provider
	modelPool []string
	oneMSlots map[string]bool

	page   int
	cursor int

	// detectionError is set when protocol detection AND model fetching both fail on Page 0.
	detectionError error

	// Page 0
	urlInput textinput.Model
	keyInput textinput.Model

	// Page 1
	activeSlot     int
	filterInput    textinput.Model
	filteredPool   []string
	slotListCursor int

	// Page 3
	effortLevels []string
	effortCursor int

	// Page 4
	IsActiveChosen bool
}

func NewAdvancedConfigModel(p *provider.Provider) *AdvancedConfigModel {
	ui := textinput.New()
	ui.Placeholder = "https://api.openai.com/v1"
	ui.Focus()
	ui.SetValue(p.Endpoint)

	ki := textinput.New()
	ki.Placeholder = "sk-..."
	ki.SetValue(p.APIKey)

	fi := textinput.New()
		fi.Placeholder = ""

	m := &AdvancedConfigModel{
		p:              p,
		oneMSlots:      make(map[string]bool),
		page:           0,
		cursor:         0,
		urlInput:       ui,
		keyInput:       ki,
		filterInput:    fi,
		effortLevels:   []string{"low", "medium", "high", "xhigh", "max", "ultracode"},
		effortCursor:   1,
		IsActiveChosen: true,
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
		if strings.HasSuffix(*modelStr, "[1m]") {
			m.oneMSlots[slotKey] = true
			*modelStr = strings.TrimSuffix(*modelStr, "[1m]")
		}
	}
	cleanAndPopulate(&m.p.OpusModel, "opus")
	cleanAndPopulate(&m.p.SonnetModel, "sonnet")
	cleanAndPopulate(&m.p.HaikuModel, "haiku")
	cleanAndPopulate(&m.p.LockModel, "custom")

	return m
}

func (m *AdvancedConfigModel) Init() tea.Cmd { return textinput.Blink }

func (m *AdvancedConfigModel) updateFilteredPool() {
	q := strings.ToLower(m.filterInput.Value())
	if q == "" {
		m.filteredPool = append([]string{locale.T("(设置为未设置/清空)", "(clear/unset)")}, m.modelPool...)
		return
	}
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

// 实时获取/检测协议名称
func (m *AdvancedConfigModel) getProtocol() string {
	if m.p.Type != "" {
		return m.p.Type
	}
	if strings.Contains(strings.ToLower(m.urlInput.Value()), "anthropic") {
		return "anthropic"
	}
	return "openai"
}

func (m *AdvancedConfigModel) goBack() {
	if m.page > 0 {
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
			m.cursor = 6
		}
	}
}

func (m *AdvancedConfigModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit

		case "esc":
			if m.page == 1 && m.filterInput.Focused() {
				m.filterInput.Blur()
			} else {
				if m.page == 0 {
					return m, tea.Quit
				}
				m.goBack()
			}
			return m, nil

		case "up", "k":
			if m.page == 1 && m.filterInput.Focused() {
				if m.slotListCursor > 0 {
					m.slotListCursor--
				}
			} else {
				if m.page == 0 && (m.cursor == 2 || m.cursor == 3) {
					m.cursor = 1
				} else if m.page == 1 && (m.cursor == 4 || m.cursor == 5) {
					m.cursor = 3
				} else if m.page == 2 && (m.cursor == 4 || m.cursor == 5) {
					m.cursor = 3
				} else if m.page == 3 && (m.cursor == 6 || m.cursor == 7) {
					m.cursor = 5
				} else if m.cursor > 0 {
					m.cursor--
				}
			}

		case "down", "j":
			if m.page == 1 && m.filterInput.Focused() {
				if m.slotListCursor < len(m.filteredPool)-1 {
					m.slotListCursor++
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
				if m.cursor < 7 {
					m.cursor++
				}
			} else if m.page == 4 {
				if m.cursor < 2 {
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
			if m.page == 3 && m.cursor == 7 {
				m.cursor = 6
			}
			if m.page == 4 && m.cursor < 2 {
				m.IsActiveChosen = true
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
			if m.page == 3 && m.cursor == 6 {
				m.cursor = 7
			}
			if m.page == 4 && m.cursor < 2 {
				m.IsActiveChosen = false
			}

		case "tab":
			// Tab → 下一项（同 ↓）
			if m.page == 1 && m.filterInput.Focused() {
				if m.slotListCursor < len(m.filteredPool)-1 {
					m.slotListCursor++
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
			} else if m.page == 3 && m.cursor > 7 {
				m.cursor = 0
			} else if m.page == 4 && m.cursor > 2 {
				m.cursor = 0
			}

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
					m.cursor = 7
				} else if m.page == 4 {
					m.cursor = 2
				}
			}
			if m.page == 2 && m.cursor < 4 {
				slot := []string{"opus", "sonnet", "haiku", "custom"}[m.cursor]
				m.oneMSlots[slot] = !m.oneMSlots[slot]
			}

		case "enter":
			// 如果点击了底部的“上一步”按钮，直接返回
			if (m.page == 0 && m.cursor == 3) || (m.page == 1 && m.cursor == 5) || (m.page == 2 && m.cursor == 5) || (m.page == 3 && m.cursor == 7) {
				m.goBack()
				return m, nil
			}

			switch m.page {
			case 0:
				if m.cursor == 0 {
					m.cursor = 1
					m.urlInput.Blur()
					m.keyInput.Focus()
				} else if m.cursor == 1 {
					m.cursor = 2
				} else if m.cursor == 2 {
					m.p.Endpoint = m.urlInput.Value()
					m.p.APIKey = m.keyInput.Value()

					// 自动探测协议与模型
					detectedType, discoveredModelsRaw, derr := detectProtocolAndModels(m.p.Endpoint, m.p.APIKey)
					m.p.Type = detectedType

					m.modelPool = []string{}
					if derr == nil && discoveredModelsRaw != "" {
						for _, mod := range strings.Split(discoveredModelsRaw, ",") {
							mod = strings.TrimSpace(mod)
							if mod != "" && !stringInSlice(mod, m.modelPool) {
								m.modelPool = append(m.modelPool, mod)
							}
						}
					}
					if m.p.Model != "" {
						for _, mod := range strings.Split(m.p.Model, ",") {
							mod = strings.TrimSpace(mod)
							if mod != "" && !stringInSlice(mod, m.modelPool) {
								m.modelPool = append(m.modelPool, mod)
							}
						}
					}

					// 协议探测 + 模型获取都失败，且没有已有模型 → 退出
					if len(m.modelPool) == 0 {
						if derr != nil {
							m.detectionError = derr
						} else {
							m.detectionError = fmt.Errorf(locale.T("未获取到任何可用模型", "no models available"))
						}
						return m, tea.Quit
					}

					m.p.Model = strings.Join(m.modelPool, ",")
					sort.Strings(m.modelPool)
					m.page = 1
					m.cursor = 0
				}
			case 1:
				if !m.filterInput.Focused() {
					if m.cursor == 4 {
						m.page = 2
						m.cursor = 0
					} else {
						m.activeSlot = m.cursor
						m.filterInput.Focus()
						m.filterInput.SetValue("")
						m.slotListCursor = 0
						m.updateFilteredPool()
					}
				} else {
					selectedModel := m.filteredPool[m.slotListCursor]
					if selectedModel == locale.T("(设置为未设置/清空)", "(clear/unset)") || selectedModel == locale.T("(无匹配模型)", "(no match)") {
						selectedModel = ""
					}
					ptr := []*string{&m.p.OpusModel, &m.p.SonnetModel, &m.p.HaikuModel, &m.p.LockModel}[m.activeSlot]
					*ptr = selectedModel
					m.filterInput.Blur()
				}
			case 2:
				if m.cursor == 4 {
					m.page = 3
					m.cursor = 0
				}
			case 3:
				if m.cursor < 6 {
					m.effortCursor = m.cursor
				} else {
					m.p.EffortLevel = m.effortLevels[m.effortCursor]
					m.page = 4
					m.cursor = 0
				}
			case 4:
				if m.cursor < 2 {
					m.cursor = 2
				} else {
					return m, tea.Quit
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

func (m *AdvancedConfigModel) View() tea.View {
	var body strings.Builder
	protoLabel := protoBadgeStyle.Render("Protocol: " + m.getProtocol())

	switch m.page {
	case 0:
		// ==================== PAGE 0: 凭据配置 ====================
		body.WriteString(titleStyle.Render(locale.T("基础凭据配置", "Base Credentials")) + badgeStyle.Render("Credentials") + protoLabel + "\n\n")
		prefURL := "  "
		if m.cursor == 0 {
			prefURL = selectedStyle.Render("> ")
		}
		body.WriteString(fmt.Sprintf("%s%-14s: %s\n", prefURL, purpleText.Render("Endpoint URL"), m.urlInput.View()))

		prefKey := "  "
		if m.cursor == 1 {
			prefKey = selectedStyle.Render("> ")
		}
		body.WriteString(fmt.Sprintf("%s%-14s: %s\n", prefKey, purpleText.Render("API Key"), m.keyInput.View()))

		body.WriteString(renderBottomButtons(m.page, m.cursor, 2, 3))
		body.WriteString(grayText.Render(locale.T("↑↓ 切换焦点 · ←→ 切换按钮 · enter 确认", "↑↓ Switch · ←→ Toggle Buttons · enter confirm")))

	case 1:
		// ==================== PAGE 1: 槽位映射配置 ====================
		if !m.filterInput.Focused() {
			body.WriteString(titleStyle.Render(locale.T("Claude Slot 映射配置", "Claude Slot Mapping")) + badgeStyle.Render("Slot List") + protoLabel + "\n\n")
			renderRow := func(idx int, label, val string) {
				prefix := "  "
				labelStr := purpleText.Render(label)
				if m.cursor == idx {
					prefix = selectedStyle.Render("> ")
					labelStr = selectedStyle.Render(label) + grayText.Render(" ("+locale.T("enter 筛选", "enter to list")+")")
				}
				modelStr := cyanText.Render(val)
				if val == "" {
					modelStr = grayText.Render(locale.T("(未设置)", "(unset)"))
				}
				body.WriteString(fmt.Sprintf("%s%-6s – %s\n", prefix, labelStr, modelStr))
			}
			renderRow(0, "Opus", m.p.OpusModel)
			renderRow(1, "Sonnet", m.p.SonnetModel)
			renderRow(2, "Haiku", m.p.HaikuModel)
			renderRow(3, "Custom", m.p.LockModel)

			body.WriteString(renderBottomButtons(m.page, m.cursor, 4, 5))
			body.WriteString(grayText.Render(locale.T("↑↓ 移动光标 · ←→ 切换按钮 · enter 选择编辑/跳转", "↑↓ Move · ←→ Toggle Buttons · enter select")))
		} else {
			slotName := []string{"Opus", "Sonnet", "Haiku", "Custom"}[m.activeSlot]
			body.WriteString(titleStyle.Render(fmt.Sprintf(locale.T("配置槽位 [%s] 模型筛选", "Select Model for Slot [%s]"), slotName)))
			body.WriteString("\n" + filterStyle.Render(locale.T("🔍 过滤模型: ", "🔍 Filter model: ")) + m.filterInput.View() + "\n")
			for i, mod := range m.filteredPool {
				prefix := "   "
				line := grayText.Render(mod)
				if i == m.slotListCursor {
					prefix = selectedStyle.Render(" > ")
					line = selectedStyle.Render(mod)
				}
				body.WriteString(prefix + line + "\n")
			}
			body.WriteString(selectedStyle.Render(fmt.Sprintf("  %d/%d", m.slotListCursor+1, len(m.filteredPool))) + "\n\n" + grayText.Render(locale.T("键盘输入任意字符快速过滤 · ↑↓ 选择 · enter 锁定 · esc 取消", "Type to filter · ↑↓ Scroll · enter lock · esc cancel")) + "\n")
		}

	case 2:
		// ==================== PAGE 2: 1M 上下文配置页 (精简) ====================
		body.WriteString(titleStyle.Render(locale.T("1M 上下文配置", "1M Context")) + badgeStyle.Render("MultiSelect") + protoLabel + "\n")
		body.WriteString(grayText.Render(locale.T("space 切换开关状态", "space Toggle Slot Status")) + "\n\n")

		renderMultiRow := func(idx int, label, modelVal string) {
			prefix := "  "
			if m.cursor == idx {
				prefix = selectedStyle.Render("> ")
			}
			box := "[ ]"
			slotKey := []string{"opus", "sonnet", "haiku", "custom"}[idx]
			if m.oneMSlots[slotKey] {
				box = "[x]"
			}

			itemText := grayText.Render(label)
			if m.cursor == idx {
				itemText = titleStyle.Render(label)
			}

			modelStr := cyanText.Render(modelVal)
			if modelVal == "" {
				modelStr = grayText.Render(locale.T("(未设置)", "(unset)"))
			}

			suffix := ""
			if m.oneMSlots[slotKey] {
				suffix = " " + lightning
			}

			body.WriteString(fmt.Sprintf("%s%s  %-14s – %s%s\n", prefix, box, itemText, modelStr, suffix))
		}

		renderMultiRow(0, "Opus Config", m.p.OpusModel)
		renderMultiRow(1, "Sonnet Config", m.p.SonnetModel)
		renderMultiRow(2, "Haiku Config", m.p.HaikuModel)
		renderMultiRow(3, "Custom Config", m.p.LockModel)

		body.WriteString(renderBottomButtons(m.page, m.cursor, 4, 5))
		body.WriteString(grayText.Render(locale.T("space 切换 · ↑↓ 移动 · ←→ 切换按钮 · enter 下一步", "space Toggle · ↑↓ Move · ←→ Toggle Buttons · enter next")))

	case 3:
		// ==================== PAGE 3: Reasoning Effort 思考流配置 ====================
		body.WriteString(titleStyle.Render(locale.T("Reasoning Effort 思考流配置", "Reasoning Effort")) + badgeStyle.Render("Effort") + protoLabel + "\n\n")
		for i, level := range m.effortLevels {
			prefix := "  "
			if m.cursor == i {
				prefix = selectedStyle.Render("> ")
			}
			radio := "( )"
			if m.effortCursor == i {
				radio = purpleText.Render("(●)")
			}
			itemText := grayText.Render(level)
			if m.cursor == i {
				itemText = titleStyle.Render(level)
			}
			body.WriteString(fmt.Sprintf("%s%s %s\n", prefix, radio, itemText))
		}

		body.WriteString(renderBottomButtons(m.page, m.cursor, 6, 7))
		body.WriteString(grayText.Render(locale.T("↑↓ 移动选择级别 · ←→ 切换按钮 · enter 前进/后退", "↑↓ Move · ←→ Toggle Buttons · enter next/back")))

	case 4:
		// ==================== PAGE 4: 核对并应用此配置 ====================
		body.WriteString(titleStyle.Render(locale.T("核对并应用此 Provider 配置", "Review & Apply")) + badgeStyle.Render("Confirm") + "\n\n")
		body.WriteString(fmt.Sprintf("  • %-12s: %s\n", "Endpoint", cyanText.Render(m.p.Endpoint)))
		body.WriteString(fmt.Sprintf("  • %-12s: %s\n", "Protocol", purpleText.Render(m.getProtocol())))
		body.WriteString(fmt.Sprintf("  • %-12s: %s\n", "Effort Level", purpleText.Render(m.effortLevels[m.effortCursor])))

		body.WriteString("\n  " + locale.T("是否将该 Provider 设为当前激活配置？", "Set as active provider right now?") + "\n\n")

		yesStr, noStr := " "+locale.T("是", "Yes")+" ", " "+locale.T("否", "No")+" "
		if m.IsActiveChosen {
			yesStr = selectedStyle.Render(" > "+locale.T("是", "Yes")+" <")
			noStr = grayText.Render("  "+locale.T("否", "No")+" ")
		} else {
			yesStr = grayText.Render("  "+locale.T("是", "Yes")+" ")
			noStr = selectedStyle.Render(" > "+locale.T("否", "No")+" <")
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
	}

	// 指示器
	dots := []string{grayText.Render("⚫"), grayText.Render("⚫"), grayText.Render("⚫"), grayText.Render("⚫"), grayText.Render("⚫")}
	activeColors := []string{"🔵", "🟣", "🟢", "🟡", "🔴"}
	dots[m.page] = activeColors[m.page]
	pager := fmt.Sprintf("\n\n%s", lipgloss.NewStyle().Width(70).Align(lipgloss.Center).Render(strings.Join(dots, "   ")))

	langTipMsg := locale.T(
		"💡 提示: 想要更改终端显示语言？使用 `ccl lang` 即可轻松修改",
		"💡 Tip: Want to change TUI display language? Use `ccl lang` to modify it",
	)
	langTipBanner := "\n" + lipgloss.NewStyle().Width(70).Align(lipgloss.Center).Render(langTipStyle.Render(langTipMsg))

	finalStr := windowStyle.Render(body.String()) + pager + langTipBanner
	return tea.NewView(finalStr)
}
