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

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"gopkg.in/yaml.v3"
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
	authDir         string
	credentialFiles map[string]struct{}
	restrictToFiles bool
	resolver        func() ([]string, error)
	client          *http.Client
	next            atomic.Uint64
	tokenMu         sync.Mutex
	tokens          map[string]copilotCachedToken
}

func newCopilotCredentialPool(authDir string, credentialFiles []string, restrictToFiles bool, resolver func() ([]string, error)) *copilotCredentialPool {
	return &copilotCredentialPool{
		authDir:         authDir,
		credentialFiles: credentialFileSet(credentialFiles),
		restrictToFiles: restrictToFiles,
		resolver:        resolver,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			ResponseHeaderTimeout: 90 * time.Second,
		}},
		tokens: make(map[string]copilotCachedToken),
	}
}

func (p *copilotCredentialPool) selectedFiles() (map[string]struct{}, error) {
	if p.resolver != nil {
		files, err := p.resolver()
		if err != nil {
			return nil, err
		}
		return credentialFileSet(files), nil
	}
	selected := make(map[string]struct{}, len(p.credentialFiles))
	for file := range p.credentialFiles {
		selected[file] = struct{}{}
	}
	return selected, nil
}

func (p *copilotCredentialPool) load() ([]*copilotCredential, error) {
	selected, err := p.selectedFiles()
	if err != nil {
		return nil, err
	}
	if p.restrictToFiles && len(selected) == 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(p.authDir)
	if err != nil {
		return nil, fmt.Errorf("read Copilot auth directory: %w", err)
	}
	credentials := make([]*copilotCredential, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		if p.restrictToFiles {
			if _, ok := selected[strings.ToLower(entry.Name())]; !ok {
				continue
			}
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

func (p *copilotCredentialPool) listAuths() []*coreauth.Auth {
	credentials, err := p.load()
	if err != nil {
		return nil
	}
	auths := make([]*coreauth.Auth, 0, len(credentials))
	for _, credential := range credentials {
		status := coreauth.StatusActive
		if credential.disabled {
			status = coreauth.StatusDisabled
		}
		auths = append(auths, &coreauth.Auth{
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
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		writeCopilotGatewayError(writer, http.StatusMethodNotAllowed, "unsupported method")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, copilotMaxBodyBytes+1))
	if err != nil {
		writeCopilotGatewayError(writer, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	if int64(len(body)) > copilotMaxBodyBytes {
		writeCopilotGatewayError(writer, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	response, err := g.do(request.Context(), request.Method, copilotUpstreamPath(request.URL.Path), request.URL.RawQuery, request.Header, body)
	if err != nil {
		LogErrorf("Copilot gateway request failed path=%q error=%v", request.URL.Path, err)
		writeCopilotGatewayError(writer, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()
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
	for _, credential := range credentials {
		token := g.pool.cachedToken(credential)
		exchanged := token != ""
		if token == "" {
			token = credential.githubToken
		}
		response, requestErr := g.doOne(ctx, method, path, rawQuery, headers, body, token, exchanged)
		if requestErr != nil {
			lastErr = requestErr
			continue
		}
		if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && !exchanged {
			closeCopilotResponse(response)
			token, exchangeErr := g.pool.exchangeToken(ctx, credential)
			if exchangeErr != nil {
				lastErr = exchangeErr
				continue
			}
			response, requestErr = g.doOne(ctx, method, path, rawQuery, headers, body, token, true)
			if requestErr != nil {
				lastErr = requestErr
				continue
			}
			exchanged = true
		}
		if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && exchanged {
			g.pool.invalidateToken(credential)
			if lastResponse != nil {
				closeCopilotResponse(lastResponse)
			}
			lastResponse = response
			continue
		}
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
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

func (g *copilotGateway) doOne(ctx context.Context, method, path, rawQuery string, headers http.Header, body []byte, token string, ideToken bool) (*http.Response, error) {
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
	LogDebugf("Copilot upstream request method=%s path=%q model=%q", method, path, copilotRequestModel(body))
	if len(body) > 0 {
		DebugHTTPBody("copilot request "+path, body)
	}
	response, err := g.pool.client.Do(req)
	if err != nil {
		return nil, err
	}
	LogUpstreamStatusf(response.StatusCode, "Copilot upstream response path=%q status=%d", path, response.StatusCode)
	debugCopilotFailureBody(path, response)
	return response, nil
}

func setCopilotClientHeaders(headers http.Header) {
	headers.Set("User-Agent", "ccl/1.0")
	headers.Set("X-GitHub-Api-Version", "2026-06-01")
}

func debugCopilotFailureBody(path string, response *http.Response) {
	if response == nil || response.Body == nil || response.StatusCode < 400 || !LogDebugEnabled() {
		return
	}
	prefix, err := io.ReadAll(io.LimitReader(response.Body, copilotMaxErrorBytes))
	if err != nil {
		LogDebugf("Copilot failed response payload read path=%q status=%d error=%v", path, response.StatusCode, err)
		return
	}
	DebugHTTPBody(fmt.Sprintf("copilot response %s status=%d", path, response.StatusCode), prefix)
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
	chat      []runtimeOpenAIModel
	responses []runtimeCodexModel
	anthropic []runtimeClaudeModel
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
			result.anthropic = append(result.anthropic, runtimeClaudeModel{Name: model.ID, Alias: route.Alias, ForceMapping: true})
		case "responses":
			result.responses = append(result.responses, runtimeCodexModel{Name: model.ID, Alias: route.Alias})
		case "chat":
			result.chat = append(result.chat, runtimeOpenAIModel{Name: model.ID, Alias: route.Alias, ForceMapping: true})
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

type runtimeClaudeKey struct {
	APIKey  string               `yaml:"api-key"`
	BaseURL string               `yaml:"base-url"`
	Models  []runtimeClaudeModel `yaml:"models"`
	Headers map[string]string    `yaml:"headers,omitempty"`
}

type runtimeClaudeModel struct {
	Name         string `yaml:"name"`
	Alias        string `yaml:"alias"`
	ForceMapping bool   `yaml:"force-mapping,omitempty"`
}

type runtimeCopilotConfigFile struct {
	runtimeConfigBase      `yaml:",inline"`
	CodexAPIKey            []runtimeCodexKey            `yaml:"codex-api-key,omitempty"`
	OpenAICompatibility    []runtimeOpenAICompatibility `yaml:"openai-compatibility,omitempty"`
	ClaudeAPIKey           []runtimeClaudeKey           `yaml:"claude-api-key,omitempty"`
	DisableClaudeCloakMode bool                         `yaml:"disable-claude-cloak-mode"`
}

func startCopilotOAuthWithFiles(parent context.Context, modelSpec string, credentialFiles []string, restrictToFiles bool, resolver func() ([]string, error)) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	pool := newCopilotCredentialPool(authDir, credentialFiles, restrictToFiles, resolver)
	credentials, err := pool.load()
	if err != nil {
		return nil, err
	}
	credentials = activeCopilotCredentials(credentials)
	if len(credentials) == 0 {
		if restrictToFiles && len(credentialFiles) == 0 {
			return nil, fmt.Errorf("OAuth group for %s has no credentials; edit it with `ccl oauth group` or run `ccl oauth sync`", ProviderCopilot)
		}
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

	runtimeDir, port, apiKey, err := prepareAPIKeyRuntime()
	if err != nil {
		return nil, err
	}
	cleanupRuntimeDir := true
	defer func() {
		if cleanupRuntimeDir {
			_ = os.RemoveAll(runtimeDir)
		}
	}()

	configFile := runtimeCopilotConfigFile{
		runtimeConfigBase:      newRuntimeConfigBase(port, runtimeDir, apiKey),
		DisableClaudeCloakMode: true,
	}
	var compat *responsesCompatibilityProxy
	if len(routes.responses) > 0 {
		compat, err = startResponsesCompatibilityProxy(gateway.endpoint, nil, 0)
		if err != nil {
			return nil, err
		}
		defer func() {
			if stopGateway && compat != nil {
				compat.Stop()
			}
		}()
		configFile.CodexAPIKey = []runtimeCodexKey{{APIKey: "copilot", BaseURL: compat.endpoint, Models: routes.responses}}
	}
	if len(routes.chat) > 0 {
		configFile.OpenAICompatibility = []runtimeOpenAICompatibility{{
			Name: "github-copilot", BaseURL: gateway.endpoint,
			APIKeyEntries: []runtimeOpenAICompatibilityKey{{APIKey: "copilot"}},
			Models:        routes.chat,
		}}
	}
	if len(routes.anthropic) > 0 {
		configFile.ClaudeAPIKey = []runtimeClaudeKey{{APIKey: "copilot", BaseURL: gateway.endpoint, Models: routes.anthropic}}
	}
	rawConfig, err := yaml.Marshal(configFile)
	if err != nil {
		return nil, fmt.Errorf("encode GitHub Copilot runtime config: %w", err)
	}
	proxyRuntime, err := startAPIKeyRuntime(parent, rawConfig, apiKey, runtimeDir)
	if err != nil {
		return nil, err
	}
	cleanupRuntimeDir = false
	stopGateway = false
	proxyRuntime.responsesCompat = compat
	proxyRuntime.copilotGateway = gateway
	proxyRuntime.listAuths = pool.listAuths
	proxyRuntime.models = append([]string(nil), routes.models...)
	LogInfof("runtime start oauth provider=copilot backend=copilot protocol=mixed port=%d credential_files=%d restricted=%t models_chat=%d models_responses=%d models_anthropic=%d",
		port, len(credentialFiles), restrictToFiles, len(routes.chat), len(routes.responses), len(routes.anthropic))
	return proxyRuntime, nil
}
