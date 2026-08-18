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
	"strings"
	"time"
)

const (
	chatMaxBodyBytes     = int64(128 << 20)
	chatMaxErrorBytes    = int64(1 << 20)
	chatMaxResponseBytes = int64(64 << 20)
)

// chatAuthorizer resolves the Bearer token for one upstream request. Static
// API-key gateways return a fixed token; OAuth subscriptions refresh on demand
// and report isOAuth so the data plane can retry once after a 401.
type chatAuthorizer interface {
	authorize(context.Context, bool) (string, error)
	isOAuth() bool
}

// chatStaticAuthorizer pins a single API key for manual gateways.
type chatStaticAuthorizer struct{ token string }

func (a *chatStaticAuthorizer) authorize(context.Context, bool) (string, error) { return a.token, nil }
func (*chatStaticAuthorizer) isOAuth() bool                                     { return false }

// chatCompletionsService is the CCL-owned data plane for an OpenAI Chat
// Completions upstream. Request conversion, SSE conversion, error mapping, and
// usage accounting all live here, so a CLIProxyAPI upgrade can no longer change
// how openai(chat) behaves.
type chatCompletionsService struct {
	apiKey     string
	endpoint   string
	authorizer chatAuthorizer
	models     []string
	modelRoute map[string]string
	client     *http.Client
	usage      *UsageTracker
	// normalizeModel rewrites the upstream model ID after alias routing. Kimi uses
	// it to strip its "kimi-" prefix and remap legacy code aliases.
	normalizeModel func(string) string
	// decorateHeader adds backend-specific headers before the upstream request.
	// Kimi uses it to attach its X-Msh-* device identity headers.
	decorateHeader func(http.Header)
	// normalizeBody rewrites the marshalled upstream body after alias routing and
	// model normalization but before the request is sent. Kimi uses it to link
	// tool results to tool calls and drop empty assistant messages.
	normalizeBody func([]byte) ([]byte, error)
}

type chatCompletionsUpstreamError struct {
	status     int
	body       string
	retryAfter string
	requestID  string
}

func (e *chatCompletionsUpstreamError) Error() string {
	message := strings.TrimSpace(e.body)
	if message == "" {
		message = http.StatusText(e.status)
	}
	return fmt.Sprintf("OpenAI Chat Completions upstream returned HTTP %d: %s", e.status, message)
}

// startOpenAIChatRuntime starts a CCL-owned Anthropic Messages entrypoint that
// translates requests to OpenAI Chat Completions upstream. It mirrors
// startCodexResponsesRuntime's loopback/server lifecycle with no OAuth and no
// compaction, since neither applies to a static-key Chat gateway.
func startOpenAIChatRuntime(parent context.Context, endpoint, upstreamAPIKey, modelSpec string) (*Runtime, error) {
	endpoint = normalizeOpenAIBaseURL(endpoint)
	upstreamAPIKey = strings.TrimSpace(upstreamAPIKey)
	if endpoint == "" || upstreamAPIKey == "" {
		return nil, fmt.Errorf("OpenAI Chat runtime requires endpoint and API key")
	}
	return startOpenAIChatRuntimeWithAuthorizer(parent, endpoint, modelSpec, &chatStaticAuthorizer{token: upstreamAPIKey})
}

// startOpenAIChatRuntimeWithAuthorizer starts the same Chat Completions data
// plane but resolves the upstream Bearer token through an authorizer, so OAuth
// subscription backends (grok/kimi/workbuddy) can reuse it unchanged.
func startOpenAIChatRuntimeWithAuthorizer(parent context.Context, endpoint, modelSpec string, authorizer chatAuthorizer) (*Runtime, error) {
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return nil, fmt.Errorf("OpenAI Chat runtime requires at least one model")
	}
	return startOpenAIChatRuntimeRoutes(parent, endpoint, routes, authorizer)
}

// startOpenAIChatRuntimeRoutes starts the Chat Completions data plane against an
// explicit route list. Backends whose alias→name mapping cannot be expressed as
// a CSV model spec (e.g. WorkBuddy, which derives routes from a live catalog)
// call this directly instead of startOpenAIChatRuntimeWithAuthorizer.
func startOpenAIChatRuntimeRoutes(parent context.Context, endpoint string, routes []runtimeModelRoute, authorizer chatAuthorizer) (*Runtime, error) {
	return startOpenAIChatRuntimeService(parent, endpoint, routes, authorizer, nil)
}

// startOpenAIChatRuntimeService is the shared server lifecycle. configure, when
// non-nil, is called with the fully-built service before the server starts, so
// backends like Kimi can install a model normalizer and header decorator.
func startOpenAIChatRuntimeService(parent context.Context, endpoint string, routes []runtimeModelRoute, authorizer chatAuthorizer, configure func(*chatCompletionsService)) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("OpenAI Chat runtime requires at least one model")
	}
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("listen for OpenAI Chat runtime: %w", err)
	}
	usage := NewUsageTracker()
	service := newChatCompletionsServiceWithAuthorizer(apiKey, endpoint, routes, authorizer, usage)
	if configure != nil {
		configure(service)
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
		started: started, models: append([]string(nil), service.models...), usage: usage,
	}
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
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
	return runtime, nil
}

// newChatCompletionsService keeps the static-key convenience signature for the
// API-key gateways (StartOpenAIChatAPI, mixed chat bucket) by wrapping a
// chatStaticAuthorizer. OAuth backends use newChatCompletionsServiceWithAuthorizer.
func newChatCompletionsService(apiKey, endpoint string, routes []runtimeModelRoute, upstreamAPIKey string, usage *UsageTracker) *chatCompletionsService {
	return newChatCompletionsServiceWithAuthorizer(apiKey, endpoint, routes, &chatStaticAuthorizer{token: upstreamAPIKey}, usage)
}

func newChatCompletionsServiceWithAuthorizer(apiKey, endpoint string, routes []runtimeModelRoute, authorizer chatAuthorizer, usage *UsageTracker) *chatCompletionsService {
	models := make([]string, 0, len(routes))
	modelRoute := make(map[string]string, len(routes))
	seenModels := make(map[string]bool, len(routes))
	for _, route := range routes {
		modelRoute[strings.ToLower(route.Alias)] = route.Name
		if !seenModels[strings.ToLower(route.Alias)] {
			models = append(models, route.Alias)
			seenModels[strings.ToLower(route.Alias)] = true
		}
	}
	return &chatCompletionsService{
		apiKey: apiKey, endpoint: endpoint, authorizer: authorizer,
		models: models, modelRoute: modelRoute, usage: usage,
		client: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
			ResponseHeaderTimeout: 90 * time.Second,
		}},
	}
}

func (s *chatCompletionsService) handler() http.Handler {
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

func (s *chatCompletionsService) authorized(request *http.Request) bool {
	if request.Header.Get("x-api-key") == s.apiKey {
		return true
	}
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) == s.apiKey
}

func (s *chatCompletionsService) handleModels(writer http.ResponseWriter, request *http.Request) {
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

func (s *chatCompletionsService) handleCountTokens(writer http.ResponseWriter, request *http.Request) {
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
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"input_tokens": estimateApproxTokensBytes(raw)})
}

func (s *chatCompletionsService) handleMessages(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	LogDebugEvent("request_received", "component", "openai_chat", "request_id", requestID,
		"path", request.URL.Path, "method", request.Method)
	if !s.authorized(request) {
		LogWarnEvent("request_rejected", "component", "openai_chat", "request_id", requestID,
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
	converted, err := convertAnthropicToChatCompletions(raw)
	if err != nil {
		LogWarnEvent("request_rejected", "component", "openai_chat", "request_id", requestID,
			"status", http.StatusBadRequest, "reason", "request_conversion", "error", err)
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if route := s.modelRoute[strings.ToLower(converted.clientModel)]; route != "" {
		if s.normalizeModel != nil {
			route = s.normalizeModel(route)
		}
		converted.model = route
		converted.upstreamModel = route
		var body map[string]any
		if json.Unmarshal(converted.body, &body) == nil {
			body["model"] = route
			converted.body, _ = json.Marshal(body)
		}
	}
	if s.normalizeBody != nil {
		normalized, normalizeErr := s.normalizeBody(converted.body)
		if normalizeErr != nil {
			LogWarnEvent("request_rejected", "component", "openai_chat", "request_id", requestID,
				"status", http.StatusBadRequest, "reason", "body_normalization", "error", normalizeErr)
			writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", normalizeErr.Error())
			return
		}
		converted.body = normalized
	}
	LogDebugEvent("request_converted", "component", "openai_chat", "request_id", requestID,
		"client_model", converted.clientModel, "upstream_model", converted.model,
		"stream", converted.stream, "body_bytes", len(converted.body))
	response, err := s.call(requestCtx, converted)
	if err != nil {
		var upstreamErr *chatCompletionsUpstreamError
		if errors.As(err, &upstreamErr) {
			if upstreamErr.retryAfter != "" {
				writer.Header().Set("Retry-After", upstreamErr.retryAfter)
			}
			if upstreamErr.requestID != "" {
				writer.Header().Set("X-Request-Id", upstreamErr.requestID)
			}
			LogUpstreamEvent(upstreamErr.status, "request_failed", "component", "openai_chat", "request_id", requestID,
				"model", converted.model, "status", upstreamErr.status, "returned_status", upstreamErr.status,
				"retry_after", upstreamErr.retryAfter, "duration", logDuration(started))
			writeAnthropicError(writer, upstreamErr.status, anthropicErrorType(upstreamErr.status), upstreamErr.Error())
			return
		}
		LogErrorEvent("request_failed", "component", "openai_chat", "request_id", requestID,
			"model", converted.model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer response.Body.Close()
	if converted.stream {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, writer)
		if err := processChatCompletionsStream(response.Body, assembler); err != nil {
			LogErrorEvent("stream_conversion_failed", "component", "openai_chat", "request_id", requestID,
				"model", converted.model, "duration", logDuration(started), "error", err)
			_ = assembler.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}})
		}
		s.recordUsage(converted, assembler)
	} else {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, chatMaxResponseBytes))
		if readErr != nil {
			LogErrorEvent("response_read_failed", "component", "openai_chat", "request_id", requestID,
				"model", converted.model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", readErr)
			writeAnthropicError(writer, http.StatusBadGateway, "api_error", readErr.Error())
			return
		}
		assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, nil)
		if err := processChatCompletionsNonStream(body, assembler); err != nil {
			LogErrorEvent("stream_conversion_failed", "component", "openai_chat", "request_id", requestID,
				"model", converted.model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", err)
			writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		s.recordUsage(converted, assembler)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(assembler.response())
	}
	LogDebugEvent("request_complete", "component", "openai_chat", "request_id", requestID,
		"model", converted.model, "status", http.StatusOK, "stream", converted.stream, "duration", logDuration(started))
}

func (s *chatCompletionsService) recordUsage(converted *chatCompletionsConvertedRequest, assembler *anthropicResponseAssembler) {
	if s.usage == nil {
		return
	}
	input, output := assembler.tokenTotals()
	s.usage.Add(converted.clientModel, int64(input), int64(output), int64(assembler.cacheReadTokens), int64(assembler.cacheWriteTokens))
}

func (s *chatCompletionsService) call(ctx context.Context, converted *chatCompletionsConvertedRequest) (*http.Response, error) {
	response, err := s.callOnce(ctx, converted)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized && s.authorizer != nil && s.authorizer.isOAuth() {
		// OAuth subscription: a stale token 401s once, then we force-refresh and
		// retry the request exactly once before surfacing the failure.
		_ = response.Body.Close()
		if _, authErr := s.authorizer.authorize(ctx, true); authErr != nil {
			return nil, fmt.Errorf("refresh OpenAI Chat OAuth token: %w", authErr)
		}
		response, err = s.callOnce(ctx, converted)
		if err != nil {
			return nil, err
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, chatDrainError(ctx, response)
	}
	return response, nil
}

func (s *chatCompletionsService) callOnce(ctx context.Context, converted *chatCompletionsConvertedRequest) (*http.Response, error) {
	target := strings.TrimRight(s.endpoint, "/") + "/chat/completions"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(converted.body))
	if err != nil {
		return nil, err
	}
	token, err := s.authorizer.authorize(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("authorize OpenAI Chat Completions upstream: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if converted.stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	if s.decorateHeader != nil {
		s.decorateHeader(request.Header)
	}
	LogDebugEvent("upstream_request", "component", "openai_chat", "request_id", requestLogID(ctx),
		"method", http.MethodPost, "endpoint", SafeLogEndpoint(target),
		"body_bytes", len(converted.body), "stream", converted.stream, "model", converted.model)
	started := time.Now()
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call OpenAI Chat Completions upstream: %w", err)
	}
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "openai_chat", "request_id", requestLogID(ctx),
		"status", response.StatusCode, "retry_after", response.Header.Get("Retry-After"),
		"upstream_request_id", firstHeader(response.Header, "X-Request-Id", "Request-Id"),
		"content_type", response.Header.Get("Content-Type"), "duration", logDuration(started))
	return response, nil
}

func chatDrainError(ctx context.Context, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, chatMaxErrorBytes))
	_ = response.Body.Close()
	DebugHTTPBody(fmt.Sprintf("openai chat response request_id=%s status=%d", requestLogID(ctx), response.StatusCode), body)
	return &chatCompletionsUpstreamError{
		status: response.StatusCode, body: strings.TrimSpace(string(body)),
		retryAfter: response.Header.Get("Retry-After"), requestID: firstHeader(response.Header, "X-Request-Id", "Request-Id"),
	}
}
