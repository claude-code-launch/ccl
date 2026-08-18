package oauthproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const (
	xaiOIDCDiscoveryURL  = "https://auth.x.ai/.well-known/openid-configuration"
	xaiOAuthScope        = "openid profile email offline_access grok-cli:access api:access"
	xaiDefaultAPIBaseURL = "https://api.x.ai/v1"
)

var (
	xaiDevicePollInterval = 5 * time.Second
	xaiMaxPollDuration    = 30 * time.Minute
)

type xaiDiscovery struct {
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

type xaiDeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type xaiDeviceToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// loginXai runs the xAI/Grok OAuth device-code flow (RFC 8628) and persists a
// credential the xaiOAuthAuthorizer reads (access_token/refresh_token/
// token_endpoint). It replaces CLIProxyAPI's xai authenticator.
func loginXai(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	client := &http.Client{Timeout: 30 * time.Second}
	discovery, err := fetchXaiDiscovery(ctx, client)
	if err != nil {
		return LoginResult{}, err
	}
	device, err := requestXaiDeviceCode(ctx, client, discovery.DeviceAuthorizationEndpoint)
	if err != nil {
		return LoginResult{}, err
	}
	verificationURL := strings.TrimSpace(device.VerificationURIComplete)
	if verificationURL == "" {
		verificationURL = strings.TrimSpace(device.VerificationURI)
	}
	fmt.Printf("Open %s and enter code %s\n", device.VerificationURI, device.UserCode)
	if !opts.NoBrowser {
		_ = openKiroBrowser(verificationURL)
	}
	fmt.Println("Waiting for authorization...")
	token, err := pollXaiToken(ctx, client, discovery.TokenEndpoint, device)
	if err != nil {
		return LoginResult{}, err
	}
	email, subject := xaiJWTIdentity(token.IDToken)
	metadata := map[string]any{
		"type":           backendXAI,
		"access_token":   strings.TrimSpace(token.AccessToken),
		"refresh_token":  strings.TrimSpace(token.RefreshToken),
		"id_token":       strings.TrimSpace(token.IDToken),
		"token_type":     strings.TrimSpace(token.TokenType),
		"expires_in":     token.ExpiresIn,
		"last_refresh":   time.Now().UTC().Format(time.RFC3339),
		"base_url":       xaiDefaultAPIBaseURL,
		"token_endpoint": discovery.TokenEndpoint,
		"auth_kind":      "oauth",
	}
	if email != "" {
		metadata["email"] = email
	}
	if subject != "" {
		metadata["sub"] = subject
	}
	if token.ExpiresIn > 0 {
		metadata["expired"] = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode xAI credential: %w", err)
	}
	raw = append(raw, '\n')
	path := filepath.Join(authDir, backendXAI+"-"+credentialIdentity(metadata, raw)+".json")
	if err := writeCredentialAtomic(path, raw); err != nil {
		return LoginResult{}, err
	}
	fmt.Println("xAI authentication successful")
	return LoginResult{Provider: ProviderGrok, Backend: backendXAI, Path: path}, nil
}

func fetchXaiDiscovery(ctx context.Context, client *http.Client) (xaiDiscovery, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, xaiOIDCDiscoveryURL, nil)
	if err != nil {
		return xaiDiscovery{}, fmt.Errorf("xAI OIDC discovery: create request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return xaiDiscovery{}, fmt.Errorf("xAI OIDC discovery: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, xaiMaxErrorBytes))
	if err != nil {
		return xaiDiscovery{}, fmt.Errorf("xAI OIDC discovery: read response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return xaiDiscovery{}, fmt.Errorf("xAI OIDC discovery: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var discovery xaiDiscovery
	if err := json.Unmarshal(body, &discovery); err != nil {
		return xaiDiscovery{}, fmt.Errorf("xAI OIDC discovery: decode response: %w", err)
	}
	if strings.TrimSpace(discovery.DeviceAuthorizationEndpoint) == "" || strings.TrimSpace(discovery.TokenEndpoint) == "" {
		return xaiDiscovery{}, fmt.Errorf("xAI OIDC discovery: missing device_authorization_endpoint or token_endpoint")
	}
	return discovery, nil
}

func requestXaiDeviceCode(ctx context.Context, client *http.Client, endpoint string) (xaiDeviceCode, error) {
	form := url.Values{"client_id": {xaiOAuthClientID}, "scope": {xaiOAuthScope}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return xaiDeviceCode{}, fmt.Errorf("xAI device code: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return xaiDeviceCode{}, fmt.Errorf("xAI device code: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, xaiMaxErrorBytes))
	if err != nil {
		return xaiDeviceCode{}, fmt.Errorf("xAI device code: read response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return xaiDeviceCode{}, fmt.Errorf("xAI device code: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var device xaiDeviceCode
	if err := json.Unmarshal(body, &device); err != nil {
		return xaiDeviceCode{}, fmt.Errorf("xAI device code: decode response: %w", err)
	}
	if strings.TrimSpace(device.DeviceCode) == "" || strings.TrimSpace(device.UserCode) == "" {
		return xaiDeviceCode{}, fmt.Errorf("xAI device code: response missing device_code or user_code")
	}
	if strings.TrimSpace(device.VerificationURI) == "" && strings.TrimSpace(device.VerificationURIComplete) == "" {
		return xaiDeviceCode{}, fmt.Errorf("xAI device code: response missing verification URI")
	}
	return device, nil
}

func pollXaiToken(ctx context.Context, client *http.Client, tokenEndpoint string, device xaiDeviceCode) (xaiDeviceToken, error) {
	interval := time.Duration(device.Interval) * time.Second
	if interval < xaiDevicePollInterval {
		interval = xaiDevicePollInterval
	}
	deadline := time.Now().Add(xaiMaxPollDuration)
	if device.ExpiresIn > 0 {
		if codeDeadline := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second); codeDeadline.Before(deadline) {
			deadline = codeDeadline
		}
	}
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {device.DeviceCode},
		"client_id":   {xaiOAuthClientID},
	}
	for {
		timer := time.NewTimer(interval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			return xaiDeviceToken{}, fmt.Errorf("xAI device authorization: %w", pollCtx.Err())
		case <-timer.C:
		}
		token, nextInterval, err := exchangeXaiDeviceToken(pollCtx, client, tokenEndpoint, form)
		if err != nil {
			return xaiDeviceToken{}, err
		}
		if token.AccessToken != "" {
			return token, nil
		}
		interval = nextInterval
	}
}

// exchangeXaiDeviceToken returns (token, nextInterval, error). A zero-value token
// with nil error means "still pending"; the caller re-polls with nextInterval.
func exchangeXaiDeviceToken(ctx context.Context, client *http.Client, tokenEndpoint string, form url.Values) (xaiDeviceToken, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return xaiDeviceToken{}, 0, fmt.Errorf("xAI device token: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return xaiDeviceToken{}, 0, fmt.Errorf("xAI device token: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, xaiMaxErrorBytes))
	if err != nil {
		return xaiDeviceToken{}, 0, fmt.Errorf("xAI device token: read response: %w", err)
	}
	var token xaiDeviceToken
	if err := json.Unmarshal(body, &token); err != nil {
		return xaiDeviceToken{}, 0, fmt.Errorf("xAI device token: decode response: %w", err)
	}
	switch token.Error {
	case "", "authorization_pending":
		if token.AccessToken == "" {
			return xaiDeviceToken{}, xaiDevicePollInterval, nil
		}
		return token, 0, nil
	case "slow_down":
		return xaiDeviceToken{}, xaiDevicePollInterval * 2, nil
	case "expired_token":
		return xaiDeviceToken{}, 0, fmt.Errorf("xAI device code expired")
	case "access_denied":
		return xaiDeviceToken{}, 0, fmt.Errorf("xAI device authorization denied")
	default:
		desc := strings.TrimSpace(token.ErrorDesc)
		if desc == "" {
			desc = token.Error
		}
		return xaiDeviceToken{}, 0, fmt.Errorf("xAI device token error: %s", desc)
	}
}

func xaiJWTIdentity(token string) (email, subject string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Email string `json:"email"`
		Sub   string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	return strings.TrimSpace(claims.Email), strings.TrimSpace(claims.Sub)
}
