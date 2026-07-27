package cmd

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestAuthGroupEditorSelectAllAndSpace(t *testing.T) {
	m := &authGroupEditorModel{
		name:  "gg",
		group: provider.AuthGroup{OAuthProvider: "grok"},
		credentials: []oauthproxy.CredentialInfo{
			{FileName: "xai-a.json", Backend: "xai", OAuthProvider: "grok"},
			{FileName: "xai-b.json", Backend: "xai", OAuthProvider: "grok"},
			{FileName: "xai-c.json", Backend: "xai", OAuthProvider: "grok"},
		},
		providers: []string{"grok"},
		selected:  map[string]bool{},
		cursor:    1,
	}

	// Space on the select-all row selects every account for the backend.
	next, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: ' '}))
	m = next.(*authGroupEditorModel)
	if m.selectedCountForBackend() != 3 {
		t.Fatalf("space on select-all did not select all; selected=%v count=%d string=%q",
			m.selected, m.selectedCountForBackend(), tea.KeyPressMsg(tea.Key{Code: ' '}).String())
	}

	// Space again deselects everything.
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: ' '}))
	m = next.(*authGroupEditorModel)
	if m.selectedCountForBackend() != 0 {
		t.Fatalf("space did not deselect all; selected=%v", m.selected)
	}

	// Shortcut "a" also toggles select-all.
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: 'a'}))
	m = next.(*authGroupEditorModel)
	if m.selectedCountForBackend() != 3 {
		t.Fatalf("a did not select all; selected=%v count=%d", m.selected, m.selectedCountForBackend())
	}

	// Compact view never lists individual credential filenames.
	view := m.View().Content
	for _, name := range []string{"xai-a.json", "xai-b.json", "xai-c.json"} {
		if strings.Contains(view, name) {
			t.Fatalf("compact editor still shows credential file %q:\n%s", name, view)
		}
	}
	if !strings.Contains(view, "3/3") {
		t.Fatalf("compact editor missing selected/total count:\n%s", view)
	}

	// Save is only one row below select-all.
	m.cursor = 1
	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = next.(*authGroupEditorModel)
	if m.cursor != authGroupEditorSaveCursor {
		t.Fatalf("expected save cursor %d after one down press, got %d", authGroupEditorSaveCursor, m.cursor)
	}
	next, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*authGroupEditorModel)
	if !m.saved || cmd == nil {
		t.Fatalf("enter on save should quit with saved=true; saved=%v cmd=%v", m.saved, cmd)
	}
}
