package cmd

import (
	"strings"
	"testing"

	tui "github.com/grindlemire/go-tui"
)

// renderView renders the model offline like a terminal frame would, so tests
// can assert on the visible text (labels, hints, badges) the same way they
// asserted on the old bubbletea View().Content.
func renderView(t *testing.T, m *AdvancedConfigModel) string {
	t.Helper()
	s := tui.Sprint(m.Render(nil), tui.WithPrintWidth(80))
	return strings.TrimRight(s, " \n")
}

// readyOAuthPage marks the subscription's loopback runtime as up. The page gates
// model editing and saving on that, so tests exercising post-start behavior have
// to put it in place; handleOAuthRuntimeDone is what does it for real.
func readyOAuthPage(m *AdvancedConfigModel) *AdvancedConfigModel {
	m.runtimeLoading = false
	m.runtimeErr = nil
	m.runtimeReady = true
	return m
}

// keyPress builds the KeyEvent a terminal sends for a printable character.
func keyPress(r rune) tui.KeyEvent {
	return tui.KeyEvent{Key: tui.KeyRune, Rune: r}
}
