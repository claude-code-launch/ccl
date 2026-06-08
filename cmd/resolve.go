package cmd

import (
	"fmt"
	"os"

	"github.com/haiboyuwen/claude-code-launch/internal/config"
	"github.com/haiboyuwen/claude-code-launch/internal/provider"
)

// resolveProvider determines the active provider.
// Config takes priority over environment variables — once a user has an active_provider
// in config.yaml, stale env vars (like leftover ANTHROPIC_API_KEY from a previous session)
// should not override it. Env vars are only used as a fallback when there is no config.
func resolveProvider() (provider.Provider, error) {
	cfg, err := config.Load()
	if err != nil {
		return provider.Provider{}, fmt.Errorf("failed to load config: %w", err)
	}

	// Config takes priority: if active_provider is set, use it
	if cfg.ActiveProvider != "" {
		p, ok := cfg.Providers[cfg.ActiveProvider]
		if !ok {
			return provider.Provider{}, fmt.Errorf("active provider %q not found in configuration", cfg.ActiveProvider)
		}
		return p, nil
	}

	// No config — fallback to environment variables
	envAnthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	envAnthropicBase := os.Getenv("ANTHROPIC_BASE_URL")

	if envAnthropicKey != "" {
		p := provider.Provider{
			Name:     "environment-anthropic",
			Type:     "anthropic",
			Endpoint: envAnthropicBase,
			APIKey:   envAnthropicKey,
			Model:    os.Getenv("ANTHROPIC_MODEL"),
		}
		if p.Endpoint == "" {
			p.Endpoint = "https://api.anthropic.com"
		}
		return p, nil
	}

	envAPIKey := os.Getenv("OPENAI_API_KEY")
	envBaseURL := os.Getenv("OPENAI_BASE_URL")

	if envAPIKey != "" {
		p := provider.Provider{
			Name:     "environment",
			Type:     "openai",
			Endpoint: envBaseURL,
			APIKey:   envAPIKey,
			Model:    os.Getenv("OPENAI_MODEL"),
		}
		if p.Endpoint == "" {
			p.Endpoint = "https://api.openai.com"
		}
		return p, nil
	}

	return provider.Provider{}, fmt.Errorf("no active provider selected. Use 'ccl add' or 'ccl use', or set OPENAI_API_KEY / ANTHROPIC_API_KEY in environment")
}
