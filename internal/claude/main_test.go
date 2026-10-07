package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain keeps every test in this package away from the developer's real
// ~/.ccl/run: settings files written by tests go to a throwaway directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ccl-claude-test-run-")
	if err != nil {
		panic(err)
	}
	runDirectory = func() (string, error) { return filepath.Join(dir, "run"), nil }
	// Nor may they read the developer's Claude Code settings, which decide
	// which preferences ccl leaves alone.
	_ = os.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude-config"))
	userSettingsCWD = func() string { return filepath.Join(dir, "project") }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
