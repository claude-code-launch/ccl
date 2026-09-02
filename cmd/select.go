package cmd

import (
	"fmt"
	"strings"

	tui "github.com/grindlemire/go-tui"

	"github.com/claude-code-launch/ccl/internal/locale"
)

// selectViewHeight is how many list items are visible at once before the
// selection window scrolls. Longer lists show "N more above/below" hints.
const selectViewHeight = 15

// selectComponent is a filterable go-tui component for selecting from a list
// of items. It owns a simple line editor (typed characters narrow the list,
// like the slot picker in the single-page config), ↑↓ move the cursor, and
// the window scrolls so the selected row stays visible.
type selectComponent struct {
	title    string
	items    []string
	filtered []string

	text   *tui.State[string] // filter text being typed
	cursor *tui.State[int]    // index into filtered
	result string             // chosen item, empty if cancelled
}

func newSelectComponent(title string, items []string) *selectComponent {
	s := &selectComponent{
		title:  title,
		items:  items,
		text:   tui.NewState(""),
		cursor: tui.NewState(0),
	}
	s.filtered = s.items
	return s
}

// applyFilter recomputes the filtered list from the filter text and clamps
// the cursor to the new list. Filtering always resets the window to the top.
func (s *selectComponent) applyFilter() {
	q := strings.ToLower(strings.TrimSpace(s.text.Get()))
	if q == "" {
		s.filtered = s.items
	} else {
		s.filtered = make([]string, 0, len(s.items))
		for _, item := range s.items {
			if strings.Contains(strings.ToLower(item), q) {
				s.filtered = append(s.filtered, item)
			}
		}
	}
	s.clampCursor()
}

func (s *selectComponent) clampCursor() {
	c := min(max(s.cursor.Get(), 0), max(len(s.filtered)-1, 0))
	s.cursor.Set(c)
}

func (s *selectComponent) moveCursor(delta int) {
	c := max(s.cursor.Get()+delta, 0)
	c = min(c, max(len(s.filtered)-1, 0))
	s.cursor.Set(c)
}

// visibleWindow returns the slice of filtered items shown at once, scrolled
// so the cursor row stays inside the window.
func (s *selectComponent) visibleWindow() (start, end int) {
	cursor := max(s.cursor.Get(), 0)
	start = max(cursor-selectViewHeight+1, 0)
	end = min(start+selectViewHeight, len(s.filtered))
	start = max(end-selectViewHeight, 0)
	return start, end
}

func (s *selectComponent) typeRune(ke tui.KeyEvent) {
	s.text.Set(s.text.Get() + string(ke.Rune))
	s.applyFilter()
}

func (s *selectComponent) backspace() {
	t := s.text.Get()
	if t == "" {
		return
	}
	s.text.Set(t[:len(t)-1])
	s.applyFilter()
}

func (s *selectComponent) KeyMap() tui.KeyMap {
	return tui.KeyMap{
		// The filter input owns the keyboard, so single-letter keys (q, k, j)
		// are filter text rather than navigation. Only ctrl+c and esc abort.
		tui.OnStop(tui.KeyEscape, func(ke tui.KeyEvent) { ke.App().Stop() }),
		tui.OnStop(tui.KeyCtrlC, func(ke tui.KeyEvent) { ke.App().Stop() }),
		tui.OnStop(tui.KeyUp, func(ke tui.KeyEvent) { s.moveCursor(-1) }),
		tui.OnStop(tui.KeyDown, func(ke tui.KeyEvent) { s.moveCursor(1) }),
		tui.OnStop(tui.KeyEnter, func(ke tui.KeyEvent) {
			c := s.cursor.Get()
			if len(s.filtered) > 0 && c >= 0 && c < len(s.filtered) {
				s.result = s.filtered[c]
				ke.App().Stop()
			}
		}),
		tui.OnStop(tui.AnyRune, s.typeRune),
		tui.OnStop(tui.KeyBackspace, func(ke tui.KeyEvent) { s.backspace() }),
	}
}

// Render builds the filter prompt and the visible slice of the filtered list.
// It is hand-written element-tree construction (no .gsx template) so the CLI
// stays free of a code-generation build step.
func (s *selectComponent) Render(app *tui.App) *tui.Element {
	root := tui.New(
		tui.WithDirection(tui.Column),
		tui.WithPadding(1),
	)

	root.AddChild(tui.New(
		tui.WithText(s.title),
		tui.WithTextStyle(tui.NewStyle().Bold()),
	))

	filterText := s.text.Get()
	if filterText == "" {
		filterText = locale.T("输入以过滤...", "type to filter...")
	}
	filterRow := tui.New(tui.WithDirection(tui.Row), tui.WithGap(1))
	filterRow.AddChild(tui.New(
		tui.WithText(locale.T("🔍 过滤: ", "🔍 Filter: ")),
		tui.WithTextStyle(tui.NewStyle().Foreground(tui.Cyan)),
	))
	filterRow.AddChild(tui.New(tui.WithText(filterText)))
	root.AddChild(filterRow)

	if len(s.filtered) == 0 {
		root.AddChild(tui.New(
			tui.WithText(locale.T("(无匹配)", "(no match)")),
			tui.WithTextStyle(tui.NewStyle().Dim()),
		))
	} else {
		start, end := s.visibleWindow()
		if start > 0 {
			root.AddChild(tui.New(
				tui.WithText(fmt.Sprintf("   ↑ ... %d more above ...", start)),
				tui.WithTextStyle(tui.NewStyle().Dim()),
			))
		}
		cursor := s.cursor.Get()
		for i := start; i < end; i++ {
			prefix := "  "
			style := tui.NewStyle()
			if i == cursor {
				prefix = "▸ "
				style = tui.NewStyle().Bold()
			}
			row := tui.New(tui.WithDirection(tui.Row))
			row.AddChild(tui.New(tui.WithText(prefix), tui.WithTextStyle(style)))
			row.AddChild(tui.New(tui.WithText(s.filtered[i]), tui.WithTextStyle(style)))
			root.AddChild(row)
		}
		if end < len(s.filtered) {
			root.AddChild(tui.New(
				tui.WithText(fmt.Sprintf("   ↓ ... %d more below ...", len(s.filtered)-end)),
				tui.WithTextStyle(tui.NewStyle().Dim()),
			))
		}
	}

	root.AddChild(tui.New(
		tui.WithText(locale.T("输入过滤 · ↑↓ 选择 · enter 确认 · esc 取消", "type to filter · ↑↓ choose · enter confirm · esc cancel")),
		tui.WithTextStyle(tui.NewStyle().Dim()),
	))
	return root
}

// runSelect runs a filterable select prompt and returns the chosen item (or ""
// if aborted).
func runSelect(title string, items []string) (string, error) {
	s := newSelectComponent(title, items)
	app, err := tui.NewApp(tui.WithRootComponent(s))
	if err != nil {
		return "", err
	}
	defer app.Close()
	if err := app.Run(); err != nil {
		return "", err
	}
	return s.result, nil
}
