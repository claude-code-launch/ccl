package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func prepareProviderRuntime(p provider.Provider) (provider.Provider, *oauthproxy.Runtime, func(), error) {
	nop := func() {}
	// Anthropic OAuth (claude) still uses the embedded CPA runtime so Claude
	// Code talks to a local /v1/messages endpoint with a session token.
	if p.OAuthProvider != "" {
		runtime, err := oauthproxy.StartProvider(context.Background(), oauthproxy.StartOptions{
			Protocol:                oauthRuntimeProtocol(p),
			Endpoint:                p.Endpoint,
			APIKey:                  p.APIKey,
			ModelSpec:               provider.RuntimeModelSpec(p),
			OAuthProvider:           p.OAuthProvider,
			OAuthAccountCredential:  p.OAuthAccountCredential,
			OAuthAccountCredentials: p.OAuthAccountCredentials,
			OAuthCredentialResolver: oauthGroupCredentialResolver(p.AuthGroup),
			MaxOutputTokens:         oauthMaxOutputTokens(p),
		})
		if err != nil {
			return provider.Provider{}, nil, nop, fmt.Errorf("start embedded provider runtime: %w", err)
		}
		p.Endpoint = runtime.Endpoint()
		p.APIKey = runtime.APIKey()
		return p, runtime, runtime.Stop, nil
	}

	if !provider.IsOpenAICompatibleType(p.Type) {
		return p, nil, nop, nil
	}

	if strings.TrimSpace(p.Model) == "" {
		models, err := protocol.GetOpenAIModels(p.Endpoint, p.APIKey)
		if err != nil {
			return provider.Provider{}, nil, nop, fmt.Errorf("discover OpenAI models before starting CLIProxyAPI: %w", err)
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
		return provider.Provider{}, nil, nop, fmt.Errorf("start embedded provider runtime: %w", err)
	}
	p.Endpoint = runtime.Endpoint()
	p.APIKey = runtime.APIKey()
	return p, runtime, runtime.Stop, nil
}

func oauthGroupCredentialResolver(groupName string) func() ([]string, error) {
	groupName = strings.TrimSpace(groupName)
	if groupName == "" {
		return nil
	}
	return func() ([]string, error) {
		cfg, err := config.Load()
		if err != nil {
			return nil, err
		}
		group, ok := cfg.AuthGroups[groupName]
		if !ok {
			return []string{}, nil
		}
		return append([]string{}, group.Credentials...), nil
	}
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
