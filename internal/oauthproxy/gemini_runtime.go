package oauthproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const (
	// backendAntigravity is the CLIProxyAPI authenticator provider key for the
	// Google Gemini subscription (Antigravity control plane).
	backendAntigravity = "antigravity"

	antigravityStreamPath   = "/v1internal:streamGenerateContent"
	antigravityGeneratePath = "/v1internal:generateContent"

	// antigravityOAuthClientID/Secret are the Google OAuth client identifiers CPA's
	// antigravity authenticator uses, so the refresh flow can mint tokens for the
	// same backend.
	antigravityOAuthClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	antigravityOAuthClientSecret = "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"

	// antigravityMaxErrorBytes bounds an upstream error body we drain before
	// surfacing a failure, and antigravityMaxResponseBytes bounds a non-streaming
	// upstream body.
	antigravityMaxErrorBytes    = int64(1 << 20)
	antigravityMaxResponseBytes = int64(64 << 20)

	// antigravityRequestUserAgent mirrors CPA's resolveUserAgent for a request
	// with no configured override: the Antigravity Hub family UA at the fallback
	// version (the version updater is not run by CCL).
	antigravityRequestUserAgent = "antigravity/hub/2.2.1 darwin/arm64"
)

var (
	// antigravityBaseURLDaily/Prod are the Antigravity control-plane bases in
	// fallback order. Vars (not consts) so tests can point them at a stub.
	antigravityBaseURLDaily = "https://daily-cloudcode-pa.googleapis.com"
	antigravityBaseURLProd  = "https://cloudcode-pa.googleapis.com"
	// antigravityTokenURL is the Google OAuth refresh endpoint. A var so tests can
	// stub it.
	antigravityTokenURL = "https://oauth2.googleapis.com/token"
)

var (
	antigravityTransportOnce sync.Once
	antigravityTransport     *http.Transport
)

// antigravityHTTPTransport returns the shared HTTP/1.1-only transport used for
// Antigravity requests. Antigravity rejects HTTP/2, so ALPN is pinned to
// http/1.1 and ForceAttemptHTTP2 is disabled, mirroring CPA's
// cloneTransportWithHTTP11.
func antigravityHTTPTransport() *http.Transport {
	antigravityTransportOnce.Do(func() {
		antigravityTransport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     false,
			TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
			TLSClientConfig:       &tls.Config{NextProtos: []string{"http/1.1"}},
			ResponseHeaderTimeout: 90 * time.Second,
		}
	})
	return antigravityTransport
}

// resolveAntigravityHost derives the Host header value from a base URL,
// mirroring CPA's resolveHost.
func resolveAntigravityHost(base string) string {
	parsed, err := url.Parse(base)
	if err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

// antigravityOAuthAuthorizer resolves and refreshes a Gemini/Antigravity OAuth
// credential written by CPA's antigravity authenticator during `ccl oauth gemini`.
// It reads the fields CPA's executor reads and refreshes against Google's OAuth
// endpoint with the Antigravity client credentials (form-encoded, unlike the
// Claude subscription's JSON body).
type antigravityOAuthAuthorizer struct {
	path   string
	client *http.Client
	mu     sync.Mutex
}

type antigravityOAuthCredential struct {
	metadata     map[string]any
	accessToken  string
	refreshToken string
	email        string
	projectID    string
	expiresAt    time.Time
	disabled     bool
}

func (a *antigravityOAuthAuthorizer) authorize(ctx context.Context, force bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	credential, err := a.load()
	if err != nil {
		return "", err
	}
	if credential.disabled {
		return "", fmt.Errorf("Gemini credential %s is disabled", filepath.Base(a.path))
	}
	if force || credential.accessToken == "" || (!credential.expiresAt.IsZero() && time.Now().Add(time.Minute).After(credential.expiresAt)) {
		credential, err = a.refresh(ctx, credential)
		if err != nil {
			return "", err
		}
	}
	return credential.accessToken, nil
}

func (*antigravityOAuthAuthorizer) isOAuth() bool { return true }

func (a *antigravityOAuthAuthorizer) listAuths() []*AuthInfo {
	credential, err := a.load()
	if err != nil {
		return nil
	}
	status := StatusActive
	if credential.disabled {
		status = StatusDisabled
	}
	return []*AuthInfo{{
		ID: filepath.Base(a.path), Provider: backendAntigravity, FileName: a.path, Label: credential.email,
		Status: status, Disabled: credential.disabled, Metadata: credential.metadata,
	}}
}

func (a *antigravityOAuthAuthorizer) load() (*antigravityOAuthCredential, error) {
	raw, err := os.ReadFile(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no Gemini credentials found; run `ccl oauth gemini` first")
		}
		return nil, fmt.Errorf("read Gemini credential %s: %w", filepath.Base(a.path), err)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode Gemini credential %s: %w", filepath.Base(a.path), err)
	}
	credentialType := strings.ToLower(strings.TrimSpace(stringValue(metadata["type"])))
	if credentialType != backendAntigravity {
		return nil, fmt.Errorf("credential %s is type %q, not Gemini", filepath.Base(a.path), credentialType)
	}
	disabled, _ := metadata["disabled"].(bool)
	return &antigravityOAuthCredential{
		metadata: metadata, accessToken: firstMetadataString(metadata, "access_token"),
		refreshToken: firstMetadataString(metadata, "refresh_token"), email: firstMetadataString(metadata, "email"),
		projectID: firstMetadataString(metadata, "project_id"),
		expiresAt: parseCodexExpiry(firstMetadataString(metadata, "expired")), disabled: disabled,
	}, nil
}

func (a *antigravityOAuthAuthorizer) refresh(ctx context.Context, credential *antigravityOAuthCredential) (*antigravityOAuthCredential, error) {
	if credential.refreshToken == "" {
		return nil, fmt.Errorf("Gemini credential %s has no refresh token", filepath.Base(a.path))
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {antigravityOAuthClientID},
		"client_secret": {antigravityOAuthClientSecret},
		"refresh_token": {credential.refreshToken},
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(refreshCtx, http.MethodPost, antigravityTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("refresh Gemini token: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, antigravityMaxErrorBytes))
	if err != nil {
		return nil, fmt.Errorf("read Gemini token refresh: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh Gemini token: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("decode Gemini token refresh: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, fmt.Errorf("refresh Gemini token: response has no access token")
	}
	credential.accessToken = token.AccessToken
	if token.RefreshToken != "" {
		credential.refreshToken = token.RefreshToken
	}
	credential.expiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	credential.metadata["type"] = backendAntigravity
	credential.metadata["access_token"] = credential.accessToken
	credential.metadata["refresh_token"] = credential.refreshToken
	credential.metadata["expired"] = credential.expiresAt.UTC().Format(time.RFC3339)
	credential.metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if err := writeCodexCredentialAtomic(a.path, credential.metadata); err != nil {
		return nil, err
	}
	LogInfof("credential refreshed component=gemini credential_file=%s expires_at=%s",
		filepath.Base(a.path), credential.expiresAt.UTC().Format(time.RFC3339))
	return credential, nil
}

// geminiService is the CCL-owned data plane for the Gemini subscription. It
// converts Anthropic Messages requests to the Antigravity generateContent
// envelope and streams the Gemini response back as Anthropic Messages SSE.
type geminiService struct {
	apiKey     string
	authorizer chatAuthorizer
	models     []string
	client     *http.Client
	usage      *UsageTracker
	projectID  string
}

func (s *geminiService) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/models", s.handleModels)
	mux.HandleFunc("/v1/messages", s.handleMessages)
	mux.HandleFunc("/messages", s.handleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.handleCountTokens)
	mux.HandleFunc("/messages/count_tokens", s.handleCountTokens)
	return mux
}

func (s *geminiService) authorized(request *http.Request) bool {
	if request.Header.Get("x-api-key") == s.apiKey {
		return true
	}
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) == s.apiKey
}

func (s *geminiService) handleModels(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodGet {
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	data := make([]map[string]any, 0, len(s.models))
	for _, model := range s.models {
		data = append(data, map[string]any{"id": model, "object": "model", "type": "model"})
	}
	first, last := modelPageBounds(s.models)
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"object": "list", "data": data, "has_more": false, "first_id": first, "last_id": last,
	})
}

func (s *geminiService) handleCountTokens(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	raw, err := readAnthropicInboundBody(writer, request, chatMaxBodyBytes)
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// Gemini exposes no count_tokens endpoint, so estimate locally the same way
	// the converter estimates the request input tokens.
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"input_tokens": estimateApproxTokensBytes(raw)})
}

func (s *geminiService) handleMessages(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	if !s.authorized(request) {
		LogWarnEvent("request_rejected", "component", "gemini", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusUnauthorized, "reason", "invalid_local_api_key")
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	raw, err := readAnthropicInboundBody(writer, request, chatMaxBodyBytes)
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	converted, err := convertAnthropicToGemini(raw)
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	envelope := antigravityEnvelope(converted.geminiBody, converted.upstreamModel, s.projectID, geminiStableSessionID(converted.geminiBody))
	response, err := s.forward(requestCtx, converted.stream, envelope)
	if err != nil {
		LogErrorEvent("request_failed", "component", "gemini", "request_id", requestID,
			"model", converted.upstreamModel, "returned_status", http.StatusBadGateway,
			"duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		LogUpstreamEvent(response.StatusCode, "request_failed", "component", "gemini", "request_id", requestID,
			"status", response.StatusCode, "duration", logDuration(started))
		s.forwardUpstreamError(writer, response)
		return
	}

	assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, nil)
	if converted.stream {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		assembler.writer = writer
		if err := assembler.start(); err != nil {
			return
		}
		streamErr := processGeminiStream(response.Body, assembler)
		s.recordGeminiUsage(converted, assembler)
		if streamErr != nil {
			LogErrorEvent("stream_conversion_failed", "component", "gemini", "request_id", requestID,
				"model", converted.upstreamModel, "duration", logDuration(started), "error", streamErr)
			_ = assembler.emit("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": streamErr.Error()},
			})
		}
		return
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, antigravityMaxResponseBytes))
	if err != nil {
		LogErrorEvent("stream_conversion_failed", "component", "gemini", "request_id", requestID,
			"model", converted.upstreamModel, "returned_status", http.StatusBadGateway,
			"duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	if err := processGeminiNonStream(body, assembler); err != nil {
		LogErrorEvent("stream_conversion_failed", "component", "gemini", "request_id", requestID,
			"model", converted.upstreamModel, "returned_status", http.StatusBadGateway,
			"duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	s.recordGeminiUsage(converted, assembler)
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(assembler.response())
	LogDebugEvent("request_complete", "component", "gemini", "request_id", requestID,
		"model", converted.upstreamModel, "status", http.StatusOK, "stream", false, "duration", logDuration(started))
}

// forward sends one upstream Antigravity request, resolving the Bearer token.
// On a 401 from a stale OAuth token it refreshes once and retries before
// surfacing the failure.
func (s *geminiService) forward(ctx context.Context, stream bool, envelope []byte) (*http.Response, error) {
	response, err := s.forwardOnce(ctx, stream, envelope)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized && s.authorizer != nil && s.authorizer.isOAuth() {
		_ = response.Body.Close()
		if _, authErr := s.authorizer.authorize(ctx, true); authErr != nil {
			return nil, fmt.Errorf("refresh Gemini OAuth token: %w", authErr)
		}
		response, err = s.forwardOnce(ctx, stream, envelope)
		if err != nil {
			return nil, err
		}
	}
	return response, nil
}

// forwardOnce issues the request against the daily base URL and falls back to
// the prod base URL on network errors, 429s and 5xx responses, mirroring CPA's
// antigravityBaseURLFallbackOrder + retry loop.
func (s *geminiService) forwardOnce(ctx context.Context, stream bool, envelope []byte) (*http.Response, error) {
	token, err := s.authorizer.authorize(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("authorize Gemini upstream: %w", err)
	}
	var lastErr error
	var lastResponse *http.Response
	for _, base := range []string{antigravityBaseURLDaily, antigravityBaseURLProd} {
		response, doErr := s.doRequest(ctx, stream, envelope, token, base)
		if doErr != nil {
			lastErr = doErr
			continue
		}
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
			_ = response.Body.Close()
			lastResponse = response
			continue
		}
		return response, nil
	}
	if lastResponse != nil {
		return lastResponse, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("Gemini upstream: no base URL available")
}

func (s *geminiService) doRequest(ctx context.Context, stream bool, envelope []byte, token, base string) (*http.Response, error) {
	path := antigravityGeneratePath
	if stream {
		path = antigravityStreamPath + "?alt=sse"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(envelope))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("User-Agent", antigravityRequestUserAgent)
	request.Header.Set("Host", resolveAntigravityHost(base))
	request.Close = true
	LogDebugEvent("upstream_request", "component", "gemini", "request_id", requestLogID(ctx),
		"method", http.MethodPost, "endpoint", SafeLogEndpoint(base+path), "stream", stream, "body_bytes", len(envelope))
	started := time.Now()
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Gemini upstream: %w", err)
	}
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "gemini", "request_id", requestLogID(ctx),
		"status", response.StatusCode, "retry_after", response.Header.Get("Retry-After"),
		"upstream_request_id", firstHeader(response.Header, "X-Request-Id", "Request-Id"),
		"content_type", response.Header.Get("Content-Type"), "duration", logDuration(started))
	return response, nil
}

// forwardUpstreamError relays a non-2xx upstream response verbatim so Claude
// Code sees the upstream error JSON and status rather than a rewritten shape.
func (s *geminiService) forwardUpstreamError(writer http.ResponseWriter, response *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(response.Body, antigravityMaxErrorBytes))
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		writer.Header().Set("Content-Type", contentType)
	} else {
		writer.Header().Set("Content-Type", "application/json")
	}
	if retryAfter := response.Header.Get("Retry-After"); retryAfter != "" {
		writer.Header().Set("Retry-After", retryAfter)
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(body)
}

func (s *geminiService) recordGeminiUsage(converted *geminiConvertedRequest, assembler *anthropicResponseAssembler) {
	if s.usage == nil {
		return
	}
	input, output := assembler.tokenTotals()
	s.usage.Add(converted.clientModel, int64(input), int64(output), 0, 0)
}

// geminiStableSessionID derives a stable session id from the first user text
// part, mirroring CPA's generateStableSessionID: a signed-non-negative int64 of
// the first 8 bytes of the text's SHA-256, prefixed with "-". When there is no
// user text it falls back to a random positive int64.
func geminiStableSessionID(geminiBody []byte) string {
	contents := gjson.GetBytes(geminiBody, "contents")
	if contents.IsArray() {
		for _, content := range contents.Array() {
			if content.Get("role").String() != "user" {
				continue
			}
			text := content.Get("parts.0.text").String()
			if text != "" {
				sum := sha256.Sum256([]byte(text))
				n := int64(binary.BigEndian.Uint64(sum[:8])) & 0x7FFFFFFFFFFFFFFF
				return "-" + strconv.FormatInt(n, 10)
			}
		}
	}
	return "-" + strconv.FormatInt(rand.Int63n(9_000_000_000_000_000_000), 10)
}

// startAntigravityOAuth starts the CCL-owned Gemini subscription data plane. It
// reuses the chat authorizer abstraction for credential resolution and replaces
// the upstream wire protocol with the Antigravity Gemini converter/stream
// pipeline.
func startAntigravityOAuth(parent context.Context, modelSpec, credentialFile string) (*Runtime, error) {
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(authDir, filepath.Base(credentialFile))
	authorizer := &antigravityOAuthAuthorizer{
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
		return nil, fmt.Errorf("Gemini credential %s is disabled", filepath.Base(path))
	}
	if credential.accessToken == "" && credential.refreshToken == "" {
		return nil, fmt.Errorf("Gemini credential %s has no access or refresh token", filepath.Base(path))
	}
	if credential.projectID == "" {
		return nil, fmt.Errorf("Gemini credential %s has no project id", filepath.Base(path))
	}
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return nil, fmt.Errorf("Gemini runtime requires at least one model")
	}
	if parent == nil {
		parent = context.Background()
	}
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("listen for Gemini runtime: %w", err)
	}
	models := make([]string, 0, len(routes))
	seenModels := make(map[string]bool, len(routes))
	for _, route := range routes {
		alias := route.Alias
		if !seenModels[strings.ToLower(alias)] {
			models = append(models, alias)
			seenModels[strings.ToLower(alias)] = true
		}
	}
	usage := NewUsageTracker()
	service := &geminiService{
		apiKey: apiKey, authorizer: authorizer, models: models, usage: usage,
		client: &http.Client{Transport: antigravityHTTPTransport()}, projectID: credential.projectID,
	}
	runCtx, cancel := context.WithCancel(parent)
	server := &http.Server{
		Handler: service.handler(), ReadHeaderTimeout: 15 * time.Second,
		BaseContext: func(net.Listener) context.Context { return runCtx },
	}
	started := make(chan struct{})
	close(started)
	runtime := &Runtime{
		endpoint: "http://" + listener.Addr().String() + "/v1", apiKey: apiKey,
		httpServer: server, cancel: cancel, done: make(chan struct{}), runErr: make(chan error, 1),
		started: started, models: models, usage: usage, listAuths: authorizer.listAuths,
	}
	go func() {
		err := server.Serve(listener)
		if err == http.ErrServerClosed {
			err = nil
		}
		runtime.runErr <- err
		close(runtime.done)
	}()
	go func() {
		select {
		case <-runCtx.Done():
			ctx, stop := context.WithTimeout(context.Background(), runtimeStopTimeout)
			_ = server.Shutdown(ctx)
			stop()
		case <-runtime.done:
		}
	}()
	LogInfof("runtime start oauth provider=gemini backend=antigravity protocol=gemini port=%s credential_file=%s model_count=%d",
		strings.TrimPrefix(strings.TrimSuffix(runtime.endpoint, "/v1"), "http://"), filepath.Base(path), len(models))
	return runtime, nil
}
