package cmd

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// ─────────────────────────────────────────────────────────────────────────────
// Slot entry
// ─────────────────────────────────────────────────────────────────────────────

type slotEntry struct {
	name  string
	model *string // points to provider field (with optional [1m] suffix)
}

// baseModel returns the model name without [1m] suffix.
func (s slotEntry) baseModel() string {
	if s.model == nil {
		return ""
	}
	return strings.TrimSuffix(*s.model, "[1m]")
}

// is1M returns true if [1m] is enabled for this slot.
func (s slotEntry) is1M() bool {
	return s.model != nil && strings.HasSuffix(*s.model, "[1m]")
}

// ─────────────────────────────────────────────────────────────────────────────
// Outer slot-list TUI
// ─────────────────────────────────────────────────────────────────────────────

type slotListTUI struct {
	slots      []slotEntry
	poolModels []string // sorted model pool
	cursor     int
	done       bool
	width      int
	height     int

	// env callback: called whenever 1M is toggled to update CLAUDE_CODE_AUTO_COMPACT_WINDOW
	setEnv func(key, val string)
}

func newSlotListTUI(slots []slotEntry, poolModels []string, setEnv func(key, val string)) *slotListTUI {
	return &slotListTUI{
		slots:      slots,
		poolModels: poolModels,
		setEnv:     setEnv,
	}
}

func (m *slotListTUI) Init() tea.Cmd {
	return nil
}

func (m *slotListTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyPressMsg:
		switch msg.Code {
		case tea.KeyEscape, tea.KeyEnter:
			if msg.Code == tea.KeyEnter {
				// Enter → configure model for current slot
				if m.cursor < len(m.slots) {
					slot := m.slots[m.cursor]
					currentModel := slot.baseModel()
					enable1M := slot.is1M()

					res, err := RunSlotConfigTUI(slot.name, m.poolModels, currentModel, enable1M)
					if err == nil && !res.cancelled {
						m.applySlotResult(m.cursor, res)
					}
					// Fall through and re-render (don't quit)
					return m, nil
				}
			} else {
				// Esc → done
				m.done = true
				return m, tea.Quit
			}

		case tea.KeySpace:
			// Space → toggle 1M for current slot
			if m.cursor < len(m.slots) {
				m.toggle1M(m.cursor)
			}

		case tea.KeyUp:
			if m.cursor > 0 {
				m.cursor--
			}

		case tea.KeyDown:
			if m.cursor < len(m.slots)-1 {
				m.cursor++
			}
		}

		// 'q' to quit
		if msg.Text == "q" {
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *slotListTUI) toggle1M(idx int) {
	s := m.slots[idx]
	if s.model == nil {
		return
	}
	base := s.baseModel()
	if s.is1M() {
		*s.model = base
	} else if base != "" {
		*s.model = base + "[1m]"
		m.setEnv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "1000000")
	}
}

func (m *slotListTUI) applySlotResult(idx int, res slotConfigResult) {
	s := m.slots[idx]
	if s.model == nil {
		return
	}
	if res.manual {
		// manual entry: leave unchanged, caller handles it after
		return
	}
	base := res.model
	if res.enable1M && base != "" {
		*s.model = base + "[1m]"
		m.setEnv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "1000000")
	} else {
		*s.model = base
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// View
// ─────────────────────────────────────────────────────────────────────────────

func (m *slotListTUI) View() tea.View {
	if m.done {
		return tea.NewView("")
	}

	w := m.width
	if w <= 0 {
		w = 100
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	cursorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	green := lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	normalStyle := lipgloss.NewStyle()

	// Column widths
	nameW := 26
	modelW := 30

	// Header
	header := fmt.Sprintf("  %-*s  %-*s  %s",
		nameW, "Slot", modelW, "Model", "1M")
	headerLine := dimStyle.Render(header)
	separator := dimStyle.Render("  " + strings.Repeat("─", nameW+modelW+8))

	var rows []string
	rows = append(rows, "")
	rows = append(rows, titleStyle.Render("Claude Slot Mapping"))
	rows = append(rows, "")
	rows = append(rows, headerLine)
	rows = append(rows, separator)

	for i, slot := range m.slots {
		prefix := "  "
		nameS := normalStyle
		if i == m.cursor {
			prefix = cursorStyle.Render("> ")
			nameS = lipgloss.NewStyle().Foreground(lipgloss.Color("99"))
		}

		model := slot.baseModel()
		if model == "" {
			model = dimStyle.Render("(not set)")
		} else if i == m.cursor {
			model = green.Render(model)
		}

		indicator := dimStyle.Render("[ ]")
		if slot.is1M() {
			indicator = red.Render("[x]")
		}

		// Truncate name/model for alignment
		name := slot.name
		if len(name) > nameW {
			name = name[:nameW-1] + "…"
		}
		modelStr := strings.TrimSuffix(fmt.Sprintf("%-*s", modelW, model), "")

		row := fmt.Sprintf("%s%s  %s  %s",
			prefix,
			nameS.Render(fmt.Sprintf("%-*s", nameW, name)),
			modelStr,
			indicator,
		)
		rows = append(rows, row)
	}

	rows = append(rows, "")
	footer := dimStyle.Render("↑↓ Navigate   Enter Configure model   Space Toggle 1M   q/Esc Done")
	rows = append(rows, footer)
	rows = append(rows, "")

	_ = w // reserved for future line-wrapping
	return tea.NewView(strings.Join(rows, "\n"))
}

// ─────────────────────────────────────────────────────────────────────────────
// RunSlotListTUI is the entry point called from set.go
// ─────────────────────────────────────────────────────────────────────────────

func RunSlotListTUI(slots []slotEntry, poolModels []string, setEnv func(key, val string)) error {
	tui := newSlotListTUI(slots, poolModels, setEnv)
	p := tea.NewProgram(tui)
	_, err := p.Run()
	return err
}
