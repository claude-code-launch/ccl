package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func prepareProviderRuntime(p provider.Provider) (provider.Provider, func(), error) {
	if !provider.IsOpenAICompatibleType(p.Type) {
		if p.OAuthProvider != "" {
			return provider.Provider{}, nil, fmt.Errorf(
				"OAuth provider %q requires the OpenAI Chat or Responses protocol",
				p.OAuthProvider,
			)
		}
		return p, func() {}, nil
	}

	if p.OAuthProvider == "" && strings.TrimSpace(p.Model) == "" {
		models, err := protocol.GetOpenAIModels(p.Endpoint, p.APIKey)
		if err != nil {
			return provider.Provider{}, nil, fmt.Errorf("discover OpenAI models before starting CLIProxyAPI: %w", err)
		}
		p.Model = models
	}
	upstreamProtocol := oauthproxy.ProtocolOpenAIChat
	if provider.IsOpenAIResponsesType(p.Type) {
		upstreamProtocol = oauthproxy.ProtocolOpenAIResponses
	}
	maxOut := 0
	if provider.IsOpenAIResponsesType(p.Type) {
		// Import cycle-safe: resolve via env directly rather than claude package.
		if p.Env != nil {
			if v := strings.TrimSpace(p.Env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"]); v != "" {
				var n int
				if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
					maxOut = n
				}
			}
		}
		if maxOut == 0 {
			maxOut = 32000
		}
	}
	runtime, err := oauthproxy.StartProvider(context.Background(), oauthproxy.StartOptions{
		Protocol:        upstreamProtocol,
		Endpoint:        p.Endpoint,
		APIKey:          p.APIKey,
		ModelSpec:       provider.RuntimeModelSpec(p),
		OAuthProvider:   p.OAuthProvider,
		MaxOutputTokens: maxOut,
	})
	if err != nil {
		return provider.Provider{}, nil, fmt.Errorf("start embedded CLIProxyAPI: %w", err)
	}
	p.Endpoint = runtime.Endpoint()
	p.APIKey = runtime.APIKey()
	return p, runtime.Stop, nil
}
