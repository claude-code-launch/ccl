package cmd

import (
	"errors"
	"fmt"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

// providerTarget selects which provider a per-provider command acts on: an
// explicit --provider, ACP's provider with --acp, or the normal-mode one.
type providerTarget struct {
	name string
	acp  bool
}

func addProviderTargetFlags(cmd *cobra.Command, target *providerTarget) {
	flags := cmd.PersistentFlags()
	flags.StringVar(&target.name, "provider", "", "Act on this provider instead of the active one")
	flags.BoolVar(&target.acp, "acp", false, "Act on the provider ACP uses")
}

// resolve returns the provider name the command should act on.
func (t providerTarget) resolve(cfg *provider.Config) (string, error) {
	if t.name != "" && t.acp {
		return "", errors.New("use either --provider or --acp, not both")
	}
	name := t.name
	switch {
	case name != "":
	case t.acp:
		name = cfg.EffectiveACPProvider()
	default:
		name = cfg.ActiveProvider
	}
	if name == "" {
		return "", errors.New(locale.T(
			"未选择 Provider。用 ccl use <name> 选择，或加 --provider <name>",
			"no provider selected. Choose one with ccl use <name>, or pass --provider <name>",
		))
	}
	if _, ok := cfg.Providers[name]; !ok {
		return "", fmt.Errorf(locale.T("未找到 Provider %q", "provider %q not found in configuration"), name)
	}
	return name, nil
}

// load resolves the target and returns its provider.
func (t providerTarget) load() (string, provider.Provider, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", provider.Provider{}, fmt.Errorf("failed to load config: %w", err)
	}
	name, err := t.resolve(cfg)
	if err != nil {
		return "", provider.Provider{}, err
	}
	return name, cfg.Providers[name], nil
}
