package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

type authOptions struct {
	noBrowser    bool
	callbackPort int
	protocol     string
}

var oauthLogin = oauthproxy.Login

var authCmd = newAuthCommand()

func newAuthCommand() *cobra.Command {
	opts := authOptions{}
	cmd := &cobra.Command{
		Use:   "auth <chatgpt|gemini>",
		Short: "Authenticate a subscription-backed provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuth(cmd.Context(), cmd.OutOrStdout(), args[0], opts)
		},
	}
	cmd.Flags().BoolVar(&opts.noBrowser, "no-browser", false, "Print the OAuth URL instead of opening a browser")
	cmd.Flags().IntVar(&opts.callbackPort, "callback-port", 0, "Override the OAuth callback port")
	cmd.Flags().StringVar(&opts.protocol, "protocol", "", "Upstream protocol: chat or responses")
	return cmd
}

func runAuth(ctx context.Context, out io.Writer, providerName string, opts authOptions) error {
	target, err := oauthproxy.ValidateLoginProvider(providerName)
	if err != nil {
		return err
	}
	protocolType, err := authProtocolType(target, opts.protocol)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Authenticating %s...\n", target)
	result, err := oauthLogin(ctx, target, oauthproxy.LoginOptions{
		NoBrowser:    opts.noBrowser,
		CallbackPort: opts.callbackPort,
	})
	if err != nil {
		return fmt.Errorf("authenticate %s: %w", target, err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}
	p, targetExists := cfg.Providers[target]
	if target == oauthproxy.ProviderChatGPT {
		if legacy, exists := cfg.Providers[oauthproxy.ProviderCodex]; exists && strings.EqualFold(strings.TrimSpace(legacy.OAuthProvider), oauthproxy.ProviderCodex) {
			if !targetExists {
				p = legacy
			}
			delete(cfg.Providers, oauthproxy.ProviderCodex)
		}
	}
	p.Name = target
	p.Type = protocolType
	p.Endpoint = "oauth://" + result.Backend
	p.APIKey = ""
	p.AnthropicAuth = ""
	p.OAuthProvider = target
	cfg.Providers[target] = p
	cfg.ActiveProvider = target
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save OAuth provider: %w", err)
	}

	fmt.Fprintf(out, "Authenticated %s and switched active provider.\n", target)
	fmt.Fprintf(out, "Credentials: %s\n", result.Path)
	fmt.Fprintf(out, "Protocol: %s\n", provider.ProtocolLabel(protocolType))
	return nil
}

func authProtocolType(providerName, requested string) (string, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		if providerName == oauthproxy.ProviderGemini {
			return "openai", nil
		}
		return "openai_responses", nil
	}
	switch requested {
	case "chat", "openai", "openai(chat)":
		return "openai", nil
	case "responses", "response", "agent", "openai(responses)", "openai(agent)":
		return "openai_responses", nil
	default:
		return "", fmt.Errorf("unsupported protocol %q (use chat or responses)", requested)
	}
}

func init() {
	rootCmd.AddCommand(authCmd)
}
