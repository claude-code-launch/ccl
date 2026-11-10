package cmd

import (
	"context"
	"os"
	"testing"

	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
)

// TestMain keeps the whole package away from the developer's real ~/.ccl and
// ~/.grok: a test that forgets t.Setenv("HOME") still lands in a throwaway
// home. The UI language is pinned to English so text assertions do not depend
// on the machine's locale or a real config's `lang`.
func TestMain(m *testing.M) {
	if os.Getenv("CCL_TEST_FAKE_GROK") == "1" {
		os.Exit(fakeGrokProcess())
	}
	home, err := os.MkdirTemp("", "ccl-cmd-test-home-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("USERPROFILE", home)
	locale.SetLanguage("en")
	// Metadata refresh tests inject local services explicitly. OAuth/config
	// tests must never contact the public catalog as a side effect.
	modelsDevRefresh = func(context.Context) (*modelsdev.Snapshot, error) {
		return &modelsdev.Snapshot{}, nil
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
