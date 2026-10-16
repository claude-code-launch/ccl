// Package config reads and writes ~/.ccl/config.yaml.
//
// Load is read-only: it normalizes in memory so every caller sees the same
// derived values, but never writes. Every change goes through Update, which holds
// an exclusive file lock across re-read → change → atomic write, so a
// long-running command (ccl set, an OAuth login) cannot overwrite what another
// ccl process changed in the meantime.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/provider"
	"gopkg.in/yaml.v3"
)

// CurrentVersion is the config schema this ccl writes.
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

// Load reads the config and normalizes it in memory. It never writes; a
// missing file yields a fresh-install config.
func Load() (*provider.Config, error) {
	return read()
}

func read() (*provider.Config, error) {
	cfg := &provider.Config{Providers: make(map[string]provider.Provider)}
	path, err := configPath()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg.LogLevel = "off"
			// A fresh install runs Claude Code on its own login (the "default"
			// selector) for normal and ACP launches until a provider is chosen.
			cfg.ActiveProvider = provider.DefaultProvider
			return cfg, nil
		}
		return cfg, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return cfg, err
	}
	normalize(cfg)
	return cfg, nil
}

// Update applies fn to the current config under the config lock and writes the
// result. fn sees the latest file, not a snapshot taken earlier, so concurrent
// ccl processes cannot drop each other's changes. Returning an error from fn
// leaves the file untouched.
func Update(fn func(*provider.Config) error) error {
	return withLock(func(path string) error {
		cfg, err := read()
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
	// An OAuth subscription's type is derived from its backend on load, so it
	// is not persisted; the in-memory config keeps it.
	onDisk := *cfg
	onDisk.Providers = make(map[string]provider.Provider, len(cfg.Providers))
	for name, p := range cfg.Providers {
		if _, ok := provider.OAuthRuntimeType(p.OAuthProvider); ok {
			p.Type = ""
		}
		onDisk.Providers[name] = p
	}
	data, err := yaml.Marshal(&onDisk)
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

// normalize fills in the values a config file may leave out.
func normalize(cfg *provider.Config) {
	// File logging is opt-in; every caller observes an explicit "off".
	if strings.TrimSpace(cfg.LogLevel) == "" {
		cfg.LogLevel = "off"
	}
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]provider.Provider)
	}
	for name, p := range cfg.Providers {
		provider.NormalizeProvider(&p)
		cfg.Providers[name] = p
	}
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return fsutil.WriteFileAtomic(path, data, mode)
}
