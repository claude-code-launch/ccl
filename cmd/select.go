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

	app           *tui.App // set by BindApp; markDirty is a no-op until then
	width, height int
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
	if s.tooSmall() {
		return
	}
	c := max(s.cursor.Get()+delta, 0)
	c = min(c, max(len(s.filtered)-1, 0))
	s.cursor.Set(c)
}

// visibleWindow returns the slice of filtered items shown at once, scrolled
// so the cursor row stays inside the window.
func (s *selectComponent) visibleWindow() (start, end int) {
	cursor := max(s.cursor.Get(), 0)
	height := s.listHeight()
	start = max(cursor-height+1, 0)
	end = min(start+height, len(s.filtered))
	start = max(end-height, 0)
	return start, end
}

func (s *selectComponent) typeRune(ke tui.KeyEvent) {
	if s.tooSmall() {
		return
	}
	s.text.Set(s.text.Get() + string(ke.Rune))
	s.applyFilter()
}

func (s *selectComponent) backspace() {
	if s.tooSmall() {
		return
	}
	t := s.text.Get()
	if t == "" {
		return
	}
	s.text.Set(removeLastRune(t))
	s.applyFilter()
}

func (s *selectComponent) KeyMap() tui.KeyMap {
	return tui.KeyMap{
		// The filter input owns the keyboard, so single-letter keys (q, k, j)
		// are filter text rather than navigation. Only ctrl+c and esc abort.
		tui.OnStop(tui.KeyEscape, func(ke tui.KeyEvent) { ke.App().Stop() }),
		tui.OnStop(tui.KeyCtrlC, func(ke tui.KeyEvent) { ke.App().Stop() }),
		tui.OnStop(tui.KeyUp, func(ke tui.KeyEvent) { s.moveCursor(-1); s.markDirty() }),
		tui.OnStop(tui.KeyDown, func(ke tui.KeyEvent) { s.moveCursor(1); s.markDirty() }),
		tui.OnStop(tui.KeyPageUp, func(ke tui.KeyEvent) { s.moveCursor(-s.listHeight()); s.markDirty() }),
		tui.OnStop(tui.KeyPageDown, func(ke tui.KeyEvent) { s.moveCursor(s.listHeight()); s.markDirty() }),
		tui.OnStop(tui.KeyEnter, func(ke tui.KeyEvent) {
			if s.tooSmall() {
				return
			}
			c := s.cursor.Get()
			if len(s.filtered) > 0 && c >= 0 && c < len(s.filtered) {
				s.result = s.filtered[c]
				ke.App().Stop()
			}
		}),
		tui.OnStop(tui.AnyRune, func(ke tui.KeyEvent) { s.typeRune(ke); s.markDirty() }),
		tui.OnStop(tui.KeyBackspace, func(ke tui.KeyEvent) { s.backspace(); s.markDirty() }),
	}
}

// markDirty triggers a re-render after a state change. The State created by
// tui.NewState is app-less until BindApp runs, so Set() alone does not mark
// the frame dirty and key presses would not repaint (the terminal shows a
// stale list with the cursor apparently frozen).
func (s *selectComponent) markDirty() {
	if s.app != nil {
		s.app.MarkDirty()
	}
}

// BindApp wires the component's States to the app (the framework calls this on
// the root component via SetRootComponent). Bound States mark the frame dirty
// on Set(); markDirty below covers the plain filtered slice as well.
func (s *selectComponent) BindApp(app *tui.App) {
	s.app = app
	s.text.BindApp(app)
	s.cursor.BindApp(app)
}

// Render builds the filter prompt and the visible slice of the filtered list.
// It is hand-written element-tree construction (no .gsx template) so the CLI
// stays free of a code-generation build step.
func (s *selectComponent) Render(app *tui.App) *tui.Element {
	if app != nil {
		s.width, s.height = app.Size()
	}
	if s.tooSmall() {
		return smallTerminalView(s.width, s.height)
	}
	root := tui.New(
		tui.WithDisplay(tui.DisplayFlex),
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
	filterRow := tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Row), tui.WithGap(1))
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
			row := tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Row))
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
	if s.width > 0 {
		for _, row := range root.Children() {
			if len(row.Children()) > 0 {
				// Flatten each list/filter row into styled spans before clipping;
				// sibling flex text children otherwise squeeze the cursor away.
				var spans []tui.TextSpan
				for _, child := range row.Children() {
					spans = append(spans, span(child.Text(), child.TextStyle()))
				}
				row.RemoveAllChildren()
				row.SetRichText(spans...)
			}
			fitElementText(row, max(s.width-2, 1))
		}
	}
	return root
}

func (s *selectComponent) tooSmall() bool {
	return s.width > 0 && s.height > 0 && (s.width < 32 || s.height < 8)
}

func (s *selectComponent) listHeight() int {
	if s.height <= 0 {
		return selectViewHeight
	}
	return min(selectViewHeight, max(s.height-7, 1))
}

func (s *selectComponent) HandleMouse(me tui.MouseEvent) bool {
	switch me.Button {
	case tui.MouseWheelUp:
		s.moveCursor(-1)
	case tui.MouseWheelDown:
		s.moveCursor(1)
	default:
		return false
	}
	s.markDirty()
	return true
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
