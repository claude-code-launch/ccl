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
	"strconv"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/google/uuid"
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
	deployment     workbuddyDeployment
	authDir        string
	credentialFile string
	client         *http.Client
}

func newWorkBuddyCredentialStore(deployment workbuddyDeployment, authDir, credentialFile string) *workbuddyCredentialStore {
	return &workbuddyCredentialStore{
		deployment:     deployment,
		authDir:        authDir,
		credentialFile: filepath.Base(strings.TrimSpace(credentialFile)),
		client:         &http.Client{Transport: newUpstreamTransport(90 * time.Second), CheckRedirect: checkCredentialRedirect},
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
			return nil, fmt.Errorf("no WorkBuddy credential %s; run `ccl oauth %s` again", s.credentialFile, s.deployment.provider)
		}
		return nil, fmt.Errorf("read WorkBuddy credential %s: %w", s.credentialFile, err)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode WorkBuddy credential %s: %w", s.credentialFile, err)
	}
	credentialType, _ := metadata["type"].(string)
	// A credential minted by the other deployment is a different account store:
	// refuse it rather than send its token to this origin.
	if !strings.EqualFold(strings.TrimSpace(credentialType), s.deployment.provider) {
		return nil, fmt.Errorf("credential %s belongs to %q, not %s", s.credentialFile, credentialType, s.deployment.provider)
	}
	disabled, _ := metadata["disabled"].(bool)
	credential := &workbuddyCredential{path: path, fileName: s.credentialFile, metadata: metadata, disabled: disabled}
	if !disabled && strings.TrimSpace(credential.token().AccessToken) == "" {
		return nil, fmt.Errorf("WorkBuddy credential %s has no access token", s.credentialFile)
	}
	return credential, nil
}

func (s *workbuddyCredentialStore) authorize(ctx context.Context, forceRefresh bool) (*workbuddyCredential, error) {
	release, err := lockOAuthCredential(ctx, filepath.Join(s.authDir, s.credentialFile))
	if err != nil {
		return nil, err
	}
	defer release()
	credential, err := s.load()
	if err != nil {
		return nil, err
	}
	if credential.disabled {
		return nil, fmt.Errorf("WorkBuddy credential %s is disabled", credential.fileName)
	}
	token := credential.token()
	forceRefresh = forceRefresh && oauthTokenStillRejected(ctx, token.AccessToken)
	if !forceRefresh && !workbuddyTokenNeedsRefresh(token) {
		return credential, nil
	}
	if strings.TrimSpace(token.RefreshToken) == "" {
		return nil, fmt.Errorf("WorkBuddy credential %s has expired and has no refresh token; run `ccl oauth %s` again", credential.fileName, s.deployment.provider)
	}
	refreshURL := strings.TrimRight(s.deployment.baseURL, "/") + "/v2/plugin/auth/token/refresh"
	headers := s.deployment.authenticatedHeaders(token, credential.account())
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

func (s *workbuddyCredentialStore) listAuths() []*AuthInfo {
	credential, err := s.load()
	if err != nil {
		return nil
	}
	status := StatusActive
	if credential.disabled {
		status = StatusDisabled
	}
	return []*AuthInfo{{
		ID:       credential.fileName,
		Provider: s.deployment.provider,
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
	key            string
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
	gatewayKey, err := sessionAPIKey()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("generate WorkBuddy gateway key: %w", err)
	}
	gateway := &workbuddyGateway{
		endpoint:       "http://" + listener.Addr().String(),
		key:            gatewayKey,
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
	if g.store != nil {
		closeIdleClients(g.store.client)()
	}
}

func (g *workbuddyGateway) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	if strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) != g.key {
		LogWarnEvent("request_rejected", "component", "workbuddy", "request_id", requestID,
			"path", request.URL.Path, "method", request.Method, "status", http.StatusUnauthorized,
			"reason", "invalid_gateway_key")
		writeWorkBuddyGatewayError(writer, http.StatusUnauthorized, "invalid gateway key")
		return
	}
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
	model := requestModelID(body)
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
	copyGatewayHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	copyGatewayResponse(writer, response.Body)
}

// do is the inner hop of the two-hop WorkBuddy path: this loopback gateway
// proxies to the real WorkBuddy API on behalf of the outer chat/responses
// services. The fast-retry loop in retry.go must NOT wrap this hop — the outer
// service already owns it, and nesting the two would retry 3×3 = 9 times.
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
	refreshed, refreshErr := g.store.authorize(withRejectedOAuthToken(ctx, credential.token().AccessToken), true)
	if refreshErr != nil {
		LogWarnEvent("upstream_auth_retry_skipped", "component", "workbuddy", "request_id", requestLogID(ctx),
			"path", path, "status", status, "credential", credential.fileName, "error", refreshErr)
		// Refresh is a WorkBuddy-specific recovery attempt. If it fails, retain
		// the original upstream status/body instead of turning a 401/403 into 502.
		return response, nil
	}
	closeGatewayResponse(response)
	return g.doOne(ctx, path, rawQuery, headers, body, refreshed)
}

func (g *workbuddyGateway) doOne(ctx context.Context, path, rawQuery string, headers http.Header, body []byte, credential *workbuddyCredential) (*http.Response, error) {
	target := strings.TrimRight(g.store.deployment.baseURL, "/") + path
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyGatewayHeaders(request.Header, headers)
	for _, name := range []string{"Authorization", "X-Api-Key", "Api-Key", "Host", "Content-Length", "User-Agent"} {
		request.Header.Del(name)
	}
	token := credential.token()
	account := credential.account()
	for name, values := range g.store.deployment.authenticatedHeaders(token, account) {
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
	request.Header.Set("X-Agent-Purpose", "conversation")
	request.Header.Set("X-IDE-Type", workbuddyChatChannel)
	request.Header.Set("X-IDE-Name", workbuddyChatChannel)
	request.Header.Set("X-IDE-Version", workbuddyClientVersion)
	request.Header.Set("X-Private-Data", "true")
	request.Header.Set("x-requested-with", "XMLHttpRequest")
	request.Header.Set("x-codebuddy-request", "1")
	model := requestModelID(body)
	LogDebugEvent("upstream_request", "component", "workbuddy", "request_id", requestLogID(ctx),
		"path", path, "model", model, "credential", credential.fileName, "conversation_id", g.conversationID)
	if len(body) > 0 {
		DebugHTTPBody(fmt.Sprintf("workbuddy request request_id=%s path=%s", requestLogID(ctx), path), body)
	}
	started := time.Now()
	response, err := doCredentialRequest(g.store.client, request)
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
	endpoint := strings.TrimRight(g.store.deployment.baseURL, "/") + "/v3/config"
	started := time.Now()
	envelope, err := workbuddyJSON[workbuddyProductConfig](ctx, g.store.client, http.MethodGet, endpoint, nil,
		g.store.deployment.authenticatedHeaders(credential.token(), credential.account()))
	var apiErr *workbuddyAPIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden) {
		refreshed, refreshErr := g.store.authorize(withRejectedOAuthToken(ctx, credential.token().AccessToken), true)
		if refreshErr == nil {
			credential = refreshed
			envelope, err = workbuddyJSON[workbuddyProductConfig](ctx, g.store.client, http.MethodGet, endpoint, nil,
				g.store.deployment.authenticatedHeaders(credential.token(), credential.account()))
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

// buildWorkBuddyRoutes maps the account catalog and the configured pool onto
// runtime routes. details carries the catalog's per-model credit multiplier so
// the model picker can show what each model costs.
func buildWorkBuddyRoutes(modelSpec string, catalog []workbuddyModel) ([]runtimeModelRoute, []string, map[string]string, map[string]protocol.ModelInfo) {
	lookup := make(map[string]workbuddyModel, len(catalog))
	routes := make([]runtimeModelRoute, 0, len(catalog)+len(runtimeModelRoutes(modelSpec)))
	models := make([]string, 0, len(catalog))
	names := make(map[string]string)
	details := make(map[string]protocol.ModelInfo)
	seenRoutes := make(map[string]bool)
	// aliasOwner keeps the first technical ID that claimed a client-facing
	// alias. Several catalog rows can share one display name ("Hy4 preview");
	// the later variant must not steal the alias, or the picker sends a name
	// that maps to an ID the chat API does not serve (11102).
	aliasOwner := make(map[string]string)
	add := func(name, alias string) {
		key := strings.ToLower(name) + "\x00" + strings.ToLower(alias)
		if seenRoutes[key] {
			return
		}
		ownerKey := strings.ToLower(alias)
		if owner, ok := aliasOwner[ownerKey]; ok && !strings.EqualFold(owner, name) {
			return
		}
		seenRoutes[key] = true
		aliasOwner[ownerKey] = name
		routes = append(routes, runtimeModelRoute{Name: name, Alias: alias})
	}
	for _, model := range catalog {
		lookup[strings.ToLower(model.ID)] = model
		models = append(models, model.ID)
		if name := strings.TrimSpace(model.Name); name != "" {
			names[model.ID] = name
		}
		if multiplier := workbuddyCreditMultiplier(model.Credits); multiplier != nil {
			details[strings.ToLower(model.ID)] = protocol.ModelInfo{
				ID:             model.ID,
				RateMultiplier: multiplier,
				RateUnit:       "credits",
			}
		}
		// Technical IDs are registered before display names so a shared label
		// cannot occupy another model's id.
		add(model.ID, model.ID)
	}
	for _, model := range catalog {
		name := strings.TrimSpace(model.Name)
		if name == "" || strings.EqualFold(name, model.ID) {
			continue
		}
		// The model picker sends display names ("快速") as request IDs; map
		// them back to the technical ID the upstream catalog expects.
		add(model.ID, name)
	}
	// The account catalog is authoritative: a saved model WorkBuddy has since
	// withdrawn must not stop the runtime, or the stale pool could never be
	// refreshed from the catalog. Requests for it reach upstream unchanged.
	for _, route := range runtimeModelRoutes(modelSpec) {
		model, ok := lookup[strings.ToLower(route.Name)]
		if !ok {
			LogWarnEvent("configured_model_not_in_catalog", "component", "workbuddy", "model", route.Name)
			continue
		}
		add(model.ID, route.Alias)
	}
	return routes, models, names, details
}

// workbuddyCreditMultiplier reads the catalog's `credits` field, which is a
// string like "x0.29" (or "x0.00" for a free model). Absent or unrecognizable
// values mean the catalog publishes no rate, so no marker is shown.
func workbuddyCreditMultiplier(credits any) *float64 {
	text, ok := credits.(string)
	if !ok {
		return nil
	}
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "x"), "X")
	multiplier, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || multiplier < 0 {
		return nil
	}
	return &multiplier
}

func startWorkBuddyOAuth(parent context.Context, providerName, modelSpec, credentialFile string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	deployment, err := workbuddyDeploymentFor(providerName)
	if err != nil {
		return nil, err
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	store := newWorkBuddyCredentialStore(deployment, authDir, credentialFile)
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
	routes, models, names, details := buildWorkBuddyRoutes(modelSpec, catalog)
	// The gateway verifies a per-runtime token on the internal hop before it
	// attaches the user's subscription credential.
	proxyRuntime, err := startOpenAIChatRuntimeRoutes(parent, gateway.endpoint, routes, &chatStaticAuthorizer{token: gateway.key})
	if err != nil {
		return nil, err
	}
	stopGateway = false
	proxyRuntime.cleanup = append(proxyRuntime.cleanup, gateway.Stop)
	proxyRuntime.listAuths = store.listAuths
	proxyRuntime.models = append([]string(nil), models...)
	proxyRuntime.modelNames = names
	proxyRuntime.modelDetails = details
	LogInfof("runtime start oauth provider=%s backend=%s protocol=openai_chat local_endpoint=%q credential_file=%s models=%d auth_owner=ccl catalog_owner=ccl data_plane=ccl",
		deployment.provider, deployment.provider, SafeLogEndpoint(proxyRuntime.endpoint), filepath.Base(credentialFile), len(models))
	return proxyRuntime, nil
}
