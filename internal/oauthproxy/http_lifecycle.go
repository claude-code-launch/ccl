package oauthproxy

import (
	"net"
	"net/http"
	"time"
)

func newUpstreamTransport(headerTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 100,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second, ResponseHeaderTimeout: headerTimeout,
	}
}

func closeIdleClients(clients ...*http.Client) func() {
	return func() {
		for _, client := range clients {
			if client != nil {
				client.CloseIdleConnections()
			}
		}
	}
}

func authorizerHTTPClient(authorizer any) *http.Client {
	switch a := authorizer.(type) {
	case *codexOAuthAuthorizer:
		return a.client
	case *xaiOAuthAuthorizer:
		return a.client
	case *kimiOAuthAuthorizer:
		return a.client
	case *antigravityOAuthAuthorizer:
		return a.client
	case *autoClawOAuthAuthorizer:
		return a.client
	default:
		return nil
	}
}
