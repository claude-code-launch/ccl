package oauthproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/tidwall/gjson"
)

const (
	// commandcodeVersion fills x-command-code-version, the CLI version the
	// reference proxy reports when the npm registry lookup fails.
	commandcodeVersion = "1.27.1"
	// commandcodeDefaultAPIBase is the Command Code gateway when the provider
	// config does not pin an endpoint.
	commandcodeDefaultAPIBase = "https://api.commandcode.ai"
	// commandcodeInitRefresh / commandcodeInitJitter mirror the reference
	// client's per-key handshake cadence: fingerprint + lifecycle events are
	// sent before the first request and re-armed 8h plus up to 2h of jitter
	// later.
	commandcodeInitRefresh = 8 * time.Hour
	commandcodeInitJitter  = 2 * time.Hour
	// commandcodeInitTimeout bounds the handshake so an unresponsive identity
	// endpoint cannot wedge requests. A failure leaves the init unsent and the
	// next request retries it, exactly like the reference client.
	commandcodeInitTimeout = 10 * time.Second
	// commandcodeRetryAfter is the fixed retry hint the reference client
	// attaches to an upstream 429.
	commandcodeRetryAfter = "30"
)

// commandcodeAPIBase resolves the Command Code gateway: a provider-pinned
// endpoint wins, then the official CLI's COMMANDCODE_API_URL override, then
// the production default.
func commandcodeAPIBase(configured string) string {
	if base := strings.TrimRight(strings.TrimSpace(configured), "/"); base != "" {
		return base
	}
	if base := strings.TrimRight(strings.TrimSpace(os.Getenv("COMMANDCODE_API_URL")), "/"); base != "" {
		return base
	}
	return commandcodeDefaultAPIBase
}

// commandcodeModelCatalog is the authoritative model list the Command Code
// gateway serves, mirrored from the reference client. IDs are served verbatim:
// Claude Code requests them directly and messages pass through unaliased.
var commandcodeModelCatalog = []struct {
	id   string
	name string
}{
	{"claude-sonnet-4-6", "Claude Sonnet 4.6"},
	{"claude-opus-4-8", "Claude Opus 4.8"},
	{"claude-opus-4-7", "Claude Opus 4.7"},
	{"claude-haiku-4-5-20251001", "Claude Haiku 4.5"},
	{"gpt-5.5", "GPT-5.5"},
	{"gpt-5.4", "GPT-5.4"},
	{"gpt-5.4-mini", "GPT-5.4 Mini"},
	{"gpt-5.3-codex", "GPT-5.3 Codex"},
	{"deepseek/deepseek-v4-pro", "DeepSeek V4 Pro"},
	{"deepseek/deepseek-v4-flash", "DeepSeek V4 Flash"},
	{"moonshotai/Kimi-K2.6", "Kimi K2.6"},
	{"moonshotai/Kimi-K2.5", "Kimi K2.5"},
	{"zai-org/GLM-5.1", "GLM 5.1"},
	{"zai-org/GLM-5", "GLM 5"},
	{"MiniMaxAI/MiniMax-M3", "MiniMax M3"},
	{"MiniMaxAI/MiniMax-M2.7", "MiniMax M2.7"},
	{"MiniMaxAI/MiniMax-M2.5", "MiniMax M2.5"},
	{"Qwen/Qwen3.6-Max-Preview", "Qwen 3.6 Max Preview"},
	{"Qwen/Qwen3.6-Plus", "Qwen 3.6 Plus"},
	{"Qwen/Qwen3.7-Max", "Qwen 3.7 Max"},
	{"stepfun/Step-3.7-Flash", "Step 3.7 Flash"},
	{"stepfun/Step-3.5-Flash", "Step 3.5 Flash"},
	{"xiaomi/mimo-v2.5-pro", "MiMo V2.5 Pro"},
	{"xiaomi/mimo-v2.5", "MiMo V2.5"},
	{"google/gemini-3.5-flash", "Gemini 3.5 Flash"},
	{"google/gemini-3.1-flash-lite", "Gemini 3.1 Flash Lite"},
}

// commandcodeService is the CCL-owned data plane for a Command Code upstream:
// a loopback Anthropic Messages entrypoint that speaks the /alpha/generate
// NDJSON protocol. Identity headers, the fingerprint/lifecycle handshake,
// error mapping, and usage accounting all live here, so no CLIProxyAPI upgrade
// can change how commandcode behaves.
type commandcodeService struct {
	apiKey      string // loopback key
	endpoint    string // Command Code API base
	upstreamKey string // user_... API key
	sessionID   string
	projectSlug string
	fingerprint map[string]any
	models      []string
	client      *http.Client
	usage       *UsageTracker
	// initMu serializes the fingerprint/lifecycle handshake: it runs once
	// before the first request and re-arms every commandcodeInitRefresh plus
	// jitter, mirroring the reference client's per-key ensureInitialized.
	initMu     sync.Mutex
	initDone   bool
	nextInitAt time.Time
}

type commandcodeUpstreamError struct {
	status     int // original upstream status
	mapped     int // Anthropic-facing status after the reference status map
	body       string
	retryAfter string
}

func (e *commandcodeUpstreamError) Error() string {
	return fmt.Sprintf("Command Code upstream returned HTTP %d: %s",
		e.status, commandcodeErrorMessage(e.body, e.status))
}

// startCommandCodeRuntime starts a CCL-owned Anthropic Messages entrypoint
// against a Command Code API key. The endpoint falls back to the official
// gateway when the config does not pin one.
func startCommandCodeRuntime(parent context.Context, endpoint, upstreamAPIKey string) (*Runtime, error) {
	endpoint = commandcodeAPIBase(endpoint)
	upstreamAPIKey = strings.TrimSpace(upstreamAPIKey)
	if upstreamAPIKey == "" {
		return nil, fmt.Errorf("Command Code runtime requires an upstream API key")
	}
	sessionID := uuidString()
	fingerprint, err := commandcodeGenerateFingerprint()
	if err != nil {
		return nil, fmt.Errorf("generate Command Code device fingerprint: %w", err)
	}
	service := &commandcodeService{
		endpoint:    endpoint,
		upstreamKey: upstreamAPIKey,
		sessionID:   sessionID,
		projectSlug: commandcodeProjectSlug(sessionID),
		fingerprint: fingerprint,
		models:      commandcodeCatalogIDs(),
		usage:       NewUsageTracker(),
		client: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
			ResponseHeaderTimeout: 90 * time.Second,
		}},
	}
	return startCommandCodeRuntimeService(parent, service)
}

// startCommandCodeRuntimeService is the shared loopback server lifecycle,
// mirroring startOpenAIChatRuntimeService: bind 127.0.0.1:0, per-session
// random key, and shutdown-on-cancel.
func startCommandCodeRuntimeService(parent context.Context, service *commandcodeService) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("listen for Command Code runtime: %w", err)
	}
	service.apiKey = apiKey
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
		started: started, models: append([]string(nil), service.models...),
		usage: service.usage,
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

func (s *commandcodeService) handler() http.Handler {
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

func (s *commandcodeService) authorized(request *http.Request) bool {
	if request.Header.Get("x-api-key") == s.apiKey {
		return true
	}
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) == s.apiKey
}

func (s *commandcodeService) handleModels(writer http.ResponseWriter, request *http.Request) {
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

func (s *commandcodeService) handleCountTokens(writer http.ResponseWriter, request *http.Request) {
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

func (s *commandcodeService) handleMessages(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	LogDebugEvent("request_received", "component", "commandcode", "request_id", requestID,
		"path", request.URL.Path, "method", request.Method)
	if !s.authorized(request) {
		LogWarnEvent("request_rejected", "component", "commandcode", "request_id", requestID,
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
	converted, err := convertAnthropicToCommandCode(raw)
	if err != nil {
		LogWarnEvent("request_rejected", "component", "commandcode", "request_id", requestID,
			"status", http.StatusBadRequest, "reason", "request_conversion", "error", err)
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	LogDebugEvent("request_converted", "component", "commandcode", "request_id", requestID,
		"client_model", converted.clientModel, "upstream_model", converted.upstreamModel,
		"stream", converted.stream, "body_bytes", len(raw))
	s.ensureInitialized(requestCtx)
	response, err := s.call(requestCtx, converted)
	if err != nil {
		var upstreamErr *commandcodeUpstreamError
		if errors.As(err, &upstreamErr) {
			if upstreamErr.retryAfter != "" {
				writer.Header().Set("Retry-After", upstreamErr.retryAfter)
			}
			errorType := anthropicErrorType(upstreamErr.mapped)
			LogUpstreamEvent(upstreamErr.status, "request_failed", "component", "commandcode", "request_id", requestID,
				"model", converted.upstreamModel, "status", upstreamErr.status, "returned_status", upstreamErr.mapped,
				"error_type", errorType, "stream", converted.stream, "duration", logDuration(started))
			writeAnthropicError(writer, upstreamErr.mapped, errorType, upstreamErr.Error())
			return
		}
		LogErrorEvent("request_failed", "component", "commandcode", "request_id", requestID,
			"model", converted.upstreamModel, "returned_status", http.StatusBadGateway,
			"stream", converted.stream, "duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer response.Body.Close()
	if converted.stream {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, writer)
		if err := assembler.start(); err != nil {
			return
		}
		if err := processCommandCodeStream(response.Body, assembler); err != nil {
			LogErrorEvent("stream_conversion_failed", "component", "commandcode", "request_id", requestID,
				"model", converted.upstreamModel, "duration", logDuration(started), "error", err)
			_ = assembler.emit("error", map[string]any{"type": "error", "error": map[string]any{
				"type": "api_error", "message": err.Error(),
			}})
		}
		s.recordUsage(converted, assembler)
	} else {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, chatMaxResponseBytes))
		if readErr != nil {
			LogErrorEvent("response_read_failed", "component", "commandcode", "request_id", requestID,
				"model", converted.upstreamModel, "returned_status", http.StatusBadGateway,
				"duration", logDuration(started), "error", readErr)
			writeAnthropicError(writer, http.StatusBadGateway, "api_error", readErr.Error())
			return
		}
		assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, nil)
		if err := processCommandCodeNonStream(body, assembler); err != nil {
			LogErrorEvent("stream_conversion_failed", "component", "commandcode", "request_id", requestID,
				"model", converted.upstreamModel, "returned_status", http.StatusBadGateway,
				"duration", logDuration(started), "error", err)
			writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		s.recordUsage(converted, assembler)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(assembler.response())
	}
	LogDebugEvent("request_complete", "component", "commandcode", "request_id", requestID,
		"model", converted.upstreamModel, "status", http.StatusOK, "stream", converted.stream,
		"duration", logDuration(started))
}

// recordUsage adds the token totals of one completed turn to the runtime's
// usage tracker, keyed by the model the client requested.
func (s *commandcodeService) recordUsage(converted *commandcodeConvertedRequest, assembler *anthropicResponseAssembler) {
	if s.usage == nil {
		return
	}
	input, output := assembler.tokenTotals()
	s.usage.Add(converted.clientModel, int64(input), int64(output),
		int64(assembler.cacheReadTokens), int64(assembler.cacheWriteTokens))
}

// ensureInitialized mirrors the reference client's per-key startup handshake:
// the device fingerprint and a CLI lifecycle event are posted before the first
// upstream request and re-armed every 8h plus up to 2h of jitter. Failures are
// advisory — the next request retries — so an unresponsive identity endpoint
// never blocks a request beyond commandcodeInitTimeout.
func (s *commandcodeService) ensureInitialized(ctx context.Context) {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	if s.initDone && time.Now().Before(s.nextInitAt) {
		return
	}
	initCtx, cancel := context.WithTimeout(ctx, commandcodeInitTimeout)
	defer cancel()
	if err := s.sendInit(initCtx); err != nil {
		LogWarnEvent("upstream_init_failed", "component", "commandcode", "request_id", requestLogID(ctx), "error", err)
		return
	}
	s.initDone = true
	jitter, err := commandcodeInitJitterDuration()
	if err != nil {
		jitter = 0
	}
	s.nextInitAt = time.Now().Add(commandcodeInitRefresh + jitter)
}

// sendInit posts the fingerprint/record and lifecycle-events handshake in
// parallel, using the reduced header set the reference client sends for these
// two calls.
func (s *commandcodeService) sendInit(ctx context.Context) error {
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	post := func(path string, body any) {
		defer wg.Done()
		raw, err := json.Marshal(body)
		if err != nil {
			errCh <- fmt.Errorf("marshal %s body: %w", path, err)
			return
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint+path, bytes.NewReader(raw))
		if err != nil {
			errCh <- err
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("x-cli-environment", "production")
		request.Header.Set("Authorization", "Bearer "+s.upstreamKey)
		request.Header.Set("x-command-code-version", commandcodeVersion)
		response, err := s.client.Do(request)
		if err != nil {
			errCh <- fmt.Errorf("POST %s: %w", path, err)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, chatMaxErrorBytes))
		_ = response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			errCh <- fmt.Errorf("POST %s returned HTTP %d", path, response.StatusCode)
			return
		}
		LogDebugEvent("upstream_init", "component", "commandcode", "request_id", requestLogID(ctx),
			"path", path, "status", response.StatusCode)
	}
	wg.Add(2)
	go post("/alpha/fingerprint/record", s.fingerprint)
	go post("/alpha/lifecycle-events", s.lifecycleEvent())
	wg.Wait()
	close(errCh)
	var firstErr error
	for err := range errCh {
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// lifecycleEvent builds the cli_session_exists payload, reporting the same
// spoofed platform as the fingerprint components.
func (s *commandcodeService) lifecycleEvent() map[string]any {
	raw := commandcodeUUIDHex()
	return map[string]any{
		"eventType": "cli_session_exists",
		"metadata": map[string]any{
			"sessionId":  "sess_" + raw[:16],
			"cliVersion": commandcodeVersion,
			"mode":       "interactive",
			"os":         "win32-x64",
		},
	}
}

// CommandCodeProbeInit checks a candidate Command Code endpoint with the
// lightweight GET /alpha/whoami first: any HTTP status it returns (even 404)
// is conclusive about the endpoint, and 2xx with the candidate key means the
// gateway is Command Code. Only a transport error — e.g. a filtered network
// that drops the route — falls back to the reference-shaped POST
// /alpha/fingerprint/record handshake, which protocol detection previously
// used as its only signal. A non-2xx status means "not detected"; the caller
// maps 401/403 to an invalid-key hint.
func CommandCodeProbeInit(ctx context.Context, endpoint, upstreamAPIKey string, timeout time.Duration) (int, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	base := strings.TrimRight(endpoint, "/")
	client := &http.Client{Timeout: timeout}
	if status, body, err := commandcodeProbeWhoami(ctx, client, base, upstreamAPIKey); err == nil {
		return status, body, nil
	}
	fingerprint, err := commandcodeGenerateFingerprint()
	if err != nil {
		return 0, "", fmt.Errorf("generate probe fingerprint: %w", err)
	}
	raw, err := json.Marshal(fingerprint)
	if err != nil {
		return 0, "", fmt.Errorf("marshal probe fingerprint: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/alpha/fingerprint/record", bytes.NewReader(raw))
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-cli-environment", "production")
	request.Header.Set("Authorization", "Bearer "+upstreamAPIKey)
	request.Header.Set("x-command-code-version", commandcodeVersion)
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, chatMaxErrorBytes))
	if err != nil {
		return response.StatusCode, "", err
	}
	return response.StatusCode, strings.TrimSpace(string(body)), nil
}

// commandcodeProbeWhoami runs the lightweight GET /alpha/whoami probe. It
// returns the transport error only when no HTTP response arrived; any status
// (including 404) is a conclusive answer about the endpoint.
func commandcodeProbeWhoami(ctx context.Context, client *http.Client, base, upstreamAPIKey string) (int, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/alpha/whoami", nil)
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("x-cli-environment", "production")
	request.Header.Set("Authorization", "Bearer "+upstreamAPIKey)
	request.Header.Set("x-command-code-version", commandcodeVersion)
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, chatMaxErrorBytes))
	if err != nil {
		return 0, "", err
	}
	return response.StatusCode, strings.TrimSpace(string(body)), nil
}

// call posts the converted request to /alpha/generate with the full identity
// header set of the reference client. The generate endpoint always returns
// NDJSON.
func (s *commandcodeService) call(ctx context.Context, converted *commandcodeConvertedRequest) (*http.Response, error) {
	raw, err := json.Marshal(converted.body)
	if err != nil {
		return nil, fmt.Errorf("marshal Command Code request: %w", err)
	}
	target := s.endpoint + "/alpha/generate"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+s.upstreamKey)
	request.Header.Set("x-cli-environment", "production")
	request.Header.Set("x-command-code-version", commandcodeVersion)
	request.Header.Set("x-session-id", s.sessionID)
	request.Header.Set("x-co-flag", "false")
	request.Header.Set("x-taste-learning", "false")
	request.Header.Set("x-project-slug", s.projectSlug)
	request.Header.Set("traceparent", commandcodeTraceparent())
	LogDebugEvent("upstream_request", "component", "commandcode", "request_id", requestLogID(ctx),
		"method", http.MethodPost, "endpoint", SafeLogEndpoint(target),
		"body_bytes", len(raw), "model", converted.upstreamModel)
	started := time.Now()
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Command Code upstream: %w", err)
	}
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "commandcode", "request_id", requestLogID(ctx),
		"status", response.StatusCode, "retry_after", response.Header.Get("Retry-After"),
		"content_type", response.Header.Get("Content-Type"), "duration", logDuration(started))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, commandcodeDrainError(ctx, response)
	}
	return response, nil
}

// commandcodeDrainError reads a bounded prefix of the upstream error body and
// applies the reference client's status map (402→429, 403→401, 500/502→502,
// 422→400, default 502) so the Anthropic-facing status stays self-consistent.
// An upstream 429 carries the reference client's fixed Retry-After hint.
func commandcodeDrainError(ctx context.Context, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, chatMaxErrorBytes))
	_ = response.Body.Close()
	DebugHTTPBody(fmt.Sprintf("commandcode response request_id=%s status=%d", requestLogID(ctx), response.StatusCode), body)
	err := &commandcodeUpstreamError{
		status: response.StatusCode,
		mapped: commandcodeMappedStatus(response.StatusCode),
		body:   strings.TrimSpace(string(body)),
	}
	if response.StatusCode == http.StatusTooManyRequests {
		err.retryAfter = commandcodeRetryAfter
	}
	return err
}

// commandcodeMappedStatus applies the reference client's CC_STATUS_MAP.
func commandcodeMappedStatus(status int) int {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusTooManyRequests:
		return status
	case http.StatusPaymentRequired:
		return http.StatusTooManyRequests
	case http.StatusForbidden:
		return http.StatusUnauthorized
	case http.StatusUnprocessableEntity:
		return http.StatusBadRequest
	case http.StatusInternalServerError, http.StatusBadGateway:
		return http.StatusBadGateway
	case http.StatusServiceUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

// commandcodeErrorMessage extracts the human message from an upstream body the
// way the reference client does: JSON error.message / message, a clipped
// non-JSON body, or a status-based default.
func commandcodeErrorMessage(body string, status int) string {
	if body == "" {
		return fmt.Sprintf("CC API error (%d)", status)
	}
	parsed := gjson.Parse(body)
	if message := parsed.Get("error.message").String(); message != "" {
		return message
	}
	if message := parsed.Get("message").String(); message != "" {
		return message
	}
	if json.Valid([]byte(body)) {
		return fmt.Sprintf("CC API error (%d)", status)
	}
	if len(body) > 200 {
		body = body[:200]
	}
	return body
}

// commandcodeCatalogIDs returns the catalog model IDs in catalog order.
func commandcodeCatalogIDs() []string {
	ids := make([]string, 0, len(commandcodeModelCatalog))
	for _, model := range commandcodeModelCatalog {
		ids = append(ids, model.id)
	}
	return ids
}

// CommandCodeModelCatalog returns the authoritative Command Code model catalog
// as ModelInfo pairs. The gateway has no /v1/models route, so model metadata
// commands and the set/map flows read this static list instead of probing.
func CommandCodeModelCatalog() []protocol.ModelInfo {
	infos := make([]protocol.ModelInfo, 0, len(commandcodeModelCatalog))
	for _, model := range commandcodeModelCatalog {
		infos = append(infos, protocol.ModelInfo{ID: model.id, DisplayName: model.name})
	}
	return infos
}

// CommandCodeSupportsModel reports whether the catalog admits a model ID,
// case-insensitively. Availability probes use it: a request for a model the
// gateway does not serve fails before any traffic leaves the machine.
func CommandCodeSupportsModel(model string) bool {
	for _, entry := range commandcodeModelCatalog {
		if strings.EqualFold(entry.id, strings.TrimSpace(model)) {
			return true
		}
	}
	return false
}

// commandcodeTraceparent builds a W3C traceparent header from the codebase's
// best-effort randomness primitive, like the reference client's
// generateTraceparent.
func commandcodeTraceparent() string {
	trace := commandcodeUUIDHex()
	parent := commandcodeUUIDHex()[:16]
	return "00-" + trace + "-" + parent + "-01"
}

// commandcodeUUIDHex returns 32 lowercase hex characters drawn from a random
// UUID, the codebase's best-effort randomness primitive.
func commandcodeUUIDHex() string {
	return strings.ReplaceAll(uuidString(), "-", "")
}

// commandcodeSlugNames is the pool of project names the reference client draws
// from when building the fake project slug.
var commandcodeSlugNames = []string{
	"app", "api", "backend", "bot", "cli", "core", "data", "frontend",
	"lib", "plugin", "proxy", "server", "service", "tool", "web", "worker",
}

var commandcodeSlugNonWord = regexp.MustCompile(`[^a-z0-9]+`)

// commandcodeProjectSlug derives x-project-slug exactly like the reference
// client: a fake Windows working directory is built from the session ID,
// lowercased, stripped of its drive letter, and reduced to dash-separated
// segments (e.g. users-dev-projects-app-a3f2).
func commandcodeProjectSlug(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	name, suffix := "app", ""
	if len(sessionID) >= 4 {
		suffix = sessionID[:4]
		if parsed, err := strconv.ParseUint(suffix, 16, 32); err == nil {
			name = commandcodeSlugNames[int(parsed)%len(commandcodeSlugNames)]
		}
	} else if sessionID != "" {
		suffix = sessionID
	}
	path := strings.ToLower(`C:\Users\dev\projects\` + name + "-" + suffix)
	path = strings.TrimPrefix(path, "c:")
	return strings.Trim(commandcodeSlugNonWord.ReplaceAllString(path, "-"), "-")
}

// commandcodeFingerprintCPU is one candidate CPU of the spoofed device
// fingerprint pool.
type commandcodeFingerprintCPU struct {
	model string
	cores int
}

// signal/pools below are ported verbatim from the reference client.
var commandcodeFingerprintCPUs = []commandcodeFingerprintCPU{
	{"12th Gen Intel(R) Core(TM) i7-12650H", 10},
	{"12th Gen Intel(R) Core(TM) i5-12400F", 6},
	{"12th Gen Intel(R) Core(TM) i9-12900K", 16},
	{"13th Gen Intel(R) Core(TM) i7-13700K", 16},
	{"13th Gen Intel(R) Core(TM) i5-13600K", 14},
	{"13th Gen Intel(R) Core(TM) i9-13900K", 24},
	{"Intel(R) Core(TM) Ultra 7 155H", 16},
	{"Intel(R) Core(TM) Ultra 9 285H", 16},
	{"Intel(R) Core(TM) i9-14900K", 24},
	{"Intel(R) Core(TM) i7-14700K", 20},
	{"AMD Ryzen 7 7800X3D", 8},
	{"AMD Ryzen 9 7950X", 16},
	{"AMD Ryzen 5 7600", 6},
	{"AMD Ryzen 9 7900X", 12},
	{"AMD Ryzen 7 5800X3D", 8},
}

var commandcodeFingerprintMems = []int{8, 16, 24, 32, 48, 64}

var commandcodeFingerprintTimezones = []string{
	"America/New_York", "America/Chicago", "America/Los_Angeles", "America/Toronto",
	"Europe/London", "Europe/Berlin", "Europe/Paris", "Europe/Moscow",
	"Asia/Shanghai", "Asia/Tokyo", "Asia/Singapore", "Asia/Seoul", "Asia/Hong_Kong",
	"Australia/Sydney", "Pacific/Auckland",
}

var commandcodeFingerprintMACCounts = []int{2, 3, 4, 5}

// commandcodeGenerateFingerprint builds the spoofed device fingerprint the
// reference client reports: random component hashes, a plausible CPU/memory
// profile, and a thumbmark that joins them all.
func commandcodeGenerateFingerprint() (map[string]any, error) {
	cpuIdx, err := commandcodeRandomIndex(len(commandcodeFingerprintCPUs))
	if err != nil {
		return nil, err
	}
	memIdx, err := commandcodeRandomIndex(len(commandcodeFingerprintMems))
	if err != nil {
		return nil, err
	}
	tzIdx, err := commandcodeRandomIndex(len(commandcodeFingerprintTimezones))
	if err != nil {
		return nil, err
	}
	macCountIdx, err := commandcodeRandomIndex(len(commandcodeFingerprintMACCounts))
	if err != nil {
		return nil, err
	}
	cpu := commandcodeFingerprintCPUs[cpuIdx]
	memGiB := commandcodeFingerprintMems[memIdx]
	timezone := commandcodeFingerprintTimezones[tzIdx]
	macCount := commandcodeFingerprintMACCounts[macCountIdx]

	hashedRandom := func(bytes int) (string, error) {
		raw, err := commandcodeRandomHexBytes(bytes)
		if err != nil {
			return "", err
		}
		return commandcodeSHA256Hex(raw), nil
	}
	macHashes := make([]string, 0, macCount)
	for range macCount {
		hash, err := hashedRandom(32)
		if err != nil {
			return nil, err
		}
		macHashes = append(macHashes, hash)
	}
	machineIDHash, err := hashedRandom(32)
	if err != nil {
		return nil, err
	}
	osUserHash, err := hashedRandom(16)
	if err != nil {
		return nil, err
	}
	hostnameHash, err := hashedRandom(16)
	if err != nil {
		return nil, err
	}
	gitEmailHash, err := hashedRandom(16)
	if err != nil {
		return nil, err
	}

	thumbData := make([]string, 0, len(macHashes)+8)
	thumbData = append(thumbData, machineIDHash)
	thumbData = append(thumbData, macHashes...)
	thumbData = append(thumbData, osUserHash, hostnameHash, gitEmailHash,
		"win32", "10.0.22631", cpu.model, strconv.Itoa(cpu.cores), strconv.Itoa(memGiB))

	return map[string]any{
		"thumbmark": commandcodeSHA256Hex(strings.Join(thumbData, "|")),
		"components": map[string]any{
			"machineIdHash":    machineIDHash,
			"macHashes":        macHashes,
			"osUserHash":       osUserHash,
			"hostnameHash":     hostnameHash,
			"gitEmailHash":     gitEmailHash,
			"platform":         "win32",
			"arch":             "x64",
			"osRelease":        "10.0.22631",
			"cpuModel":         cpu.model,
			"cpuCount":         cpu.cores,
			"memGiB":           memGiB,
			"isContainer":      false,
			"timezone":         timezone,
			"runtime":          "cli",
			"collectorVersion": 1,
		},
	}, nil
}

// commandcodeInitJitterDuration returns a random duration below the init
// jitter window so re-arms spread the refresh load.
func commandcodeInitJitterDuration() (time.Duration, error) {
	value, err := commandcodeRandUint()
	if err != nil {
		return 0, err
	}
	return time.Duration(value % uint64(commandcodeInitJitter)), nil
}

// commandcodeSHA256Hex mirrors the reference client's sha256 helper.
func commandcodeSHA256Hex(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

// commandcodeRandomHexBytes mirrors the reference client's randHex(n).
func commandcodeRandomHexBytes(length int) (string, error) {
	if length <= 0 {
		return "", nil
	}
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// commandcodeRandomIndex returns a random index into a pool of the given
// length.
func commandcodeRandomIndex(limit int) (int, error) {
	if limit <= 1 {
		return 0, nil
	}
	value, err := commandcodeRandUint()
	if err != nil {
		return 0, err
	}
	return int(value % uint64(limit)), nil
}

func commandcodeRandUint() (uint64, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return 0, fmt.Errorf("read random bytes: %w", err)
	}
	return binary.BigEndian.Uint64(raw), nil
}
