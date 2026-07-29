package oauthproxy

import (
	"testing"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"gopkg.in/yaml.v3"
)

// The three runtime config files inline runtimeConfigBase. Without the inline tag
// the shared block would be emitted as a nested "runtimeconfigbase" mapping and
// CLIProxyAPI would silently start with defaults, so pin the encoded shape.
func TestRuntimeConfigFilesInlineSharedBase(t *testing.T) {
	base := newRuntimeConfigBase(45678, "/tmp/ccl-auth", "ccl-test-key")
	configs := map[string]any{
		"oauth":  runtimeConfigFile{runtimeConfigBase: base},
		"codex":  runtimeCodexConfigFile{runtimeConfigBase: base},
		"openai": runtimeOpenAIConfigFile{runtimeConfigBase: base},
	}

	for name, config := range configs {
		raw, err := yaml.Marshal(config)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var decoded map[string]any
		if err := yaml.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		if decoded["host"] != runtimeLoopbackHost {
			t.Errorf("%s: host = %v, want %s\n%s", name, decoded["host"], runtimeLoopbackHost, raw)
		}
		if decoded["port"] != 45678 {
			t.Errorf("%s: port = %v, want 45678\n%s", name, decoded["port"], raw)
		}
		if decoded["auth-dir"] != "/tmp/ccl-auth" {
			t.Errorf("%s: auth-dir = %v\n%s", name, decoded["auth-dir"], raw)
		}
		if decoded["logging-to-file"] != false {
			t.Errorf("%s: logging-to-file = %v\n%s", name, decoded["logging-to-file"], raw)
		}
		if decoded["disable-image-generation"] != "passthrough" {
			t.Errorf("%s: disable-image-generation = %v\n%s", name, decoded["disable-image-generation"], raw)
		}
		keys, _ := decoded["api-keys"].([]any)
		if len(keys) != 1 || keys[0] != "ccl-test-key" {
			t.Errorf("%s: api-keys = %v\n%s", name, decoded["api-keys"], raw)
		}

		parsed, err := sdkconfig.ParseConfigBytes(raw)
		if err != nil {
			t.Fatalf("%s: CLIProxyAPI rejected the config: %v\n%s", name, err, raw)
		}
		if parsed.Port != 45678 {
			t.Errorf("%s: parsed port = %d, want 45678", name, parsed.Port)
		}
	}
}
