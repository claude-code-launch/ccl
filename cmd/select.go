package cmd

import (

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/claude-code-launch/ccl/internal/locale"
)

// selectModel is a minimal bubbletea model for selecting from a list of items.
type selectModel struct {
	title  string
	items  []string
	cursor int
	result string // selected item, empty if cancelled
}

// runSelect runs a select prompt and returns the chosen item (or "" if aborted).
func runSelect(title string, items []string) (string, error) {
	m := &selectModel{title: title, items: items}
	p := tea.NewProgram(m)
	result, err := p.Run()
	if err != nil {
		return "", err
	}
	return result.(*selectModel).result, nil
}

func (m *selectModel) Init() tea.Cmd {
	// No blink/init needed for static list
	return nil
}

func (m *selectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case "enter":
			m.result = m.items[m.cursor]
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *selectModel) View() tea.View {
	var buf string
	buf += titleStyle.Render(m.title) + "\n\n"
	for i, item := range m.items {
		prefix := "  "
		line := item
		if i == m.cursor {
			prefix = "▸ "
			line = selectedStyle.Render(item)
		}
		buf += prefix + line + "\n"
	}
	buf += "\n" + grayText.Render(locale.T("↑↓ 选择 · enter 确认 · esc 取消", "↑↓ choose · enter confirm · esc cancel"))
	return tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render(buf))
}
