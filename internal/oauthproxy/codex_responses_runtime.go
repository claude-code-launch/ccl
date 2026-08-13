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
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/codexidentity"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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
}

type codexResponsesAuthorizer interface {
	authorize(context.Context, bool) (codexResponsesAuthorization, error)
	listAuths() []*coreauth.Auth
	isOAuth() bool
}

type codexStaticAuthorizer struct{ token string }

func (a *codexStaticAuthorizer) authorize(context.Context, bool) (codexResponsesAuthorization, error) {
	return codexResponsesAuthorization{token: a.token, credential: "api-key"}, nil
}
func (*codexStaticAuthorizer) listAuths() []*coreauth.Auth { return nil }
func (*codexStaticAuthorizer) isOAuth() bool               { return false }

type codexOAuthAuthorizer struct {
	path   string
	client *http.Client
	mu     sync.Mutex
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
}

type codexResponsesUpstreamError struct {
	status     int
	body       string
	retryAfter string
	requestID  string
}

type codexBufferedSSEWriter struct {
	bytes.Buffer
	header http.Header
}

func newCodexBufferedSSEWriter() *codexBufferedSSEWriter {
	return &codexBufferedSSEWriter{header: make(http.Header)}
}

func (writer *codexBufferedSSEWriter) Header() http.Header { return writer.header }
func (*codexBufferedSSEWriter) WriteHeader(int)            {}

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
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(authDir, filepath.Base(credentialFile))
	authorizer := &codexOAuthAuthorizer{
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
		return nil, fmt.Errorf("Codex credential %s is disabled", filepath.Base(path))
	}
	if credential.accessToken == "" && credential.refreshToken == "" {
		return nil, fmt.Errorf("Codex credential %s has no access or refresh token", filepath.Base(path))
	}
	if strings.TrimSpace(modelSpec) == "" {
		modelSpec = "gpt-5.6-sol,gpt-5.6-terra,gpt-5.6-luna"
	}
	runtime, err := startCodexResponsesRuntime(parent, codexOAuthBaseURL, modelSpec, authorizer)
	if err != nil {
		return nil, err
	}
	runtime.listAuths = authorizer.listAuths
	LogInfof("runtime start oauth provider=gpt backend=codex protocol=codex_responses port=%s credential_file=%s model_count=%d",
		strings.TrimPrefix(strings.TrimSuffix(runtime.endpoint, "/v1"), "http://"), filepath.Base(path), len(runtime.models))
	return runtime, nil
}

func startCodexResponsesRuntime(parent context.Context, endpoint, modelSpec string, authorizer codexResponsesAuthorizer) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	endpoint = normalizeOpenAIBaseURL(endpoint)
	if endpoint == "" || authorizer == nil {
		return nil, fmt.Errorf("Codex Responses runtime requires endpoint and authorization")
	}
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return nil, fmt.Errorf("Codex Responses runtime requires at least one model")
	}
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("listen for Codex Responses runtime: %w", err)
	}
	usage := NewUsageTracker()
	service := newCodexResponsesService(apiKey, endpoint, routes, authorizer, usage)
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

func newCodexResponsesService(apiKey, endpoint string, routes []runtimeModelRoute, authorizer codexResponsesAuthorizer, usage *UsageTracker) *codexResponsesService {
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
		client: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
			ResponseHeaderTimeout: 90 * time.Second,
		}},
	}
}

func (s *codexResponsesService) handler() http.Handler {
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
	mux.HandleFunc("/v1/responses", s.handleRawResponses)
	mux.HandleFunc("/responses", s.handleRawResponses)
	return mux
}

func (s *codexResponsesService) authorized(request *http.Request) bool {
	if request.Header.Get("x-api-key") == s.apiKey {
		return true
	}
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) == s.apiKey
}

func (s *codexResponsesService) handleModels(writer http.ResponseWriter, request *http.Request) {
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

func (s *codexResponsesService) handleCountTokens(writer http.ResponseWriter, request *http.Request) {
	_, requestID := withRequestLogID(request.Context())
	started := time.Now()
	if !s.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	raw, err := readAnthropicInboundBody(writer, request, codexResponsesMaxBodyBytes)
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	converted, err := convertAnthropicToCodexResponses(raw)
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if route := s.modelRoute[strings.ToLower(converted.clientModel)]; route != "" {
		var body map[string]any
		if json.Unmarshal(converted.body, &body) == nil {
			body["model"] = route
			converted.body, _ = json.Marshal(body)
		}
	}
	inputTokens, countErr := countCodexResponsesInputTokens(converted.body)
	estimator := "o200k_base"
	if countErr != nil {
		inputTokens = conservativeCodexTokenEstimate(converted.body)
		estimator = "bytes_over_2_fallback"
		LogWarnEvent("token_count_fallback", "component", "codex_responses", "request_id", requestID,
			"model", converted.model, "body_bytes", len(converted.body), "error", countErr)
	}
	LogDebugEvent("token_counted", "component", "codex_responses", "request_id", requestID,
		"model", converted.model, "body_bytes", len(converted.body), "input_tokens", inputTokens,
		"estimator", estimator, "compaction", converted.compaction,
		"compaction_reason", converted.compactionReason, "compaction_signals", converted.compactionSignals,
		"source_messages", converted.sourceMessages, "body_fingerprint", codexRequestFingerprint(converted.body),
		"duration", logDuration(started))
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"input_tokens": inputTokens})
}

func (s *codexResponsesService) handleMessages(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	LogDebugEvent("request_received", "component", "codex_responses", "request_id", requestID,
		"path", request.URL.Path, "method", request.Method)
	if !s.authorized(request) {
		LogWarnEvent("request_rejected", "component", "codex_responses", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusUnauthorized, "reason", "invalid_local_api_key")
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	readStarted := time.Now()
	raw, err := readAnthropicInboundBody(writer, request, codexResponsesMaxBodyBytes)
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	LogDebugEvent("request_body_read", "component", "codex_responses", "request_id", requestID,
		"body_bytes", len(raw), "duration", logDuration(readStarted))
	convertStarted := time.Now()
	converted, err := convertAnthropicToCodexResponses(raw)
	if err != nil {
		LogWarnEvent("request_rejected", "component", "codex_responses", "request_id", requestID,
			"status", http.StatusBadRequest, "reason", "request_conversion", "error", err)
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if route := s.modelRoute[strings.ToLower(converted.clientModel)]; route != "" {
		converted.model = route
		converted.upstreamModel = route
		var body map[string]any
		if json.Unmarshal(converted.body, &body) == nil {
			body["model"] = route
			converted.body, _ = json.Marshal(body)
		}
	}
	LogDebugEvent("request_converted", "component", "codex_responses", "request_id", requestID,
		"client_model", converted.clientModel, "upstream_model", converted.model,
		"stream", converted.stream, "compaction", converted.compaction,
		"compaction_reason", converted.compactionReason, "compaction_signals", converted.compactionSignals,
		"source_messages", converted.sourceMessages, "body_bytes", len(converted.body),
		"body_fingerprint", codexRequestFingerprint(converted.body),
		"duration", logDuration(convertStarted), "auth", map[bool]string{true: "oauth", false: "api_key"}[s.authorizer.isOAuth()])
	if converted.compaction {
		LogInfoEvent("compaction_detected", "component", "codex_responses", "request_id", requestID,
			"model", converted.model, "reason", converted.compactionReason,
			"signals", converted.compactionSignals, "source_messages", converted.sourceMessages,
			"source_bytes", len(raw), "converted_bytes", len(converted.body))
		trimStarted := time.Now()
		trimmed, stats, trimErr := trimCodexCompactionBody(converted.body, codexCompactionSoftTargetTokens)
		converted.compactionTokens = stats.finalTokens
		if trimErr != nil {
			LogWarnEvent("compaction_preflight_failed", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "tokenizer_passes", stats.tokenizerPasses,
				"duration", logDuration(trimStarted), "error", trimErr)
		} else if stats.droppedItems > 0 {
			converted.body = trimmed
			converted.droppedItems += stats.droppedItems
			LogWarnEvent("compaction_context_trimmed", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "phase", "preflight", "original_tokens", stats.originalTokens,
				"final_tokens", stats.finalTokens, "dropped_items", stats.droppedItems,
				"tokenizer_passes", stats.tokenizerPasses, "duration", logDuration(trimStarted))
		} else {
			LogDebugEvent("compaction_preflight_complete", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "input_tokens", stats.originalTokens,
				"tokenizer_passes", stats.tokenizerPasses, "duration", logDuration(trimStarted))
		}
	}
	if converted.compaction {
		LogDebugEvent("compaction_upstream_start", "component", "codex_responses", "request_id", requestID,
			"model", converted.model, "attempt", 1, "input_tokens", converted.compactionTokens,
			"input_bytes", len(converted.body), "input_fingerprint", codexRequestFingerprint(converted.body),
			"dropped_items", converted.droppedItems,
			"elapsed", logDuration(started))
	}
	response, err := s.call(requestCtx, converted.body, converted.sessionID, !converted.compaction)
	if err != nil && converted.compaction && isCodexContextOverflow(err) {
		trimStarted := time.Now()
		trimmed, stats, trimErr := trimCodexCompactionBody(converted.body, codexCompactionRecoveryTarget(converted.body))
		if trimErr != nil {
			LogWarnEvent("compaction_recovery_failed", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "tokenizer_passes", stats.tokenizerPasses,
				"duration", logDuration(trimStarted), "error", trimErr)
		} else if stats.droppedItems > 0 {
			converted.body = trimmed
			converted.compactionTokens = stats.finalTokens
			converted.droppedItems += stats.droppedItems
			LogWarnEvent("compaction_context_trimmed", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "phase", "retry", "original_tokens", stats.originalTokens,
				"final_tokens", stats.finalTokens, "dropped_items", stats.droppedItems,
				"tokenizer_passes", stats.tokenizerPasses, "duration", logDuration(trimStarted))
			LogDebugEvent("compaction_upstream_start", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "attempt", 2, "trigger", "http_context_overflow",
				"input_tokens", converted.compactionTokens, "input_bytes", len(converted.body),
				"input_fingerprint", codexRequestFingerprint(converted.body),
				"dropped_items", converted.droppedItems, "elapsed", logDuration(started))
			response, err = s.call(requestCtx, converted.body, converted.sessionID, false)
		}
	}
	if err != nil {
		var upstreamErr *codexResponsesUpstreamError
		if errors.As(err, &upstreamErr) {
			if upstreamErr.retryAfter != "" {
				writer.Header().Set("Retry-After", upstreamErr.retryAfter)
			}
			if upstreamErr.requestID != "" {
				writer.Header().Set("X-Request-Id", upstreamErr.requestID)
			}
			LogUpstreamEvent(upstreamErr.status, "request_failed", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "status", upstreamErr.status, "returned_status", upstreamErr.status,
				"retry_after", upstreamErr.retryAfter, "duration", logDuration(started))
			writeAnthropicError(writer, upstreamErr.status, anthropicErrorType(upstreamErr.status), upstreamErr.Error())
			return
		}
		LogErrorEvent("request_failed", "component", "codex_responses", "request_id", requestID,
			"model", converted.model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	if converted.compaction {
		s.handleCompactionResponse(writer, requestCtx, requestID, started, converted, response)
		return
	}
	defer response.Body.Close()
	if converted.stream {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, writer)
		err = processCodexResponsesStream(response.Body, assembler)
		s.recordUsage(converted, assembler)
		if err != nil {
			LogErrorEvent("stream_conversion_failed", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "duration", logDuration(started), "error", err)
			_ = assembler.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}})
			return
		}
	} else {
		assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, nil)
		if err = processCodexResponsesStream(response.Body, assembler); err != nil {
			LogErrorEvent("stream_conversion_failed", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", err)
			writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		s.recordUsage(converted, assembler)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(assembler.response())
	}
	LogDebugEvent("request_complete", "component", "codex_responses", "request_id", requestID,
		"model", converted.model, "status", http.StatusOK, "stream", converted.stream, "duration", logDuration(started))
}

// handleCompactionResponse buffers the upstream stream before exposing it to
// Claude Code. Codex can return HTTP 200 and only then report context overflow
// in an SSE error event; buffering lets CCL shrink and retry once without
// Claude Code turning that event into a minutes-long retry loop.
func (s *codexResponsesService) handleCompactionResponse(
	writer http.ResponseWriter,
	requestCtx context.Context,
	requestID string,
	started time.Time,
	converted *codexResponsesConvertedRequest,
	response *http.Response,
) {
	var (
		assembler *anthropicResponseAssembler
		stream    []byte
		metrics   codexResponsesStreamMetrics
		err       error
	)
	for attempt := 0; attempt < 2; attempt++ {
		streamStarted := time.Now()
		upstreamRequestID := firstHeader(response.Header, "X-Request-Id", "Request-Id")
		assembler, stream, metrics, err = bufferCodexCompactionResponseObserved(response.Body, converted)
		_ = response.Body.Close()
		inputTokens, outputTokens := 0, 0
		if assembler != nil {
			inputTokens, outputTokens = assembler.tokenTotals()
		}
		attrs := []any{
			"component", "codex_responses", "request_id", requestID, "model", converted.model,
			"attempt", attempt + 1, "upstream_request_id", upstreamRequestID,
			"input_tokens", inputTokens, "output_tokens", outputTokens,
			"events", metrics.events, "text_delta_events", metrics.textDeltaEvents,
			"reasoning_delta_events", metrics.reasoningDeltaEvents,
			"text_bytes", metrics.textBytes, "reasoning_bytes", metrics.reasoningBytes,
			"anthropic_stream_bytes", len(stream), "terminal_event", metrics.terminalType,
			"time_to_first_event", codexStreamMilestone(streamStarted, metrics.firstEventAt),
			"time_to_first_content", codexStreamMilestone(streamStarted, metrics.firstContentAt),
			"stream_duration", logDuration(streamStarted), "total_elapsed", logDuration(started),
		}
		if err != nil {
			attrs = append(attrs, "context_overflow", isCodexContextOverflow(err), "error", err)
			LogWarnEvent("compaction_stream_failed", attrs...)
		} else {
			LogDebugEvent("compaction_stream_complete", attrs...)
		}
		if err == nil || attempt > 0 || !isCodexContextOverflow(err) {
			break
		}

		trimStarted := time.Now()
		trimmed, stats, trimErr := trimCodexCompactionBody(converted.body, codexCompactionRecoveryTarget(converted.body))
		if trimErr != nil || stats.droppedItems == 0 {
			if trimErr != nil {
				LogWarnEvent("compaction_recovery_failed", "component", "codex_responses", "request_id", requestID,
					"model", converted.model, "tokenizer_passes", stats.tokenizerPasses,
					"duration", logDuration(trimStarted), "error", trimErr)
			}
			break
		}
		converted.body = trimmed
		LogWarnEvent("compaction_context_trimmed", "component", "codex_responses", "request_id", requestID,
			"model", converted.model, "phase", "sse_retry", "original_tokens", stats.originalTokens,
			"final_tokens", stats.finalTokens, "dropped_items", stats.droppedItems,
			"tokenizer_passes", stats.tokenizerPasses, "duration", logDuration(trimStarted))
		converted.compactionTokens = stats.finalTokens
		converted.droppedItems += stats.droppedItems
		LogDebugEvent("compaction_upstream_start", "component", "codex_responses", "request_id", requestID,
			"model", converted.model, "attempt", attempt+2, "trigger", "sse_context_overflow",
			"input_tokens", converted.compactionTokens, "input_bytes", len(converted.body),
			"input_fingerprint", codexRequestFingerprint(converted.body),
			"dropped_items", converted.droppedItems, "elapsed", logDuration(started))
		response, err = s.call(requestCtx, converted.body, converted.sessionID, false)
		if err != nil {
			break
		}
	}
	if err != nil {
		LogErrorEvent("compaction_failed", "component", "codex_responses", "request_id", requestID,
			"model", converted.model, "returned_status", http.StatusBadGateway,
			"duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	s.recordUsage(converted, assembler)
	if converted.stream {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		_, _ = writer.Write(stream)
	} else {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(assembler.response())
	}
	LogDebugEvent("request_complete", "component", "codex_responses", "request_id", requestID,
		"model", converted.model, "status", http.StatusOK, "stream", converted.stream,
		"compaction", true, "duration", logDuration(started))
}

func bufferCodexCompactionResponseObserved(reader io.Reader, converted *codexResponsesConvertedRequest) (*anthropicResponseAssembler, []byte, codexResponsesStreamMetrics, error) {
	if converted == nil {
		return nil, nil, codexResponsesStreamMetrics{}, fmt.Errorf("missing converted compaction request")
	}
	var buffered *codexBufferedSSEWriter
	var writer http.ResponseWriter
	if converted.stream {
		buffered = newCodexBufferedSSEWriter()
		writer = buffered
	}
	assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, writer)
	metrics, err := processCodexResponsesStreamObserved(reader, assembler)
	if err != nil {
		return assembler, nil, metrics, err
	}
	if buffered == nil {
		return assembler, nil, metrics, nil
	}
	return assembler, append([]byte(nil), buffered.Bytes()...), metrics, nil
}

func codexStreamMilestone(started, milestone time.Time) time.Duration {
	if milestone.IsZero() {
		return 0
	}
	return milestone.Sub(started).Round(time.Millisecond)
}

func (s *codexResponsesService) recordUsage(converted *codexResponsesConvertedRequest, assembler *anthropicResponseAssembler) {
	if s.usage == nil {
		return
	}
	input, output := assembler.tokenTotals()
	s.usage.Add(converted.clientModel, int64(input), int64(output), int64(assembler.cacheReadTokens), int64(assembler.cacheWriteTokens))
}

func (s *codexResponsesService) call(ctx context.Context, body []byte, sessionID string, dumpPayload bool) (*http.Response, error) {
	var err error
	body, err = s.addClientMetadata(body, sessionID)
	if err != nil {
		return nil, err
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
			auth, err = s.authorizer.authorize(ctx, true)
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
}

func (s *codexResponsesService) callOnce(ctx context.Context, body []byte, sessionID string, auth codexResponsesAuthorization, dumpPayload bool) (*http.Response, error) {
	target := strings.TrimRight(s.endpoint, "/") + "/responses"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	codexidentity.ApplyTurnHeaders(request.Header, sessionID, sessionID, s.windowID)
	request.Header.Set("Authorization", "Bearer "+auth.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	if auth.accountID != "" {
		request.Header.Set("Chatgpt-Account-Id", auth.accountID)
	}
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

func (s *codexResponsesService) addClientMetadata(body []byte, sessionID string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
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

// handleRawResponses keeps the local Responses surface useful for diagnostics
// while applying the exact same CCL-owned Codex identity and OAuth behavior.
func (s *codexResponsesService) handleRawResponses(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	body, err := readAnthropicInboundBody(writer, request, codexResponsesMaxBodyBytes)
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "invalid Responses request JSON")
		return
	}
	payload["stream"] = true
	payload["store"] = false
	if payload["include"] == nil {
		payload["include"] = []string{"reasoning.encrypted_content"}
	}
	sessionID := uuidString()
	if payload["prompt_cache_key"] == nil {
		payload["prompt_cache_key"] = sessionID
	}
	body, _ = json.Marshal(payload)
	ctx, _ := withRequestLogID(request.Context())
	response, err := s.call(ctx, body, sessionID, true)
	if err != nil {
		var upstreamErr *codexResponsesUpstreamError
		if errors.As(err, &upstreamErr) {
			if upstreamErr.retryAfter != "" {
				writer.Header().Set("Retry-After", upstreamErr.retryAfter)
			}
			writeAnthropicError(writer, upstreamErr.status, anthropicErrorType(upstreamErr.status), upstreamErr.Error())
			return
		}
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		if strings.EqualFold(name, "Content-Length") || strings.EqualFold(name, "Content-Encoding") {
			continue
		}
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

func (a *codexOAuthAuthorizer) isOAuth() bool { return true }

func (a *codexOAuthAuthorizer) authorize(ctx context.Context, force bool) (codexResponsesAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	credential, err := a.load()
	if err != nil {
		return codexResponsesAuthorization{}, err
	}
	if credential.disabled {
		return codexResponsesAuthorization{}, fmt.Errorf("Codex credential %s is disabled", filepath.Base(a.path))
	}
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
	if credentialType != ProviderCodex && credentialType != ProviderChatGPT && credentialType != ProviderChatGPTLegacy {
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

func (a *codexOAuthAuthorizer) listAuths() []*coreauth.Auth {
	credential, err := a.load()
	if err != nil {
		return nil
	}
	status := coreauth.StatusActive
	if credential.disabled {
		status = coreauth.StatusDisabled
	}
	return []*coreauth.Auth{{
		ID: filepath.Base(a.path), Provider: ProviderCodex, FileName: a.path, Label: credential.email,
		Status: status, Disabled: credential.disabled, Metadata: credential.metadata,
	}}
}

func writeCodexCredentialAtomic(path string, metadata map[string]any) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".codex-credential-*")
	if err != nil {
		return fmt.Errorf("create Codex credential update: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(metadata); err != nil {
		_ = file.Close()
		return fmt.Errorf("encode Codex credential update: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace Codex credential %s: %w", filepath.Base(path), err)
	}
	return nil
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
