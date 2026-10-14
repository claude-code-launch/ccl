package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	tui "github.com/grindlemire/go-tui"
)

func assertFrameFits(t *testing.T, frame string, width, height int) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(frame, "\n"), "\n")
	if len(lines) > height {
		t.Fatalf("%dx%d frame has %d lines\n%s", width, height, len(lines), stripANSI(frame))
	}
	for _, line := range lines {
		if tui.StringWidth(stripANSI(line)) > width {
			t.Fatalf("line exceeds width %d: %s", width, stripANSI(line))
		}
	}
}

func TestConfigAllRowsRemainVisibleAtTerminalSizes(t *testing.T) {
	saved := locale.Current()
	t.Cleanup(func() { locale.SetLanguage(saved) })
	for _, lang := range []string{"en", "zh-CN"} {
		locale.SetLanguage(lang)
		for _, size := range [][2]int{{32, 8}, {40, 12}, {50, 16}, {60, 20}, {80, 24}, {110, 35}, {160, 50}} {
			t.Run(fmt.Sprintf("%s-%dx%d", lang, size[0], size[1]), func(t *testing.T) {
				p := providerFrom("long-provider-name", "https://example.test/"+strings.Repeat("long/", 40), "openai")
				p.APIKey = "sk-" + strings.Repeat("secret", 50)
				m := NewAdvancedConfigModel(&p)
				enterDetectedReview(m, "model-a", strings.Repeat("long-model", 15))
				m.width, m.height = size[0], size[1]
				for i, row := range m.visibleRows() {
					m.cursor = i
					frame := tui.Sprint(m.Render(nil), tui.WithPrintWidth(m.width))
					assertFrameFits(t, frame, m.width, m.height)
					if !strings.Contains(stripANSI(frame), "> ") {
						t.Fatalf("row %v is not visibly selected\n%s", row.kind, stripANSI(frame))
					}
				}
			})
		}
	}
}

func TestPickersFollowSelectionAfterResize(t *testing.T) {
	p := providerFrom("p", "https://unused.invalid/v1", "openai")
	m := NewAdvancedConfigModel(&p)
	for i := 0; i < 100; i++ {
		m.filteredPool = append(m.filteredPool, fmt.Sprintf("model-%03d", i))
		m.modelsDevFiltered = append(m.modelsDevFiltered, modelsdev.Provider{ID: fmt.Sprintf("p-%03d", i), Name: fmt.Sprintf("Provider %03d", i)})
	}
	m.slotListCursor, m.modelsDevCursor = 73, 73
	for _, size := range [][2]int{{110, 35}, {40, 12}, {32, 8}, {80, 24}, {50, 16}} {
		m.width, m.height = size[0], size[1]
		m.filterFocused, m.modelsDevPicker = true, false
		frame := tui.Sprint(m.Render(nil), tui.WithPrintWidth(m.width))
		assertFrameFits(t, frame, m.width, m.height)
		if !strings.Contains(stripANSI(frame), "model-073") {
			t.Fatalf("selected model disappeared after resize\n%s", stripANSI(frame))
		}
		m.filterFocused, m.modelsDevPicker = false, true
		frame = tui.Sprint(m.Render(nil), tui.WithPrintWidth(m.width))
		assertFrameFits(t, frame, m.width, m.height)
		if !strings.Contains(stripANSI(frame), "Provider 073") {
			t.Fatalf("selected provider disappeared after resize\n%s", stripANSI(frame))
		}
	}
}

func TestCJKCancelMousePositionUsesCells(t *testing.T) {
	text := "│  > 保存并激活     > 取消 │"
	x := tui.StringWidth("│  > 保存并激活     > ")
	row, ok := matchRowLabel(text, x, true)
	if !ok || row != rowCancel {
		t.Fatalf("CJK cancel click selected %v, ok=%v", row, ok)
	}
}

func TestProviderSelectionFitsShortTerminal(t *testing.T) {
	var items []string
	for i := 0; i < 100; i++ {
		items = append(items, fmt.Sprintf("provider-%03d-%s", i, strings.Repeat("long", 30)))
	}
	s := newSelectComponent("Select a provider", items)
	s.width, s.height = 40, 12
	s.cursor.Set(73)
	frame := tui.Sprint(s.Render(nil), tui.WithPrintWidth(s.width))
	assertFrameFits(t, frame, s.width, s.height)
	if !strings.Contains(stripANSI(frame), "provider-073") {
		t.Fatal("selected provider is not visible")
	}
}

func TestOAuthAndMappingPagesFitNarrowTerminal(t *testing.T) {
	for _, mode := range []string{"oauth", "mapping"} {
		t.Run(mode, func(t *testing.T) {
			p := providerFrom("subscription", "oauth://gpt", "openai_responses")
			p.OAuthProvider = "gpt"
			m := readyOAuthPage(NewAdvancedConfigModel(&p))
			if mode == "mapping" {
				m = NewAdvancedMappingModel(&p, []string{"model-a", "model-b"}, nil)
			}
			enterDetectedReview(m, "model-a", "model-b")
			m.width, m.height = 40, 12
			for i, row := range m.visibleRows() {
				m.cursor = i
				frame := tui.Sprint(m.Render(nil), tui.WithPrintWidth(m.width))
				assertFrameFits(t, frame, m.width, m.height)
				if !strings.Contains(stripANSI(frame), "> ") {
					t.Fatalf("row %v has no visible cursor\n%s", row.kind, stripANSI(frame))
				}
			}
		})
	}
}

func TestTooSmallTerminalPausesEditing(t *testing.T) {
	p := providerFrom("p", "https://unused.invalid/v1", "openai")
	m := NewAdvancedConfigModel(&p)
	m.width, m.height = 20, 5
	m.cursor = m.mainRowIndex(rowAPIKey)
	before := m.keyText.Get()
	m.handleKey(keyPress('x'))
	if m.keyText.Get() != before {
		t.Fatal("hidden field was edited")
	}
	frame := tui.Sprint(m.Render(nil), tui.WithPrintWidth(m.width))
	assertFrameFits(t, frame, m.width, m.height)
	s := newSelectComponent("Choose", []string{"a", "b"})
	s.width, s.height = 20, 5
	s.moveCursor(1)
	if s.cursor.Get() != 0 {
		t.Fatal("hidden selection moved")
	}
	assertFrameFits(t, tui.Sprint(s.Render(nil), tui.WithPrintWidth(s.width)), s.width, s.height)
}
