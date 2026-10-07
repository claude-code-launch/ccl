package cmd

import (
	"os"
	"testing"

	"github.com/claude-code-launch/ccl/internal/locale"
)

// TestMain keeps the whole package away from the developer's real ~/.ccl and
// ~/.claude: a test that forgets t.Setenv("HOME") still lands in a throwaway
// home. The UI language is pinned to English so text assertions do not depend
// on the machine's locale or a real config's `lang`.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "ccl-cmd-test-home-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("CLAUDE_CONFIG_DIR", home+"/.claude")
	locale.SetLanguage("en")
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
