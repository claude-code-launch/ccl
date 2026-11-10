package oauthproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const mixedMaxBodyBytes = int64(128 << 20)

// mixedRouteSet buckets a model spec's routes by upstream protocol using
// an explicit per-model table from models.dev metadata.
type mixedRouteSet struct {
	chat      []runtimeModelRoute
	responses []runtimeModelRoute
	models    []string
}

// mixedProtocolForModel resolves a route's upstream protocol from the protocol
// table, keyed by lowercase model ID. The alias is consulted first (it is what
// the client sends), then the technical name; an unknown model falls back to
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
// produced (empty spec, or every model uses an unsupported protocol).
func buildMixedRoutes(modelSpec string, protocols map[string]string) (mixedRouteSet, error) {
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return mixedRouteSet{}, fmt.Errorf("mixed-protocol runtime requires at least one model")
	}
	result := mixedRouteSet{}
	seen := make(map[string]bool)
	modelSeen := make(map[string]bool)
	skippedUnsupportedProtocol := ""
	for _, route := range routes {
		proto := mixedProtocolForModel(protocols, route.Name, route.Alias)
		key := strings.ToLower(proto + "\x00" + route.Name + "\x00" + route.Alias)
		if seen[key] {
			continue
		}
		seen[key] = true
		switch proto {
		case "anthropic":
			skippedUnsupportedProtocol = proto
			continue
		case "openai_responses":
			result.responses = append(result.responses, runtimeModelRoute{Name: route.Name, Alias: route.Alias})
		default:
			result.chat = append(result.chat, runtimeModelRoute{Name: route.Name, Alias: route.Alias})
		}
		alias := strings.TrimSpace(route.Alias)
		aliasKey := strings.ToLower(alias)
		if aliasKey != "" && !modelSeen[aliasKey] {
			modelSeen[aliasKey] = true
			result.models = append(result.models, alias)
		}
	}
	if len(result.chat)+len(result.responses) == 0 {
		if skippedUnsupportedProtocol != "" {
			return mixedRouteSet{}, fmt.Errorf("gateway models only offer the %s protocol, which CCL no longer supports", skippedUnsupportedProtocol)
		}
		return mixedRouteSet{}, fmt.Errorf("mixed-protocol runtime produced no usable model routes")
	}
	return result, nil
}

// StartMixedProtocolAPIKeyRuntime starts a single Responses entrypoint that
// routes each request to the correct upstream protocol for the requested
// model: OpenAI Responses is forwarded, and OpenAI Chat goes through CCL's Chat
// Completions adapter.
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
	LogInfof("runtime start mixed_protocol endpoint=%q local_endpoint=%q models_chat=%d models_responses=%d",
		SafeLogEndpoint(endpoint), SafeLogEndpoint(proxyRuntime.endpoint), len(routes.chat), len(routes.responses))
	return proxyRuntime, nil
}

// mixedProtocolRouter exposes the Responses front and dispatches each request
// by model to the service for that model's upstream protocol.
type mixedProtocolRouter struct {
	apiKey    string
	models    []string
	responses map[string]bool
	chat      map[string]bool
	codex     *codexResponsesService
	chatSvc   *chatCompletionsService
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
	usage := NewUsageTracker()
	router := &mixedProtocolRouter{
		apiKey: apiKey, models: append([]string(nil), routes.models...), responses: responseModels,
		chat: chatModels,
	}
	if len(responseRoutes) > 0 {
		router.codex = newCodexResponsesService(apiKey, endpoint, responseRoutes, &codexStaticAuthorizer{token: upstreamAPIKey}, usage)
	}
	if len(chatRoutes) > 0 {
		router.chatSvc = newChatCompletionsService(apiKey, endpoint, chatRoutes, upstreamAPIKey, usage)
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
		// Surface the catalog so callers like `ccl map` and `ccl provider models --all`
		// see the routed models instead of falling through to heuristics.
		models: append([]string(nil), routes.models...),
	}
	if router.codex != nil {
		proxyRuntime.cleanup = append(proxyRuntime.cleanup, closeIdleClients(router.codex.client))
	}
	if router.chatSvc != nil {
		proxyRuntime.cleanup = append(proxyRuntime.cleanup, closeIdleClients(router.chatSvc.client))
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
	return loopbackFrontMux(r.apiKey, r.handleModels, r.handleResponses)
}

func (r *mixedProtocolRouter) handleModels(writer http.ResponseWriter, request *http.Request) {
	serveModelList(writer, request, r.apiKey, r.models)
}

// handleResponses dispatches one Responses request to the service that owns
// the requested model's upstream protocol.
func (r *mixedProtocolRouter) handleResponses(writer http.ResponseWriter, request *http.Request) {
	if !inferenceRequestAllowed(writer, request, r.apiKey) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, mixedMaxBodyBytes+1))
	if err != nil {
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if int64(len(body)) > mixedMaxBodyBytes {
		writeProxyError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	model := requestModelID(body)
	lower := strings.ToLower(model)
	if r.codex != nil && r.responses[lower] {
		LogDebugEvent("protocol_route", "component", "mixed", "model", model, "protocol", "openai_responses", "owner", "ccl")
		r.codex.handleResponses(writer, request)
		return
	}
	if r.chatSvc != nil && r.chat[lower] {
		LogDebugEvent("protocol_route", "component", "mixed", "model", model, "protocol", "openai_chat", "owner", "ccl")
		r.chatSvc.handleResponses(writer, request)
		return
	}
	writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "model is not routed to a supported protocol")
}
