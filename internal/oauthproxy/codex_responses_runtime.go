package oauthproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/codexidentity"
	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexResponsesDefaultOAuthBaseURL = "https://chatgpt.com/backend-api/codex"
	codexResponsesOAuthTokenURL       = "https://auth.openai.com/oauth/token"
	codexResponsesOAuthClientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexResponsesMaxBodyBytes        = int64(128 << 20)
	codexResponsesMaxErrorBytes       = int64(1 << 20)
)

var (
	codexOAuthBaseURL  = codexResponsesDefaultOAuthBaseURL
	codexOAuthTokenURL = codexResponsesOAuthTokenURL
)

type codexResponsesAuthorization struct {
	token      string
	accountID  string
	credential string
	// userID and email are xAI session identity fields. Codex leaves them empty;
	// keeping them on the shared authorization value lets the Grok data plane
	// attach the same account headers as the official Grok Build client without
	// re-reading the credential on every inference request.
	userID string
	email  string
}

type codexResponsesAuthorizer interface {
	authorize(context.Context, bool) (codexResponsesAuthorization, error)
	listAuths() []*AuthInfo
	isOAuth() bool
}

type codexStaticAuthorizer struct{ token string }

func (a *codexStaticAuthorizer) authorize(context.Context, bool) (codexResponsesAuthorization, error) {
	return codexResponsesAuthorization{token: a.token, credential: "api-key"}, nil
}
func (*codexStaticAuthorizer) listAuths() []*AuthInfo { return nil }
func (*codexStaticAuthorizer) isOAuth() bool          { return false }

type codexOAuthAuthorizer struct {
	path   string
	client *http.Client
}

type codexOAuthCredential struct {
	metadata     map[string]any
	accessToken  string
	refreshToken string
	accountID    string
	email        string
	expiresAt    time.Time
	disabled     bool
}

type codexResponsesService struct {
	apiKey         string
	endpoint       string
	models         []string
	modelRoute     map[string]string
	authorizer     codexResponsesAuthorizer
	client         *http.Client
	usage          *UsageTracker
	installationID string
	windowID       string
	// xai switches the Responses data plane from Codex identity to xAI/Grok
	// identity: xAI Grok headers replace the Codex turn headers, and the
	// Codex-specific client_metadata block is omitted. Both backends share the
	// same OpenAI Responses wire protocol.
	xai bool
	// flattenTools re-renders requests that use tool kinds only the OpenAI
	// backend implements (custom tools, namespaces, client tool search) as
	// plain function tools. Grok needs it when Codex is the client.
	flattenTools bool
}

type codexResponsesUpstreamError struct {
	status     int
	body       string
	retryAfter string
	requestID  string
}

// upstreamStatus implements the retry.go fast-retry probe against the raw
// upstream status, before any client-facing mapping.
func (e *codexResponsesUpstreamError) upstreamStatus() int        { return e.status }
func (e *codexResponsesUpstreamError) upstreamRetryAfter() string { return e.retryAfter }

func (e *codexResponsesUpstreamError) Error() string {
	message := strings.TrimSpace(e.body)
	if message == "" {
		message = http.StatusText(e.status)
	}
	return fmt.Sprintf("Codex Responses upstream returned HTTP %d: %s", e.status, message)
}

func startCodexResponsesAPI(parent context.Context, endpoint, upstreamAPIKey, modelSpec string) (*Runtime, error) {
	return startCodexResponsesRuntime(parent, endpoint, modelSpec, &codexStaticAuthorizer{token: strings.TrimSpace(upstreamAPIKey)})
}

func startCodexOAuth(parent context.Context, modelSpec, credentialFile string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(authDir, filepath.Base(credentialFile))
	authorizer := &codexOAuthAuthorizer{
		path:   path,
		client: &http.Client{Transport: newUpstreamTransport(30 * time.Second)},
	}
	credential, err := authorizer.load()
	if err != nil {
		return nil, err
	}
	if credential.disabled {
		return nil, fmt.Errorf("Codex credential %s is disabled", filepath.Base(path))
	}
	if credential.accessToken == "" && credential.refreshToken == "" {
		return nil, fmt.Errorf("Codex credential %s has no access or refresh token", filepath.Base(path))
	}
	discoveryCtx, cancelDiscovery := context.WithTimeout(parent, 20*time.Second)
	modelInfos, discoverErr := discoverCodexModelInfos(discoveryCtx, codexOAuthBaseURL, authorizer)
	models := codexModelIDs(modelInfos)
	cancelDiscovery()
	catalogFallback := discoverErr != nil
	if catalogFallback {
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		LogWarnf("GPT model discovery failed; keeping configured models error=%v", discoverErr)
		if strings.TrimSpace(modelSpec) == "" {
			modelSpec = "gpt-5.6-sol,gpt-5.6-terra,gpt-5.6-luna"
		}
		models = runtimeModelAliases(modelSpec)
	}
	// Preserve configured routes, but publish the live account catalog so stale
	// mappings do not limit the choices shown by ccl map and ccl set.
	effectiveModelSpec := strings.Join(append([]string{modelSpec}, models...), ",")
	runtime, err := startCodexResponsesRuntimeWithService(parent, codexOAuthBaseURL, effectiveModelSpec, authorizer,
		func(apiKey, endpoint string, routes []runtimeModelRoute, auth codexResponsesAuthorizer, usage *UsageTracker) *codexResponsesService {
			service := newCodexResponsesService(apiKey, endpoint, routes, auth, usage)
			service.models = append([]string(nil), models...)
			return service
		})
	if err != nil {
		return nil, err
	}
	runtime.listAuths = authorizer.listAuths
	runtime.modelDetails = make(map[string]protocol.ModelInfo, len(modelInfos))
	for _, info := range modelInfos {
		runtime.modelDetails[strings.ToLower(info.ID)] = info
	}
	runtime.catalogFallback = catalogFallback
	LogInfof("runtime start oauth provider=gpt backend=codex protocol=codex_responses port=%s credential_file=%s model_count=%d",
		strings.TrimPrefix(strings.TrimSuffix(runtime.endpoint, "/v1"), "http://"), filepath.Base(path), len(runtime.models))
	return runtime, nil
}

func startCodexResponsesRuntime(parent context.Context, endpoint, modelSpec string, authorizer codexResponsesAuthorizer) (*Runtime, error) {
	return startCodexResponsesRuntimeWithService(parent, endpoint, modelSpec, authorizer, newCodexResponsesService)
}

// startCodexResponsesRuntimeWithService starts a Responses data plane using the
// supplied service factory. The Codex and xAI/Grok backends share the loopback
// server lifecycle but build a different service identity (see newResponsesService).
func startCodexResponsesRuntimeWithService(parent context.Context, endpoint, modelSpec string, authorizer codexResponsesAuthorizer, makeService func(apiKey, endpoint string, routes []runtimeModelRoute, authorizer codexResponsesAuthorizer, usage *UsageTracker) *codexResponsesService) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	endpoint = normalizeOpenAIBaseURL(endpoint)
	if endpoint == "" || authorizer == nil {
		return nil, fmt.Errorf("Responses runtime requires endpoint and authorization")
	}
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return nil, fmt.Errorf("Responses runtime requires at least one model")
	}
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("listen for Responses runtime: %w", err)
	}
	usage := NewUsageTracker()
	service := makeService(apiKey, endpoint, routes, authorizer, usage)
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
	runtime.cleanup = append(runtime.cleanup, closeIdleClients(service.client, authorizerHTTPClient(authorizer)))
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

func newCodexResponsesService(apiKey, endpoint string, routes []runtimeModelRoute, authorizer codexResponsesAuthorizer, usage *UsageTracker) *codexResponsesService {
	return newResponsesService(apiKey, endpoint, routes, authorizer, usage, false)
}

// newXaiResponsesService builds the same Responses data plane with xAI/Grok
// identity. Grok uses the OpenAI Responses wire protocol with Grok-specific
// headers and no Codex client metadata.
func newXaiResponsesService(apiKey, endpoint string, routes []runtimeModelRoute, authorizer codexResponsesAuthorizer, usage *UsageTracker) *codexResponsesService {
	return newResponsesService(apiKey, endpoint, routes, authorizer, usage, true)
}

func newResponsesService(apiKey, endpoint string, routes []runtimeModelRoute, authorizer codexResponsesAuthorizer, usage *UsageTracker, xai bool) *codexResponsesService {
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
	return &codexResponsesService{
		apiKey: apiKey, endpoint: endpoint, models: models, modelRoute: modelRoute,
		authorizer: authorizer, usage: usage, installationID: uuidString(), windowID: uuidString(),
		xai: xai, flattenTools: xai,
		client: &http.Client{Transport: newUpstreamTransport(90 * time.Second)},
	}
}

func (s *codexResponsesService) handler() http.Handler {
	return loopbackFrontMux(s.apiKey, s.handleModels, s.handleResponses)
}

func (s *codexResponsesService) authorized(request *http.Request) bool {
	return loopbackAuthorized(request, s.apiKey)
}

func (s *codexResponsesService) handleModels(writer http.ResponseWriter, request *http.Request) {
	serveModelList(writer, request, s.apiKey, s.models)
}

// handleResponses serves one Responses request. The upstream speaks Responses
// too, so the request is forwarded as the client sent it; only a request whose
// tool dialect this upstream does not implement is re-rendered first.
func (s *codexResponsesService) handleResponses(writer http.ResponseWriter, request *http.Request) {
	if !inferenceRequestAllowed(writer, request, s.apiKey) {
		return
	}
	body, err := readInboundBody(writer, request, codexResponsesMaxBodyBytes)
	if err != nil {
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var payload map[string]any
	if decodeProtocolJSON(body, &payload) != nil {
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "invalid Responses request JSON")
		return
	}
	compact := strings.HasSuffix(request.URL.Path, "/compact")
	if s.flattenTools && !compact && responsesNeedFlattening(payload) {
		s.serveConverted(writer, request, body)
		return
	}
	s.servePassthrough(writer, request, payload, compact)
}

// serveConverted re-renders the request with plain function tools, then
// assembles the upstream stream back into the client's own tool dialect.
func (s *codexResponsesService) serveConverted(writer http.ResponseWriter, request *http.Request, raw []byte) {
	requestCtx, requestID := withRequestLogID(request.Context())
	requestCtx = withClientSession(requestCtx, request)
	started := time.Now()
	converted, err := convertResponsesForUpstream(raw, s.requestSessionID(request))
	if err != nil {
		LogWarnEvent("request_rejected", "component", "codex_responses", "request_id", requestID,
			"status", http.StatusBadRequest, "reason", "request_conversion", "error", err)
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	route, ok := s.modelRoute[strings.ToLower(converted.clientModel)]
	if !ok {
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "model is not in the provider catalog")
		return
	}
	converted.model, converted.upstreamModel = route, route
	var body map[string]any
	if decodeProtocolJSON(converted.body, &body) == nil {
		body["model"] = route
		converted.body, _ = json.Marshal(body)
	}
	LogDebugEvent("request_converted", "component", "codex_responses", "request_id", requestID,
		"client_model", converted.clientModel, "upstream_model", converted.model,
		"stream", converted.stream, "body_bytes", len(converted.body))
	response, err := s.call(requestCtx, converted.body, converted.sessionID, true)
	if err != nil {
		s.writeUpstreamError(writer, requestID, converted.model, started, err)
		return
	}
	defer response.Body.Close()
	if converted.stream {
		prepareEventStream(writer)
		assembler := newResponseAssembler(&converted.adapterRequest, writer)
		err = processCodexResponsesStream(response.Body, assembler)
		s.recordUsage(converted.clientModel, assembler)
		if err != nil {
			LogErrorEvent("stream_conversion_failed", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "duration", logDuration(started), "error", err)
			assembler.fail("api_error", err.Error())
			return
		}
	} else {
		assembler := newResponseAssembler(&converted.adapterRequest, nil)
		if err = processCodexResponsesStream(response.Body, assembler); err != nil {
			LogErrorEvent("stream_conversion_failed", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", err)
			writeProxyError(writer, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		s.recordUsage(converted.clientModel, assembler)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(assembler.response())
	}
	LogDebugEvent("request_complete", "component", "codex_responses", "request_id", requestID,
		"model", converted.model, "status", http.StatusOK, "stream", converted.stream, "duration", logDuration(started))
}

func (s *codexResponsesService) writeUpstreamError(writer http.ResponseWriter, requestID, model string, started time.Time, err error) {
	var upstreamErr *codexResponsesUpstreamError
	if errors.As(err, &upstreamErr) {
		if upstreamErr.retryAfter != "" {
			writer.Header().Set("Retry-After", upstreamErr.retryAfter)
		}
		if upstreamErr.requestID != "" {
			writer.Header().Set("X-Request-Id", upstreamErr.requestID)
		}
		LogUpstreamEvent(upstreamErr.status, "request_failed", "component", "codex_responses", "request_id", requestID,
			"model", model, "status", upstreamErr.status, "returned_status", upstreamErr.status,
			"retry_after", upstreamErr.retryAfter, "duration", logDuration(started))
		writeProxyError(writer, upstreamErr.status, proxyErrorType(upstreamErr.status), upstreamErr.Error())
		return
	}
	LogErrorEvent("request_failed", "component", "codex_responses", "request_id", requestID,
		"model", model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", err)
	writeProxyError(writer, http.StatusBadGateway, "api_error", err.Error())
}

// requestSessionID is the client's conversation id, or a fresh one. Codex sends
// Session_id; Grok Build sends x-grok-conv-id.
func (s *codexResponsesService) requestSessionID(request *http.Request) string {
	for _, name := range []string{"Session_id", "X-Grok-Conv-Id", "X-Session-Id"} {
		if value := strings.TrimSpace(request.Header.Get(name)); value != "" && len(value) <= 256 {
			return value
		}
	}
	return uuidString()
}

func (s *codexResponsesService) recordUsage(clientModel string, assembler *responseAssembler) {
	if s.usage == nil {
		return
	}
	input, output := assembler.tokenTotals()
	s.usage.Add(clientModel, int64(input), int64(output), int64(assembler.cacheReadTokens), int64(assembler.cacheWriteTokens))
}

// call runs one upstream round-trip inside the shared fast-retry loop: a 429
// or 5xx outcome is retried twice more (500ms/1s) before being returned
// as-is, letting Claude Code do its own full backoff over the relayed
// status/body/Retry-After. The 401 refresh below is part of one attempt.
func (s *codexResponsesService) call(ctx context.Context, body []byte, sessionID string, dumpPayload bool) (*http.Response, error) {
	// The Codex subscription backend does not accept max_output_tokens.
	// Generic Responses gateways (including xAI) keep the caller's limit.
	if _, subscription := s.authorizer.(*codexOAuthAuthorizer); subscription {
		var err error
		body, err = sjson.DeleteBytes(body, "max_output_tokens")
		if err != nil {
			return nil, err
		}
	}
	return retryUpstream(ctx, "codex_responses", func() (*http.Response, error) {
		var err error
		if !s.xai && ctx.Value(codexCompactRequest{}) != true {
			body, err = s.addClientMetadata(body, sessionID)
			if err != nil {
				return nil, err
			}
		}
		auth, err := s.authorizer.authorize(ctx, false)
		if err != nil {
			return nil, err
		}
		response, err := s.callOnce(ctx, body, sessionID, auth, dumpPayload)
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusUnauthorized && s.authorizer.isOAuth() {
			drainAndClose(response)
			LogWarnEvent("credential_refresh", "component", "codex_responses", "request_id", requestLogID(ctx),
				"credential", auth.credential, "status", http.StatusUnauthorized, "action", "refresh_and_retry_once")
			// Another concurrent request may already have refreshed this credential.
			// Reuse its token instead of rotating the same refresh token twice.
			current, currentErr := s.authorizer.authorize(ctx, false)
			if currentErr == nil && current.token != "" && current.token != auth.token {
				auth = current
			} else {
				auth, err = s.authorizer.authorize(withRejectedOAuthToken(ctx, auth.token), true)
			}
			if err != nil {
				return nil, &codexResponsesUpstreamError{
					status: http.StatusUnauthorized,
					body:   fmt.Sprintf("upstream rejected the access token and credential refresh failed: %v", err),
				}
			}
			response, err = s.callOnce(ctx, body, sessionID, auth, dumpPayload)
			if err != nil {
				return nil, err
			}
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, codexDrainError(ctx, response)
		}
		return response, nil
	})
}

func (s *codexResponsesService) callOnce(ctx context.Context, body []byte, sessionID string, auth codexResponsesAuthorization, dumpPayload bool) (*http.Response, error) {
	target := protocol.NormalizeOpenAIResponsesURL(s.endpoint)
	if ctx.Value(codexCompactRequest{}) == true {
		target = strings.TrimRight(target, "/") + "/compact"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if s.xai {
		applyXaiGrokHeaders(request.Header, sessionID, uuidString(), s.installationID, codexResponsesModel(body), auth)
	} else {
		codexidentity.ApplyTurnHeaders(request.Header, sessionID, sessionID, s.windowID)
	}
	request.Header.Set("Authorization", "Bearer "+auth.token)
	request.Header.Set("Content-Type", "application/json")
	if ctx.Value(codexCompactRequest{}) == true {
		request.Header.Set("Accept", "application/json")
	} else {
		request.Header.Set("Accept", "text/event-stream")
	}
	if auth.accountID != "" {
		request.Header.Set("Chatgpt-Account-Id", auth.accountID)
	}
	applyOpenCodeSession(ctx, request)
	LogDebugEvent("upstream_request", "component", "codex_responses", "request_id", requestLogID(ctx),
		"method", http.MethodPost, "endpoint", SafeLogEndpoint(target), "credential", auth.credential,
		"body_bytes", len(body), "payload_logged", dumpPayload,
		"codex_version", codexidentity.ClientVersion, "originator", codexidentity.Originator,
		"user_agent", codexidentity.UserAgent(), "session_id", sessionID, "thread_id", sessionID,
		"window_id", s.windowID)
	if dumpPayload {
		DebugHTTPBody(fmt.Sprintf("codex responses request request_id=%s", requestLogID(ctx)), body)
	}
	started := time.Now()
	trace, traceSnapshot := newCodexHTTPTrace(started)
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := s.client.Do(request)
	if err != nil {
		timings := traceSnapshot()
		LogWarnEvent("upstream_transport_failed", "component", "codex_responses", "request_id", requestLogID(ctx),
			"connection_reused", timings.connectionReused, "connection_wait", timings.connectionWait,
			"dns", timings.dns, "connect", timings.connect, "tls", timings.tls,
			"request_write", timings.requestWrite, "ttfb", timings.ttfb,
			"write_to_first_byte", timings.writeToFirstByte, "wrote_request_error", timings.wroteRequestErr,
			"duration", logDuration(started), "error", err)
		return nil, fmt.Errorf("call Codex Responses upstream: %w", err)
	}
	timings := traceSnapshot()
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "codex_responses", "request_id", requestLogID(ctx),
		"status", response.StatusCode, "credential", auth.credential, "retry_after", response.Header.Get("Retry-After"),
		"upstream_request_id", firstHeader(response.Header, "X-Request-Id", "Request-Id"),
		"content_type", response.Header.Get("Content-Type"), "content_length", response.ContentLength,
		"connection_reused", timings.connectionReused, "connection_wait", timings.connectionWait,
		"dns", timings.dns, "connect", timings.connect, "tls", timings.tls,
		"request_write", timings.requestWrite, "ttfb", timings.ttfb,
		"write_to_first_byte", timings.writeToFirstByte, "wrote_request_error", timings.wroteRequestErr,
		"duration", logDuration(started))
	return response, nil
}

func codexResponsesModel(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	if decodeProtocolJSON(body, &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.Model)
}

func (s *codexResponsesService) addClientMetadata(body []byte, sessionID string) ([]byte, error) {
	var payload map[string]any
	if err := decodeProtocolJSON(body, &payload); err != nil {
		return nil, fmt.Errorf("decode Codex Responses request metadata: %w", err)
	}
	metadata, _ := payload["client_metadata"].(map[string]any)
	if metadata == nil {
		metadata = make(map[string]any, 4)
	}
	metadata["x-codex-installation-id"] = s.installationID
	metadata["session_id"] = sessionID
	metadata["thread_id"] = sessionID
	metadata["x-codex-window-id"] = s.windowID
	payload["client_metadata"] = metadata
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Codex Responses request metadata: %w", err)
	}
	return encoded, nil
}

func codexDrainError(ctx context.Context, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, codexResponsesMaxErrorBytes))
	_ = response.Body.Close()
	DebugHTTPBody(fmt.Sprintf("codex responses response request_id=%s status=%d", requestLogID(ctx), response.StatusCode), body)
	return &codexResponsesUpstreamError{
		status: response.StatusCode, body: strings.TrimSpace(string(body)),
		retryAfter: response.Header.Get("Retry-After"), requestID: firstHeader(response.Header, "X-Request-Id", "Request-Id"),
	}
}

func drainAndClose(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, codexResponsesMaxErrorBytes))
	_ = response.Body.Close()
}

func firstHeader(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

type codexCompactRequest struct{}

// servePassthrough forwards a Responses request to the Responses upstream. It
// shares authorization, refresh and retry with converted requests, and reads
// token usage off the relayed stream.
func (s *codexResponsesService) servePassthrough(writer http.ResponseWriter, request *http.Request, payload map[string]any, compact bool) {
	model, _ := payload["model"].(string)
	resolved, ok := s.modelRoute[strings.ToLower(strings.TrimSpace(model))]
	if !ok {
		writeProxyError(writer, 400, "invalid_request_error", "model is not in the provider catalog")
		return
	}
	payload["model"] = resolved
	wantsStream, _ := payload["stream"].(bool)
	if !compact {
		payload["stream"] = true
		payload["store"] = false
		if payload["include"] == nil {
			payload["include"] = []string{"reasoning.encrypted_content"}
		}
	}
	sessionID := s.requestSessionID(request)
	if !compact && payload["prompt_cache_key"] == nil {
		payload["prompt_cache_key"] = sessionID
	}
	body, _ := json.Marshal(payload)
	ctx, _ := withRequestLogID(request.Context())
	ctx = withClientSession(ctx, request)
	if compact {
		ctx = context.WithValue(ctx, codexCompactRequest{}, true)
	}
	response, err := s.call(ctx, body, sessionID, true)
	if err != nil {
		var upstreamErr *codexResponsesUpstreamError
		if errors.As(err, &upstreamErr) {
			if upstreamErr.retryAfter != "" {
				writer.Header().Set("Retry-After", upstreamErr.retryAfter)
			}
			writeProxyError(writer, upstreamErr.status, proxyErrorType(upstreamErr.status), upstreamErr.Error())
			return
		}
		writeProxyError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer response.Body.Close()
	for _, name := range []string{"Content-Type", "Retry-After", "X-Request-Id"} {
		if value := response.Header.Get(name); value != "" {
			writer.Header().Set(name, value)
		}
	}
	if !compact && !wantsStream && strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		var completed json.RawMessage
		err := readChatCompletionsSSE(response.Body, func(raw []byte) error {
			var event struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(raw, &event) != nil {
				return fmt.Errorf("invalid Responses stream event")
			}
			if event.Type == "response.completed" || event.Type == "response.incomplete" {
				completed = append(json.RawMessage(nil), event.Response...)
			}
			if event.Type == "response.failed" || event.Type == "error" {
				return fmt.Errorf("upstream Responses stream failed")
			}
			return nil
		})
		if err != nil || len(completed) == 0 {
			writeProxyError(writer, 502, "api_error", "upstream Responses stream did not complete")
			return
		}
		observer := &responsesUsageObserver{}
		observer.event(completed)
		s.recordObservedUsage(model, observer)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(response.StatusCode)
		_, _ = writer.Write(completed)
		return
	}
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(response.StatusCode)
	observer := &responsesUsageObserver{}
	defer func() { s.recordObservedUsage(model, observer) }()
	buffer := make([]byte, 32<<10)
	for {
		n, err := response.Body.Read(buffer)
		if n > 0 {
			observer.observe(buffer[:n])
			if _, writeErr := writer.Write(buffer[:n]); writeErr != nil {
				return
			}
			_ = http.NewResponseController(writer).Flush()
		}
		if err != nil {
			return
		}
	}
}

func (s *codexResponsesService) recordObservedUsage(clientModel string, observer *responsesUsageObserver) {
	if s.usage == nil || !observer.seen {
		return
	}
	s.usage.Add(clientModel, int64(max(0, observer.input-observer.cached)), int64(observer.output), int64(observer.cached), 0)
}

// responsesUsageObserver reads token usage off a relayed Responses stream (or
// JSON body) without altering it. It keeps only the bytes of the event in
// progress, and gives up on an event too large to be a usage report.
type responsesUsageObserver struct {
	pending               []byte
	input, output, cached int
	seen                  bool
}

const responsesUsageObserverMaxEvent = 8 << 20

func (o *responsesUsageObserver) observe(data []byte) {
	o.pending = append(o.pending, data...)
	for {
		end := bytes.Index(o.pending, []byte("\n\n"))
		if end < 0 {
			break
		}
		o.event(o.pending[:end])
		o.pending = o.pending[end+2:]
	}
	if len(o.pending) > responsesUsageObserverMaxEvent {
		o.pending = nil
	}
}

func (o *responsesUsageObserver) event(raw []byte) {
	if !bytes.Contains(raw, []byte(`"usage"`)) {
		return
	}
	var data []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimSpace(value)...)
		}
	}
	if len(data) == 0 {
		data = bytes.TrimSpace(raw)
	}
	usage := gjson.GetBytes(data, "response.usage")
	if !usage.Exists() {
		usage = gjson.GetBytes(data, "usage")
	}
	if !usage.IsObject() {
		return
	}
	o.seen = true
	o.input = int(usage.Get("input_tokens").Int())
	o.output = int(usage.Get("output_tokens").Int())
	o.cached = int(usage.Get("input_tokens_details.cached_tokens").Int())
}

func (a *codexOAuthAuthorizer) isOAuth() bool { return true }

func (a *codexOAuthAuthorizer) authorize(ctx context.Context, force bool) (codexResponsesAuthorization, error) {
	release, err := lockOAuthCredential(ctx, a.path)
	if err != nil {
		return codexResponsesAuthorization{}, err
	}
	defer release()
	credential, err := a.load()
	if err != nil {
		return codexResponsesAuthorization{}, err
	}
	if credential.disabled {
		return codexResponsesAuthorization{}, fmt.Errorf("Codex credential %s is disabled", filepath.Base(a.path))
	}
	force = force && oauthTokenStillRejected(ctx, credential.accessToken)
	if force || credential.accessToken == "" || (!credential.expiresAt.IsZero() && time.Now().Add(time.Minute).After(credential.expiresAt)) {
		credential, err = a.refresh(ctx, credential)
		if err != nil {
			return codexResponsesAuthorization{}, err
		}
	}
	return codexResponsesAuthorization{
		token: credential.accessToken, accountID: credential.accountID, credential: filepath.Base(a.path),
	}, nil
}

func (a *codexOAuthAuthorizer) load() (*codexOAuthCredential, error) {
	raw, err := os.ReadFile(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no Codex credentials found; run `ccl oauth gpt` first")
		}
		return nil, fmt.Errorf("read Codex credential %s: %w", filepath.Base(a.path), err)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode Codex credential %s: %w", filepath.Base(a.path), err)
	}
	credentialType := strings.ToLower(strings.TrimSpace(stringValue(metadata["type"])))
	if credentialType != ProviderCodex && credentialType != ProviderChatGPT {
		return nil, fmt.Errorf("credential %s is type %q, not Codex", filepath.Base(a.path), credentialType)
	}
	expiresAt := parseCodexExpiry(firstMetadataString(metadata, "expired", "expires_at", "expiry"))
	disabled, _ := metadata["disabled"].(bool)
	return &codexOAuthCredential{
		metadata: metadata, accessToken: firstMetadataString(metadata, "access_token"),
		refreshToken: firstMetadataString(metadata, "refresh_token"), accountID: firstMetadataString(metadata, "account_id"),
		email: firstMetadataString(metadata, "email"), expiresAt: expiresAt, disabled: disabled,
	}, nil
}

func (a *codexOAuthAuthorizer) refresh(ctx context.Context, credential *codexOAuthCredential) (*codexOAuthCredential, error) {
	if credential.refreshToken == "" {
		return nil, fmt.Errorf("Codex credential %s has no refresh token", filepath.Base(a.path))
	}
	form := url.Values{
		"client_id": {codexResponsesOAuthClientID}, "grant_type": {"refresh_token"},
		"refresh_token": {credential.refreshToken}, "scope": {"openid profile email"},
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(refreshCtx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("refresh Codex token: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, codexResponsesMaxErrorBytes))
	if err != nil {
		return nil, fmt.Errorf("read Codex token refresh: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh Codex token: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("decode Codex token refresh: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, fmt.Errorf("refresh Codex token: response has no access token")
	}
	credential.accessToken = token.AccessToken
	if token.RefreshToken != "" {
		credential.refreshToken = token.RefreshToken
	}
	if token.ExpiresIn > 0 {
		credential.expiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	if accountID, email := codexJWTIdentity(token.IDToken); accountID != "" || email != "" {
		if accountID != "" {
			credential.accountID = accountID
		}
		if email != "" {
			credential.email = email
		}
	}
	credential.metadata["type"] = ProviderCodex
	credential.metadata["access_token"] = credential.accessToken
	credential.metadata["refresh_token"] = credential.refreshToken
	if token.IDToken != "" {
		credential.metadata["id_token"] = token.IDToken
	}
	if credential.accountID != "" {
		credential.metadata["account_id"] = credential.accountID
	}
	if credential.email != "" {
		credential.metadata["email"] = credential.email
	}
	if !credential.expiresAt.IsZero() {
		credential.metadata["expired"] = credential.expiresAt.UTC().Format(time.RFC3339)
	}
	credential.metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if err := writeCodexCredentialAtomic(a.path, credential.metadata); err != nil {
		return nil, err
	}
	LogInfof("credential refreshed component=codex_responses credential_file=%s expires_at=%s",
		filepath.Base(a.path), credential.expiresAt.UTC().Format(time.RFC3339))
	return credential, nil
}

func (a *codexOAuthAuthorizer) listAuths() []*AuthInfo {
	credential, err := a.load()
	if err != nil {
		return nil
	}
	status := StatusActive
	if credential.disabled {
		status = StatusDisabled
	}
	return []*AuthInfo{{
		ID: filepath.Base(a.path), Provider: ProviderCodex, FileName: a.path, Label: credential.email,
		Status: status, Disabled: credential.disabled, Metadata: credential.metadata,
	}}
}

func writeCodexCredentialAtomic(path string, metadata map[string]any) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Codex credential update: %w", err)
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

func parseCodexExpiry(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed
	}
	return time.Time{}
}

func codexJWTIdentity(token string) (accountID, email string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Email string `json:"email"`
		Auth  struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	return claims.Auth.AccountID, claims.Email
}
