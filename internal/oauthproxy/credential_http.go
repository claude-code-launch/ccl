package oauthproxy

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

var errCredentialRedirect = errors.New("refusing to redirect provider credentials to a different origin")
var errCredentialRedirectLimit = errors.New("provider redirect limit exceeded")

// Go only strips a small set of sensitive headers on redirects. Refresh
// tokens in custom headers or a replayable POST body need a stricter policy.
func checkCredentialRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errCredentialRedirectLimit
	}
	if len(via) == 0 {
		return nil
	}
	first := via[0].URL
	if request.URL.User != nil || !strings.EqualFold(first.Scheme, request.URL.Scheme) ||
		!strings.EqualFold(first.Hostname(), request.URL.Hostname()) ||
		credentialURLPort(first) != credentialURLPort(request.URL) {
		return errCredentialRedirect
	}
	return nil
}

func credentialURLPort(target *url.URL) string {
	if port := target.Port(); port != "" {
		return port
	}
	if strings.EqualFold(target.Scheme, "https") {
		return "443"
	}
	return "80"
}

func doCredentialRequest(client *http.Client, request *http.Request) (*http.Response, error) {
	response, err := client.Do(request)
	if errors.Is(err, errCredentialRedirect) {
		// net/http wraps redirect errors with the Location URL, which may itself
		// contain a token. Preserve the refusal without exposing that URL.
		return nil, errCredentialRedirect
	}
	if errors.Is(err, errCredentialRedirectLimit) {
		return nil, errCredentialRedirectLimit
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// Device-login polling puts PKCE verifiers and states in the query.
		// Preserve network error types, but keep those values out of logs/UI.
		safe := *request.URL
		safe.User, safe.RawQuery, safe.Fragment = nil, "", ""
		return response, &url.Error{Op: urlErr.Op, URL: safe.String(), Err: urlErr.Err}
	}
	return response, err
}
