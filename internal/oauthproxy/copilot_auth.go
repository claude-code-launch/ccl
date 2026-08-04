package oauthproxy

import (
	"bytes"
	"context"
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
	// This is GitHub Copilot's public OAuth application client ID. It is not a
	// secret and is used by other native Copilot integrations.
	copilotOAuthClientID = "Iv1.b507a08c87ecfe98"
	copilotOAuthScope    = "read:user"
	copilotOAuthTimeout  = 10 * time.Minute
	copilotHTTPTimeout   = 30 * time.Second
)

var (
	copilotGitHubBaseURL    = "https://github.com"
	copilotGitHubAPIBaseURL = "https://api.github.com"
	copilotOAuthPollFloor   = time.Second
	copilotBrowserOpener    = openKiroBrowser
)

type copilotDeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

type copilotOAuthToken struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type copilotGitHubUser struct {
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
	ID    int64  `json:"id"`
}

func loginCopilot(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	client := &http.Client{Timeout: copilotHTTPTimeout}
	var device copilotDeviceAuthorization
	if err := copilotOAuthForm(ctx, client, copilotGitHubBaseURL+"/login/device/code", url.Values{
		"client_id": {copilotOAuthClientID},
		"scope":     {copilotOAuthScope},
	}, &device); err != nil {
		return LoginResult{}, fmt.Errorf("start GitHub Copilot device authorization: %w", err)
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.VerificationURI == "" {
		return LoginResult{}, fmt.Errorf("start GitHub Copilot device authorization: incomplete response")
	}

	verificationURL := device.VerificationURIComplete
	if verificationURL == "" {
		verificationURL = device.VerificationURI
	}
	fmt.Printf("Open %s and enter code %s\n", device.VerificationURI, device.UserCode)
	if !opts.NoBrowser {
		_ = copilotBrowserOpener(verificationURL)
	}

	interval := time.Duration(device.Interval) * time.Second
	if interval < copilotOAuthPollFloor {
		interval = copilotOAuthPollFloor
	}
	expiresIn := time.Duration(device.ExpiresIn) * time.Second
	if expiresIn <= 0 || expiresIn > copilotOAuthTimeout {
		expiresIn = copilotOAuthTimeout
	}
	pollCtx, cancel := context.WithTimeout(ctx, expiresIn)
	defer cancel()

	var token copilotOAuthToken
	for {
		timer := time.NewTimer(interval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			return LoginResult{}, fmt.Errorf("GitHub Copilot device authorization: %w", pollCtx.Err())
		case <-timer.C:
		}

		token = copilotOAuthToken{}
		err := copilotOAuthForm(pollCtx, client, copilotGitHubBaseURL+"/login/oauth/access_token", url.Values{
			"client_id":   {copilotOAuthClientID},
			"device_code": {device.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}, &token)
		if err != nil {
			return LoginResult{}, fmt.Errorf("poll GitHub Copilot device authorization: %w", err)
		}
		if token.AccessToken != "" {
			break
		}
		switch token.Error {
		case "authorization_pending", "":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "access_denied", "expired_token":
			return LoginResult{}, fmt.Errorf("GitHub Copilot device authorization: %s", copilotOAuthError(token))
		default:
			return LoginResult{}, fmt.Errorf("GitHub Copilot device authorization: %s", copilotOAuthError(token))
		}
	}

	user, err := fetchCopilotGitHubUser(ctx, client, token.AccessToken)
	if err != nil {
		return LoginResult{}, err
	}
	if err := validateCopilotEntitlement(ctx, client, token.AccessToken); err != nil {
		return LoginResult{}, err
	}
	metadata := map[string]any{
		"type":         ProviderCopilot,
		"github_token": token.AccessToken,
		"token_type":   token.TokenType,
		"scope":        token.Scope,
		"login":        user.Login,
		"github_id":    user.ID,
	}
	if strings.TrimSpace(user.Name) != "" {
		metadata["name"] = strings.TrimSpace(user.Name)
	}
	if strings.TrimSpace(user.Email) != "" {
		metadata["email"] = strings.TrimSpace(user.Email)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode GitHub Copilot credential: %w", err)
	}
	raw = append(raw, '\n')
	identity := credentialIdentity(metadata, raw)
	path := filepath.Join(authDir, ProviderCopilot+"-"+identity+".json")
	if err := writeCredentialAtomic(path, raw); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Provider: ProviderCopilot, Backend: ProviderCopilot, Path: path}, nil
}

func validateCopilotEntitlement(ctx context.Context, client *http.Client, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(copilotAPIBaseURL, "/")+"/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	setCopilotClientHeaders(req.Header)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("validate GitHub Copilot subscription: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, copilotMaxErrorBytes))
	if err != nil {
		return fmt.Errorf("validate GitHub Copilot subscription: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub login succeeded but Copilot is unavailable: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var catalog copilotModelsResponse
	if err := json.Unmarshal(body, &catalog); err != nil {
		return fmt.Errorf("validate GitHub Copilot subscription: decode models: %w", err)
	}
	if len(filterCopilotModels(catalog.Data)) == 0 {
		return fmt.Errorf("GitHub login succeeded but Copilot returned no selectable inference models")
	}
	return nil
}

func copilotOAuthForm(ctx context.Context, client *http.Client, endpoint string, values url.Values, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "ccl")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func fetchCopilotGitHubUser(ctx context.Context, client *http.Client, token string) (copilotGitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(copilotGitHubAPIBaseURL, "/")+"/user", nil)
	if err != nil {
		return copilotGitHubUser{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ccl")
	resp, err := client.Do(req)
	if err != nil {
		return copilotGitHubUser{}, fmt.Errorf("read GitHub Copilot account: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return copilotGitHubUser{}, fmt.Errorf("read GitHub Copilot account: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return copilotGitHubUser{}, fmt.Errorf("read GitHub Copilot account: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var user copilotGitHubUser
	if err := json.Unmarshal(body, &user); err != nil {
		return copilotGitHubUser{}, fmt.Errorf("decode GitHub Copilot account: %w", err)
	}
	if strings.TrimSpace(user.Login) == "" {
		return copilotGitHubUser{}, fmt.Errorf("read GitHub Copilot account: missing login")
	}
	return user, nil
}

func copilotOAuthError(token copilotOAuthToken) string {
	if description := strings.TrimSpace(token.ErrorDescription); description != "" {
		return description
	}
	if code := strings.TrimSpace(token.Error); code != "" {
		return code
	}
	return "GitHub returned neither a token nor an error"
}
