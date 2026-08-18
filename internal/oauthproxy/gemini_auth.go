package oauthproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const (
	geminiOAuthAuthEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	geminiOAuthUserInfoURL   = "https://www.googleapis.com/oauth2/v2/userinfo?alt=json"
	geminiOAuthCallbackPath  = "/oauth-callback"
	geminiOAuthCallbackPort  = 51121
	geminiOAuthOnboardNodeUA = "google-api-nodejs-client/10.3.0"
	geminiOAuthGoogAPIUA     = "gl-node/22.21.1"
)

var geminiOAuthScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

type geminiOAuthToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

type geminiOAuthCallback struct {
	code  string
	state string
	err   string
}

// loginGemini runs the Google Antigravity OAuth authorization-code flow with a
// local loopback callback, then discovers the GCP project id via the Antigravity
// control plane. It persists a credential the antigravityOAuthAuthorizer reads
// (type/access_token/refresh_token/email/project_id/expired).
func loginGemini(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	state, err := randomKiroOAuthValue(16)
	if err != nil {
		return LoginResult{}, fmt.Errorf("create Gemini state: %w", err)
	}
	callbackPort := geminiOAuthCallbackPort
	if opts.CallbackPort > 0 {
		callbackPort = opts.CallbackPort
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", callbackPort)))
	if err != nil {
		return LoginResult{}, fmt.Errorf("listen for Gemini OAuth callback on port %d: %w", callbackPort, err)
	}
	resultCh := make(chan geminiOAuthCallback, 1)
	server := &http.Server{Handler: geminiCallbackHandler(resultCh)}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	redirectURI := fmt.Sprintf("http://localhost:%d%s", listener.Addr().(*net.TCPAddr).Port, geminiOAuthCallbackPath)
	authURL := geminiOAuthAuthEndpoint + "?" + url.Values{
		"access_type":   {"offline"},
		"client_id":     {antigravityOAuthClientID},
		"prompt":        {"consent"},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {strings.Join(geminiOAuthScopes, " ")},
		"state":         {state},
	}.Encode()
	fmt.Printf("Open %s to authorize Gemini\n", authURL)
	if !opts.NoBrowser {
		_ = openKiroBrowser(authURL)
	}
	fmt.Println("Waiting for Gemini authentication callback...")

	var result geminiOAuthCallback
	select {
	case result = <-resultCh:
	case <-time.After(5 * time.Minute):
		return LoginResult{}, fmt.Errorf("Gemini authentication timed out")
	case <-ctx.Done():
		return LoginResult{}, fmt.Errorf("Gemini authentication: %w", ctx.Err())
	}
	if result.err != "" {
		return LoginResult{}, fmt.Errorf("Gemini authentication failed: %s", result.err)
	}
	if result.state != state {
		return LoginResult{}, fmt.Errorf("Gemini authentication failed: state mismatch")
	}
	if strings.TrimSpace(result.code) == "" {
		return LoginResult{}, fmt.Errorf("Gemini authentication failed: missing authorization code")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	token, err := exchangeGeminiCode(ctx, client, result.code, redirectURI)
	if err != nil {
		return LoginResult{}, err
	}
	email, err := fetchGeminiUserInfo(ctx, client, token.AccessToken)
	if err != nil {
		return LoginResult{}, err
	}
	projectID, err := fetchGeminiProjectID(ctx, client, token.AccessToken)
	if err != nil {
		return LoginResult{}, err
	}

	metadata := map[string]any{
		"type":          backendAntigravity,
		"access_token":  strings.TrimSpace(token.AccessToken),
		"refresh_token": strings.TrimSpace(token.RefreshToken),
		"expires_in":    token.ExpiresIn,
		"timestamp":     time.Now().UnixMilli(),
		"email":         email,
		"project_id":    projectID,
	}
	if token.ExpiresIn > 0 {
		metadata["expired"] = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode Gemini credential: %w", err)
	}
	raw = append(raw, '\n')
	fileName := backendAntigravity + "-" + credentialIdentity(metadata, raw) + ".json"
	path := filepath.Join(authDir, fileName)
	if err := writeCredentialAtomic(path, raw); err != nil {
		return LoginResult{}, err
	}
	fmt.Println("Gemini authentication successful")
	fmt.Printf("Using GCP project: %s\n", projectID)
	return LoginResult{Provider: ProviderGemini, Backend: backendAntigravity, Path: path}, nil
}

func geminiCallbackHandler(resultCh chan<- geminiOAuthCallback) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(geminiOAuthCallbackPath, func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		result := geminiOAuthCallback{
			code:  strings.TrimSpace(query.Get("code")),
			state: strings.TrimSpace(query.Get("state")),
			err:   strings.TrimSpace(query.Get("error")),
		}
		select {
		case resultCh <- result:
		default:
		}
		if result.err != "" || result.code == "" {
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(writer, "<h1>Login failed</h1><p>Please check the CLI output.</p>")
			return
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(writer, "<h1>Login successful</h1><p>You can close this window.</p>")
	})
	return mux
}

func exchangeGeminiCode(ctx context.Context, client *http.Client, code, redirectURI string) (geminiOAuthToken, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {antigravityOAuthClientID},
		"client_secret": {antigravityOAuthClientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, antigravityTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return geminiOAuthToken{}, fmt.Errorf("Gemini token exchange: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return geminiOAuthToken{}, fmt.Errorf("Gemini token exchange: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, antigravityMaxErrorBytes))
	if err != nil {
		return geminiOAuthToken{}, fmt.Errorf("Gemini token exchange: read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return geminiOAuthToken{}, fmt.Errorf("Gemini token exchange: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var token geminiOAuthToken
	if err := json.Unmarshal(body, &token); err != nil {
		return geminiOAuthToken{}, fmt.Errorf("Gemini token exchange: decode response: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return geminiOAuthToken{}, fmt.Errorf("Gemini token exchange: response missing access_token")
	}
	return token, nil
}

func fetchGeminiUserInfo(ctx context.Context, client *http.Client, accessToken string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, geminiOAuthUserInfoURL, nil)
	if err != nil {
		return "", fmt.Errorf("Gemini userinfo: create request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("User-Agent", antigravityRequestUserAgent)
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("Gemini userinfo: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, antigravityMaxErrorBytes))
	if err != nil {
		return "", fmt.Errorf("Gemini userinfo: read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Gemini userinfo: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var info struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("Gemini userinfo: decode response: %w", err)
	}
	email := strings.TrimSpace(info.Email)
	if email == "" {
		return "", fmt.Errorf("Gemini userinfo: response missing email")
	}
	return email, nil
}

func fetchGeminiProjectID(ctx context.Context, client *http.Client, accessToken string) (string, error) {
	projectID, loadErr := geminiLoadCodeAssist(ctx, client, accessToken)
	if loadErr == nil && projectID != "" {
		return projectID, nil
	}
	projectID, err := geminiOnboardUser(ctx, client, accessToken)
	if err != nil {
		if loadErr != nil {
			return "", fmt.Errorf("Gemini project discovery: %v; onboard: %v", loadErr, err)
		}
		return "", err
	}
	if projectID == "" {
		return "", fmt.Errorf("Gemini project discovery returned empty project")
	}
	return projectID, nil
}

func geminiLoadCodeAssist(ctx context.Context, client *http.Client, accessToken string) (string, error) {
	rawBody, err := json.Marshal(map[string]any{"metadata": map[string]string{"ideType": "ANTIGRAVITY"}})
	if err != nil {
		return "", fmt.Errorf("marshal loadCodeAssist body: %w", err)
	}
	endpoint := strings.TrimRight(antigravityBaseURLProd, "/") + "/v1internal:loadCodeAssist"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(rawBody)))
	if err != nil {
		return "", fmt.Errorf("loadCodeAssist: create request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", antigravityRequestUserAgent)
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("loadCodeAssist: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, antigravityMaxErrorBytes))
	if err != nil {
		return "", fmt.Errorf("loadCodeAssist: read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("loadCodeAssist: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("loadCodeAssist: decode response: %w", err)
	}
	return geminiExtractProjectID(data), nil
}

func geminiOnboardUser(ctx context.Context, client *http.Client, accessToken string) (string, error) {
	userAgent := antigravityRequestUserAgent + " " + geminiOAuthOnboardNodeUA
	rawBody, err := json.Marshal(map[string]any{
		"tier_id": "free-tier",
		"metadata": map[string]string{
			"ide_type":    "ANTIGRAVITY",
			"ide_version": "2.2.1",
			"ide_name":    "antigravity",
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshal onboardUser body: %w", err)
	}
	endpoint := strings.TrimRight(antigravityBaseURLDaily, "/") + "/v1internal:onboardUser"
	for attempt := 0; attempt < 5; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(rawBody)))
		if err != nil {
			return "", fmt.Errorf("onboardUser: create request: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+accessToken)
		request.Header.Set("Accept", "*/*")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", userAgent)
		request.Header.Set("X-Goog-Api-Client", geminiOAuthGoogAPIUA)
		response, err := client.Do(request)
		if err != nil {
			return "", fmt.Errorf("onboardUser: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, antigravityMaxErrorBytes))
		_ = response.Body.Close()
		if readErr != nil {
			return "", fmt.Errorf("onboardUser: read response: %w", readErr)
		}
		if response.StatusCode != http.StatusOK {
			return "", fmt.Errorf("onboardUser: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
		}
		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			return "", fmt.Errorf("onboardUser: decode response: %w", err)
		}
		if done, ok := data["done"].(bool); ok && done {
			responseData, _ := data["response"].(map[string]any)
			projectID := geminiExtractProjectID(responseData)
			if projectID != "" {
				return projectID, nil
			}
			return "", fmt.Errorf("onboardUser: no project id in response")
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("onboardUser did not complete after 5 attempts")
}

func geminiExtractProjectID(data map[string]any) string {
	if data == nil {
		return ""
	}
	for _, key := range []string{"cloudaicompanionProject", "projectId", "project"} {
		switch value := data[key].(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		case map[string]any:
			if id, ok := value["id"].(string); ok {
				if trimmed := strings.TrimSpace(id); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}
