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
	// Anthropic OAuth (claude) still uses the embedded CPA runtime so Claude
	// Code talks to a local /v1/messages endpoint with a session token.
	if p.OAuthProvider != "" {
		runtime, err := oauthproxy.StartProvider(context.Background(), oauthproxy.StartOptions{
			Protocol:               oauthRuntimeProtocol(p),
			Endpoint:               p.Endpoint,
			APIKey:                 p.APIKey,
			ModelSpec:              provider.RuntimeModelSpec(p),
			OAuthProvider:          p.OAuthProvider,
			OAuthAccountCredential: p.OAuthAccountCredential,
			MaxOutputTokens:        oauthMaxOutputTokens(p),
		})
		if err != nil {
			return provider.Provider{}, nil, fmt.Errorf("start embedded CLIProxyAPI: %w", err)
		}
		p.Endpoint = runtime.Endpoint()
		p.APIKey = runtime.APIKey()
		return p, runtime.Stop, nil
	}

	if !provider.IsOpenAICompatibleType(p.Type) {
		return p, func() {}, nil
	}

	if strings.TrimSpace(p.Model) == "" {
		models, err := protocol.GetOpenAIModels(p.Endpoint, p.APIKey)
		if err != nil {
			return provider.Provider{}, nil, fmt.Errorf("discover OpenAI models before starting CLIProxyAPI: %w", err)
		}
		p.Model = models
	}
	runtime, err := oauthproxy.StartProvider(context.Background(), oauthproxy.StartOptions{
		Protocol:        oauthRuntimeProtocol(p),
		Endpoint:        p.Endpoint,
		APIKey:          p.APIKey,
		ModelSpec:       provider.RuntimeModelSpec(p),
		MaxOutputTokens: oauthMaxOutputTokens(p),
	})
	if err != nil {
		return provider.Provider{}, nil, fmt.Errorf("start embedded CLIProxyAPI: %w", err)
	}
	p.Endpoint = runtime.Endpoint()
	p.APIKey = runtime.APIKey()
	return p, runtime.Stop, nil
}

func oauthRuntimeProtocol(p provider.Provider) oauthproxy.UpstreamProtocol {
	if provider.IsOpenAIResponsesType(p.Type) {
		return oauthproxy.ProtocolOpenAIResponses
	}
	// Claude OAuth and OpenAI Chat OAuth both go through CPA's local
	// /v1/messages surface; Claude executor is selected by OAuth backend.
	return oauthproxy.ProtocolOpenAIChat
}

func oauthMaxOutputTokens(p provider.Provider) int {
	if !provider.IsOpenAIResponsesType(p.Type) {
		return 0
	}
	// Import cycle-safe: resolve via env directly rather than claude package.
	if p.Env != nil {
		if v := strings.TrimSpace(p.Env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"]); v != "" {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
				return n
			}
		}
	}
	return 32000
}
