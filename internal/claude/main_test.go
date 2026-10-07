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
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
