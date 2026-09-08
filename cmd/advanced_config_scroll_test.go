package cmd

import "testing"

// TestCursorScrollFollowsMovement locks the scroll invariant that was broken:
// moving the cursor down through a page taller than the terminal must advance
// the scroll window so the cursor line stays inside it, rather than staying at
// the top until the cursor hits the tail. The check uses the same window the
// renderer slices (scrollWindow) and the same cursor locator
// (cursorBodyLine), so an off-screen cursor fails as soon as it drifts.
func TestCursorScrollFollowsMovement(t *testing.T) {
	p := providerFrom("demo", "https://example.test/v1", "openai")
	p.APIKey = "sk-fake-test-key"
	m := NewAdvancedConfigModel(&p)
	enterDetectedReview(m, "model-a", "model-b", "model-c")
	m.width = 100
	m.height = 20 // small terminal: the body overflows

	body := m.bodyRows()
	maxBody := scrollBodyBudget(m.height)
	if len(body) <= maxBody {
		t.Fatalf("test needs an overflowing body, got %d lines <= %d", len(body), maxBody)
	}

	// Cursor at the top: no scroll.
	m.cursor = m.mainRowIndex(rowSource)
	m.keepCursorVisible()
	if m.scrollOffset != 0 {
		t.Fatalf("cursor at top should not scroll, offset=%d", m.scrollOffset)
	}

	// Cursor down into Model Mapping: the window must follow.
	for _, kind := range []configRowKind{rowTest, rowOpus, rowCustom, rowTestModels} {
		m.cursor = m.mainRowIndex(kind)
		m.keepCursorVisible()
		line := m.cursorBodyLine(body)
		if line < m.scrollOffset || line >= m.scrollOffset+maxBody {
			t.Fatalf("cursor %d (line %d) outside window [%d,%d)", kind, line, m.scrollOffset, m.scrollOffset+maxBody)
		}
	}
	bottomOffset := m.scrollOffset

	// Cursor back to the top: the window follows back up.
	m.cursor = m.mainRowIndex(rowSource)
	m.keepCursorVisible()
	backLine := m.cursorBodyLine(body)
	if backLine < m.scrollOffset || backLine >= m.scrollOffset+maxBody {
		t.Fatalf("after scrolling back up, cursor line %d outside window [%d,%d)", backLine, m.scrollOffset, m.scrollOffset+maxBody)
	}
	if m.scrollOffset >= bottomOffset {
		t.Fatalf("scroll should decrease when moving back up: %d >= %d", m.scrollOffset, bottomOffset)
	}
}
