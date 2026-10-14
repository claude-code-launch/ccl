package cmd

import (
	"strings"

	"github.com/claude-code-launch/ccl/internal/locale"
	tui "github.com/grindlemire/go-tui"
)

func (m *AdvancedConfigModel) bodyWidth() int { return max(m.panelWidth()-6, 1) }

func (m *AdvancedConfigModel) splitActions() bool {
	return m.bodyWidth() < 38
}

func (m *AdvancedConfigModel) stepperRow(label, value string, selected bool) *tui.Element {
	if m.bodyWidth() >= 48 {
		return stepperRow(label, value, selected)
	}
	if label == "Context & Compact" {
		label = "Context"
	}
	if label == "上下文与压缩" {
		label = "上下文"
	}
	pad := max(tui.StringWidth(label)+1, 9)
	style := stPurple
	prefix := "  "
	if selected {
		style = stSelected
		prefix = "> "
	}
	return line(span(prefix, style), span(label+strings.Repeat(" ", pad-tui.StringWidth(label))+" ", style),
		span(truncateMiddle(value, max(m.bodyWidth()-pad-3, 1)), style))
}

func (m *AdvancedConfigModel) kvRow(label string, value tui.TextSpan) *tui.Element {
	if m.bodyWidth() >= 48 {
		return kvRow(label, value)
	}
	pad := max(tui.StringWidth(label)+1, 9)
	value.Text = truncateMiddle(value.Text, max(m.bodyWidth()-pad-3, 1))
	return line(span("  "+label+strings.Repeat(" ", pad-tui.StringWidth(label))+" ", tui.NewStyle()), value)
}

func (m *AdvancedConfigModel) pickerHeight(modelsDev bool) int {
	if m.height <= 0 {
		return filterViewHeight
	}
	chrome := 11
	if modelsDev {
		chrome = 12
	}
	// Compact overlays have no inner padding, gaps or separate scroll hints.
	if m.height < 18 {
		chrome = 6
		if modelsDev {
			chrome = 5
		}
	}
	return min(filterViewHeight, max(m.height-chrome, 1))
}

func pickerWindow(cursor, offset, count, height int) int {
	if count == 0 {
		return 0
	}
	if cursor < offset {
		offset = cursor
	}
	if cursor >= offset+height {
		offset = cursor - height + 1
	}
	return min(max(offset, 0), max(count-height, 0))
}

func (m *AdvancedConfigModel) keepPickersVisible() {
	m.filterWindowStart = pickerWindow(m.slotListCursor, m.filterWindowStart, len(m.filteredPool), m.pickerHeight(false))
	m.modelsDevWindow = pickerWindow(m.modelsDevCursor, m.modelsDevWindow, len(m.modelsDevFiltered), m.pickerHeight(true))
}

// fitElementText preserves span styles while bounding a row to terminal cells.
// A long value never expands the panel or pushes its right border off screen.
func fitElementText(e *tui.Element, width int) {
	spans := e.RichText()
	if len(spans) == 0 {
		if tui.StringWidth(e.Text()) > width {
			e.SetText(truncateMiddle(e.Text(), width))
		}
		return
	}
	remaining := width
	fit := make([]tui.TextSpan, 0, len(spans))
	for _, s := range spans {
		if remaining <= 0 {
			break
		}
		if tui.StringWidth(s.Text) > remaining {
			leading := len(s.Text) - len(strings.TrimLeft(s.Text, " \t"))
			padding := min(leading, remaining)
			s.Text = s.Text[:padding] + truncateMiddle(s.Text[leading:], remaining-padding)
		}
		remaining -= tui.StringWidth(s.Text)
		fit = append(fit, s)
	}
	e.SetRichText(fit...)
}

// editViewport keeps the end and insertion cursor visible for the append-only
// editors; the full value remains in state and is saved unchanged.
func editViewport(text string, width int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	for i, line := range lines {
		cursor := ""
		if i == len(lines)-1 {
			cursor = "▏"
		}
		budget := max(width-tui.StringWidth(cursor), 0)
		if tui.StringWidth(line) > budget {
			var tail string
			runes := []rune(line)
			for j := len(runes) - 1; j >= 0; j-- {
				candidate := string(runes[j]) + tail
				if tui.StringWidth(candidate) > max(budget-1, 0) {
					break
				}
				tail = candidate
			}
			line = "…" + tail
		}
		lines[i] = line + cursor
	}
	return strings.Join(lines, "\n")
}

func (m *AdvancedConfigModel) wrapPicker(body *tui.Element) *tui.Element {
	rows := body.Children()
	compact := m.height > 0 && m.height < 18
	if compact {
		// Remove blank separators and the optional above/below hints. The
		// selection position remains visible in the footer.
		body = tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Column), tui.WithWrap(false))
		for _, row := range rows {
			text := row.Text()
			for _, s := range row.RichText() {
				text += s.Text
			}
			if strings.TrimSpace(text) == "" || strings.Contains(text, " more above ") || strings.Contains(text, " more below ") {
				continue
			}
			fitElementText(row, m.bodyWidth())
			body.AddChild(row)
		}
	} else {
		for _, row := range rows {
			fitElementText(row, max(m.bodyWidth()-2, 1))
		}
	}
	return panelWrap(body, m.panelWidth())
}

func (m *AdvancedConfigModel) smallTerminalView() *tui.Element {
	return smallTerminalView(m.width, m.height)
}

func smallTerminalView(width, height int) *tui.Element {
	root := tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Column), tui.WithWrap(false))
	for i, text := range []string{locale.T("窗口过小", "Terminal too small"), locale.T("请放大至 32×8", "Resize to 32x8"), "Esc / Ctrl+C"} {
		if i >= height {
			break
		}
		root.AddChild(plainLine(truncateMiddle(text, width)))
	}
	return root
}
