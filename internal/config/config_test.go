package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
)

func TestLoadMigratesLegacyCustomSlot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfgDir := filepath.Join(home, ".ccl")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	path := filepath.Join(cfgDir, "config.yaml")
	data := []byte(`active_provider: legacy
providers:
    legacy:
        name: legacy
        type: openai
        endpoint: http://127.0.0.1:8080/v1
        apikey: sk-test
        model: model-a,model-b
        lockModel: old-custom-model
`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	p := cfg.Providers["legacy"]
	if p.CustomModelID != "old-custom-model" {
		t.Fatalf("legacy lockModel was not migrated to customModelId: %+v", p)
	}
	if p.LockModel != "" {
		t.Fatalf("legacy lockModel should be cleared after migration: %+v", p)
	}
}
