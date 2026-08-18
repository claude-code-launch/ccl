// Package providersession prepares the one provider runtime shape shared by
// interactive Claude sessions and management commands.
package providersession

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// Session is the resolved provider view for one operation. Provider is always
// a copy, so preparing a session never mutates persisted configuration.
type Session struct {
	Provider provider.Provider
	Runtime  *oauthproxy.Runtime
	BaseURL  string
	UseProxy bool

	closeOnce sync.Once
}

// Prepare discovers any missing API-key gateway models, starts the shared
// Anthropic Messages adapter when required, and returns the resolved endpoint.
func Prepare(ctx context.Context, configured provider.Provider) (*Session, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resolved := configured
	session := &Session{
		Provider: resolved,
		BaseURL:  resolved.Endpoint,
		UseProxy: provider.IsOpenAICompatibleType(resolved.Type) || provider.IsModelsDevType(resolved.Type) || strings.TrimSpace(resolved.OAuthProvider) != "",
	}
	if !session.UseProxy {
		return session, nil
	}

	if resolved.OAuthProvider == "" && strings.TrimSpace(resolved.Model) == "" {
		models, err := protocol.GetOpenAIModels(resolved.Endpoint, resolved.APIKey)
		if err != nil {
			return nil, fmt.Errorf("discover OpenAI models before starting CLIProxyAPI: %w", err)
		}
		resolved.Model = models
	}

	runtime, err := oauthproxy.StartProvider(ctx, oauthproxy.StartOptions{
		Protocol:               upstreamProtocol(resolved),
		Endpoint:               resolved.Endpoint,
		APIKey:                 resolved.APIKey,
		ModelSpec:              provider.RuntimeModelSpec(resolved),
		OAuthProvider:          resolved.OAuthProvider,
		OAuthAccountCredential: resolved.OAuthAccountCredential,
		ModelProtocols:         resolved.ModelProtocols,
	})
	if err != nil {
		return nil, fmt.Errorf("start embedded provider runtime: %w", err)
	}

	resolved.Endpoint = runtime.Endpoint()
	resolved.APIKey = runtime.APIKey()
	if strings.TrimSpace(resolved.Model) == "" && len(runtime.Models()) > 0 {
		resolved.Model = strings.Join(runtime.Models(), ",")
	}
	session.Provider = resolved
	session.Runtime = runtime
	session.BaseURL = runtime.ClaudeBaseURL()
	return session, nil
}

func upstreamProtocol(p provider.Provider) oauthproxy.UpstreamProtocol {
	if provider.IsOpenAIResponsesType(p.Type) {
		return oauthproxy.ProtocolOpenAIResponses
	}
	// OAuth backends select their executor by OAuthProvider; this value only
	// matters for manual API-key gateways.
	return oauthproxy.ProtocolOpenAIChat
}

// Close releases the embedded runtime. It is safe to call more than once.
func (s *Session) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.Runtime != nil {
			s.Runtime.Stop()
		}
	})
}
