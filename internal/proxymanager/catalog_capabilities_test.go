package proxymanager

import (
	"encoding/json"
	"testing"

	"github.com/claude-code-launch/ccl/internal/codex"
)

func TestCatalogCapabilityPolicyAndJournalCompatibility(t *testing.T) {
	var state State
	if err := json.Unmarshal([]byte(`{"catalog_mode":"remote","codex_mode":"independent"}`), &state); err != nil {
		t.Fatal(err)
	}
	if state.CatalogMode != codex.CatalogRemote || !state.CatalogCapabilities().SupportsRemoteFetch || state.CatalogCapabilities().SupportsLiveReload {
		t.Fatal("remote transport implied Desktop reload or broke the journal")
	}
	state.CodexMode = "shared"
	if state.CatalogCapabilities().SupportsRemoteFetch {
		t.Fatal("shared mode claimed a loopback remote source")
	}
	var absent *State
	if absent.CatalogCapabilities().SupportsLiveReload || absent.SupportsGatewayHotReload() {
		t.Fatal("missing runtime advertised capabilities")
	}
}
