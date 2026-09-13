package claude

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestGatewayDiscoveryDefaultsToCCLMapping(t *testing.T) {
	for _, model := range []string{"", "fake-model", "fake-one,fake-two"} {
		for _, proxy := range []bool{false, true} {
			p := provider.Provider{Type: "anthropic", Model: model, OpusModel: "mapped-opus", SonnetModel: "mapped-sonnet", HaikuModel: "mapped-haiku"}
			env := buildEnv(p, "https://example.invalid", proxy)
			if env["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] != "0" {
				t.Fatalf("gateway discovery enabled for model=%q proxy=%t", model, proxy)
			}
			if env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "mapped-opus" || env["ANTHROPIC_DEFAULT_SONNET_MODEL"] != "mapped-sonnet" || env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "mapped-haiku" {
				t.Fatal("explicit mapping was changed")
			}
			p.Env = map[string]string{"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1"}
			if buildEnv(p, "https://example.invalid", proxy)["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] != "1" {
				t.Fatal("explicit opt-in was ignored")
			}
		}
	}
}
