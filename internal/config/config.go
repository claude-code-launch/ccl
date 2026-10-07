// Package config reads and writes ~/.ccl/config.yaml.
//
// Load is read-only: it applies the current migrations in memory so every
// caller sees normalized values, but never writes. Migrate persists those
// migrations once per process. Every change goes through Update, which holds
// an exclusive file lock across re-read → change → atomic write, so a
// long-running command (ccl set, an OAuth login) cannot overwrite what another
// ccl process changed in the meantime.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/claude-code-launch/ccl/internal/provider"
	"gopkg.in/yaml.v3"
)

// CurrentVersion is the config schema this ccl writes. Migrations that are not
// idempotent with respect to user intent run only for older files.
const CurrentVersion = 2

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

func legacyConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".cc", "config.yaml"), nil
}

// Load reads the config and normalizes it in memory. It never writes; a
// missing file yields an empty config. A pre-rename ~/.cc/config.yaml is read
// in place until Migrate moves it.
func Load() (*provider.Config, error) {
	cfg, _, err := read()
	return cfg, err
}

// read loads and normalizes the config, reporting whether the normalized form
// differs from what is on disk.
func read() (*provider.Config, bool, error) {
	cfg := &provider.Config{Providers: make(map[string]provider.Provider)}
	path, err := configPath()
	if err != nil {
		return cfg, false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		legacy, legacyErr := legacyConfigPath()
		if legacyErr == nil {
			data, err = os.ReadFile(legacy)
		}
	}
	if err != nil {
		if os.IsNotExist(err) {
			cfg.LogLevel = "off"
			return cfg, false, nil
		}
		return cfg, false, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return cfg, false, err
	}
	changed := normalize(cfg)
	return cfg, changed, nil
}

// Migrate persists the migrations Load applies in memory, and moves a
// pre-rename ~/.cc/config.yaml into ~/.ccl. It runs once at startup.
func Migrate() error {
	return withLock(func(path string) error {
		if err := moveLegacyConfig(path); err != nil {
			return err
		}
		cfg, changed, err := read()
		if err != nil || !changed {
			return err
		}
		return write(path, cfg)
	})
}

// Update applies fn to the current config under the config lock and writes the
// result. fn sees the latest file, not a snapshot taken earlier, so concurrent
// ccl processes cannot drop each other's changes. Returning an error from fn
// leaves the file untouched.
func Update(fn func(*provider.Config) error) error {
	return withLock(func(path string) error {
		cfg, _, err := read()
		if err != nil {
			return err
		}
		if err := fn(cfg); err != nil {
			return err
		}
		return write(path, cfg)
	})
}

// Save replaces the whole config. Prefer Update, which cannot lose a change
// another process made since cfg was loaded.
func Save(cfg *provider.Config) error {
	return withLock(func(path string) error {
		normalize(cfg)
		return write(path, cfg)
	})
}

func write(path string, cfg *provider.Config) error {
	cfg.ConfigVersion = CurrentVersion
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o600)
}

// withLock runs fn while holding an exclusive lock on ~/.ccl/.config.lock.
func withLock(fn func(path string) error) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(filepath.Dir(path), ".config.lock"))
	if err != nil {
		return fmt.Errorf("lock ccl config: %w", err)
	}
	defer unlock()
	return fn(path)
}

func moveLegacyConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat config: %w", err)
	}
	legacyPath, err := legacyConfigPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(legacyPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat legacy config: %w", err)
	}
	if err := os.Rename(legacyPath, path); err != nil {
		return fmt.Errorf("migrate legacy config: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure migrated config permissions: %w", err)
	}
	return nil
}

// normalize applies every migration in memory and reports whether the result
// should be written back. Idempotent migrations run on every load; the
// version-gated one runs only for files older than CurrentVersion.
func normalize(cfg *provider.Config) bool {
	changed := cfg.ConfigVersion < CurrentVersion

	// The old boolean debug switch maps to a logging threshold; `verbose` meant
	// request-body tracing, which is slog's DEBUG level.
	if strings.TrimSpace(cfg.LogLevel) == "" && cfg.DebugMode {
		cfg.LogLevel = "info"
		if cfg.DebugVerbose {
			cfg.LogLevel = "debug"
		}
		changed = true
	}
	if cfg.DebugMode || cfg.DebugVerbose {
		cfg.DebugMode, cfg.DebugVerbose = false, false
		changed = true
	}
	// File logging is opt-in; every caller observes an explicit "off".
	if strings.TrimSpace(cfg.LogLevel) == "" {
		cfg.LogLevel = "off"
	}
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]provider.Provider)
	}

	legacySlots := cfg.ConfigVersion < CurrentVersion
	for name, p := range cfg.Providers {
		if provider.NormalizeProvider(&p, name, legacySlots) {
			changed = true
		}
		cfg.Providers[name] = p
	}
	return changed
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
