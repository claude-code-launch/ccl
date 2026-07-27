package cmd

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
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

type textPromptModel struct {
	title  string
	input  textinput.Model
	result string
}

func runTextPrompt(title, placeholder string) (string, error) {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = placeholder
	input.SetWidth(40)
	input.Focus()
	m := &textPromptModel{title: title, input: input}
	p := tea.NewProgram(m)
	result, err := p.Run()
	if err != nil {
		return "", err
	}
	return result.(*textPromptModel).result, nil
}

func (m *textPromptModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m *textPromptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "enter":
			value := strings.TrimSpace(m.input.Value())
			if value != "" {
				m.result = value
				return m, tea.Quit
			}
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *textPromptModel) View() tea.View {
	body := titleStyle.Render(m.title) + "\n\n"
	body += selectedStyle.Render("> ") + m.input.View()
	body += "\n\n" + grayText.Render(locale.T("输入名称 · enter 确认 · esc 取消", "enter a name · enter confirm · esc cancel"))
	return tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render(body))
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
