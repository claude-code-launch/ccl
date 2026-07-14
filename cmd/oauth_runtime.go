package cmd

import (
	"context"
	"fmt"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func prepareProviderRuntime(p provider.Provider) (provider.Provider, func(), error) {
	var runtime *oauthproxy.Runtime
	var err error
	if p.OAuthProvider != "" {
		if !provider.IsOpenAICompatibleType(p.Type) {
			return provider.Provider{}, nil, fmt.Errorf(
				"OAuth provider %q requires the OpenAI Chat or Responses protocol",
				p.OAuthProvider,
			)
		}
		runtime, err = oauthproxy.Start(context.Background(), p.OAuthProvider)
	} else if provider.IsOpenAIResponsesType(p.Type) {
		runtime, err = oauthproxy.StartCodexAPI(context.Background(), p.Endpoint, p.APIKey, p.Model)
	} else {
		return p, func() {}, nil
	}
	if err != nil {
		return provider.Provider{}, nil, fmt.Errorf("start embedded CLIProxyAPI: %w", err)
	}
	p.Endpoint = runtime.Endpoint()
	p.APIKey = runtime.APIKey()
	return p, runtime.Stop, nil
}
