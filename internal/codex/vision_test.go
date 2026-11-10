package codex

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/protocol"
)

func TestCodexCatalogKeepsAdvertisedImageCapabilities(t *testing.T) {
	for _, modalities := range [][]string{nil, {"text"}, {"text", "image"}} {
		model := newModel("alias", "model", map[string]protocol.ModelInfo{"model": {InputModalities: modalities}}, nil)
		got := model["input_modalities"].([]string)
		wantImage := len(modalities) == 2
		if (len(got) == 2) != wantImage || got[0] != "text" || model["node_repl_disabled"] != false {
			t.Fatalf("advertised=%v model=%+v", modalities, model)
		}
	}
}
