package config

import (
	"os"
	"path/filepath"

	"github.com/claude-code-launch/ccl/internal/provider"
	"gopkg.in/yaml.v3"
)

func ConfigPath() string {
	home, _ := os.UserHomeDir()
	newPath := filepath.Join(home, ".ccl", "config.yaml")

	// Automatic Migration logic:
	// If the new config directory ~/.ccl does not exist, but old ~/.cc/config.yaml exists,
	// automatically migrate/move it to the new path.
	oldPath := filepath.Join(home, ".cc", "config.yaml")
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		if _, errOld := os.Stat(oldPath); errOld == nil {
			_ = os.MkdirAll(filepath.Dir(newPath), 0755)
			_ = os.Rename(oldPath, newPath)
		}
	}

	return newPath
}

func Load() (*provider.Config, error) {
	cfg := &provider.Config{
		Providers: make(map[string]provider.Provider),
	}

	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		// If the config file does not exist, return an empty initialized config
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}

	err = yaml.Unmarshal(data, cfg)
	if err != nil {
		return cfg, err
	}

	if cfg.Providers == nil {
		cfg.Providers = make(map[string]provider.Provider)
	}
	for name, p := range cfg.Providers {
		cfg.Providers[name] = provider.NormalizeLegacyCustomSlot(p)
	}

	return cfg, nil
}

func Save(cfg *provider.Config) error {
	path := ConfigPath()

	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0600)
}
