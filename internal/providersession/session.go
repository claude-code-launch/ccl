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
	if strings.TrimSpace(resolved.OAuthProvider) != "" {
		// Normalize legacy persisted compatibility types to the protocol exposed by
		// the current embedded runtime. Grok moved from the old Chat classification
		// to the Responses data plane; probes must therefore use /v1/responses.
		if runtimeType, ok := provider.OAuthRuntimeType(resolved.OAuthProvider); ok {
			resolved.Type = runtimeType
		}
	}
	session := &Session{
		Provider: resolved,
		BaseURL:  resolved.Endpoint,
	}
	session.UseProxy = provider.IsOpenAICompatibleType(resolved.Type) || provider.IsModelsDevType(resolved.Type) ||
		strings.TrimSpace(resolved.OAuthProvider) != ""
	if !session.UseProxy {
		return session, nil
	}

	// The provider has not declared its own model pool: discover it from the
	// gateway's OpenAI-shaped model list before the runtime starts.
	if resolved.OAuthProvider == "" && strings.TrimSpace(resolved.Model) == "" {
		models, err := protocol.GetOpenAIModels(resolved.Endpoint, resolved.APIKey)
		if err != nil {
			return nil, fmt.Errorf("discover OpenAI models before starting the provider runtime: %w", err)
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
	if runtimeModels := runtime.Models(); len(runtimeModels) > 0 {
		// A successful OAuth catalog fetch is authoritative for the account. This
		// refreshes stale persisted pools (for example Grok 4.5 -> 4.6) before the
		// launcher validates slot mappings. A compatibility fallback never replaces
		// a non-empty saved pool because it is only a guess.
		if strings.TrimSpace(resolved.Model) == "" || (strings.TrimSpace(resolved.OAuthProvider) != "" && !runtime.ModelCatalogIsFallback()) {
			resolved.Model = strings.Join(runtimeModels, ",")
		}
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
