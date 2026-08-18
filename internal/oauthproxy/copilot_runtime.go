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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	copilotDefaultAPIBaseURL = "https://api.githubcopilot.com"
	copilotMaxBodyBytes      = int64(128 << 20)
	copilotMaxErrorBytes     = int64(1 << 20)
	// IDE tokens returned by /copilot_internal/v2/token are rejected without an
	// editor identity. Direct GitHub OAuth tokens deliberately omit it because
	// they use Copilot's supported third-party integration path.
	copilotIDETokenEditorVersion = "vscode/1.107.0"
)

var copilotAPIBaseURL = copilotDefaultAPIBaseURL

type copilotCredential struct {
	path        string
	fileName    string
	githubToken string
	metadata    map[string]any
	disabled    bool
}

type copilotCachedToken struct {
	token     string
	expiresAt time.Time
}

type copilotCredentialPool struct {
	authDir        string
	credentialFile string
	client         *http.Client
	next           atomic.Uint64
	tokenMu        sync.Mutex
	tokens         map[string]copilotCachedToken
}

func newCopilotCredentialPool(authDir, credentialFile string) *copilotCredentialPool {
	return &copilotCredentialPool{
		authDir:        authDir,
		credentialFile: strings.TrimSpace(credentialFile),
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			ResponseHeaderTimeout: 90 * time.Second,
		}},
		tokens: make(map[string]copilotCachedToken),
	}
}

func (p *copilotCredentialPool) load() ([]*copilotCredential, error) {
	entries, err := os.ReadDir(p.authDir)
	if err != nil {
		return nil, fmt.Errorf("read Copilot auth directory: %w", err)
	}
	credentials := make([]*copilotCredential, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		if p.credentialFile != "" && !strings.EqualFold(entry.Name(), filepath.Base(p.credentialFile)) {
			continue
		}
		path := filepath.Join(p.authDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read Copilot credential %s: %w", entry.Name(), err)
		}
		var metadata map[string]any
		if err := json.Unmarshal(raw, &metadata); err != nil {
			LogWarnf("skip malformed Copilot credential file %s: %v", entry.Name(), err)
			continue
		}
		credentialType, _ := metadata["type"].(string)
		if !strings.EqualFold(strings.TrimSpace(credentialType), ProviderCopilot) {
			continue
		}
		disabled, _ := metadata["disabled"].(bool)
		token := firstMetadataString(metadata, "github_token", "access_token", "token")
		if token == "" && !disabled {
			LogWarnf("skip Copilot credential file %s: missing GitHub token", entry.Name())
			continue
		}
		credentials = append(credentials, &copilotCredential{
			path:        path,
			fileName:    entry.Name(),
			githubToken: token,
			metadata:    metadata,
			disabled:    disabled,
		})
	}
	sort.Slice(credentials, func(i, j int) bool {
		return strings.ToLower(credentials[i].fileName) < strings.ToLower(credentials[j].fileName)
	})
	return credentials, nil
}

func activeCopilotCredentials(credentials []*copilotCredential) []*copilotCredential {
	active := make([]*copilotCredential, 0, len(credentials))
	for _, credential := range credentials {
		if credential == nil || credential.disabled || strings.TrimSpace(credential.githubToken) == "" {
			continue
		}
		active = append(active, credential)
	}
	return active
}

func (p *copilotCredentialPool) ordered() ([]*copilotCredential, error) {
	credentials, err := p.load()
	credentials = activeCopilotCredentials(credentials)
	if err != nil || len(credentials) < 2 {
		return credentials, err
	}
	start := int((p.next.Add(1) - 1) % uint64(len(credentials)))
	ordered := append([]*copilotCredential(nil), credentials[start:]...)
	ordered = append(ordered, credentials[:start]...)
	return ordered, nil
}

func (p *copilotCredentialPool) listAuths() []*AuthInfo {
	credentials, err := p.load()
	if err != nil {
		return nil
	}
	auths := make([]*AuthInfo, 0, len(credentials))
	for _, credential := range credentials {
		status := StatusActive
		if credential.disabled {
			status = StatusDisabled
		}
		auths = append(auths, &AuthInfo{
			ID:       credential.fileName,
			Provider: ProviderCopilot,
			FileName: credential.path,
			Label:    firstMetadataString(credential.metadata, "email", "login", "name"),
			Status:   status,
			Disabled: credential.disabled,
			Metadata: credential.metadata,
		})
	}
	return auths
}

func firstMetadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func (p *copilotCredentialPool) cachedToken(credential *copilotCredential) string {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	cached := p.tokens[credential.path]
	if cached.token == "" || (!cached.expiresAt.IsZero() && time.Now().Add(time.Minute).After(cached.expiresAt)) {
		delete(p.tokens, credential.path)
		return ""
	}
	return cached.token
}

func (p *copilotCredentialPool) invalidateToken(credential *copilotCredential) {
	p.tokenMu.Lock()
	delete(p.tokens, credential.path)
	p.tokenMu.Unlock()
}

func (p *copilotCredentialPool) exchangeToken(ctx context.Context, credential *copilotCredential) (string, error) {
	if token := p.cachedToken(credential); token != "" {
		return token, nil
	}
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	if cached := p.tokens[credential.path]; cached.token != "" &&
		(cached.expiresAt.IsZero() || time.Now().Add(time.Minute).Before(cached.expiresAt)) {
		return cached.token, nil
	}

	endpoint := strings.TrimRight(copilotGitHubAPIBaseURL, "/") + "/copilot_internal/v2/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+credential.githubToken)
	setCopilotClientHeaders(req.Header)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange GitHub Copilot token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, copilotMaxErrorBytes))
	if err != nil {
		return "", fmt.Errorf("read GitHub Copilot token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("exchange GitHub Copilot token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tokenResponse struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		ExpiresIn int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		return "", fmt.Errorf("decode GitHub Copilot token response: %w", err)
	}
	if strings.TrimSpace(tokenResponse.Token) == "" {
		return "", fmt.Errorf("exchange GitHub Copilot token: response has no token")
	}
	expiresAt := time.Time{}
	if tokenResponse.ExpiresAt > 0 {
		expiresAt = time.Unix(tokenResponse.ExpiresAt, 0)
	} else if tokenResponse.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(tokenResponse.ExpiresIn) * time.Second)
	}
	p.tokens[credential.path] = copilotCachedToken{token: tokenResponse.Token, expiresAt: expiresAt}
	return tokenResponse.Token, nil
}

type copilotGateway struct {
	endpoint string
	pool     *copilotCredentialPool
	server   *http.Server
	done     chan struct{}
}

func startCopilotGateway(parent context.Context, pool *copilotCredentialPool) (*copilotGateway, error) {
	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("start Copilot gateway listener: %w", err)
	}
	gateway := &copilotGateway{
		endpoint: "http://" + listener.Addr().String(),
		pool:     pool,
		done:     make(chan struct{}),
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
			LogErrorf("Copilot gateway stopped endpoint=%q error=%v", gateway.endpoint, err)
		}
		close(gateway.done)
	}()
	return gateway, nil
}

func (g *copilotGateway) Stop() {
	if g == nil || g.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeStopTimeout)
	_ = g.server.Shutdown(ctx)
	cancel()
	waitClosed(g.done, runtimeStopTimeout)
}

func (g *copilotGateway) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		LogWarnEvent("request_rejected", "component", "copilot", "request_id", requestID,
			"path", request.URL.Path, "method", request.Method, "status", http.StatusMethodNotAllowed,
			"reason", "unsupported_method")
		writeCopilotGatewayError(writer, http.StatusMethodNotAllowed, "unsupported method")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, copilotMaxBodyBytes+1))
	if err != nil {
		LogWarnEvent("request_rejected", "component", "copilot", "request_id", requestID,
			"path", request.URL.Path, "method", request.Method, "status", http.StatusBadRequest,
			"reason", "read_body", "error", err)
		writeCopilotGatewayError(writer, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	if int64(len(body)) > copilotMaxBodyBytes {
		LogWarnEvent("request_rejected", "component", "copilot", "request_id", requestID,
			"path", request.URL.Path, "method", request.Method, "status", http.StatusRequestEntityTooLarge,
			"reason", "body_too_large", "body_bytes", len(body), "limit_bytes", copilotMaxBodyBytes)
		writeCopilotGatewayError(writer, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	model := copilotRequestModel(body)
	LogDebugEvent("request_received", "component", "copilot", "request_id", requestID,
		"method", request.Method, "path", request.URL.Path, "model", model, "body_bytes", len(body))
	response, err := g.do(requestCtx, request.Method, copilotUpstreamPath(request.URL.Path), request.URL.RawQuery, request.Header, body)
	if err != nil {
		LogErrorEvent("request_failed", "component", "copilot", "request_id", requestID,
			"path", request.URL.Path, "model", model, "status", http.StatusBadGateway,
			"duration", logDuration(started), "error", err)
		writeCopilotGatewayError(writer, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()
	LogUpstreamEvent(response.StatusCode, "request_complete", "component", "copilot", "request_id", requestID,
		"path", request.URL.Path, "model", model, "status", response.StatusCode,
		"retry_after", response.Header.Get("Retry-After"), "duration", logDuration(started))
	copyCopilotHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	copyCopilotResponse(writer, response.Body)
}

func (g *copilotGateway) do(ctx context.Context, method, path, rawQuery string, headers http.Header, body []byte) (*http.Response, error) {
	credentials, err := g.pool.ordered()
	if err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("no active Copilot credentials")
	}
	var lastResponse *http.Response
	var lastErr error
	for index, credential := range credentials {
		attempt := index + 1
		token := g.pool.cachedToken(credential)
		exchanged := token != ""
		if token == "" {
			token = credential.githubToken
		}
		LogDebugEvent("upstream_attempt", "component", "copilot", "request_id", requestLogID(ctx),
			"attempt", attempt, "credential_count", len(credentials), "credential", credential.fileName,
			"path", path, "token_kind", map[bool]string{true: "ide", false: "github"}[exchanged])
		response, requestErr := g.doOne(ctx, method, path, rawQuery, headers, body, token, exchanged, credential.fileName)
		if requestErr != nil {
			LogWarnEvent("upstream_attempt_failed", "component", "copilot", "request_id", requestLogID(ctx),
				"attempt", attempt, "credential_count", len(credentials), "credential", credential.fileName,
				"path", path, "error", requestErr)
			lastErr = requestErr
			continue
		}
		if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && !exchanged {
			closeCopilotResponse(response)
			LogWarnEvent("credential_token_exchange", "component", "copilot", "request_id", requestLogID(ctx),
				"attempt", attempt, "credential", credential.fileName, "status", response.StatusCode)
			token, exchangeErr := g.pool.exchangeToken(ctx, credential)
			if exchangeErr != nil {
				LogWarnEvent("credential_token_exchange_failed", "component", "copilot", "request_id", requestLogID(ctx),
					"attempt", attempt, "credential", credential.fileName, "error", exchangeErr)
				lastErr = exchangeErr
				continue
			}
			response, requestErr = g.doOne(ctx, method, path, rawQuery, headers, body, token, true, credential.fileName)
			if requestErr != nil {
				LogWarnEvent("upstream_attempt_failed", "component", "copilot", "request_id", requestLogID(ctx),
					"attempt", attempt, "credential_count", len(credentials), "credential", credential.fileName,
					"path", path, "phase", "after_token_exchange", "error", requestErr)
				lastErr = requestErr
				continue
			}
			exchanged = true
		}
		if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && exchanged {
			g.pool.invalidateToken(credential)
			LogWarnEvent("credential_rejected", "component", "copilot", "request_id", requestLogID(ctx),
				"attempt", attempt, "credential_count", len(credentials), "credential", credential.fileName,
				"status", response.StatusCode, "action", "invalidate_and_try_next")
			if lastResponse != nil {
				closeCopilotResponse(lastResponse)
			}
			lastResponse = response
			continue
		}
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
			action := "return_last_response"
			if attempt < len(credentials) {
				action = "try_next_credential"
			}
			LogUpstreamEvent(response.StatusCode, "upstream_retry_decision", "component", "copilot", "request_id", requestLogID(ctx),
				"attempt", attempt, "credential_count", len(credentials), "credential", credential.fileName,
				"status", response.StatusCode, "retry_after", response.Header.Get("Retry-After"),
				"action", action)
			if lastResponse != nil {
				closeCopilotResponse(lastResponse)
			}
			lastResponse = response
			continue
		}
		if lastResponse != nil {
			closeCopilotResponse(lastResponse)
		}
		return response, nil
	}
	if lastResponse != nil {
		return lastResponse, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all Copilot credentials failed")
	}
	return nil, lastErr
}

func (g *copilotGateway) doOne(ctx context.Context, method, path, rawQuery string, headers http.Header, body []byte, token string, ideToken bool, credentialID string) (*http.Response, error) {
	target := strings.TrimRight(copilotAPIBaseURL, "/") + path
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyCopilotHeaders(req.Header, headers)
	for _, name := range []string{"Authorization", "X-Api-Key", "Api-Key", "Host", "Content-Length"} {
		req.Header.Del(name)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	setCopilotClientHeaders(req.Header)
	if ideToken {
		req.Header.Set("Editor-Version", copilotIDETokenEditorVersion)
	} else {
		req.Header.Del("Editor-Version")
	}
	req.Header.Set("Openai-Intent", "conversation-edits")
	req.Header.Set("X-Initiator", copilotInitiator(body))
	if copilotVisionRequest(body) {
		req.Header.Set("Copilot-Vision-Request", "true")
	}
	if req.Header.Get("Content-Type") == "" && len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	LogDebugEvent("upstream_request", "component", "copilot", "request_id", requestLogID(ctx),
		"method", method, "path", path, "model", copilotRequestModel(body), "credential", credentialID)
	if len(body) > 0 {
		DebugHTTPBody(fmt.Sprintf("copilot request request_id=%s path=%s", requestLogID(ctx), path), body)
	}
	started := time.Now()
	response, err := g.pool.client.Do(req)
	if err != nil {
		return nil, err
	}
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "copilot", "request_id", requestLogID(ctx),
		"path", path, "model", copilotRequestModel(body), "credential", credentialID,
		"status", response.StatusCode, "retry_after", response.Header.Get("Retry-After"),
		"duration", logDuration(started))
	debugCopilotFailureBody(ctx, path, response)
	return response, nil
}

func setCopilotClientHeaders(headers http.Header) {
	headers.Set("User-Agent", "ccl/1.0")
	headers.Set("X-GitHub-Api-Version", "2026-06-01")
}

func debugCopilotFailureBody(ctx context.Context, path string, response *http.Response) {
	if response == nil || response.Body == nil || response.StatusCode < 400 || !LogDebugEnabled() {
		return
	}
	prefix, err := io.ReadAll(io.LimitReader(response.Body, copilotMaxErrorBytes))
	if err != nil {
		LogDebugEvent("upstream_error_body_read_failed", "component", "copilot", "request_id", requestLogID(ctx),
			"path", path, "status", response.StatusCode, "error", err)
		return
	}
	DebugHTTPBody(fmt.Sprintf("copilot response request_id=%s path=%s status=%d", requestLogID(ctx), path, response.StatusCode), prefix)
	response.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.MultiReader(bytes.NewReader(prefix), response.Body), Closer: response.Body}
}

func copilotUpstreamPath(path string) string {
	switch path {
	case "/v1/responses":
		return "/responses"
	case "/v1/chat/completions":
		return "/chat/completions"
	case "/v1/models":
		return "/models"
	default:
		return path
	}
}

func copilotInitiator(body []byte) string {
	var payload struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Input []struct {
			Role string `json:"role"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return "user"
	}
	if len(payload.Messages) > 0 && !strings.EqualFold(payload.Messages[len(payload.Messages)-1].Role, "user") {
		return "agent"
	}
	if len(payload.Input) > 0 && !strings.EqualFold(payload.Input[len(payload.Input)-1].Role, "user") {
		return "agent"
	}
	return "user"
}

func copilotVisionRequest(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte(`"image_url"`)) ||
		bytes.Contains(lower, []byte(`"input_image"`)) ||
		bytes.Contains(lower, []byte(`"type":"image"`))
}

func copilotRequestModel(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Model
}

func copyCopilotHeaders(target, source http.Header) {
	for name, values := range source {
		if copilotHopByHopHeader(name) {
			continue
		}
		for _, value := range values {
			target.Add(name, value)
		}
	}
}

func copilotHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func copyCopilotResponse(writer http.ResponseWriter, body io.Reader) {
	buffer := make([]byte, 32<<10)
	flusher, _ := writer.(http.Flusher)
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			_, _ = writer.Write(buffer[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func closeCopilotResponse(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
}

func writeCopilotGatewayError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{"type": "copilot_error", "message": message},
	})
}

type copilotModel struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	ModelPickerEnabled *bool    `json:"model_picker_enabled"`
	SupportedEndpoints []string `json:"supported_endpoints"`
	Policy             struct {
		State string `json:"state"`
	} `json:"policy"`
}

type copilotModelsResponse struct {
	Data []copilotModel `json:"data"`
}

func (g *copilotGateway) discoverModels(ctx context.Context) ([]copilotModel, error) {
	response, err := g.do(ctx, http.MethodGet, "/models", "", http.Header{"Accept": {"application/json"}}, nil)
	if err != nil {
		return nil, fmt.Errorf("discover GitHub Copilot models: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, copilotMaxErrorBytes))
	if err != nil {
		return nil, fmt.Errorf("read GitHub Copilot models: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("discover GitHub Copilot models: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var catalog copilotModelsResponse
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("decode GitHub Copilot models: %w", err)
	}
	models := filterCopilotModels(catalog.Data)
	if len(models) == 0 {
		return nil, fmt.Errorf("discover GitHub Copilot models: no selectable inference models returned")
	}
	return models, nil
}

func filterCopilotModels(models []copilotModel) []copilotModel {
	selectable := make([]copilotModel, 0, len(models))
	fallback := make([]copilotModel, 0, len(models))
	seen := make(map[string]bool)
	for _, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			model.ID = strings.TrimSpace(model.Name)
		}
		if model.ID == "" || seen[strings.ToLower(model.ID)] ||
			strings.EqualFold(strings.TrimSpace(model.Policy.State), "disabled") ||
			copilotModelProtocol(model) == "" {
			continue
		}
		seen[strings.ToLower(model.ID)] = true
		fallback = append(fallback, model)
		if model.ModelPickerEnabled == nil || *model.ModelPickerEnabled {
			selectable = append(selectable, model)
		}
	}
	if len(selectable) > 0 {
		return selectable
	}
	return fallback
}

func copilotModelProtocol(model copilotModel) string {
	for _, endpoint := range model.SupportedEndpoints {
		if strings.Contains(strings.ToLower(endpoint), "messages") {
			return "anthropic"
		}
	}
	for _, endpoint := range model.SupportedEndpoints {
		if strings.Contains(strings.ToLower(endpoint), "responses") {
			return "responses"
		}
	}
	for _, endpoint := range model.SupportedEndpoints {
		if strings.Contains(strings.ToLower(endpoint), "chat") {
			return "chat"
		}
	}
	if len(model.SupportedEndpoints) == 0 {
		return "chat"
	}
	return ""
}

type copilotRouteSet struct {
	chat      []runtimeModelRoute
	responses []runtimeModelRoute
	anthropic []runtimeModelRoute
	models    []string
}

func buildCopilotRoutes(modelSpec string, catalog []copilotModel) (copilotRouteSet, error) {
	byID := make(map[string]copilotModel, len(catalog))
	for _, model := range catalog {
		byID[strings.ToLower(model.ID)] = model
	}
	// Publish every selectable upstream model. Configured model specs add aliases
	// but never narrow the authoritative catalog exposed by this runtime.
	routes := make([]runtimeModelRoute, 0, len(catalog)+len(runtimeModelRoutes(modelSpec)))
	for _, model := range catalog {
		routes = append(routes, runtimeModelRoute{Name: model.ID, Alias: model.ID})
	}
	configuredRoutes := runtimeModelRoutes(modelSpec)
	routes = append(routes, configuredRoutes...)
	result := copilotRouteSet{models: make([]string, 0, len(catalog))}
	for _, model := range catalog {
		result.models = append(result.models, model.ID)
	}
	seenRoutes := make(map[string]bool)
	missing := make([]string, 0)
	for index, route := range routes {
		model, ok := byID[strings.ToLower(route.Name)]
		if !ok {
			if index >= len(catalog) {
				missing = append(missing, route.Name)
			}
			continue
		}
		protocol := copilotModelProtocol(model)
		key := strings.ToLower(protocol + "\x00" + model.ID + "\x00" + route.Alias)
		if seenRoutes[key] {
			continue
		}
		seenRoutes[key] = true
		switch protocol {
		case "anthropic":
			result.anthropic = append(result.anthropic, runtimeModelRoute{Name: model.ID, Alias: route.Alias})
		case "responses":
			result.responses = append(result.responses, runtimeModelRoute{Name: model.ID, Alias: route.Alias})
		case "chat":
			result.chat = append(result.chat, runtimeModelRoute{Name: model.ID, Alias: route.Alias})
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return copilotRouteSet{}, fmt.Errorf("GitHub Copilot does not expose configured model(s): %s", strings.Join(missing, ", "))
	}
	if len(result.chat)+len(result.responses)+len(result.anthropic) == 0 {
		return copilotRouteSet{}, fmt.Errorf("GitHub Copilot returned no usable model routes")
	}
	return result, nil
}

func startCopilotOAuth(parent context.Context, modelSpec, credentialFile string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	pool := newCopilotCredentialPool(authDir, credentialFile)
	credentials, err := pool.load()
	if err != nil {
		return nil, err
	}
	credentials = activeCopilotCredentials(credentials)
	if len(credentials) == 0 {
		return nil, fmt.Errorf("no %s credentials found; run `ccl oauth %s` first", ProviderCopilot, ProviderCopilot)
	}

	gateway, err := startCopilotGateway(parent, pool)
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
	routes, err := buildCopilotRoutes(modelSpec, catalog)
	if err != nil {
		return nil, err
	}

	proxyRuntime, err := startCopilotProtocolRouter(parent, gateway, routes, pool)
	if err != nil {
		return nil, err
	}
	stopGateway = false
	proxyRuntime.cleanup = append(proxyRuntime.cleanup, gateway.Stop)
	proxyRuntime.listAuths = pool.listAuths
	proxyRuntime.models = append([]string(nil), routes.models...)
	LogInfof("runtime start oauth provider=copilot backend=copilot protocol=mixed local_endpoint=%q credential_file=%s models_chat=%d models_responses=%d models_anthropic=%d responses_owner=ccl",
		SafeLogEndpoint(proxyRuntime.endpoint), filepath.Base(credentialFile), len(routes.chat), len(routes.responses), len(routes.anthropic))
	return proxyRuntime, nil
}

// copilotProtocolRouter serves Copilot's mixed catalog entirely on CCL-owned
// data planes: Responses models on the Codex adapter, Chat models on the Chat
// Completions adapter, and native Anthropic models on the Messages passthrough.
type copilotProtocolRouter struct {
	apiKey       string
	models       []string
	responses    map[string]bool
	chat         map[string]bool
	anthropic    map[string]bool
	codex        *codexResponsesService
	chatSvc      *chatCompletionsService
	anthropicSvc *anthropicPassthroughService
}

func startCopilotProtocolRouter(parent context.Context, gateway *copilotGateway, routes copilotRouteSet, pool *copilotCredentialPool) (*Runtime, error) {
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("listen for Copilot protocol router: %w", err)
	}
	responseRoutes := make([]runtimeModelRoute, 0, len(routes.responses))
	responseModels := make(map[string]bool, len(routes.responses))
	for _, route := range routes.responses {
		alias := route.Alias
		if strings.TrimSpace(alias) == "" {
			alias = route.Name
		}
		responseRoutes = append(responseRoutes, runtimeModelRoute{Name: route.Name, Alias: alias})
		responseModels[strings.ToLower(alias)] = true
	}
	chatRoutes := make([]runtimeModelRoute, 0, len(routes.chat))
	chatModels := make(map[string]bool, len(routes.chat))
	for _, route := range routes.chat {
		alias := route.Alias
		if strings.TrimSpace(alias) == "" {
			alias = route.Name
		}
		chatRoutes = append(chatRoutes, runtimeModelRoute{Name: route.Name, Alias: alias})
		chatModels[strings.ToLower(alias)] = true
	}
	anthropicRoutes := make([]runtimeModelRoute, 0, len(routes.anthropic))
	anthropicModels := make(map[string]bool, len(routes.anthropic))
	for _, route := range routes.anthropic {
		alias := route.Alias
		if strings.TrimSpace(alias) == "" {
			alias = route.Name
		}
		anthropicRoutes = append(anthropicRoutes, runtimeModelRoute{Name: route.Name, Alias: alias})
		anthropicModels[strings.ToLower(alias)] = true
	}
	usage := NewUsageTracker()
	router := &copilotProtocolRouter{
		apiKey: apiKey, models: append([]string(nil), routes.models...), responses: responseModels,
		chat: chatModels, anthropic: anthropicModels,
	}
	if len(responseRoutes) > 0 {
		router.codex = newCodexResponsesService(apiKey, gateway.endpoint, responseRoutes, &codexStaticAuthorizer{token: "copilot"}, usage)
	}
	if len(chatRoutes) > 0 {
		router.chatSvc = newChatCompletionsServiceWithAuthorizer(apiKey, gateway.endpoint, chatRoutes, &chatStaticAuthorizer{token: "copilot"}, usage)
	}
	if len(anthropicRoutes) > 0 {
		anthropicNames := make([]string, 0, len(anthropicRoutes))
		for _, route := range anthropicRoutes {
			anthropicNames = append(anthropicNames, route.Alias)
		}
		router.anthropicSvc = newAnthropicPassthroughService(apiKey, gateway.endpoint, anthropicNames, &chatStaticAuthorizer{token: "copilot"}, usage)
	}
	runCtx, cancel := context.WithCancel(parent)
	server := &http.Server{
		Handler: router.handler(), ReadHeaderTimeout: 15 * time.Second,
		BaseContext: func(net.Listener) context.Context { return runCtx },
	}
	started := make(chan struct{})
	close(started)
	proxyRuntime := &Runtime{
		endpoint: "http://" + listener.Addr().String() + "/v1", apiKey: apiKey,
		httpServer: server, cancel: cancel, done: make(chan struct{}), runErr: make(chan error, 1),
		started: started, usage: usage, listAuths: pool.listAuths,
	}
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		proxyRuntime.runErr <- err
		close(proxyRuntime.done)
	}()
	go func() {
		select {
		case <-runCtx.Done():
			ctx, stop := context.WithTimeout(context.Background(), runtimeStopTimeout)
			_ = server.Shutdown(ctx)
			stop()
		case <-proxyRuntime.done:
		}
	}()
	return proxyRuntime, nil
}

func (r *copilotProtocolRouter) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/models", r.handleModels)
	mux.HandleFunc("/models", r.handleModels)
	mux.HandleFunc("/v1/messages", r.handleMessages)
	mux.HandleFunc("/messages", r.handleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", r.handleCountTokens)
	mux.HandleFunc("/messages/count_tokens", r.handleCountTokens)
	return mux
}

func (r *copilotProtocolRouter) authorized(request *http.Request) bool {
	if request.Header.Get("x-api-key") == r.apiKey {
		return true
	}
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) == r.apiKey
}

func (r *copilotProtocolRouter) handleModels(writer http.ResponseWriter, request *http.Request) {
	if !r.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodGet {
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	data := make([]map[string]any, 0, len(r.models))
	for _, model := range r.models {
		data = append(data, map[string]any{"id": model, "object": "model", "type": "model"})
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": data})
}

func (r *copilotProtocolRouter) handleCountTokens(writer http.ResponseWriter, request *http.Request) {
	if !r.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, copilotMaxBodyBytes+1))
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if int64(len(body)) > copilotMaxBodyBytes {
		writeAnthropicError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	lower := strings.ToLower(copilotRequestModel(body))
	if r.codex != nil && r.responses[lower] {
		r.codex.handleCountTokens(writer, request)
		return
	}
	if r.chatSvc != nil && r.chat[lower] {
		r.chatSvc.handleCountTokens(writer, request)
		return
	}
	if r.anthropicSvc != nil && r.anthropic[lower] {
		r.anthropicSvc.handleCountTokens(writer, request)
		return
	}
	writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "model is not routed to a supported Copilot protocol")
}

func (r *copilotProtocolRouter) handleMessages(writer http.ResponseWriter, request *http.Request) {
	if !r.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, copilotMaxBodyBytes+1))
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if int64(len(body)) > copilotMaxBodyBytes {
		writeAnthropicError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	model := copilotRequestModel(body)
	lower := strings.ToLower(model)
	if r.codex != nil && r.responses[lower] {
		LogDebugEvent("protocol_route", "component", "copilot", "model", model, "protocol", "codex_responses", "owner", "ccl")
		r.codex.handleMessages(writer, request)
		return
	}
	if r.chatSvc != nil && r.chat[lower] {
		LogDebugEvent("protocol_route", "component", "copilot", "model", model, "protocol", "openai_chat", "owner", "ccl")
		r.chatSvc.handleMessages(writer, request)
		return
	}
	if r.anthropicSvc != nil && r.anthropic[lower] {
		LogDebugEvent("protocol_route", "component", "copilot", "model", model, "protocol", "anthropic_messages", "owner", "ccl")
		r.anthropicSvc.handleMessages(writer, request)
		return
	}
	writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "model is not routed to a supported Copilot protocol")
}
