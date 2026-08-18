package oauthproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const kimiDeviceCodeURL = "https://auth.kimi.com/api/oauth/device_authorization"

// kimiSlowDownStep is the RFC 8628 polling-interval increment applied after
// each slow_down response.
const kimiSlowDownStep = 5 * time.Second

var kimiPollInterval = 5 * time.Second

type kimiDeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type kimiDeviceToken struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	TokenType    string  `json:"token_type"`
	ExpiresIn    float64 `json:"expires_in"`
	Scope        string  `json:"scope"`
	Error        string  `json:"error"`
	ErrorDesc    string  `json:"error_description"`
}

// loginKimi runs the Kimi (Moonshot AI) OAuth device-code flow and persists a
// credential the kimiOAuthAuthorizer reads (access_token/refresh_token/device_id).
// It replaces CLIProxyAPI's kimi authenticator.
func loginKimi(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	client := &http.Client{Timeout: 30 * time.Second}
	deviceID := uuid.NewString()
	device, err := requestKimiDeviceCode(ctx, client, deviceID)
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
	token, err := pollKimiToken(ctx, client, deviceID, device)
	if err != nil {
		return LoginResult{}, err
	}
	metadata := map[string]any{
		"type":          ProviderKimi,
		"access_token":  strings.TrimSpace(token.AccessToken),
		"refresh_token": strings.TrimSpace(token.RefreshToken),
		"token_type":    strings.TrimSpace(token.TokenType),
		"scope":         strings.TrimSpace(token.Scope),
		"timestamp":     time.Now().UnixMilli(),
		"device_id":     deviceID,
	}
	if token.ExpiresIn > 0 {
		metadata["expired"] = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode Kimi credential: %w", err)
	}
	raw = append(raw, '\n')
	path := filepath.Join(authDir, ProviderKimi+"-"+credentialIdentity(metadata, raw)+".json")
	if err := writeCredentialAtomic(path, raw); err != nil {
		return LoginResult{}, err
	}
	fmt.Println("Kimi authentication successful")
	return LoginResult{Provider: ProviderKimi, Backend: ProviderKimi, Path: path}, nil
}

func requestKimiDeviceCode(ctx context.Context, client *http.Client, deviceID string) (kimiDeviceCode, error) {
	form := url.Values{"client_id": {kimiOAuthClientID}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, kimiDeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return kimiDeviceCode{}, fmt.Errorf("Kimi device code: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	for key, values := range kimiCommonHeaders(deviceID, kimiAuthDeviceModel()) {
		for _, value := range values {
			request.Header.Set(key, value)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return kimiDeviceCode{}, fmt.Errorf("Kimi device code: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, kimiMaxErrorBytes))
	if err != nil {
		return kimiDeviceCode{}, fmt.Errorf("Kimi device code: read response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return kimiDeviceCode{}, fmt.Errorf("Kimi device code: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var device kimiDeviceCode
	if err := json.Unmarshal(body, &device); err != nil {
		return kimiDeviceCode{}, fmt.Errorf("Kimi device code: decode response: %w", err)
	}
	if strings.TrimSpace(device.DeviceCode) == "" || strings.TrimSpace(device.UserCode) == "" {
		return kimiDeviceCode{}, fmt.Errorf("Kimi device code: response missing device_code or user_code")
	}
	return device, nil
}

func pollKimiToken(ctx context.Context, client *http.Client, deviceID string, device kimiDeviceCode) (kimiDeviceToken, error) {
	interval := time.Duration(device.Interval) * time.Second
	if interval < kimiPollInterval {
		interval = kimiPollInterval
	}
	deadline := time.Now().Add(15 * time.Minute)
	if device.ExpiresIn > 0 {
		if codeDeadline := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second); codeDeadline.Before(deadline) {
			deadline = codeDeadline
		}
	}
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	form := url.Values{
		"client_id":   {kimiOAuthClientID},
		"device_code": {device.DeviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	for {
		timer := time.NewTimer(interval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			return kimiDeviceToken{}, fmt.Errorf("Kimi device authorization: %w", pollCtx.Err())
		case <-timer.C:
		}
		token, next, err := exchangeKimiDeviceToken(pollCtx, client, deviceID, form, interval)
		if err != nil {
			return kimiDeviceToken{}, err
		}
		if next > 0 {
			interval = next
			continue
		}
		return token, nil
	}
}

// exchangeKimiDeviceToken returns the next polling interval (zero once the
// token is issued). RFC 8628 requires a slow_down response to grow the
// interval rather than re-poll at the same pace.
func exchangeKimiDeviceToken(ctx context.Context, client *http.Client, deviceID string, form url.Values, interval time.Duration) (token kimiDeviceToken, nextPoll time.Duration, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, kimiTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return kimiDeviceToken{}, 0, fmt.Errorf("Kimi device token: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	for key, values := range kimiCommonHeaders(deviceID, kimiAuthDeviceModel()) {
		for _, value := range values {
			request.Header.Set(key, value)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return kimiDeviceToken{}, 0, fmt.Errorf("Kimi device token: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, kimiMaxErrorBytes))
	if err != nil {
		return kimiDeviceToken{}, 0, fmt.Errorf("Kimi device token: read response: %w", err)
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return kimiDeviceToken{}, 0, fmt.Errorf("Kimi device token: decode response: %w", err)
	}
	switch token.Error {
	case "", "authorization_pending":
		if strings.TrimSpace(token.AccessToken) == "" {
			return kimiDeviceToken{}, interval, nil
		}
		return token, 0, nil
	case "slow_down":
		return kimiDeviceToken{}, interval + kimiSlowDownStep, nil
	case "expired_token":
		return kimiDeviceToken{}, 0, fmt.Errorf("Kimi device code expired")
	case "access_denied":
		return kimiDeviceToken{}, 0, fmt.Errorf("Kimi device authorization denied")
	default:
		desc := strings.TrimSpace(token.ErrorDesc)
		if desc == "" {
			desc = token.Error
		}
		return kimiDeviceToken{}, 0, fmt.Errorf("Kimi device token error: %s", desc)
	}
}
