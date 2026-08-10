package cmd

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

type authOptions struct {
	noBrowser    bool
	callbackPort int
	kiroAuthMode string
}

var oauthLogin = oauthproxy.Login

var authCmd = newAuthCommand()

func newAuthCommand() *cobra.Command {
	opts := authOptions{}
	cmd := &cobra.Command{
		Use:     "oauth <gpt|gemini|grok|copilot|qoder|kimi|kiro|claude> [alias]",
		Aliases: []string{"auth"},
		Short:   "Authenticate a subscription-backed provider",
		Long: `Authenticate subscription-backed providers.

Login (creates/updates a provider and stores JSON under ~/.ccl/auth):

  ccl oauth gpt                 # ChatGPT / Codex subscription
  ccl oauth gpt work            # same backend, provider name "work"
  ccl oauth gemini|grok|copilot|qoder|kimi|kiro|claude

Notes:
  - Alias "auth" still works: ccl auth gpt
  - Legacy "chatgpt" is accepted and normalized to "gpt"
  - Fast mode (gpt): Claude /fast or ccl set Review & Apply
  - Qoder uses direct browser OAuth; qodercli is neither required nor invoked
  - Kiro defaults to Portal OAuth (Google/GitHub); use --kiro-auth builder for Builder ID
  - Flags: --no-browser, --callback-port, --kiro-auth
`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuth(cmd.Context(), cmd.OutOrStdout(), args, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.noBrowser, "no-browser", false, "Print the OAuth URL instead of opening a browser")
	cmd.Flags().IntVar(&opts.callbackPort, "callback-port", 0, "Override the OAuth callback port (ChatGPT/Claude/Kiro Portal)")
	cmd.Flags().StringVar(&opts.kiroAuthMode, "kiro-auth", oauthproxy.KiroAuthModePortal, "Kiro login mode: portal or builder")
	return cmd
}

// supportsFastMode reports whether the GPT subscription backend honours the
// Claude Code fastMode toggle.
func supportsFastMode(providerName string) bool {
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case oauthproxy.ProviderChatGPT, oauthproxy.ProviderChatGPTLegacy:
		return true
	default:
		return false
	}
}

func runAuth(ctx context.Context, out io.Writer, args []string, opts authOptions) error {
	target, err := oauthproxy.ValidateLoginProvider(args[0])
	if err != nil {
		return err
	}
	// OAuth backends have a fixed runtime protocol. StartProvider ignores any
	// Type override when OAuthProvider is set, so always persist the real path.
	protocolType := oauthRuntimeType(target)

	var alias string
	if len(args) > 1 {
		alias = strings.TrimSpace(args[1])
		if isReservedProviderName(alias) {
			return fmt.Errorf("alias %q collides with a reserved provider name; choose a different alias", alias)
		}
		if alias == "" {
			return fmt.Errorf("alias cannot be empty")
		}
	}

	fmt.Fprintf(out, "Authenticating %s...\n", target)
	result, err := oauthLogin(ctx, target, oauthproxy.LoginOptions{
		NoBrowser:    opts.noBrowser,
		CallbackPort: opts.callbackPort,
		KiroAuthMode: opts.kiroAuthMode,
	})
	if err != nil {
		return fmt.Errorf("authenticate %s: %w", target, err)
	}

	// Every login produces an independent provider entry. With an explicit
	// alias it becomes the provider key; without one we derive one from the
	// credential file so multiple accounts on the same backend never overwrite
	// each other.
	providerName := alias
	if providerName == "" {
		providerName = derivedProviderName(target, result.Path)
	}
	credentialFile := filepath.Base(result.Path)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}
	p, targetExists := cfg.Providers[providerName]
	// GPT migrates the legacy "codex" OAuth provider alias when no explicit
	// alias is used. Copilot is a separate GitHub backend.
	if target == oauthproxy.ProviderChatGPT && alias == "" {
		if legacy, exists := cfg.Providers[oauthproxy.ProviderCodex]; exists && strings.EqualFold(strings.TrimSpace(legacy.OAuthProvider), oauthproxy.ProviderCodex) {
			if !targetExists {
				p = legacy
			}
			delete(cfg.Providers, oauthproxy.ProviderCodex)
		}
	}
	p = configureOAuthProvider(p, providerName, target, credentialFile)
	cfg.Providers[providerName] = p
	cfg.ActiveProvider = providerName
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save OAuth provider: %w", err)
	}

	fmt.Fprintf(out, "Authenticated %s as provider %q and switched active provider.\n", target, providerName)
	fmt.Fprintf(out, "Credentials: %s\n", result.Path)
	if target == oauthproxy.ProviderCopilot {
		fmt.Fprintln(out, "Protocol: automatic (Responses / Chat Completions / Anthropic Messages per model)")
	} else {
		fmt.Fprintf(out, "Protocol: %s (fixed for this OAuth backend)\n", provider.ProtocolLabel(protocolType))
	}
	if supportsFastMode(target) {
		fmt.Fprintf(out, "Fast: %s (toggle with /fast or ccl set Review & Apply)\n", providerFastSummary(p))
	}
	return nil
}

// configureOAuthProvider normalizes the provider created or refreshed by login
// while preserving model mappings and other user-tuned fields on p.
func configureOAuthProvider(p provider.Provider, name, oauthProvider, credentialFile string) provider.Provider {
	backend, _ := oauthproxy.BackendProvider(oauthProvider)
	p.Name = name
	p.Type = oauthRuntimeType(oauthProvider)
	p.Endpoint = "oauth://" + backend
	p.APIKey = ""
	p.AnthropicAuth = ""
	p.OAuthProvider = oauthProvider
	p.OAuthAccountCredential = strings.TrimSpace(credentialFile)
	if !supportsFastMode(oauthProvider) {
		p.FastMode = false
	}
	provider.ApplyOAuthSlotDefaults(&p)
	return p
}

// oauthRuntimeType is the internal compatibility type persisted for each
// subscription backend. Copilot's real upstream protocol remains per-model.
func oauthRuntimeType(providerName string) string {
	if runtimeType, ok := provider.OAuthRuntimeType(providerName); ok {
		return runtimeType
	}
	return "openai_responses"
}

// isReservedProviderName blocks aliases that would collide with canonical
// provider names or SDK backend keys.
func isReservedProviderName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case oauthproxy.ProviderChatGPT, oauthproxy.ProviderChatGPTLegacy, oauthproxy.ProviderCodex,
		oauthproxy.ProviderGemini, "antigravity",
		oauthproxy.ProviderGrok, "xai",
		oauthproxy.ProviderCopilot,
		oauthproxy.ProviderQoder,
		oauthproxy.ProviderKimi,
		oauthproxy.ProviderKiro,
		oauthproxy.ProviderClaude:
		return true
	default:
		return false
	}
}

// derivedProviderName builds an implicit alias from the credential filename so a
// bare `ccl oauth gpt` still creates a distinct provider per account: e.g.
// `codex-alice@example.com.json` → `gpt-alice@example.com`; if the basename
// offers no usable fragment we fall back to `<target>-<basename>`.
func derivedProviderName(target, credentialPath string) string {
	base := filepath.Base(credentialPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	fragments := strings.SplitN(base, "-", 2)
	if len(fragments) == 2 && strings.TrimSpace(fragments[1]) != "" {
		return target + "-" + fragments[1]
	}
	if strings.TrimSpace(base) != "" && base != target {
		return target + "-" + base
	}
	return target
}

func init() {
	rootCmd.AddCommand(authCmd)
}
