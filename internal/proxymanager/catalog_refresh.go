package proxymanager

import (
	"fmt"
	"os"

	"github.com/claude-code-launch/ccl/internal/codex"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/provider"
)

type catalogRefresh struct {
	configStamp string
	initialized bool
}

func (r *catalogRefresh) update(m *Manager, gateway *codex.Gateway, state *State, runtimeProvider provider.Provider) error {
	var stamp string
	if info, err := os.Stat(config.ConfigPath()); err == nil {
		stamp = fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	}
	metadataStamp := modelsdev.CacheStamp()
	if r.initialized && stamp == r.configStamp && metadataStamp == state.MetadataStamp {
		return nil
	}
	latest := runtimeProvider
	if stamp != "" {
		cfg, err := config.Load()
		if err != nil {
			// Parse errors may contain configuration values; do not log them.
			return fmt.Errorf("read CCL configuration for catalog refresh failed")
		}
		found, exists := cfg.Providers[state.Provider]
		if !exists {
			return nil // Never follow active_provider into a different account.
		}
		latest = found
	}
	// Only catalog settings change in place. Endpoint, protocol and bound
	// credentials still require an explicit start to replace the runtime.
	latest.Name = runtimeProvider.Name
	latest.Type = runtimeProvider.Type
	latest.OAuthProvider = runtimeProvider.OAuthProvider
	latest.ModelsDevProvider = runtimeProvider.ModelsDevProvider
	if err := gateway.RefreshMetadata(latest, state.Codex.RefreshCatalog); err != nil {
		return err
	}
	if state.MetadataStamp != metadataStamp {
		updated := *state
		updated.MetadataStamp = metadataStamp
		if err := writeJSON(m.statePath(), &updated); err != nil {
			return err
		}
		state.MetadataStamp = metadataStamp
	}
	r.configStamp, r.initialized = stamp, true
	return nil
}
