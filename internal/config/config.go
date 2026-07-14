package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/claude-code-launch/ccl/internal/provider"
	"gopkg.in/yaml.v3"
)

func ConfigPath() string {
	path, _ := configPath()
	return path
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".ccl", "config.yaml"), nil
}

func migrateLegacyConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat config: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory for migration: %w", err)
	}
	legacyPath := filepath.Join(home, ".cc", "config.yaml")
	if _, err := os.Stat(legacyPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat legacy config: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory for migration: %w", err)
	}
	if err := os.Rename(legacyPath, path); err != nil {
		return fmt.Errorf("migrate legacy config: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure migrated config permissions: %w", err)
	}
	return nil
}

func Load() (*provider.Config, error) {
	cfg := &provider.Config{
		Providers: make(map[string]provider.Provider),
	}

	path, err := configPath()
	if err != nil {
		return cfg, err
	}
	if err := migrateLegacyConfig(path); err != nil {
		return cfg, err
	}
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
		if p.OAuthProvider == "" {
			p.OAuthProvider = provider.InferOAuthProvider(name, p.Endpoint)
			cfg.Providers[name] = p
		}
	}
	return cfg, nil
}

func Save(cfg *provider.Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := migrateLegacyConfig(path); err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	return writeFileAtomic(path, data, 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set temporary config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config atomically: %w", err)
	}
	return nil
}
