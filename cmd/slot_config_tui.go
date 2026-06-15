package cmd

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// ─────────────────────────────────────────────────────────────────────────────
// Result type returned to caller
// ─────────────────────────────────────────────────────────────────────────────

type slotConfigResult struct {
	model     string // chosen model (empty = manual entry)
	manual    bool   // true → show manual input after
	enable1M  bool
	cancelled bool
}

// ─────────────────────────────────────────────────────────────────────────────
// Bubbletea model
// ─────────────────────────────────────────────────────────────────────────────

type slotConfigTUI struct {
	slotName   string
	models     []string // full sorted pool
	cursor     int      // cursor in filtered list
	selected   string   // currently highlighted model (value, not display)
	enable1M   bool
	filter     string
	filtered   []string // filtered subset
	focusRight bool     // false = left list, true = right options
	result     slotConfigResult
	done       bool
	width      int
	height     int
}

const manualEntry = "(Enter custom model ID)"

func newSlotConfigTUI(slotName string, models []string, currentModel string, enable1M bool) *slotConfigTUI {
	m := &slotConfigTUI{
		slotName: slotName,
		models:   models,
		enable1M: enable1M,
	}
	m.applyFilter("")
	// Pre-select current model
	if currentModel != "" {
		for i, v := range m.filtered {
			if v == currentModel {
				m.cursor = i
				m.selected = currentModel
				break
			}
		}
	}
	return m
}

func (m *slotConfigTUI) applyFilter(f string) {
	m.filter = f
	m.filtered = nil
	low := strings.ToLower(f)
	for _, model := range m.models {
		if low == "" || strings.Contains(strings.ToLower(model), low) {
			m.filtered = append(m.filtered, model)
		}
	}
	m.filtered = append(m.filtered, manualEntry)
	// Clamp cursor
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Bubbletea interface
// ─────────────────────────────────────────────────────────────────────────────

func (m *slotConfigTUI) Init() tea.Cmd {
	return nil
}

func (m *slotConfigTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyPressMsg:
		switch msg.Code {
		case tea.KeyEscape:
			m.result = slotConfigResult{cancelled: true}
			m.done = true
			return m, tea.Quit

		case tea.KeyTab:
			m.focusRight = !m.focusRight

		case tea.KeyEnter:
			if m.focusRight {
				// Select model → update left panel, return focus to left
				if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
					m.selected = m.filtered[m.cursor]
				}
				m.focusRight = false
				return m, nil
			}
			// Enter on left panel (options) → confirm and exit
			m.commitResult()
			m.done = true
			return m, tea.Quit

		case tea.KeySpace:
			// Space toggles 1M when options panel (LEFT, !focusRight) is focused
			if !m.focusRight {
				m.enable1M = !m.enable1M
			}

		case tea.KeyUp:
			// Arrow keys navigate model list (RIGHT, focusRight)
			if m.focusRight && m.cursor > 0 {
				m.cursor--
			}

		case tea.KeyDown:
			if m.focusRight && m.cursor < len(m.filtered)-1 {
				m.cursor++
			}

		case tea.KeyBackspace:
			if m.focusRight && len(m.filter) > 0 {
				m.applyFilter(m.filter[:len(m.filter)-1])
			}

		default:
			// Typing filters the model list (RIGHT panel)
			if m.focusRight && msg.Text != "" {
				m.applyFilter(m.filter + msg.Text)
			}
		}
	}
	return m, nil
}

func (m *slotConfigTUI) commitResult() {
	chosen := ""
	if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
		chosen = m.filtered[m.cursor]
	}
	if m.selected != "" {
		chosen = m.selected
	}
	m.result = slotConfigResult{
		model:    chosen,
		manual:   chosen == manualEntry || chosen == "",
		enable1M: m.enable1M,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// View
// ─────────────────────────────────────────────────────────────────────────────

func (m *slotConfigTUI) View() tea.View {
	if m.done {
		return tea.NewView("")
	}

	w := m.width
	if w <= 0 {
		w = 80
	}

	panelW := (w - 3) / 2 // 3 = borders between panels

	// ── styles ──
	borderActive := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("99")).
		Width(panelW).
		Padding(0, 1)

	borderInactive := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("8")).
		Width(panelW).
		Padding(0, 1)

	cursorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true)
	selectedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	green := lipgloss.NewStyle().Foreground(lipgloss.Color("10"))

	// ── Left panel: options (1M toggle) — focused by default ──
	checkbox := "[ ]"
	checkLabel := "Enable 1M Context"
	if m.enable1M {
		checkbox = red.Render("[x]")
		checkLabel = green.Render("Enable 1M Context")
	}

	curModel := "(not set)"
	if m.selected != "" && m.selected != manualEntry {
		curModel = m.selected
	} else if len(m.filtered) > 0 && m.cursor < len(m.filtered) && m.filtered[m.cursor] != manualEntry {
		curModel = m.filtered[m.cursor]
	}

	optionsLines := []string{
		dimStyle.Render("Slot: ") + m.slotName,
		dimStyle.Render("Model: ") + curModel,
		"",
		fmt.Sprintf("%s %s", checkbox, checkLabel),
		"",
		dimStyle.Render("Space to toggle"),
		"",
		dimStyle.Render("Tab → select model"),
	}
	if !m.focusRight {
		optionsLines[3] = lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("%s %s", checkbox, checkLabel))
	}

	optionsContent := strings.Join(optionsLines, "\n")
	var optionsPanel string
	if !m.focusRight {
		optionsPanel = borderActive.Render(optionsContent)
	} else {
		optionsPanel = borderInactive.Render(optionsContent)
	}

	// ── Right panel: model list ──
	var modelLines []string

	if m.filter != "" {
		modelLines = append(modelLines, dimStyle.Render("/ "+m.filter+"▌"))
	} else if m.focusRight {
		modelLines = append(modelLines, dimStyle.Render("/ type to filter..."))
	} else {
		modelLines = append(modelLines, dimStyle.Render("Tab to select model"))
	}

	visibleStart := 0
	maxRows := 10
	if m.cursor >= visibleStart+maxRows {
		visibleStart = m.cursor - maxRows + 1
	}
	visibleEnd := visibleStart + maxRows
	if visibleEnd > len(m.filtered) {
		visibleEnd = len(m.filtered)
	}

	for i := visibleStart; i < visibleEnd; i++ {
		label := m.filtered[i]
		prefix := "  "
		if i == m.cursor && m.focusRight {
			prefix = cursorStyle.Render("> ")
		}
		line := label
		if label == m.selected && label != manualEntry {
			line = selectedStyle.Render("✓ " + label)
			if i == m.cursor && m.focusRight {
				line = cursorStyle.Render("> ") + selectedStyle.Render("✓ "+label)
				prefix = ""
			}
		}
		modelLines = append(modelLines, prefix+line)
	}

	if len(m.filtered) > maxRows {
		modelLines = append(modelLines, dimStyle.Render(fmt.Sprintf("  … %d/%d", m.cursor+1, len(m.filtered))))
	}

	modelContent := strings.Join(modelLines, "\n")
	var modelPanel string
	if m.focusRight {
		modelPanel = borderActive.Render(modelContent)
	} else {
		modelPanel = borderInactive.Render(modelContent)
	}

	// Options LEFT, model list RIGHT
	body := lipgloss.JoinHorizontal(lipgloss.Top, optionsPanel, " ", modelPanel)

	var footer string
	if m.focusRight {
		footer = dimStyle.Render("↑↓ Navigate  Enter Select model (returns to left)  Esc Cancel")
	} else {
		footer = dimStyle.Render("Space Toggle 1M  Tab → model list  Enter Confirm  Esc Cancel")
	}

	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99")).
		Render("Configure: " + m.slotName)

	return tea.NewView(lipgloss.JoinVertical(lipgloss.Left,
		"",
		title,
		"",
		body,
		"",
		footer,
	))
}

// ─────────────────────────────────────────────────────────────────────────────
// RunSlotConfigTUI is the entry point called from set.go
// ─────────────────────────────────────────────────────────────────────────────

func RunSlotConfigTUI(slotName string, models []string, currentModel string, enable1M bool) (slotConfigResult, error) {
	tui := newSlotConfigTUI(slotName, models, currentModel, enable1M)
	p := tea.NewProgram(tui)
	final, err := p.Run()
	if err != nil {
		return slotConfigResult{cancelled: true}, err
	}
	m := final.(*slotConfigTUI)
	return m.result, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
