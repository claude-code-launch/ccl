package oauthproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// zedRouteSet buckets the account's models by data plane.
type zedRouteSet struct {
	responses []runtimeModelRoute
	chat      []runtimeModelRoute
	anthropic []runtimeModelRoute
	google    []runtimeModelRoute
	models    []string
}

// buildZedRoutes maps the account catalog, plus any configured aliases such as
// "claude-opus-4-6[1m]", onto data planes. Like Copilot, configured models
// never narrow the authoritative catalog; a configured model the account does
// not offer is an error so a stale slot cannot silently route elsewhere.
func buildZedRoutes(modelSpec string, catalog []zedModel) (zedRouteSet, error) {
	byID := make(map[string]zedModel, len(catalog))
	routes := make([]runtimeModelRoute, 0, len(catalog))
	result := zedRouteSet{models: make([]string, 0, len(catalog))}
	for _, model := range catalog {
		byID[strings.ToLower(model.ID)] = model
		routes = append(routes, runtimeModelRoute{Name: model.ID, Alias: model.ID})
		result.models = append(result.models, model.ID)
	}
	catalogRoutes := len(routes)
	routes = append(routes, runtimeModelRoutes(modelSpec)...)

	seen := make(map[string]bool, len(routes))
	missing := make(map[string]bool)
	for index, route := range routes {
		model, ok := byID[strings.ToLower(route.Name)]
		if !ok {
			if index >= catalogRoutes {
				missing[route.Name] = true
			}
			continue
		}
		wire, _ := zedModelWire(model)
		key := wire.String() + "\x00" + strings.ToLower(model.ID) + "\x00" + strings.ToLower(route.Alias)
		if seen[key] {
			continue
		}
		seen[key] = true
		resolved := runtimeModelRoute{Name: model.ID, Alias: route.Alias}
		switch wire {
		case zedWireResponses:
			result.responses = append(result.responses, resolved)
		case zedWireChat:
			result.chat = append(result.chat, resolved)
		case zedWireGoogle:
			result.google = append(result.google, resolved)
		default:
			result.anthropic = append(result.anthropic, resolved)
		}
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		sort.Strings(names)
		return zedRouteSet{}, fmt.Errorf("Zed does not offer configured model(s) on this account: %s", strings.Join(names, ", "))
	}
	if len(result.responses)+len(result.chat)+len(result.anthropic)+len(result.google) == 0 {
		return zedRouteSet{}, fmt.Errorf("Zed returned no usable model routes")
	}
	return result, nil
}

// startZedOAuth starts the Zed subscription data plane: it verifies the stored
// sign-in, discovers the account's models, and routes each model to the CCL
// service that speaks its provider's native protocol.
func startZedOAuth(parent context.Context, modelSpec, credentialFile string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	credential, err := loadZedCredential(authDir, credentialFile)
	if err != nil {
		return nil, err
	}
	if credential.disabled {
		return nil, fmt.Errorf("Zed credential %s is disabled", credential.fileName)
	}
	session := newZedSession(credential)
	catalog, err := session.discoverModels(parent)
	if err != nil {
		var httpErr *zedHTTPError
		if errors.As(err, &httpErr) && httpErr.status == http.StatusUnauthorized {
			return nil, fmt.Errorf("Zed rejected the stored credentials; run `ccl oauth %s` again: %w", ProviderZed, err)
		}
		return nil, err
	}
	routes, err := buildZedRoutes(modelSpec, catalog)
	if err != nil {
		return nil, err
	}
	gateway, err := startZedGateway(parent, session, catalog)
	if err != nil {
		return nil, err
	}
	proxyRuntime, err := startZedProtocolRouter(parent, gateway, routes, session)
	if err != nil {
		gateway.Stop()
		return nil, err
	}
	proxyRuntime.cleanup = append(proxyRuntime.cleanup, gateway.Stop)
	proxyRuntime.listAuths = session.listAuths
	proxyRuntime.models = append([]string(nil), routes.models...)
	LogInfof("runtime start oauth provider=zed backend=zed protocol=mixed local_endpoint=%q credential_file=%s models_responses=%d models_chat=%d models_anthropic=%d models_google=%d",
		SafeLogEndpoint(proxyRuntime.endpoint), filepath.Base(credentialFile),
		len(routes.responses), len(routes.chat), len(routes.anthropic), len(routes.google))
	return proxyRuntime, nil
}

// zedProtocolRouter serves Zed's mixed catalog on CCL-owned data planes:
// Anthropic models on the Messages passthrough, OpenAI models on the Responses
// adapter, xAI models on the Chat adapter, and Google models on the Gemini
// converter. All of them reach Zed through the loopback gateway.
type zedProtocolRouter struct {
	apiKey       string
	models       []string
	wires        map[string]zedWire
	codex        *codexResponsesService
	chatSvc      *chatCompletionsService
	anthropicSvc *anthropicPassthroughService
	gemini       *zedGeminiService
}

func zedRouteAliases(routes []runtimeModelRoute, wire zedWire, wires map[string]zedWire) []runtimeModelRoute {
	resolved := make([]runtimeModelRoute, 0, len(routes))
	for _, route := range routes {
		alias := route.Alias
		if strings.TrimSpace(alias) == "" {
			alias = route.Name
		}
		resolved = append(resolved, runtimeModelRoute{Name: route.Name, Alias: alias})
		wires[strings.ToLower(alias)] = wire
	}
	return resolved
}

func startZedProtocolRouter(parent context.Context, gateway *zedGateway, routes zedRouteSet, session *zedSession) (*Runtime, error) {
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("listen for Zed protocol router: %w", err)
	}
	wires := make(map[string]zedWire)
	responseRoutes := zedRouteAliases(routes.responses, zedWireResponses, wires)
	chatRoutes := zedRouteAliases(routes.chat, zedWireChat, wires)
	anthropicRoutes := zedRouteAliases(routes.anthropic, zedWireAnthropic, wires)
	googleRoutes := zedRouteAliases(routes.google, zedWireGoogle, wires)

	usage := NewUsageTracker()
	router := &zedProtocolRouter{apiKey: apiKey, models: append([]string(nil), routes.models...), wires: wires}
	if len(responseRoutes) > 0 {
		router.codex = newCodexResponsesService(apiKey, gateway.endpoint, responseRoutes, &codexStaticAuthorizer{token: gateway.key}, usage)
	}
	if len(chatRoutes) > 0 {
		router.chatSvc = newChatCompletionsServiceWithAuthorizer(apiKey, gateway.endpoint, chatRoutes, &chatStaticAuthorizer{token: gateway.key}, usage)
	}
	if len(anthropicRoutes) > 0 {
		names := make([]string, 0, len(anthropicRoutes))
		for _, route := range anthropicRoutes {
			names = append(names, route.Alias)
		}
		router.anthropicSvc = newAnthropicPassthroughService(apiKey, gateway.endpoint, names, &chatStaticAuthorizer{token: gateway.key}, usage)
	}
	if len(googleRoutes) > 0 {
		router.gemini = newZedGeminiService(apiKey, gateway, usage)
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
		started: started, usage: usage, listAuths: session.listAuths,
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

func (r *zedProtocolRouter) handler() http.Handler {
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

func (r *zedProtocolRouter) authorized(request *http.Request) bool {
	if request.Header.Get("x-api-key") == r.apiKey {
		return true
	}
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) == r.apiKey
}

func (r *zedProtocolRouter) handleModels(writer http.ResponseWriter, request *http.Request) {
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

// readBody reads a request body once so the router can inspect the model and
// still hand the body to the selected service.
func (r *zedProtocolRouter) readBody(writer http.ResponseWriter, request *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(request.Body, zedMaxBodyBytes+1))
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, false
	}
	if int64(len(body)) > zedMaxBodyBytes {
		writeAnthropicError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return nil, false
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	return body, true
}

func (r *zedProtocolRouter) handleCountTokens(writer http.ResponseWriter, request *http.Request) {
	if !r.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	body, ok := r.readBody(writer, request)
	if !ok {
		return
	}
	switch wire, found := r.wires[strings.ToLower(copilotRequestModel(body))]; {
	case found && wire == zedWireResponses && r.codex != nil:
		r.codex.handleCountTokens(writer, request)
	case found && wire == zedWireChat && r.chatSvc != nil:
		r.chatSvc.handleCountTokens(writer, request)
	case found && wire == zedWireAnthropic && r.anthropicSvc != nil:
		r.anthropicSvc.handleCountTokens(writer, request)
	case found && wire == zedWireGoogle && r.gemini != nil:
		r.gemini.handleCountTokens(writer, request)
	default:
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "model is not routed to a supported Zed protocol")
	}
}

func (r *zedProtocolRouter) handleMessages(writer http.ResponseWriter, request *http.Request) {
	if !r.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	body, ok := r.readBody(writer, request)
	if !ok {
		return
	}
	model := copilotRequestModel(body)
	wire, found := r.wires[strings.ToLower(model)]
	if !found {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "model is not routed to a supported Zed protocol")
		return
	}
	LogDebugEvent("protocol_route", "component", "zed", "model", model, "protocol", wire.String(), "owner", "ccl")
	switch {
	case wire == zedWireResponses && r.codex != nil:
		r.codex.handleMessages(writer, request)
	case wire == zedWireGoogle && r.gemini != nil:
		r.gemini.handleMessages(writer, request)
	case wire == zedWireChat && r.chatSvc != nil:
		r.serveStreamed(writer, request, body, r.chatSvc.handleMessages)
	case wire == zedWireAnthropic && r.anthropicSvc != nil:
		r.serveStreamed(writer, request, body, r.anthropicSvc.handleMessages)
	default:
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "model is not routed to a supported Zed protocol")
	}
}

// serveStreamed runs a service whose Zed upstream can only stream. A streaming
// client is served directly. A non-streaming client gets the same request
// forced to stream, and the Anthropic SSE the service produces is folded back
// into one Message; this keeps a single fold instead of a per-protocol
// aggregator inside the gateway.
func (r *zedProtocolRouter) serveStreamed(writer http.ResponseWriter, request *http.Request, body []byte, serve http.HandlerFunc) {
	if gjson.GetBytes(body, "stream").Bool() {
		serve(writer, request)
		return
	}
	forced, err := sjson.SetBytes(body, "stream", true)
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(forced))
	request.ContentLength = int64(len(forced))
	buffered := newZedBufferedResponse()
	serve(buffered, request)
	buffered.writeFolded(writer)
}

// zedBufferedResponse captures a handler's response in memory.
type zedBufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newZedBufferedResponse() *zedBufferedResponse {
	return &zedBufferedResponse{header: make(http.Header)}
}

func (b *zedBufferedResponse) Header() http.Header { return b.header }

func (b *zedBufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *zedBufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	if b.body.Len()+len(p) > anthropicAssemblerMaxRetainedBytes {
		return 0, fmt.Errorf("buffered response exceeds %d bytes", anthropicAssemblerMaxRetainedBytes)
	}
	return b.body.Write(p)
}

// Flush satisfies http.Flusher for services that flush after each event.
func (b *zedBufferedResponse) Flush() {}

// writeFolded relays an error response verbatim and folds a successful SSE
// response into a single Anthropic Message.
func (b *zedBufferedResponse) writeFolded(writer http.ResponseWriter) {
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		for _, name := range []string{"Content-Type", "Retry-After", "X-Request-Id"} {
			if value := b.header.Get(name); value != "" {
				writer.Header().Set(name, value)
			}
		}
		writer.WriteHeader(status)
		_, _ = writer.Write(b.body.Bytes())
		return
	}
	message, streamErr := foldAnthropicStream(b.body.Bytes())
	if streamErr != nil {
		writeAnthropicError(writer, streamErr.status(), streamErr.errorType, streamErr.message)
		return
	}
	zedWriteJSON(writer, http.StatusOK, message)
}

// zedFoldError is an error event found inside an otherwise successful stream.
type zedFoldError struct {
	errorType string
	message   string
}

func (e *zedFoldError) status() int {
	switch e.errorType {
	case "overloaded_error":
		return 529
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "authentication_error":
		return http.StatusUnauthorized
	case "permission_error":
		return http.StatusForbidden
	case "not_found_error":
		return http.StatusNotFound
	case "invalid_request_error":
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

// foldAnthropicStream rebuilds the non-streaming Message from an Anthropic SSE
// stream.
func foldAnthropicStream(stream []byte) (map[string]any, *zedFoldError) {
	var message map[string]any
	blocks := make(map[int]map[string]any)
	partialInput := make(map[int]*strings.Builder)
	usage := make(map[string]any)
	maxIndex := -1

	scanner := bufio.NewScanner(bytes.NewReader(stream))
	scanner.Buffer(make([]byte, 0, 64<<10), zedMaxStreamLine)
	for scanner.Scan() {
		payload, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "data:")
		payload = strings.TrimSpace(payload)
		if !ok || payload == "" || payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		index := intValue(event["index"])
		switch stringValue(event["type"]) {
		case "message_start":
			message = mapValue(event["message"])
			for key, value := range mapValue(message["usage"]) {
				usage[key] = value
			}
		case "content_block_start":
			if block := mapValue(event["content_block"]); block != nil {
				blocks[index] = block
				maxIndex = max(maxIndex, index)
			}
		case "content_block_delta":
			block := blocks[index]
			delta := mapValue(event["delta"])
			if block == nil || delta == nil {
				continue
			}
			switch stringValue(delta["type"]) {
			case "text_delta":
				block["text"] = stringValue(block["text"]) + stringValue(delta["text"])
			case "thinking_delta":
				block["thinking"] = stringValue(block["thinking"]) + stringValue(delta["thinking"])
			case "signature_delta":
				block["signature"] = stringValue(block["signature"]) + stringValue(delta["signature"])
			case "input_json_delta":
				if partialInput[index] == nil {
					partialInput[index] = &strings.Builder{}
				}
				partialInput[index].WriteString(stringValue(delta["partial_json"]))
			}
		case "content_block_stop":
			if builder := partialInput[index]; builder != nil && blocks[index] != nil {
				var input any
				if json.Unmarshal([]byte(builder.String()), &input) == nil {
					blocks[index]["input"] = input
				}
			}
		case "message_delta":
			if message == nil {
				continue
			}
			delta := mapValue(event["delta"])
			if value, present := delta["stop_reason"]; present {
				message["stop_reason"] = value
			}
			if value, present := delta["stop_sequence"]; present {
				message["stop_sequence"] = value
			}
			for key, value := range mapValue(event["usage"]) {
				usage[key] = value
			}
		case "error":
			errorBody := mapValue(event["error"])
			return nil, &zedFoldError{
				errorType: zedFirstNonEmpty(stringValue(errorBody["type"]), "api_error"),
				message:   zedFirstNonEmpty(stringValue(errorBody["message"]), "upstream stream error"),
			}
		}
	}
	if message == nil {
		return nil, &zedFoldError{errorType: "api_error", message: "upstream stream ended before message_start"}
	}
	content := make([]any, 0, len(blocks))
	for index := 0; index <= maxIndex; index++ {
		if block, ok := blocks[index]; ok {
			content = append(content, block)
		}
	}
	message["content"] = content
	if len(usage) > 0 {
		message["usage"] = usage
	}
	return message, nil
}

// zedGeminiService is the CCL-owned data plane for Zed's Google models. It
// reuses the Gemini converter and stream decoder and replaces the Antigravity
// transport with a call to the Zed gateway, which carries the request in Zed's
// envelope.
type zedGeminiService struct {
	apiKey  string
	gateway *zedGateway
	client  *http.Client
	usage   *UsageTracker
}

func newZedGeminiService(apiKey string, gateway *zedGateway, usage *UsageTracker) *zedGeminiService {
	return &zedGeminiService{
		apiKey: apiKey, gateway: gateway, usage: usage,
		client: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
			ResponseHeaderTimeout: 90 * time.Second,
		}},
	}
}

func (s *zedGeminiService) authorized(request *http.Request) bool {
	if request.Header.Get("x-api-key") == s.apiKey {
		return true
	}
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) == s.apiKey
}

func (s *zedGeminiService) handleCountTokens(writer http.ResponseWriter, request *http.Request) {
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
	// Zed has no token counting for Google models, so estimate locally as the
	// Antigravity Gemini service does.
	zedWriteJSON(writer, http.StatusOK, map[string]any{"input_tokens": estimateApproxTokensBytes(raw)})
}

func (s *zedGeminiService) post(ctx context.Context, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.gateway.endpoint+"/v1/google", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+s.gateway.key)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Zed gateway: %w", err)
	}
	return response, nil
}

func (s *zedGeminiService) handleMessages(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	if !s.authorized(request) {
		LogWarnEvent("request_rejected", "component", "zed_gemini", "request_id", requestID,
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
	body, err := sjson.SetBytes(converted.geminiBody, "model", converted.upstreamModel)
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// The gateway is the inner hop, so this outer service owns the fast retry.
	response, err := retryUpstream(requestCtx, "zed_gemini", func() (*http.Response, error) {
		return s.post(requestCtx, body)
	})
	if err != nil {
		LogErrorEvent("request_failed", "component", "zed_gemini", "request_id", requestID,
			"model", converted.upstreamModel, "returned_status", http.StatusBadGateway,
			"duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, chatMaxErrorBytes))
		if retryAfter := response.Header.Get("Retry-After"); retryAfter != "" {
			writer.Header().Set("Retry-After", retryAfter)
		}
		LogUpstreamEvent(response.StatusCode, "request_failed", "component", "zed_gemini", "request_id", requestID,
			"model", converted.upstreamModel, "status", response.StatusCode, "duration", logDuration(started))
		writeAnthropicError(writer, response.StatusCode, anthropicErrorType(response.StatusCode), zedErrorMessage(raw))
		return
	}

	assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, nil)
	if converted.stream {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		assembler.writer = writer
		assembler.flusher, _ = writer.(http.Flusher)
		if err := assembler.start(); err != nil {
			return
		}
		streamErr := processGeminiStream(response.Body, assembler)
		s.recordUsage(converted, assembler)
		if streamErr != nil {
			LogErrorEvent("stream_conversion_failed", "component", "zed_gemini", "request_id", requestID,
				"model", converted.upstreamModel, "duration", logDuration(started), "error", streamErr)
			_ = assembler.emit("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": streamErr.Error()},
			})
		}
		return
	}
	if err := processGeminiStream(response.Body, assembler); err != nil {
		LogErrorEvent("stream_conversion_failed", "component", "zed_gemini", "request_id", requestID,
			"model", converted.upstreamModel, "returned_status", http.StatusBadGateway,
			"duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	s.recordUsage(converted, assembler)
	zedWriteJSON(writer, http.StatusOK, assembler.response())
	LogDebugEvent("request_complete", "component", "zed_gemini", "request_id", requestID,
		"model", converted.upstreamModel, "status", http.StatusOK, "stream", false, "duration", logDuration(started))
}

func (s *zedGeminiService) recordUsage(converted *geminiConvertedRequest, assembler *anthropicResponseAssembler) {
	if s.usage == nil {
		return
	}
	input, output := assembler.tokenTotals()
	s.usage.Add(converted.clientModel, int64(input), int64(output), int64(assembler.cacheReadTokens), 0)
}
