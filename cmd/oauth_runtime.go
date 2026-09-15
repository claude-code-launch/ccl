package cmd

import (
	"context"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/providersession"
)

// prepareProviderRuntime starts the loopback runtime a subscription needs and
// returns it alongside the session copy of the provider. The context bounds the
// credential refresh and catalog discovery the runtime performs on the way up,
// so a slow upstream can be abandoned instead of holding the caller.
func prepareProviderRuntime(ctx context.Context, p provider.Provider) (provider.Provider, *oauthproxy.Runtime, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	session, err := providersession.Prepare(ctx, p)
	if err != nil {
		return provider.Provider{}, nil, func() {}, err
	}
	return session.Provider, session.Runtime, session.Close, nil
}
