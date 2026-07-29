package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewSessionNameIsUniqueAndSafe(t *testing.T) {
	seen := make(map[string]struct{}, 32)
	for i := 0; i < 32; i++ {
		name := newSessionName()
		if !strings.HasPrefix(name, "claude_") {
			t.Fatalf("session name = %q, want a claude_ prefix", name)
		}
		if name != filepath.Base(name) || strings.ContainsAny(name, `/\ .`) {
			t.Fatalf("session name is not safe in a file name: %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			t.Fatalf("session name %q was generated twice", name)
		}
		seen[name] = struct{}{}
	}
}

func TestWriteSettingsFileNamesFileAfterSession(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	session := newSessionName()

	path, err := writeSettingsFile(settingsJSON{Env: map[string]string{"A": "B"}}, session)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if want := session + "_settings.json"; filepath.Base(path) != want {
		t.Fatalf("settings file = %q, want base %q", path, want)
	}

	// The session name also has to be reusable as the log file name, so a second
	// session must never collide with an existing settings file.
	if _, err := writeSettingsFile(settingsJSON{}, session); err == nil {
		t.Fatal("expected the second write for the same session to fail")
	}
}
