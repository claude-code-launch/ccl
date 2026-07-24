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
}

var oauthLogin = oauthproxy.Login

var authCmd = newAuthCommand()

func newAuthCommand() *cobra.Command {
	opts := authOptions{}
	cmd := &cobra.Command{
		Use:   "auth <gpt|gemini|grok|copilot|kimi|claude> [alias]",
		Short: "Authenticate a subscription-backed provider",
		Long: `Authenticate a subscription-backed provider.

Without an alias, ccl derives a unique provider name from the credential
file (e.g. "gpt-alice@example.com") so multiple accounts never overwrite
each other. With an alias, that name is used as the provider key:

  ccl auth gpt
  ccl auth gpt work
  ccl auth gemini personal
  ccl auth grok
  ccl auth copilot
  ccl auth kimi
  ccl auth claude

"chatgpt" is still accepted as a legacy alias for "gpt".

Fast mode is not controlled here. Toggle it in Claude Code with /fast,
or on the Review & Apply page of ccl set (GPT/Copilot only).
`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuth(cmd.Context(), cmd.OutOrStdout(), args, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.noBrowser, "no-browser", false, "Print the OAuth URL instead of opening a browser")
	cmd.Flags().IntVar(&opts.callbackPort, "callback-port", 0, "Override the OAuth callback port (ChatGPT/Claude only)")
	return cmd
}

// supportsFastMode reports whether the provider's OAuth backend honours the
// Claude Code fastMode toggle (Codex Responses backends only).
func supportsFastMode(providerName string) bool {
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case oauthproxy.ProviderChatGPT, oauthproxy.ProviderChatGPTLegacy, oauthproxy.ProviderCopilot:
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
	protocolType := fixedOAuthProtocol(target)

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
	// GPT and Copilot share the Codex backend; only GPT migrates the
	// legacy "codex" OAuth provider alias when no explicit alias is used.
	if target == oauthproxy.ProviderChatGPT && alias == "" {
		if legacy, exists := cfg.Providers[oauthproxy.ProviderCodex]; exists && strings.EqualFold(strings.TrimSpace(legacy.OAuthProvider), oauthproxy.ProviderCodex) {
			if !targetExists {
				p = legacy
			}
			delete(cfg.Providers, oauthproxy.ProviderCodex)
		}
	}
	p.Name = providerName
	p.Type = protocolType
	p.Endpoint = "oauth://" + result.Backend
	p.APIKey = ""
	p.AnthropicAuth = ""
	p.OAuthProvider = target
	p.OAuthAccountCredential = credentialFile
	// FastMode is managed by Claude Code /fast or ccl set Review & Apply.
	// Re-auth preserves an existing pin; non-Codex backends never keep it.
	if !supportsFastMode(target) {
		p.FastMode = false
	}
	// Seed empty Claude slots with provider-preferred defaults (e.g. GPT/Grok/Gemini).
	// Existing user mappings are preserved; runtime drops any preferred
	// default that is missing from the live model catalog.
	provider.ApplyOAuthSlotDefaults(&p)
	cfg.Providers[providerName] = p
	cfg.ActiveProvider = providerName
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save OAuth provider: %w", err)
	}

	fmt.Fprintf(out, "Authenticated %s as provider %q and switched active provider.\n", target, providerName)
	fmt.Fprintf(out, "Credentials: %s\n", result.Path)
	fmt.Fprintf(out, "Protocol: %s (fixed for this OAuth backend)\n", provider.ProtocolLabel(protocolType))
	if supportsFastMode(target) {
		fmt.Fprintf(out, "Fast: %s (toggle with /fast or ccl set Review & Apply)\n", providerFastSummary(p))
	}
	return nil
}

// fixedOAuthProtocol is the protocol label persisted for each subscription backend.
// ChatGPT/Codex/Copilot → Responses; Gemini/Grok/Kimi → Chat; Claude → Anthropic.
func fixedOAuthProtocol(providerName string) string {
	switch providerName {
	case oauthproxy.ProviderGemini, oauthproxy.ProviderGrok, oauthproxy.ProviderKimi:
		return "openai"
	case oauthproxy.ProviderClaude:
		return "anthropic"
	default:
		return "openai_responses"
	}
}

// isReservedProviderName blocks aliases that would collide with canonical
// provider names or SDK backend keys.
func isReservedProviderName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case oauthproxy.ProviderChatGPT, oauthproxy.ProviderChatGPTLegacy, oauthproxy.ProviderCodex,
		oauthproxy.ProviderGemini, "antigravity",
		oauthproxy.ProviderGrok, "xai",
		oauthproxy.ProviderCopilot,
		oauthproxy.ProviderKimi,
		oauthproxy.ProviderClaude:
		return true
	default:
		return false
	}
}

// derivedProviderName builds an implicit alias from the credential filename so a
// bare `ccl auth gpt` still creates a distinct provider per account: e.g.
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
