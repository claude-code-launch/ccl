package config

import (
	"os"
	"path/filepath"

	"github.com/spf13/viper"
	"github.com/haiboyuwen/cc/internal/provider"
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

	v := viper.New()
	v.SetConfigFile(ConfigPath())

	if err := v.ReadInConfig(); err != nil {
		// If the config file does not exist, return an empty initialized config
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}

	err := v.Unmarshal(cfg)
	if err != nil {
		return cfg, err
	}

	if cfg.Providers == nil {
		cfg.Providers = make(map[string]provider.Provider)
	}

	return cfg, nil
}

func Save(cfg *provider.Config) error {
	path := ConfigPath()

	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		return err
	}

	v := viper.New()
	v.SetConfigFile(path)
	v.Set("active_provider", cfg.ActiveProvider)
	v.Set("providers", cfg.Providers)

	return v.WriteConfigAs(path)
}
