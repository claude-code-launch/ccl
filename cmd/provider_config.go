package cmd

import (
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
)

const (
	maxContextTokensEnv  = provider.EnvMaxContextTokens
	autoCompactWindowEnv = provider.EnvAutoCompactWindow
	autoCompactPctEnv    = provider.EnvAutoCompactPct
)

// compactPreset selects the provider-wide context behavior exposed by the TUI.
type compactPreset = provider.ContextPreset

const (
	compactPresetDefault      = provider.ContextPresetDefault
	compactPresetBalanced500K = provider.ContextPresetBalanced500K
	compactPresetBalanced800K = provider.ContextPresetBalanced800K
	// compactPresetBalanced keeps existing call sites and tests pinned to the
	// original 500K Balanced tier.
	compactPresetBalanced = compactPresetBalanced500K
)

func compactPresetFromProvider(p provider.Provider) compactPreset {
	return provider.ProviderContextPreset(p)
}

func hasUnsupportedContextConfig(p provider.Provider) bool {
	return p.ContextPreset == "" && provider.HasUnsupportedContextEnv(p)
}

func compactPresetLabel(preset compactPreset) string {
	switch preset {
	case compactPresetBalanced500K:
		return "Balanced 500K / 1M & 85%"
	case compactPresetBalanced800K:
		return "Balanced 800K / 1M & 85%"
	default:
		return "Default  200K / 1M & 85%"
	}
}

func applyCompactPreset(p *provider.Provider, preset compactPreset) {
	provider.SetProviderContextPreset(p, preset)
}

func oneMModelsFromProvider(p provider.Provider) []string {
	var models []string
	seen := make(map[string]struct{})
	for _, raw := range parseModelList(provider.RuntimeModelSpec(p)) {
		if !hasOneMSuffix(raw) {
			continue
		}
		base := stripOneMSuffix(raw)
		if base == "" {
			continue
		}
		key := strings.ToLower(base)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, base)
	}
	return models
}

func stripOneMSuffix(model string) string {
	model = strings.TrimSpace(model)
	for strings.HasSuffix(model, "[1m]") {
		model = strings.TrimSpace(strings.TrimSuffix(model, "[1m]"))
	}
	return model
}

func hasOneMSuffix(model string) bool {
	return strings.HasSuffix(strings.TrimSpace(model), "[1m]")
}

// updateProvider applies fn to one provider under the config lock, against the
// latest file, so a concurrent change to another field or provider survives.
func updateProvider(name string, fn func(*provider.Provider) error) error {
	return config.Update(func(cfg *provider.Config) error {
		p, ok := cfg.Providers[name]
		if !ok {
			return fmt.Errorf(locale.T("未找到 Provider %q", "provider %q not found in configuration"), name)
		}
		if err := fn(&p); err != nil {
			return err
		}
		cfg.Providers[name] = p
		return nil
	})
}

// replaceProvider stores p as the whole entry for name under the config lock.
// Commands that edit one provider as a unit (set) use it: the rest of the
// config — other providers, selections, settings — is re-read, not replayed.
func replaceProvider(name string, p provider.Provider) error {
	return config.Update(func(cfg *provider.Config) error {
		cfg.Providers[name] = p
		return nil
	})
}
