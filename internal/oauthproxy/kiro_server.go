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
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	kiroIDEVersion             = "2.3.0"
	kiroMaxInboundRequestBytes = int64(128 << 20)
	kiroMaxUpstreamErrorBytes  = int64(1 << 20)
)

// kiroRateLimitBackoff is how long to wait before each retry of a rate-limited
// request. Kiro answers short bursts with HTTP 429 / USER_REQUEST_RATE_EXCEEDED,
// which normally clears within seconds, so the turn is retried here instead of
// being handed straight back to the user.
var kiroRateLimitBackoff = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

type kiroService struct {
	apiKey       string
	models       []string
	modelCatalog *kiroModelCatalog
	pool         *kiroCredentialPool
	client       *http.Client
	upstreamURL  func(*kiroCredential) string
	// rateLimitBackoff overrides kiroRateLimitBackoff (tests only).
	rateLimitBackoff []time.Duration
	// usage accumulates per-model token totals for this runtime.
	usage *UsageTracker
}

type kiroUpstreamError struct {
	status int
	body   string
}

func (e *kiroUpstreamError) Error() string {
	return fmt.Sprintf("Kiro upstream returned HTTP %d: %s", e.status, e.body)
}

func startKiroOAuth(parent context.Context, modelSpec, credentialFile string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	pool := newKiroCredentialPool(authDir, credentialFile)
	credentials, err := pool.load()
	if err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("no %s credentials found; run `ccl oauth %s` first", ProviderKiro, ProviderKiro)
	}

	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for Kiro runtime: %w", err)
	}
	models := kiroRuntimeModels(modelSpec)
	usageTracker := NewUsageTracker()
	service := &kiroService{
		apiKey:       apiKey,
		models:       models,
		modelCatalog: newKiroModelCatalog(kiroAvailableModelsEndpoint),
		pool:         pool,
		usage:        usageTracker,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			ResponseHeaderTimeout: 60 * time.Second,
		}},
		upstreamURL: func(credential *kiroCredential) string {
			return "https://q." + credential.effectiveAPIRegion() + ".amazonaws.com/generateAssistantResponse"
		},
	}
	runCtx, cancel := context.WithCancel(parent)
	server := &http.Server{
		Handler:           service.handler(),
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return runCtx
		},
	}
	started := make(chan struct{})
	close(started)
	proxyRuntime := &Runtime{
		endpoint:   "http://" + listener.Addr().String() + "/v1",
		apiKey:     apiKey,
		httpServer: server,
		listAuths:  pool.listAuths,
		cancel:     cancel,
		done:       make(chan struct{}),
		runErr:     make(chan error, 1),
		started:    started,
		usage:      usageTracker,
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
			ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			_ = server.Shutdown(ctx)
			stop()
		case <-proxyRuntime.done:
		}
	}()
	LogInfof("runtime start oauth provider=kiro backend=kiro protocol=anthropic port=%s credential_file=%s model_count=%d",
		listener.Addr().String(), filepath.Base(credentialFile), len(models))
	return proxyRuntime, nil
}

// kiroRuntimeModels lists the client-visible model aliases of a model spec.
func kiroRuntimeModels(modelSpec string) []string {
	return runtimeModelAliases(modelSpec)
}

func (s *kiroService) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/messages", s.handleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.handleCountTokens)
	return mux
}

func (s *kiroService) authorized(request *http.Request) bool {
	if request.Header.Get("x-api-key") == s.apiKey {
		return true
	}
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) == s.apiKey
}

func (s *kiroService) handleModels(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodGet {
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	models, err := s.availableModels(request.Context())
	if err != nil {
		LogErrorf("kiro models discovery failed error=%v", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", "Unable to load available models from Kiro: "+err.Error())
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	data := make([]map[string]any, 0, len(models))
	ids := make([]string, 0, len(models))
	for _, model := range models {
		displayName := strings.TrimSpace(model.ModelName)
		if displayName == "" {
			displayName = model.ModelID
		}
		item := map[string]any{
			"id":           model.ModelID,
			"type":         "model",
			"display_name": displayName,
			"created_at":   now,
		}
		if model.Description != "" {
			item["description"] = model.Description
		}
		if model.TokenLimits != nil {
			if model.TokenLimits.MaxInputTokens > 0 {
				item["max_input_tokens"] = model.TokenLimits.MaxInputTokens
			}
			if model.TokenLimits.MaxOutputTokens > 0 {
				item["max_output_tokens"] = model.TokenLimits.MaxOutputTokens
			}
		}
		if model.RateMultiplier != nil {
			item["rate_multiplier"] = *model.RateMultiplier
		}
		if model.RateUnit != "" {
			item["rate_unit"] = model.RateUnit
		}
		if len(model.SupportedInputTypes) > 0 {
			item["supported_input_types"] = model.SupportedInputTypes
		}
		data = append(data, item)
		ids = append(ids, model.ModelID)
	}
	firstID, lastID := modelPageBounds(ids)
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"data":     data,
		"has_more": false,
		"first_id": firstID,
		"last_id":  lastID,
	})
}

func (s *kiroService) handleCountTokens(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	raw, err := readAnthropicInboundBody(writer, request, kiroMaxInboundRequestBytes)
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"input_tokens": estimateApproxTokensBytes(raw)})
}

func (s *kiroService) handleMessages(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	if !s.authorized(request) {
		LogWarnEvent("request_rejected", "component", "kiro", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusUnauthorized, "reason", "invalid_local_api_key")
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		LogWarnEvent("request_rejected", "component", "kiro", "request_id", requestID,
			"path", request.URL.Path, "method", request.Method, "status", http.StatusMethodNotAllowed,
			"reason", "unsupported_method")
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	LogDebugEvent("request_received", "component", "kiro", "request_id", requestID,
		"path", request.URL.RequestURI(), "content_length", request.ContentLength,
		"transfer_encoding_count", len(request.TransferEncoding), "content_type", request.Header.Get("Content-Type"),
		"content_encoding", request.Header.Get("Content-Encoding"))
	raw, err := readAnthropicInboundBody(writer, request, kiroMaxInboundRequestBytes)
	if err != nil {
		LogWarnEvent("request_rejected", "component", "kiro", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusBadRequest, "reason", "read_body", "error", err)
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	LogDebugEvent("request_body_read", "component", "kiro", "request_id", requestID, "body_bytes", len(raw))
	if len(bytes.TrimSpace(raw)) == 0 {
		LogWarnEvent("request_rejected", "component", "kiro", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusBadRequest, "reason", "empty_body")
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "invalid Anthropic Messages request: request body is empty")
		return
	}
	converted, err := convertAnthropicToKiro(raw)
	if err != nil {
		LogWarnEvent("request_rejected", "component", "kiro", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusBadRequest, "reason", "request_conversion", "error", err)
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if converted.droppedMedia > 0 || converted.dedupedMedia > 0 ||
		converted.resizedMedia > 0 || converted.correctedMedia > 0 {
		LogWarnEvent("request_normalized", "component", "kiro", "request_id", requestID,
			"kind", "inline_media", "kept", converted.inlineMedia, "capped", converted.droppedMedia,
			"deduplicated", converted.dedupedMedia, "resized", converted.resizedMedia,
			"mime_corrected", converted.correctedMedia, "limit", kiroMaxInlineMediaSegments)
	}
	if converted.droppedToolUses > 0 || converted.droppedToolRuns > 0 || converted.emptyToolRuns > 0 {
		// dropped_results > 0 means the model will not see those tool outputs at
		// all, which it reports as the tooling being broken; empty_results are
		// forwarded with a placeholder instead of an empty string.
		LogWarnEvent("request_normalized", "component", "kiro", "request_id", requestID,
			"kind", "tool_pairing", "dropped_uses", converted.droppedToolUses,
			"dropped_results", converted.droppedToolRuns, "empty_results", converted.emptyToolRuns)
	}
	if converted.truncatedTexts > 0 {
		LogWarnEvent("request_normalized", "component", "kiro", "request_id", requestID,
			"kind", "content_truncation", "fields", converted.truncatedTexts,
			"dropped_bytes", converted.droppedText, "largest_original_bytes", converted.largestText,
			"limit", kiroMaxTextFieldBytes)
	}
	LogDebugEvent("request_converted", "component", "kiro", "request_id", requestID,
		"client_model", converted.clientModel, "upstream_model", converted.model, "stream", converted.stream,
		"body_bytes", len(raw))
	upstream, err := s.callUpstream(requestCtx, converted)
	if err != nil {
		// Forward upstream client errors unchanged (status and Anthropic error
		// type) so Claude Code can apply its own handling: back off on 429,
		// re-authenticate on 401, shrink the request on 413. Collapsing them all
		// into 400 made every one of those look like a malformed request.
		var upstreamErr *kiroUpstreamError
		if errors.As(err, &upstreamErr) && upstreamErr.status >= 400 && upstreamErr.status < 500 {
			errorType := anthropicErrorType(upstreamErr.status)
			LogUpstreamEvent(upstreamErr.status, "request_failed", "component", "kiro", "request_id", requestID,
				"model", converted.model, "status", upstreamErr.status, "returned_status", upstreamErr.status,
				"error_type", errorType, "stream", converted.stream, "duration", logDuration(started))
			// err, not upstreamErr: the wrapper carries extra context such as a
			// failed token refresh.
			writeAnthropicError(writer, upstreamErr.status, errorType, err.Error())
			return
		}
		// Everything else is a proxy-side or upstream server failure.
		LogErrorEvent("request_failed", "component", "kiro", "request_id", requestID,
			"model", converted.model, "returned_status", http.StatusBadGateway, "stream", converted.stream,
			"duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer upstream.Body.Close()

	if converted.stream {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, writer)
		if err := assembler.start(); err != nil {
			return
		}
		streamErr := processKiroEventStream(upstream.Body, assembler)
		s.recordKiroUsage(converted, assembler)
		if streamErr != nil {
			LogErrorEvent("stream_conversion_failed", "component", "kiro", "request_id", requestID,
				"model", converted.model, "duration", logDuration(started), "error", streamErr)
			_ = assembler.emit("error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "api_error",
					"message": streamErr.Error(),
				},
			})
		}
		if streamErr == nil {
			LogDebugEvent("request_complete", "component", "kiro", "request_id", requestID,
				"model", converted.model, "status", http.StatusOK, "stream", true, "duration", logDuration(started))
		}
		return
	}

	assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, nil)
	if err := processKiroEventStream(upstream.Body, assembler); err != nil {
		LogErrorEvent("stream_conversion_failed", "component", "kiro", "request_id", requestID,
			"model", converted.model, "returned_status", http.StatusBadGateway,
			"duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	s.recordKiroUsage(converted, assembler)
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(assembler.response())
	LogDebugEvent("request_complete", "component", "kiro", "request_id", requestID,
		"model", converted.model, "status", http.StatusOK, "stream", false, "duration", logDuration(started))
}

// recordKiroUsage adds the token totals of one completed turn to the runtime's
// usage tracker, keyed by the model the client requested (the [1m]-suffixed
// alias when present) rather than the upstream model id, so it matches the slot
// name Claude Code shows the user.
func (s *kiroService) recordKiroUsage(converted *kiroConvertedRequest, assembler *anthropicResponseAssembler) {
	if s.usage == nil {
		return
	}
	input, output := assembler.tokenTotals()
	s.usage.Add(converted.clientModel, int64(input), int64(output), 0, 0)
}

func readAnthropicInboundBody(writer http.ResponseWriter, request *http.Request, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("invalid Anthropic request body limit")
	}
	if request.ContentLength > maxBytes {
		return nil, fmt.Errorf("Anthropic Messages request body exceeds %s limit", requestLimitLabel(maxBytes))
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxBytes)
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return nil, fmt.Errorf("Anthropic Messages request body exceeds %s limit", requestLimitLabel(maxBytes))
		}
		return nil, fmt.Errorf("read Anthropic Messages request body: %w", err)
	}
	return raw, nil
}

// drainKiroErrorBody reads a bounded prefix of an upstream error body and closes
// it, so the diagnostics survive after the response is discarded.
func drainKiroErrorBody(ctx context.Context, response *http.Response) string {
	if response == nil || response.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, kiroMaxUpstreamErrorBytes))
	_ = response.Body.Close()
	DebugHTTPBody(fmt.Sprintf("kiro response request_id=%s status=%d", requestLogID(ctx), response.StatusCode), body)
	return strings.TrimSpace(string(body))
}

func requestLimitLabel(maxBytes int64) string {
	if maxBytes >= 1<<20 && maxBytes%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", maxBytes>>20)
	}
	return fmt.Sprintf("%d bytes", maxBytes)
}

// callUpstream sends the converted request upstream, retrying while the account
// is rate limited. Every credential is tried before a retry sleeps, so rotating
// to a second account is always preferred over waiting. Once the backoff budget
// is spent the 429 is returned to the caller.
func (s *kiroService) callUpstream(ctx context.Context, converted *kiroConvertedRequest) (*http.Response, error) {
	backoff := s.rateLimitBackoff
	if backoff == nil {
		backoff = kiroRateLimitBackoff
	}
	for attempt := 0; ; attempt++ {
		response, err := s.callUpstreamOnce(ctx, converted, attempt+1)
		if err == nil {
			return response, nil
		}
		if !isKiroRateLimitError(err) || attempt >= len(backoff) {
			return nil, err
		}
		delay := backoff[attempt]
		LogWarnEvent("upstream_retry", "component", "kiro", "request_id", requestLogID(ctx),
			"model", converted.model, "status", http.StatusTooManyRequests,
			"retry", attempt+1, "retry_limit", len(backoff), "wait", delay, "action", "retry_all_credentials")
		if waitErr := sleepContext(ctx, delay); waitErr != nil {
			// The client gave up: report the rate limit, not the cancellation.
			return nil, err
		}
	}
}

// isKiroRateLimitError reports whether err is an upstream HTTP 429.
func isKiroRateLimitError(err error) bool {
	var upstreamErr *kiroUpstreamError
	return errors.As(err, &upstreamErr) && upstreamErr.status == http.StatusTooManyRequests
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// callUpstreamOnce tries every selected credential once and returns the first
// successful response.
func (s *kiroService) callUpstreamOnce(ctx context.Context, converted *kiroConvertedRequest, round int) (*http.Response, error) {
	credentials, err := s.pool.orderedCredentials()
	if err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("no usable Kiro credentials")
	}
	var lastErr error
	var rateLimitErr error
	otherFailure := false
	for index, candidate := range credentials {
		attempt := index + 1
		credential, err := s.pool.usableCredential(ctx, candidate, false)
		if err != nil {
			LogWarnEvent("credential_unavailable", "component", "kiro", "request_id", requestLogID(ctx),
				"round", round, "attempt", attempt, "credential_count", len(credentials),
				"credential", candidate.fileName, "error", err)
			lastErr = err
			otherFailure = true
			continue
		}
		LogDebugEvent("upstream_attempt", "component", "kiro", "request_id", requestLogID(ctx),
			"round", round, "attempt", attempt, "credential_count", len(credentials),
			"credential", credential.fileName, "model", converted.model)
		response, err := s.doUpstreamRequest(ctx, converted, credential)
		if err != nil {
			LogWarnEvent("upstream_attempt_failed", "component", "kiro", "request_id", requestLogID(ctx),
				"round", round, "attempt", attempt, "credential_count", len(credentials),
				"credential", credential.fileName, "model", converted.model, "error", err)
			lastErr = err
			otherFailure = true
			continue
		}
		if response.StatusCode == http.StatusUnauthorized {
			// Drain the rejected response before closing it: if the refresh then
			// fails we still have the upstream diagnostics to report.
			unauthorized := drainKiroErrorBody(ctx, response)
			LogWarnEvent("credential_refresh", "component", "kiro", "request_id", requestLogID(ctx),
				"round", round, "attempt", attempt, "credential", credential.fileName,
				"status", http.StatusUnauthorized, "action", "force_refresh")
			refreshed, refreshErr := s.pool.usableCredential(ctx, credential, true)
			if refreshErr != nil {
				lastErr = fmt.Errorf("%w (credential refresh failed: %v)",
					&kiroUpstreamError{status: http.StatusUnauthorized, body: unauthorized}, refreshErr)
				otherFailure = true
				continue
			}
			response, err = s.doUpstreamRequest(ctx, converted, refreshed)
			if err != nil {
				lastErr = err
				otherFailure = true
				continue
			}
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		}
		upstreamErr := &kiroUpstreamError{status: response.StatusCode, body: drainKiroErrorBody(ctx, response)}
		if response.StatusCode == http.StatusBadRequest {
			return nil, upstreamErr
		}
		if response.StatusCode == http.StatusTooManyRequests {
			rateLimitErr = upstreamErr
		} else {
			otherFailure = true
		}
		lastErr = upstreamErr
	}
	// A rate limit is the retryable outcome, so report it when it is the only
	// thing that went wrong. If another credential failed for a reason waiting
	// cannot fix, that error is the useful one and it also stops the backoff from
	// burning its full budget.
	if rateLimitErr != nil && !otherFailure {
		return nil, rateLimitErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all Kiro credentials failed")
	}
	return nil, lastErr
}

func (s *kiroService) doUpstreamRequest(ctx context.Context, converted *kiroConvertedRequest, credential *kiroCredential) (*http.Response, error) {
	body := make(map[string]any, len(converted.body)+1)
	for key, value := range converted.body {
		body[key] = value
	}
	if profileARN := credential.streamingProfileARN(); profileARN != "" {
		body["profileArn"] = profileARN
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	endpoint := s.upstreamURL(credential)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	machineID := credential.effectiveMachineID()
	osName := runtime.GOOS
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connection", "close")
	request.Header.Set("x-amzn-codewhisperer-optout", "true")
	request.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	request.Header.Set("x-amz-user-agent", "aws-sdk-js/1.0.34 KiroIDE-"+kiroIDEVersion+"-"+machineID)
	request.Header.Set("User-Agent", "aws-sdk-js/1.0.34 ua/2.1 os/"+osName+" lang/js md/nodejs#22.22.0 api/codewhispererstreaming#1.0.34 m/E KiroIDE-"+kiroIDEVersion+"-"+machineID)
	request.Header.Set("amz-sdk-invocation-id", uuidString())
	request.Header.Set("amz-sdk-request", "attempt=1; max=3")
	request.Header.Set("Authorization", "Bearer "+credential.accessToken)
	if strings.EqualFold(credential.authMethod, "external_idp") {
		request.Header.Set("tokentype", "EXTERNAL_IDP")
	}
	LogDebugEvent("upstream_request", "component", "kiro", "request_id", requestLogID(ctx),
		"host", request.URL.Host, "model", converted.model, "credential", credential.fileName,
		"body_bytes", len(raw), "media", converted.inlineMedia, "budget_original_bytes", converted.originalBody,
		"budget_final_bytes", converted.finalBody, "budget_original_tokens", converted.originalTokens,
		"budget_final_tokens", converted.finalTokens, "budget_dropped_media", converted.budgetMedia,
		"budget_dropped_history", converted.budgetHistory, "budget_truncated_texts", converted.budgetTexts,
		"budget_dropped_text_bytes", converted.budgetTextBytes, "budget_dropped_tools", converted.budgetTools)
	DebugHTTPBody(fmt.Sprintf("kiro request request_id=%s path=%s", requestLogID(ctx), request.URL.Path), raw)
	started := time.Now()
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Kiro upstream: %w", err)
	}
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "kiro", "request_id", requestLogID(ctx),
		"status", response.StatusCode, "model", converted.model, "credential", credential.fileName,
		"retry_after", response.Header.Get("Retry-After"), "duration", logDuration(started))
	return response, nil
}

// anthropicErrorType maps an HTTP status onto the error type the Anthropic
// Messages API uses for it, so a forwarded status stays self-consistent for
// clients that branch on error.type rather than on the status code.
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusRequestTimeout:
		return "request_timeout_error"
	case 529:
		return "overloaded_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}

func writeAnthropicError(writer http.ResponseWriter, status int, errorType, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errorType,
			"message": message,
		},
	})
}

// modelPageBounds reports the first and last model id of a page, or empty
// strings when the page is empty.
func modelPageBounds(models []string) (string, string) {
	if len(models) == 0 {
		return "", ""
	}
	return models[0], models[len(models)-1]
}

func uuidString() string {
	return uuid.NewString()
}
