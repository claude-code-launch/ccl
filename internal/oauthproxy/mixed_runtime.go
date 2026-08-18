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

	"github.com/claude-code-launch/ccl/internal/protocol"
)

const mixedMaxBodyBytes = int64(128 << 20)

// mixedRouteSet buckets a model spec's routes by upstream protocol. It mirrors
// copilotRouteSet but derives the protocol from an explicit per-model table
// (models.dev metadata) instead of Copilot's supported_endpoints catalog.
type mixedRouteSet struct {
	chat      []runtimeModelRoute
	responses []runtimeModelRoute
	anthropic []runtimeModelRoute
	models    []string
}

// mixedProtocolForModel resolves a route's upstream protocol from the protocol
// table, keyed by lowercase model ID. The alias is consulted first (it is what
// Claude Code sends), then the technical name; an unknown model falls back to
// chat so a single hand-edited slot never blocks the whole runtime.
func mixedProtocolForModel(protocols map[string]string, name, alias string) string {
	if proto, ok := protocols[strings.ToLower(strings.TrimSpace(alias))]; ok && proto != "" {
		return proto
	}
	if proto, ok := protocols[strings.ToLower(strings.TrimSpace(name))]; ok && proto != "" {
		return proto
	}
	return "openai"
}

// buildMixedRoutes maps a model spec into per-protocol route buckets using an
// explicit protocol table. It reports an error only when no usable route is
// produced (empty spec, or every protocol unknown).
func buildMixedRoutes(modelSpec string, protocols map[string]string) (mixedRouteSet, error) {
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return mixedRouteSet{}, fmt.Errorf("mixed-protocol runtime requires at least one model")
	}
	result := mixedRouteSet{}
	seen := make(map[string]bool)
	for _, route := range routes {
		proto := mixedProtocolForModel(protocols, route.Name, route.Alias)
		key := strings.ToLower(proto + "\x00" + route.Name + "\x00" + route.Alias)
		if seen[key] {
			continue
		}
		seen[key] = true
		switch proto {
		case "anthropic":
			result.anthropic = append(result.anthropic, runtimeModelRoute{Name: route.Name, Alias: route.Alias})
		case "openai_responses":
			result.responses = append(result.responses, runtimeModelRoute{Name: route.Name, Alias: route.Alias})
		default:
			result.chat = append(result.chat, runtimeModelRoute{Name: route.Name, Alias: route.Alias})
		}
	}
	for _, route := range routes {
		result.models = append(result.models, route.Alias)
	}
	if len(result.chat)+len(result.responses)+len(result.anthropic) == 0 {
		return mixedRouteSet{}, fmt.Errorf("mixed-protocol runtime produced no usable model routes")
	}
	return result, nil
}

// StartMixedProtocolAPIKeyRuntime starts a single Anthropic Messages entrypoint
// that routes each request to the correct upstream protocol for the requested
// model: native Anthropic through a CCL passthrough, OpenAI Chat through CCL's
// Chat Completions adapter, and OpenAI Responses through CCL's Codex Responses
// adapter. It is the API-key gateway counterpart to the Copilot OAuth router.
func StartMixedProtocolAPIKeyRuntime(parent context.Context, endpoint, upstreamAPIKey, modelSpec string, protocols map[string]string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	endpoint = normalizeOpenAIBaseURL(endpoint)
	upstreamAPIKey = strings.TrimSpace(upstreamAPIKey)
	if endpoint == "" || upstreamAPIKey == "" {
		return nil, fmt.Errorf("mixed-protocol runtime requires endpoint and API key")
	}
	routes, err := buildMixedRoutes(modelSpec, protocols)
	if err != nil {
		return nil, err
	}
	proxyRuntime, err := startMixedProtocolRouter(parent, endpoint, upstreamAPIKey, routes)
	if err != nil {
		return nil, err
	}
	LogInfof("runtime start mixed_protocol endpoint=%q local_endpoint=%q models_chat=%d models_responses=%d models_anthropic=%d",
		SafeLogEndpoint(endpoint), SafeLogEndpoint(proxyRuntime.endpoint), len(routes.chat), len(routes.responses), len(routes.anthropic))
	return proxyRuntime, nil
}

// mixedProtocolRouter exposes an Anthropic Messages surface and dispatches each
// request by model: Responses models to the CCL Codex adapter, Chat models to
// the CCL Chat Completions adapter, and native Anthropic models to the CCL
// Messages passthrough.
type mixedProtocolRouter struct {
	apiKey       string
	models       []string
	responses    map[string]bool
	chat         map[string]bool
	anthropic    map[string]bool
	codex        *codexResponsesService
	chatSvc      *chatCompletionsService
	anthropicSvc *anthropicPassthroughService
}

func startMixedProtocolRouter(parent context.Context, endpoint, upstreamAPIKey string, routes mixedRouteSet) (*Runtime, error) {
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("listen for mixed-protocol router: %w", err)
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
	router := &mixedProtocolRouter{
		apiKey: apiKey, models: append([]string(nil), routes.models...), responses: responseModels,
		chat: chatModels, anthropic: anthropicModels,
	}
	if len(responseRoutes) > 0 {
		router.codex = newCodexResponsesService(apiKey, endpoint, responseRoutes, &codexStaticAuthorizer{token: upstreamAPIKey}, usage)
	}
	if len(chatRoutes) > 0 {
		router.chatSvc = newChatCompletionsService(apiKey, endpoint, chatRoutes, upstreamAPIKey, usage)
	}
	if len(anthropicRoutes) > 0 {
		anthropicNames := make([]string, 0, len(anthropicRoutes))
		for _, route := range anthropicRoutes {
			anthropicNames = append(anthropicNames, route.Alias)
		}
		router.anthropicSvc = newAnthropicPassthroughService(apiKey, protocol.NormalizeAnthropicBaseURLForClaude(endpoint), anthropicNames, &chatStaticAuthorizer{token: upstreamAPIKey}, usage)
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
		started: started, usage: usage,
		// Surface the catalog so callers like `ccl map` and `ccl models --all`
		// see the routed models instead of falling through to heuristics.
		models: append([]string(nil), routes.models...),
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

func (r *mixedProtocolRouter) handler() http.Handler {
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

func (r *mixedProtocolRouter) authorized(request *http.Request) bool {
	if request.Header.Get("x-api-key") == r.apiKey {
		return true
	}
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) == r.apiKey
}

func (r *mixedProtocolRouter) handleModels(writer http.ResponseWriter, request *http.Request) {
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

func (r *mixedProtocolRouter) handleCountTokens(writer http.ResponseWriter, request *http.Request) {
	if !r.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, mixedMaxBodyBytes+1))
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if int64(len(body)) > mixedMaxBodyBytes {
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
	writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "model is not routed to a supported protocol")
}

func (r *mixedProtocolRouter) handleMessages(writer http.ResponseWriter, request *http.Request) {
	if !r.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, mixedMaxBodyBytes+1))
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if int64(len(body)) > mixedMaxBodyBytes {
		writeAnthropicError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	model := copilotRequestModel(body)
	lower := strings.ToLower(model)
	if r.codex != nil && r.responses[lower] {
		LogDebugEvent("protocol_route", "component", "mixed", "model", model, "protocol", "codex_responses", "owner", "ccl")
		r.codex.handleMessages(writer, request)
		return
	}
	if r.chatSvc != nil && r.chat[lower] {
		LogDebugEvent("protocol_route", "component", "mixed", "model", model, "protocol", "openai_chat", "owner", "ccl")
		r.chatSvc.handleMessages(writer, request)
		return
	}
	if r.anthropicSvc != nil && r.anthropic[lower] {
		LogDebugEvent("protocol_route", "component", "mixed", "model", model, "protocol", "anthropic_messages", "owner", "ccl")
		r.anthropicSvc.handleMessages(writer, request)
		return
	}
	writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "model is not routed to a supported protocol")
}
