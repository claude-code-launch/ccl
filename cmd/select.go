package cmd

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/claude-code-launch/ccl/internal/locale"
)

// selectViewHeight is how many list items are visible at once before the
// selection window scrolls. Longer lists show "N more above/below" hints.
const selectViewHeight = 15

// selectModel is a filterable bubbletea model for selecting from a list of
// items. The filter input owns the keyboard so typing narrows the list (like the
// slot picker in the single-page config), ↑↓ move the cursor, and the window
// scrolls so the selected row stays visible.
type selectModel struct {
	title       string
	items       []string
	filtered    []string
	cursor      int
	windowStart int
	input       textinput.Model
	result      string // selected item, empty if cancelled
}

// runSelect runs a filterable select prompt and returns the chosen item (or ""
// if aborted).
func runSelect(title string, items []string) (string, error) {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = locale.T("输入以过滤...", "type to filter...")
	input.SetWidth(40)
	input.Focus()
	m := &selectModel{title: title, items: items, filtered: items, input: input}
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
	return textinput.Blink
}

// updateFilter recomputes the filtered list from the filter input and clamps the
// cursor/window to the new list. Filtering always resets the window to the top.
func (m *selectModel) updateFilter() {
	q := strings.ToLower(strings.TrimSpace(m.input.Value()))
	if q == "" {
		m.filtered = m.items
	} else {
		m.filtered = make([]string, 0, len(m.items))
		for _, item := range m.items {
			if strings.Contains(strings.ToLower(item), q) {
				m.filtered = append(m.filtered, item)
			}
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.windowStart = 0
}

func (m *selectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		// The filter input owns the keyboard, so single-letter keys (q, k, j) are
		// filter text rather than navigation. Only ctrl+c and esc abort.
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "up":
			if m.cursor > 0 {
				m.cursor--
			}
			if m.cursor < m.windowStart {
				m.windowStart = m.cursor
			}
			return m, nil
		case "down":
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
			}
			if m.cursor >= m.windowStart+selectViewHeight {
				m.windowStart = m.cursor - selectViewHeight + 1
			}
			return m, nil
		case "enter":
			if len(m.filtered) > 0 && m.cursor >= 0 && m.cursor < len(m.filtered) {
				m.result = m.filtered[m.cursor]
				return m, tea.Quit
			}
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.updateFilter()
	return m, cmd
}

func (m *selectModel) View() tea.View {
	var buf strings.Builder
	buf.WriteString(titleStyle.Render(m.title) + "\n\n")
	buf.WriteString(filterStyle.Render(locale.T("🔍 过滤: ", "🔍 Filter: ")) + m.input.View() + "\n\n")

	if len(m.filtered) == 0 {
		buf.WriteString(grayText.Render(locale.T("(无匹配)", "(no match)")) + "\n")
	} else {
		start := m.windowStart
		end := start + selectViewHeight
		if end > len(m.filtered) {
			end = len(m.filtered)
		}
		if start > 0 {
			buf.WriteString(grayText.Render(fmt.Sprintf("   ↑ ... %d more above ...", start)) + "\n")
		}
		for i := start; i < end; i++ {
			prefix := "  "
			line := m.filtered[i]
			if i == m.cursor {
				prefix = "▸ "
				line = selectedStyle.Render(line)
			}
			buf.WriteString(prefix + line + "\n")
		}
		if end < len(m.filtered) {
			buf.WriteString(grayText.Render(fmt.Sprintf("   ↓ ... %d more below ...", len(m.filtered)-end)) + "\n")
		}
	}

	buf.WriteString("\n" + grayText.Render(locale.T("输入过滤 · ↑↓ 选择 · enter 确认 · esc 取消", "type to filter · ↑↓ choose · enter confirm · esc cancel")))
	return tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render(buf.String()))
}
