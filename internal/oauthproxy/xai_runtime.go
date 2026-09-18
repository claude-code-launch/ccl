package oauthproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	xaiChatProxyBaseURL = "https://cli-chat-proxy.grok.com/v1"
	xaiOAuthClientID    = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiDefaultTokenURL  = "https://auth.x.ai/oauth2/token"
	// Keep this compatible with the current public xai-org/grok-build client.
	// cli-chat-proxy uses the value for its client version gate.
	xaiClientVersion    = "1.0.32"
	xaiClientIdentifier = "grok-shell"
	xaiClientMode       = "headless"
	xaiMaxErrorBytes    = int64(1 << 20)
)

// applyXaiClientHeaders attaches the process and account identity expected by
// cli-chat-proxy. The field names mirror the public Grok Build implementation.
func applyXaiClientHeaders(header http.Header, auth codexResponsesAuthorization) {
	header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	header.Set("x-grok-client-version", xaiClientVersion)
	header.Set("User-Agent", "grok-shell/"+xaiClientVersion)
	header.Set("x-grok-client-identifier", xaiClientIdentifier)
	header.Set("x-grok-client-mode", xaiClientMode)
	header.Set("x-authenticateresponse", "authenticate-response")
	if auth.userID != "" {
		header.Set("x-userid", auth.userID)
	}
	if auth.email != "" {
		header.Set("x-email", auth.email)
	}
}

// applyXaiGrokHeaders adds the per-inference routing and trace identity used by
// Grok Build. In particular, x-grok-model-override is the authoritative model
// selector at cli-chat-proxy; a model in the JSON body alone is insufficient.
func applyXaiGrokHeaders(header http.Header, sessionID, requestID, agentID, modelID string, auth codexResponsesAuthorization) {
	applyXaiClientHeaders(header, auth)
	if sessionID != "" {
		header.Set("x-grok-conv-id", sessionID)
		header.Set("x-grok-session-id", sessionID)
	}
	if requestID != "" {
		header.Set("x-grok-req-id", requestID)
	}
	if agentID != "" {
		header.Set("x-grok-agent-id", agentID)
	}
	if modelID != "" {
		header.Set("x-grok-model-override", modelID)
	}
}

// xaiOAuthAuthorizer resolves and refreshes an xAI/Grok OAuth credential. The
// credential is written by CCL during `ccl oauth grok`, and this authorizer
// reads the persisted access_token, refresh_token, and token_endpoint fields.
type xaiOAuthAuthorizer struct {
	path   string
	client *http.Client
	mu     sync.Mutex
}

type xaiOAuthCredential struct {
	metadata      map[string]any
	accessToken   string
	refreshToken  string
	tokenEndpoint string
	email         string
	userID        string
	expiresAt     time.Time
	disabled      bool
}

func (a *xaiOAuthAuthorizer) authorize(ctx context.Context, force bool) (codexResponsesAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	credential, err := a.load()
	if err != nil {
		return codexResponsesAuthorization{}, err
	}
	if credential.disabled {
		return codexResponsesAuthorization{}, fmt.Errorf("xAI credential %s is disabled", filepath.Base(a.path))
	}
	if force || credential.accessToken == "" || (!credential.expiresAt.IsZero() && time.Now().Add(time.Minute).After(credential.expiresAt)) {
		credential, err = a.refresh(ctx, credential)
		if err != nil {
			return codexResponsesAuthorization{}, err
		}
	}
	return codexResponsesAuthorization{
		token: credential.accessToken, credential: filepath.Base(a.path),
		userID: credential.userID, email: credential.email,
	}, nil
}

func (*xaiOAuthAuthorizer) isOAuth() bool { return true }

func (a *xaiOAuthAuthorizer) listAuths() []*AuthInfo {
	credential, err := a.load()
	if err != nil {
		return nil
	}
	status := StatusActive
	if credential.disabled {
		status = StatusDisabled
	}
	return []*AuthInfo{{
		ID: filepath.Base(a.path), Provider: backendXAI, FileName: a.path, Label: credential.email,
		Status: status, Disabled: credential.disabled, Metadata: credential.metadata,
	}}
}

func (a *xaiOAuthAuthorizer) load() (*xaiOAuthCredential, error) {
	raw, err := os.ReadFile(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no xAI credentials found; run `ccl oauth grok` first")
		}
		return nil, fmt.Errorf("read xAI credential %s: %w", filepath.Base(a.path), err)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode xAI credential %s: %w", filepath.Base(a.path), err)
	}
	credentialType := strings.ToLower(strings.TrimSpace(stringValue(metadata["type"])))
	if credentialType != backendXAI {
		return nil, fmt.Errorf("credential %s is type %q, not xAI", filepath.Base(a.path), credentialType)
	}
	tokenEndpoint := firstMetadataString(metadata, "token_endpoint")
	if tokenEndpoint == "" {
		tokenEndpoint = xaiDefaultTokenURL
	}
	disabled, _ := metadata["disabled"].(bool)
	return &xaiOAuthCredential{
		metadata: metadata, accessToken: firstMetadataString(metadata, "access_token"),
		refreshToken: firstMetadataString(metadata, "refresh_token"), tokenEndpoint: tokenEndpoint,
		email:     firstMetadataString(metadata, "email"),
		userID:    firstMetadataString(metadata, "sub", "user_id", "userid"),
		expiresAt: parseCodexExpiry(firstMetadataString(metadata, "expired")),
		disabled:  disabled,
	}, nil
}

func (a *xaiOAuthAuthorizer) refresh(ctx context.Context, credential *xaiOAuthCredential) (*xaiOAuthCredential, error) {
	if credential.refreshToken == "" {
		return nil, fmt.Errorf("xAI credential %s has no refresh token", filepath.Base(a.path))
	}
	form := url.Values{
		"grant_type": {"refresh_token"}, "client_id": {xaiOAuthClientID},
		"refresh_token": {credential.refreshToken},
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(refreshCtx, http.MethodPost, credential.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("refresh xAI token: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, xaiMaxErrorBytes))
	if err != nil {
		return nil, fmt.Errorf("read xAI token refresh: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh xAI token: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
		Email        string `json:"email"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("decode xAI token refresh: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, fmt.Errorf("refresh xAI token: response has no access token")
	}
	credential.accessToken = token.AccessToken
	if token.RefreshToken != "" {
		credential.refreshToken = token.RefreshToken
	}
	if token.ExpiresIn > 0 {
		credential.expiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	if token.Email != "" {
		credential.email = token.Email
	}
	credential.metadata["type"] = backendXAI
	credential.metadata["access_token"] = credential.accessToken
	credential.metadata["refresh_token"] = credential.refreshToken
	if token.IDToken != "" {
		credential.metadata["id_token"] = token.IDToken
	}
	if token.Email != "" {
		credential.metadata["email"] = token.Email
	}
	if !credential.expiresAt.IsZero() {
		credential.metadata["expired"] = credential.expiresAt.UTC().Format(time.RFC3339)
	}
	credential.metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if err := writeCodexCredentialAtomic(a.path, credential.metadata); err != nil {
		return nil, err
	}
	LogInfof("credential refreshed component=xai_responses credential_file=%s expires_at=%s",
		filepath.Base(a.path), credential.expiresAt.UTC().Format(time.RFC3339))
	return credential, nil
}

// startXaiOAuth starts the CCL-owned Grok Responses data plane. Grok speaks the
// OpenAI Responses protocol, so it reuses the Codex Responses converter, stream
// pipeline, and lifecycle, swapping Codex identity for xAI/Grok identity and the
// Codex OAuth authorizer for xAI's.
func startXaiOAuth(parent context.Context, modelSpec, credentialFile string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(authDir, filepath.Base(credentialFile))
	authorizer := &xaiOAuthAuthorizer{
		path: path,
		client: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
			ResponseHeaderTimeout: 30 * time.Second,
		}},
	}
	credential, err := authorizer.load()
	if err != nil {
		return nil, err
	}
	if credential.disabled {
		return nil, fmt.Errorf("xAI credential %s is disabled", filepath.Base(path))
	}
	if credential.accessToken == "" && credential.refreshToken == "" {
		return nil, fmt.Errorf("xAI credential %s has no access or refresh token", filepath.Base(path))
	}
	discoveryCtx, cancelDiscovery := context.WithTimeout(parent, 20*time.Second)
	models, discoverErr := discoverXaiModels(discoveryCtx, xaiChatProxyBaseURL, authorizer)
	cancelDiscovery()
	catalogFallback := false
	if discoverErr != nil || len(models) == 0 {
		if discoverErr == nil {
			discoverErr = fmt.Errorf("xAI returned an empty model catalog")
		}
		LogWarnf("Grok model discovery failed; using compatibility catalog error=%v", discoverErr)
		models = xaiFallbackModels()
		catalogFallback = true
	}
	effectiveModelSpec := xaiModelSpec(modelSpec, models)
	runtime, err := startCodexResponsesRuntimeWithService(parent, xaiChatProxyBaseURL, effectiveModelSpec, authorizer, func(apiKey, endpoint string, routes []runtimeModelRoute, auth codexResponsesAuthorizer, usage *UsageTracker) *codexResponsesService {
		service := newXaiResponsesService(apiKey, endpoint, routes, auth, usage)
		service.models = xaiModelIDs(models)
		addXaiDisplayAliases(service, models)
		return service
	})
	if err != nil {
		return nil, err
	}
	runtime.listAuths = authorizer.listAuths
	runtime.modelNames = xaiModelDisplayNames(models)
	runtime.catalogFallback = catalogFallback
	LogInfof("runtime start oauth provider=grok backend=xai protocol=openai_responses port=%s credential_file=%s model_count=%d",
		strings.TrimPrefix(strings.TrimSuffix(runtime.endpoint, "/v1"), "http://"), filepath.Base(path), len(runtime.models))
	return runtime, nil
}
