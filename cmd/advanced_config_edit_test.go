package cmd

import (
	"strings"
	"testing"

	tui "github.com/grindlemire/go-tui"
)

// TestConnectedProviderEditsURLKey verifies that an already-connected (Custom)
// provider still lets the user edit Endpoint URL and API Key, and that an edit
// flips the connection to dirty (requiring a fresh Auto Configure) and blocks
// saving until re-detected.
func TestConnectedProviderEditsURLKey(t *testing.T) {
	p := providerFrom("resp", "https://example.test/v1", "openai")
	m := NewAdvancedConfigModel(&p)
	enterDetectedReview(m, "gpt-x")

	if m.canSave() == false {
		t.Fatalf("freshly detected provider must be saveable, got dirty=%t", m.live().connectionDirty)
	}

	// Move to Endpoint URL and type a character: it must append and mark dirty.
	idx := m.mainRowIndex(rowEndpoint)
	if idx < 0 {
		t.Fatalf("rowEndpoint missing from visibleRows")
	}
	m.cursor = idx
	m.handleKey(tui.KeyEvent{Key: tui.KeyRune, Rune: 'x'})
	if !strings.HasSuffix(m.urlText.Get(), "x") {
		t.Fatalf("url edit not applied: %q", m.urlText.Get())
	}
	if !m.live().connectionDirty {
		t.Fatalf("url edit did not mark connection dirty")
	}
	if m.canSave() {
		t.Fatalf("dirty connection must not be saveable")
	}

	// Same for API Key.
	keyIdx := m.mainRowIndex(rowAPIKey)
	m.cursor = keyIdx
	m.handleKey(tui.KeyEvent{Key: tui.KeyRune, Rune: 'y'})
	if !strings.HasSuffix(m.keyText.Get(), "y") {
		t.Fatalf("key edit not applied: %q", m.keyText.Get())
	}
}
