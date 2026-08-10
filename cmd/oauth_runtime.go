package cmd

import (
	"context"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/providersession"
)

func prepareProviderRuntime(p provider.Provider) (provider.Provider, *oauthproxy.Runtime, func(), error) {
	session, err := providersession.Prepare(context.Background(), p)
	if err != nil {
		return provider.Provider{}, nil, func() {}, err
	}
	return session.Provider, session.Runtime, session.Close, nil
}
