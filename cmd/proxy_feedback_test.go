package cmd

import (
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/codex"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/proxymanager"
)

func TestCatalogFeedbackSeparatesRouteUpdatesFromPickerReload(t *testing.T) {
	before := locale.Current()
	locale.SetLanguage("en")
	t.Cleanup(func() { locale.SetLanguage(before) })
	for _, mode := range []codex.CatalogMode{codex.CatalogLocal, codex.CatalogRemote} {
		text := codexCatalogConnectionMessage(mode.Capabilities())
		if !strings.Contains(text, "restart") || strings.Contains(text, "live reload") {
			t.Fatalf("source %q misrepresented picker freshness: %s", mode, text)
		}
	}
	state := &proxymanager.State{ProxyVersion: 17, Provider: "one", AdditionalProviders: []string{"two"}}
	if codexModelUpdateMessage(state, "unrelated") != "" || codexModelUpdateMessage(nil, "one") != "" {
		t.Fatal("unrelated provider save gained a Codex notice")
	}
	for _, name := range []string{"one", "two"} {
		text := codexModelUpdateMessage(state, name)
		if !strings.Contains(text, "will update routes") || !strings.Contains(text, "may need a restart") {
			t.Fatalf("save notice conflated gateway and picker updates: %s", text)
		}
	}
}
