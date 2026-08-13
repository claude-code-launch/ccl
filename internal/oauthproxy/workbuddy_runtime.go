package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

const workbuddyMaxBodyBytes = int64(128 << 20)

type workbuddyCredential struct {
	path     string
	fileName string
	metadata map[string]any
	disabled bool
}

func (c *workbuddyCredential) token() workbuddyToken {
	return workbuddyToken{
		AccessToken:      firstMetadataString(c.metadata, "access_token"),
		RefreshToken:     firstMetadataString(c.metadata, "refresh_token"),
		TokenType:        firstMetadataString(c.metadata, "token_type"),
		Scope:            firstMetadataString(c.metadata, "scope"),
		Domain:           firstMetadataString(c.metadata, "domain"),
		ExpiresAt:        metadataInt64(c.metadata, "expires_at"),
		RefreshExpiresAt: metadataInt64(c.metadata, "refresh_expires_at"),
	}
}

func (c *workbuddyCredential) account() workbuddyAccount {
	return workbuddyAccount{
		UID:                firstMetadataString(c.metadata, "user_id"),
		Nickname:           firstMetadataString(c.metadata, "nickname"),
		Email:              firstMetadataString(c.metadata, "email"),
		Type:               firstMetadataString(c.metadata, "account_type"),
		EnterpriseID:       firstMetadataString(c.metadata, "enterprise_id"),
		DepartmentFullName: firstMetadataString(c.metadata, "department_full_name"),
	}
}

type workbuddyCredentialStore struct {
	authDir        string
	credentialFile string
	client         *http.Client
	mu             sync.Mutex
}

func newWorkBuddyCredentialStore(authDir, credentialFile string) *workbuddyCredentialStore {
	return &workbuddyCredentialStore{
		authDir:        authDir,
		credentialFile: filepath.Base(strings.TrimSpace(credentialFile)),
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			ResponseHeaderTimeout: 90 * time.Second,
		}},
	}
}

func (s *workbuddyCredentialStore) load() (*workbuddyCredential, error) {
	if s.credentialFile == "" || s.credentialFile == "." {
		return nil, fmt.Errorf("WorkBuddy runtime is not bound to a credential file")
	}
	path := filepath.Join(s.authDir, s.credentialFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no WorkBuddy credential %s; run `ccl oauth workbuddy` again", s.credentialFile)
		}
		return nil, fmt.Errorf("read WorkBuddy credential %s: %w", s.credentialFile, err)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode WorkBuddy credential %s: %w", s.credentialFile, err)
	}
	credentialType, _ := metadata["type"].(string)
	if !strings.EqualFold(strings.TrimSpace(credentialType), ProviderWorkBuddy) {
		return nil, fmt.Errorf("credential %s belongs to %q, not WorkBuddy", s.credentialFile, credentialType)
	}
	disabled, _ := metadata["disabled"].(bool)
	credential := &workbuddyCredential{path: path, fileName: s.credentialFile, metadata: metadata, disabled: disabled}
	if !disabled && strings.TrimSpace(credential.token().AccessToken) == "" {
		return nil, fmt.Errorf("WorkBuddy credential %s has no access token", s.credentialFile)
	}
	return credential, nil
}

func (s *workbuddyCredentialStore) authorize(ctx context.Context, forceRefresh bool) (*workbuddyCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, err := s.load()
	if err != nil {
		return nil, err
	}
	if credential.disabled {
		return nil, fmt.Errorf("WorkBuddy credential %s is disabled", credential.fileName)
	}
	token := credential.token()
	if !forceRefresh && !workbuddyTokenNeedsRefresh(token) {
		return credential, nil
	}
	if strings.TrimSpace(token.RefreshToken) == "" {
		return nil, fmt.Errorf("WorkBuddy credential %s has expired and has no refresh token; run `ccl oauth workbuddy` again", credential.fileName)
	}
	refreshURL := strings.TrimRight(workbuddyBaseURL, "/") + "/v2/plugin/auth/token/refresh"
	headers := workbuddyAuthenticatedHeaders(token, credential.account())
	headers.Set("X-Refresh-Token", token.RefreshToken)
	headers.Set("X-Auth-Refresh-Source", "plugin")
	LogDebugEvent("token_refresh_start", "component", "workbuddy", "credential", credential.fileName, "forced", forceRefresh)
	started := time.Now()
	envelope, err := workbuddyJSON[workbuddyToken](ctx, s.client, http.MethodPost, refreshURL, []byte(`{}`), headers)
	if err != nil {
		LogErrorEvent("token_refresh_failed", "component", "workbuddy", "credential", credential.fileName,
			"duration", logDuration(started), "error", err)
		return nil, fmt.Errorf("refresh WorkBuddy credential %s: %w", credential.fileName, err)
	}
	refreshed := envelope.Data
	if refreshed.AccessToken == "" {
		return nil, fmt.Errorf("refresh WorkBuddy credential %s: response has no access token", credential.fileName)
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = token.RefreshToken
	}
	if refreshed.Domain == "" {
		refreshed.Domain = token.Domain
	}
	now := time.Now()
	if refreshed.ExpiresAt == 0 && refreshed.ExpiresIn > 0 {
		refreshed.ExpiresAt = now.Add(time.Duration(refreshed.ExpiresIn) * time.Second).UnixMilli()
	}
	if refreshed.RefreshExpiresAt == 0 && refreshed.RefreshExpiresIn > 0 {
		refreshed.RefreshExpiresAt = now.Add(time.Duration(refreshed.RefreshExpiresIn) * time.Second).UnixMilli()
	}
	credential.metadata["access_token"] = refreshed.AccessToken
	credential.metadata["refresh_token"] = refreshed.RefreshToken
	credential.metadata["domain"] = refreshed.Domain
	credential.metadata["expires_at"] = refreshed.ExpiresAt
	credential.metadata["refresh_expires_at"] = refreshed.RefreshExpiresAt
	if refreshed.TokenType != "" {
		credential.metadata["token_type"] = refreshed.TokenType
	}
	if refreshed.Scope != "" {
		credential.metadata["scope"] = refreshed.Scope
	}
	raw, err := json.MarshalIndent(credential.metadata, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode refreshed WorkBuddy credential: %w", err)
	}
	if err := writeCredentialAtomic(credential.path, append(raw, '\n')); err != nil {
		return nil, fmt.Errorf("persist refreshed WorkBuddy credential: %w", err)
	}
	LogInfof("WorkBuddy token refreshed credential=%s duration=%s", credential.fileName, logDuration(started))
	return credential, nil
}

func (s *workbuddyCredentialStore) listAuths() []*coreauth.Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, err := s.load()
	if err != nil {
		return nil
	}
	status := coreauth.StatusActive
	if credential.disabled {
		status = coreauth.StatusDisabled
	}
	return []*coreauth.Auth{{
		ID:       credential.fileName,
		Provider: ProviderWorkBuddy,
		FileName: credential.path,
		Label:    firstMetadataString(credential.metadata, "nickname", "email", "user_id"),
		Status:   status,
		Disabled: credential.disabled,
		Metadata: credential.metadata,
	}}
}

func workbuddyTokenNeedsRefresh(token workbuddyToken) bool {
	if strings.TrimSpace(token.AccessToken) == "" {
		return true
	}
	if token.ExpiresAt <= 0 {
		return false
	}
	expiresAt := time.UnixMilli(token.ExpiresAt)
	// Accept legacy second timestamps if a credential was edited by hand.
	if token.ExpiresAt < 10_000_000_000 {
		expiresAt = time.Unix(token.ExpiresAt, 0)
	}
	return time.Now().Add(time.Minute).After(expiresAt)
}

func metadataInt64(metadata map[string]any, key string) int64 {
	switch value := metadata[key].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	case json.Number:
		parsed, _ := value.Int64()
		return parsed
	case string:
		parsed, _ := time.Parse(time.RFC3339Nano, value)
		if !parsed.IsZero() {
			return parsed.UnixMilli()
		}
	}
	return 0
}

type workbuddyGateway struct {
	endpoint       string
	store          *workbuddyCredentialStore
	server         *http.Server
	done           chan struct{}
	conversationID string
}

func startWorkBuddyGateway(parent context.Context, store *workbuddyCredentialStore) (*workbuddyGateway, error) {
	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("start WorkBuddy gateway listener: %w", err)
	}
	gateway := &workbuddyGateway{
		endpoint:       "http://" + listener.Addr().String(),
		store:          store,
		done:           make(chan struct{}),
		conversationID: uuid.NewString(),
	}
	gateway.server = &http.Server{
		Handler:           http.HandlerFunc(gateway.serveHTTP),
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return parent
		},
	}
	go func() {
		err := gateway.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			LogErrorf("WorkBuddy gateway stopped endpoint=%q error=%v", gateway.endpoint, err)
		}
		close(gateway.done)
	}()
	return gateway, nil
}

func (g *workbuddyGateway) Stop() {
	if g == nil || g.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeStopTimeout)
	_ = g.server.Shutdown(ctx)
	cancel()
	waitClosed(g.done, runtimeStopTimeout)
}

func (g *workbuddyGateway) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	if request.Method != http.MethodPost || workbuddyUpstreamPath(request.URL.Path) == "" {
		LogWarnEvent("request_rejected", "component", "workbuddy", "request_id", requestID,
			"path", request.URL.Path, "method", request.Method, "status", http.StatusNotFound,
			"reason", "unsupported_route")
		writeWorkBuddyGatewayError(writer, http.StatusNotFound, "unsupported WorkBuddy route")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, workbuddyMaxBodyBytes+1))
	if err != nil {
		writeWorkBuddyGatewayError(writer, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	if int64(len(body)) > workbuddyMaxBodyBytes {
		writeWorkBuddyGatewayError(writer, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	model := copilotRequestModel(body)
	LogDebugEvent("request_received", "component", "workbuddy", "request_id", requestID,
		"method", request.Method, "path", request.URL.Path, "model", model, "body_bytes", len(body))
	response, err := g.do(requestCtx, workbuddyUpstreamPath(request.URL.Path), request.URL.RawQuery, request.Header, body)
	if err != nil {
		LogErrorEvent("request_failed", "component", "workbuddy", "request_id", requestID,
			"path", request.URL.Path, "model", model, "status", http.StatusBadGateway,
			"duration", logDuration(started), "error", err)
		writeWorkBuddyGatewayError(writer, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()
	LogUpstreamEvent(response.StatusCode, "request_complete", "component", "workbuddy", "request_id", requestID,
		"path", request.URL.Path, "model", model, "status", response.StatusCode,
		"retry_after", response.Header.Get("Retry-After"), "duration", logDuration(started))
	copyCopilotHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	copyCopilotResponse(writer, response.Body)
}

func (g *workbuddyGateway) do(ctx context.Context, path, rawQuery string, headers http.Header, body []byte) (*http.Response, error) {
	credential, err := g.store.authorize(ctx, false)
	if err != nil {
		return nil, err
	}
	response, err := g.doOne(ctx, path, rawQuery, headers, body, credential)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
		return response, nil
	}
	status := response.StatusCode
	LogWarnEvent("upstream_auth_retry", "component", "workbuddy", "request_id", requestLogID(ctx),
		"path", path, "status", status, "credential", credential.fileName)
	refreshed, refreshErr := g.store.authorize(ctx, true)
	if refreshErr != nil {
		LogWarnEvent("upstream_auth_retry_skipped", "component", "workbuddy", "request_id", requestLogID(ctx),
			"path", path, "status", status, "credential", credential.fileName, "error", refreshErr)
		// Refresh is a WorkBuddy-specific recovery attempt. If it fails, retain
		// the original upstream status/body instead of turning a 401/403 into 502.
		return response, nil
	}
	closeCopilotResponse(response)
	return g.doOne(ctx, path, rawQuery, headers, body, refreshed)
}

func (g *workbuddyGateway) doOne(ctx context.Context, path, rawQuery string, headers http.Header, body []byte, credential *workbuddyCredential) (*http.Response, error) {
	target := strings.TrimRight(workbuddyBaseURL, "/") + path
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyCopilotHeaders(request.Header, headers)
	for _, name := range []string{"Authorization", "X-Api-Key", "Api-Key", "Host", "Content-Length", "User-Agent"} {
		request.Header.Del(name)
	}
	token := credential.token()
	account := credential.account()
	for name, values := range workbuddyAuthenticatedHeaders(token, account) {
		request.Header.Del(name)
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	traceID := strings.ReplaceAll(uuid.NewString(), "-", "")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Conversation-ID", g.conversationID)
	request.Header.Set("X-Conversation-Request-ID", traceID)
	request.Header.Set("X-Conversation-Message-ID", traceID)
	request.Header.Set("X-Request-ID", traceID)
	request.Header.Set("X-Agent-Intent", "craft")
	request.Header.Set("X-IDE-Type", workbuddyPlatform)
	request.Header.Set("X-IDE-Name", workbuddyPlatform)
	request.Header.Set("X-IDE-Version", workbuddyClientVersion)
	request.Header.Set("X-Private-Data", "false")
	model := copilotRequestModel(body)
	LogDebugEvent("upstream_request", "component", "workbuddy", "request_id", requestLogID(ctx),
		"path", path, "model", model, "credential", credential.fileName, "conversation_id", g.conversationID)
	if len(body) > 0 {
		DebugHTTPBody(fmt.Sprintf("workbuddy request request_id=%s path=%s", requestLogID(ctx), path), body)
	}
	started := time.Now()
	response, err := g.store.client.Do(request)
	if err != nil {
		return nil, err
	}
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "workbuddy", "request_id", requestLogID(ctx),
		"path", path, "model", model, "credential", credential.fileName, "status", response.StatusCode,
		"retry_after", response.Header.Get("Retry-After"), "duration", logDuration(started))
	debugWorkBuddyFailureBody(ctx, path, response)
	return response, nil
}

func debugWorkBuddyFailureBody(ctx context.Context, path string, response *http.Response) {
	if response == nil || response.Body == nil || response.StatusCode < 400 || !LogDebugEnabled() {
		return
	}
	prefix, err := io.ReadAll(io.LimitReader(response.Body, workbuddyMaxErrorBytes))
	if err != nil {
		LogDebugEvent("upstream_error_body_read_failed", "component", "workbuddy", "request_id", requestLogID(ctx),
			"path", path, "status", response.StatusCode, "error", err)
		return
	}
	DebugHTTPBody(fmt.Sprintf("workbuddy response request_id=%s path=%s status=%d", requestLogID(ctx), path, response.StatusCode), prefix)
	response.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.MultiReader(bytes.NewReader(prefix), response.Body), Closer: response.Body}
}

func workbuddyUpstreamPath(path string) string {
	switch strings.TrimRight(path, "/") {
	case "/v1/chat/completions", "/chat/completions":
		return "/v2/chat/completions"
	default:
		return ""
	}
}

func writeWorkBuddyGatewayError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{"type": "workbuddy_error", "message": message},
	})
}

type workbuddyModel struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Credits           any    `json:"credits"`
	MaxAllowedSize    int64  `json:"maxAllowedSize"`
	MaxOutputTokens   int64  `json:"maxOutputTokens"`
	SupportsImages    bool   `json:"supportsImages"`
	SupportsToolCall  bool   `json:"supportsToolCall"`
	SupportsReasoning bool   `json:"supportsReasoning"`
}

type workbuddyProductConfig struct {
	Models []workbuddyModel `json:"models"`
}

func (g *workbuddyGateway) discoverModels(ctx context.Context) ([]workbuddyModel, error) {
	credential, err := g.store.authorize(ctx, false)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(workbuddyBaseURL, "/") + "/v3/config"
	started := time.Now()
	envelope, err := workbuddyJSON[workbuddyProductConfig](ctx, g.store.client, http.MethodGet, endpoint, nil,
		workbuddyAuthenticatedHeaders(credential.token(), credential.account()))
	var apiErr *workbuddyAPIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden) {
		refreshed, refreshErr := g.store.authorize(ctx, true)
		if refreshErr == nil {
			credential = refreshed
			envelope, err = workbuddyJSON[workbuddyProductConfig](ctx, g.store.client, http.MethodGet, endpoint, nil,
				workbuddyAuthenticatedHeaders(credential.token(), credential.account()))
		} else {
			LogWarnEvent("model_discovery_auth_refresh_failed", "component", "workbuddy",
				"credential", credential.fileName, "status", apiErr.Status, "error", refreshErr)
		}
	}
	if err != nil {
		status := 0
		if errors.As(err, &apiErr) {
			status = apiErr.Status
		}
		LogUpstreamEvent(status, "model_discovery_failed", "component", "workbuddy",
			"credential", credential.fileName, "status", status, "duration", logDuration(started), "error", err)
		return nil, fmt.Errorf("discover WorkBuddy models: %w", err)
	}
	models := make([]workbuddyModel, 0, len(envelope.Data.Models))
	seen := make(map[string]bool)
	for _, model := range envelope.Data.Models {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" || seen[strings.ToLower(model.ID)] {
			continue
		}
		seen[strings.ToLower(model.ID)] = true
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("discover WorkBuddy models: upstream returned no models")
	}
	LogInfof("WorkBuddy model discovery credential=%s models=%d duration=%s", credential.fileName, len(models), logDuration(started))
	return models, nil
}

func buildWorkBuddyRoutes(modelSpec string, catalog []workbuddyModel) ([]runtimeOpenAIModel, []string, map[string]string, error) {
	lookup := make(map[string]workbuddyModel, len(catalog))
	routes := make([]runtimeOpenAIModel, 0, len(catalog)+len(runtimeModelRoutes(modelSpec)))
	models := make([]string, 0, len(catalog))
	names := make(map[string]string)
	seenRoutes := make(map[string]bool)
	add := func(name, alias string) {
		key := strings.ToLower(name) + "\x00" + strings.ToLower(alias)
		if seenRoutes[key] {
			return
		}
		seenRoutes[key] = true
		routes = append(routes, runtimeOpenAIModel{Name: name, Alias: alias, ForceMapping: true})
	}
	for _, model := range catalog {
		lookup[strings.ToLower(model.ID)] = model
		models = append(models, model.ID)
		if strings.TrimSpace(model.Name) != "" {
			names[model.ID] = strings.TrimSpace(model.Name)
		}
		add(model.ID, model.ID)
	}
	for _, route := range runtimeModelRoutes(modelSpec) {
		model, ok := lookup[strings.ToLower(route.Name)]
		if !ok {
			return nil, nil, nil, fmt.Errorf("configured WorkBuddy model %q is not present in the upstream catalog", route.Name)
		}
		add(model.ID, route.Alias)
	}
	return routes, models, names, nil
}

func startWorkBuddyOAuth(parent context.Context, modelSpec, credentialFile string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	store := newWorkBuddyCredentialStore(authDir, credentialFile)
	if _, err := store.authorize(parent, false); err != nil {
		return nil, err
	}
	gateway, err := startWorkBuddyGateway(parent, store)
	if err != nil {
		return nil, err
	}
	stopGateway := true
	defer func() {
		if stopGateway {
			gateway.Stop()
		}
	}()
	catalog, err := gateway.discoverModels(parent)
	if err != nil {
		return nil, err
	}
	routes, models, names, err := buildWorkBuddyRoutes(modelSpec, catalog)
	if err != nil {
		return nil, err
	}
	runtimeDir, port, apiKey, err := prepareAPIKeyRuntime()
	if err != nil {
		return nil, err
	}
	configFile := runtimeOpenAIConfigFile{
		runtimeConfigBase: newRuntimeConfigBase(port, runtimeDir, apiKey),
		OpenAICompatibility: []runtimeOpenAICompatibility{{
			Name:           ProviderWorkBuddy,
			BaseURL:        gateway.endpoint,
			APIKeyEntries:  []runtimeOpenAICompatibilityKey{{APIKey: ProviderWorkBuddy}},
			Models:         routes,
			DisableCooling: true,
		}},
	}
	rawConfig, err := yaml.Marshal(configFile)
	if err != nil {
		_ = os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("encode WorkBuddy compatibility runtime config: %w", err)
	}
	proxyRuntime, err := startAPIKeyRuntime(parent, rawConfig, apiKey, runtimeDir)
	if err != nil {
		return nil, err
	}
	stopGateway = false
	proxyRuntime.cleanup = append(proxyRuntime.cleanup, gateway.Stop)
	proxyRuntime.listAuths = store.listAuths
	proxyRuntime.models = append([]string(nil), models...)
	proxyRuntime.modelNames = names
	LogInfof("runtime start oauth provider=workbuddy backend=workbuddy protocol=openai_chat local_endpoint=%q credential_file=%s models=%d auth_owner=ccl catalog_owner=ccl data_plane=cpa",
		SafeLogEndpoint(proxyRuntime.endpoint), filepath.Base(credentialFile), len(models))
	return proxyRuntime, nil
}
