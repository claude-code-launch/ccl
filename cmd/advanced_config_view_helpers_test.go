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

// keyPress builds the KeyEvent a terminal sends for a printable character.
func keyPress(r rune) tui.KeyEvent {
	return tui.KeyEvent{Key: tui.KeyRune, Rune: r}
}

// specialKey builds the KeyEvent for a non-printable key (arrows, enter, esc).
func specialKey(key tui.Key) tui.KeyEvent {
	return tui.KeyEvent{Key: key}
}

// pressKey routes one key through the component's key handler (as the app
// would) and reports whether the session was asked to quit. The quit signal
// itself is carried by the model's saveConfirmed/canceled state in tests.
func pressKey(m *AdvancedConfigModel, ke tui.KeyEvent) {
	m.handleKey(ke)
}
