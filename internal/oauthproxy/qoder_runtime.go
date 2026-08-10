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
)

type qoderService struct {
	apiKey string
	models []qoderModel
	pool   *qoderCredentialPool
	client *http.Client
	usage  *UsageTracker
}

type qoderUpstreamError struct {
	status int
	body   string
}

func (e *qoderUpstreamError) Error() string {
	return fmt.Sprintf("Qoder upstream returned HTTP %d: %s", e.status, e.body)
}

type qoderStreamUsage struct {
	input      int64
	output     int64
	cacheRead  int64
	cacheWrite int64
}

type qoderQueueInfo struct {
	IsQueued          bool   `json:"isQueued"`
	ModelKey          string `json:"modelKey"`
	QueueCount        int    `json:"queueCount"`
	QueueType         string `json:"queueType"`
	RetryAfterSeconds int    `json:"retryAfterSeconds"`
	ServiceAvailable  bool   `json:"serviceAvailable"`
	WaitTime          int    `json:"waitTime"`
}

type qoderStreamError struct {
	status int
	body   string
	queue  *qoderQueueInfo
}

func (err *qoderStreamError) Error() string {
	if err.queue == nil || !err.queue.IsQueued {
		return fmt.Sprintf("Qoder stream returned status %d: %s", err.status, err.body)
	}
	detail := "Qoder model is temporarily queued"
	if err.queue.QueueCount > 0 {
		detail += fmt.Sprintf(" (%d requests ahead", err.queue.QueueCount)
		if err.queue.RetryAfterSeconds > 0 {
			detail += fmt.Sprintf(", retry after %ds", err.queue.RetryAfterSeconds)
		}
		detail += ")"
	} else if err.queue.RetryAfterSeconds > 0 {
		detail += fmt.Sprintf(" (retry after %ds)", err.queue.RetryAfterSeconds)
	}
	return detail
}

func startQoderOAuth(parent context.Context, _ string, credentialFile string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	pool := newQoderCredentialPool(authDir, credentialFile)
	credentials, err := pool.load()
	if err != nil {
		return nil, err
	}
	credentials = activeQoderCredentials(credentials)
	if len(credentials) == 0 {
		return nil, fmt.Errorf("no %s credentials found; run `ccl oauth %s` first", ProviderQoder, ProviderQoder)
	}
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	usageTracker := NewUsageTracker()
	service := &qoderService{apiKey: apiKey, pool: pool, client: pool.client, usage: usageTracker}
	models, discoverErr := service.discoverModels(parent)
	if discoverErr != nil {
		LogWarnf("Qoder model discovery failed; using compatibility catalog error=%v", discoverErr)
		models = qoderFallbackModels()
	}
	// The live catalog is authoritative. Context suffixes such as [1m] are
	// accepted by qoderSelectModel without being published as phantom upstream
	// models.
	service.models = models

	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("listen for Qoder runtime: %w", err)
	}
	runCtx, cancel := context.WithCancel(parent)
	server := &http.Server{
		Handler:           service.handler(),
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return runCtx
		},
	}
	modelIDs := make([]string, 0, len(service.models))
	modelNames := make(map[string]string, len(service.models))
	for _, model := range service.models {
		modelIDs = append(modelIDs, model.ID)
		if name := strings.TrimSpace(model.Name); name != "" {
			modelNames[model.ID] = name
		}
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
		models:     modelIDs,
		modelNames: modelNames,
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
			ctx, stop := context.WithTimeout(context.Background(), runtimeStopTimeout)
			_ = server.Shutdown(ctx)
			stop()
		case <-proxyRuntime.done:
		}
	}()
	LogInfof("runtime start oauth provider=qoder backend=qoder protocol=anthropic port=%s credential_file=%s model_count=%d",
		listener.Addr().String(), filepath.Base(credentialFile), len(modelIDs))
	return proxyRuntime, nil
}

func (service *qoderService) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/models", service.handleModels)
	mux.HandleFunc("/v1/messages", service.handleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", service.handleCountTokens)
	return mux
}

func (service *qoderService) authorized(request *http.Request) bool {
	if request.Header.Get("x-api-key") == service.apiKey {
		return true
	}
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) == service.apiKey
}

func (service *qoderService) handleModels(writer http.ResponseWriter, request *http.Request) {
	if !service.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodGet {
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	data := make([]map[string]any, 0, len(service.models))
	ids := make([]string, 0, len(service.models))
	for _, model := range service.models {
		item := map[string]any{
			"id":           model.ID,
			"type":         "model",
			"display_name": model.Name,
			"created_at":   now,
		}
		if model.ContextWindow > 0 {
			item["max_input_tokens"] = model.ContextWindow
		}
		if model.MaxTokens > 0 {
			item["max_output_tokens"] = model.MaxTokens
		}
		if model.PriceFactor != nil {
			item["rate_multiplier"] = *model.PriceFactor
			item["rate_unit"] = "credits"
		}
		item["is_new"] = model.IsNew
		item["promotion_available"] = model.PromotionAvailable
		item["reasoning"] = model.Reasoning
		item["supported_input_types"] = []string{"text"}
		if model.Vision {
			item["supported_input_types"] = []string{"text", "image"}
		}
		data = append(data, item)
		ids = append(ids, model.ID)
	}
	firstID, lastID := modelPageBounds(ids)
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"data": data, "has_more": false, "first_id": firstID, "last_id": lastID,
	})
}

func (service *qoderService) handleCountTokens(writer http.ResponseWriter, request *http.Request) {
	if !service.authorized(request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	raw, err := readAnthropicInboundBody(writer, request, qoderMaxBodyBytes)
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"input_tokens": estimateApproxTokensBytes(raw)})
}

func (service *qoderService) handleMessages(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	if !service.authorized(request) {
		LogWarnEvent("request_rejected", "component", "qoder", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusUnauthorized, "reason", "invalid_local_api_key")
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		LogWarnEvent("request_rejected", "component", "qoder", "request_id", requestID,
			"path", request.URL.Path, "method", request.Method, "status", http.StatusMethodNotAllowed,
			"reason", "unsupported_method")
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	raw, err := readAnthropicInboundBody(writer, request, qoderMaxBodyBytes)
	if err != nil {
		LogWarnEvent("request_rejected", "component", "qoder", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusBadRequest, "reason", "read_body", "error", err)
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	converted, err := convertAnthropicToQoder(raw, service.models)
	if err != nil {
		LogWarnEvent("request_rejected", "component", "qoder", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusBadRequest, "reason", "request_conversion",
			"body_bytes", len(raw), "error", err)
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	LogDebugEvent("request_converted", "component", "qoder", "request_id", requestID,
		"client_model", converted.clientModel, "upstream_model", converted.model.ID,
		"stream", converted.stream, "body_bytes", len(raw))
	upstream, err := service.callUpstream(requestCtx, converted)
	if err != nil {
		var upstreamErr *qoderUpstreamError
		if errors.As(err, &upstreamErr) && upstreamErr.status >= 400 && upstreamErr.status < 500 {
			LogUpstreamEvent(upstreamErr.status, "request_failed", "component", "qoder", "request_id", requestID,
				"model", converted.model.ID, "status", upstreamErr.status, "returned_status", upstreamErr.status,
				"error_type", anthropicErrorType(upstreamErr.status), "stream", converted.stream,
				"duration", logDuration(started))
			writeAnthropicError(writer, upstreamErr.status, anthropicErrorType(upstreamErr.status), err.Error())
			return
		}
		LogErrorEvent("request_failed", "component", "qoder", "request_id", requestID,
			"model", converted.model.ID, "returned_status", http.StatusBadGateway,
			"stream", converted.stream, "duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer upstream.Body.Close()

	if converted.stream {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		assembler := newAnthropicResponseAssembler(converted.anthropic, writer)
		if err := assembler.start(); err != nil {
			return
		}
		usage, streamErr := processQoderEventStream(requestCtx, upstream.Body, assembler)
		if streamErr == nil {
			service.recordUsage(converted.model, usage)
		}
		if streamErr != nil {
			if qoderAnthropicErrorType(streamErr) == "rate_limit_error" {
				logQoderQueue(requestCtx, converted.model.ID, streamErr)
			} else {
				LogErrorEvent("stream_conversion_failed", "component", "qoder", "request_id", requestID,
					"model", converted.model.ID, "stream", true, "duration", logDuration(started), "error", streamErr)
			}
			_ = assembler.emit("error", map[string]any{
				"type": "error", "error": map[string]any{"type": qoderAnthropicErrorType(streamErr), "message": streamErr.Error()},
			})
		}
		if streamErr == nil {
			LogDebugEvent("request_complete", "component", "qoder", "request_id", requestID,
				"model", converted.model.ID, "status", http.StatusOK, "stream", true, "duration", logDuration(started))
		}
		return
	}

	assembler := newAnthropicResponseAssembler(converted.anthropic, nil)
	if err := assembler.start(); err != nil {
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	usage, err := processQoderEventStream(requestCtx, upstream.Body, assembler)
	if err != nil {
		status := http.StatusBadGateway
		if qoderAnthropicErrorType(err) == "rate_limit_error" {
			status = http.StatusTooManyRequests
			logQoderQueue(requestCtx, converted.model.ID, err)
		} else {
			LogErrorEvent("stream_conversion_failed", "component", "qoder", "request_id", requestID,
				"model", converted.model.ID, "stream", false, "returned_status", status,
				"duration", logDuration(started), "error", err)
		}
		writeAnthropicError(writer, status, qoderAnthropicErrorType(err), err.Error())
		return
	}
	service.recordUsage(converted.model, usage)
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(assembler.response())
	LogDebugEvent("request_complete", "component", "qoder", "request_id", requestID,
		"model", converted.model.ID, "status", http.StatusOK, "stream", false, "duration", logDuration(started))
}

func (service *qoderService) recordUsage(model qoderModel, usage qoderStreamUsage) {
	if service.usage == nil {
		return
	}
	label := strings.TrimSpace(model.Name)
	if label == "" {
		label = model.ID
	}
	service.usage.Add(label, usage.input, usage.output, usage.cacheRead, usage.cacheWrite)
}

func (service *qoderService) callUpstream(ctx context.Context, converted *qoderConvertedRequest) (*http.Response, error) {
	credentials, err := service.pool.ordered()
	if err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("no active Qoder credentials")
	}
	var lastResponse *http.Response
	var lastErr error
	for index, candidate := range credentials {
		attempt := index + 1
		credential, usableErr := service.pool.usable(ctx, candidate, false)
		if usableErr != nil {
			LogWarnEvent("credential_unavailable", "component", "qoder", "request_id", requestLogID(ctx),
				"attempt", attempt, "credential_count", len(credentials), "credential", candidate.fileName,
				"error", usableErr)
			lastErr = usableErr
			continue
		}
		LogDebugEvent("upstream_attempt", "component", "qoder", "request_id", requestLogID(ctx),
			"attempt", attempt, "credential_count", len(credentials), "credential", credential.fileName,
			"model", converted.model.ID)
		response, requestErr := service.doUpstreamRequest(ctx, converted, credential)
		if requestErr != nil {
			LogWarnEvent("upstream_attempt_failed", "component", "qoder", "request_id", requestLogID(ctx),
				"attempt", attempt, "credential_count", len(credentials), "credential", credential.fileName,
				"model", converted.model.ID, "error", requestErr)
			lastErr = requestErr
			continue
		}
		if response.StatusCode == http.StatusUnauthorized {
			closeQoderResponse(response)
			LogWarnEvent("credential_refresh", "component", "qoder", "request_id", requestLogID(ctx),
				"attempt", attempt, "credential", credential.fileName, "status", http.StatusUnauthorized,
				"action", "force_refresh")
			credential, usableErr = service.pool.usable(ctx, credential, true)
			if usableErr != nil {
				LogWarnEvent("credential_refresh_failed", "component", "qoder", "request_id", requestLogID(ctx),
					"attempt", attempt, "credential", candidate.fileName, "error", usableErr)
				lastErr = usableErr
				continue
			}
			response, requestErr = service.doUpstreamRequest(ctx, converted, credential)
			if requestErr != nil {
				LogWarnEvent("upstream_attempt_failed", "component", "qoder", "request_id", requestLogID(ctx),
					"attempt", attempt, "credential_count", len(credentials), "credential", credential.fileName,
					"model", converted.model.ID, "phase", "after_refresh", "error", requestErr)
				lastErr = requestErr
				continue
			}
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if lastResponse != nil {
				closeQoderResponse(lastResponse)
			}
			return response, nil
		}
		if lastResponse != nil {
			closeQoderResponse(lastResponse)
		}
		lastResponse = response
		if response.StatusCode == http.StatusBadRequest {
			break
		}
		action := "return_last_response"
		if attempt < len(credentials) {
			action = "try_next_credential"
		}
		LogUpstreamEvent(response.StatusCode, "upstream_retry_decision", "component", "qoder", "request_id", requestLogID(ctx),
			"attempt", attempt, "credential_count", len(credentials), "credential", credential.fileName,
			"model", converted.model.ID, "status", response.StatusCode,
			"retry_after", response.Header.Get("Retry-After"), "action", action)
	}
	if lastResponse != nil {
		status := lastResponse.StatusCode
		body := drainQoderResponse(ctx, lastResponse)
		return nil, &qoderUpstreamError{status: status, body: body}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all Qoder credentials failed")
	}
	return nil, lastErr
}

func (service *qoderService) doUpstreamRequest(ctx context.Context, converted *qoderConvertedRequest, credential *qoderCredential) (*http.Response, error) {
	raw, err := json.Marshal(converted.body)
	if err != nil {
		return nil, fmt.Errorf("encode Qoder request: %w", err)
	}
	encoded := []byte(qoderEncodeBody(raw))
	endpoint := qoderChatURL()
	headers, err := qoderBuildAuthHeaders(encoded, endpoint, credential.signingCredential())
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header = headers
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", "ccl")
	request.Header.Set("X-Model-Key", converted.model.ID)
	request.Header.Set("X-Model-Source", converted.model.Source)
	LogDebugEvent("upstream_request", "component", "qoder", "request_id", requestLogID(ctx),
		"host", request.URL.Host, "model", converted.model.ID, "credential", credential.fileName,
		"encoded_bytes", len(encoded))
	DebugHTTPBody(fmt.Sprintf("qoder request request_id=%s path=%s", requestLogID(ctx), request.URL.Path), raw)
	started := time.Now()
	response, err := service.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send Qoder request: %w", err)
	}
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "qoder", "request_id", requestLogID(ctx),
		"model", converted.model.ID, "credential", credential.fileName, "status", response.StatusCode,
		"retry_after", response.Header.Get("Retry-After"), "duration", logDuration(started))
	return response, nil
}

func (service *qoderService) discoverModels(ctx context.Context) ([]qoderModel, error) {
	credentials, err := service.pool.ordered()
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, candidate := range credentials {
		credential, err := service.pool.usable(ctx, candidate, false)
		if err != nil {
			lastErr = err
			continue
		}
		models, status, err := service.discoverModelsWithCredential(ctx, credential)
		if status == http.StatusUnauthorized {
			if refreshed, refreshErr := service.pool.usable(ctx, credential, true); refreshErr == nil {
				models, _, err = service.discoverModelsWithCredential(ctx, refreshed)
			} else {
				err = refreshErr
			}
		}
		if err == nil && len(models) > 0 {
			return models, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("Qoder returned no enabled chat models")
	}
	return nil, lastErr
}

func (service *qoderService) discoverModelsWithCredential(ctx context.Context, credential *qoderCredential) ([]qoderModel, int, error) {
	endpoint := qoderModelListURL()
	headers, err := qoderBuildAuthHeaders(nil, endpoint, credential.signingCredential())
	if err != nil {
		return nil, 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header = headers
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ccl")
	response, err := service.client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("discover Qoder models: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, qoderMaxErrorBytes))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("discover Qoder models: read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.StatusCode, fmt.Errorf("discover Qoder models: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	DebugHTTPBody("qoder model catalog", body)
	var catalog struct {
		Chat []map[string]any `json:"chat"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, response.StatusCode, fmt.Errorf("decode Qoder models: %w", err)
	}
	models := qoderModelsFromCatalog(catalog.Chat)
	if len(models) == 0 {
		return nil, response.StatusCode, fmt.Errorf("Qoder returned no enabled chat models")
	}
	return models, response.StatusCode, nil
}

func qoderModelsFromCatalog(entries []map[string]any) []qoderModel {
	models := make([]qoderModel, 0, len(entries))
	seen := make(map[string]bool)
	for _, entry := range entries {
		id := firstMetadataString(entry, "key")
		enabled, _ := entry["enable"].(bool)
		if id == "" || !enabled || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		entry = qoderMaxContextConfig(entry)
		contextWindow := qoderInt(entry["max_input_tokens"])
		if configurations, ok := entry["context_config"].(map[string]any); ok {
			for _, rawConfiguration := range configurations {
				if configuration, ok := rawConfiguration.(map[string]any); ok {
					contextWindow = max(contextWindow, qoderInt(configuration["token_count"]))
				}
			}
		}
		reasoning, _ := entry["is_reasoning"].(bool)
		thinkingConfig, hasThinking := entry["thinking_config"]
		hasThinking = hasThinking && thinkingConfig != nil
		vision, _ := entry["is_vl"].(bool)
		isNew, _ := entry["is_new"].(bool)
		_, promotionAvailable := entry["promotion"].(map[string]any)
		name := firstMetadataString(entry, "display_name")
		if name == "" {
			name = id
		}
		source := firstMetadataString(entry, "source")
		if source == "" {
			source = "system"
		}
		models = append(models, qoderModel{
			ID: id, Name: name, ContextWindow: contextWindow,
			MaxTokens: qoderInt(entry["max_output_tokens"]), PriceFactor: qoderFloatPointer(entry, "price_factor"),
			Reasoning: reasoning || hasThinking, Vision: vision, IsNew: isNew,
			PromotionAvailable: promotionAvailable, Source: source, Config: entry,
		})
	}
	return models
}

func qoderMaxContextConfig(entry map[string]any) map[string]any {
	configurations, ok := entry["context_config"].(map[string]any)
	if !ok {
		return entry
	}
	maximum := 0
	for _, rawConfiguration := range configurations {
		if configuration, ok := rawConfiguration.(map[string]any); ok {
			maximum = max(maximum, qoderInt(configuration["token_count"]))
		}
	}
	if maximum == 0 {
		return entry
	}
	for _, rawConfiguration := range configurations {
		if configuration, ok := rawConfiguration.(map[string]any); ok {
			configuration["is_default"] = qoderInt(configuration["token_count"]) == maximum
		}
	}
	return entry
}

func qoderInt(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case json.Number:
		value, _ := typed.Int64()
		return int(value)
	}
	return 0
}

func qoderFloatPointer(metadata map[string]any, key string) *float64 {
	value, exists := metadata[key]
	if !exists {
		return nil
	}
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return nil
		}
		number = parsed
	default:
		return nil
	}
	return &number
}

func qoderFallbackModels() []qoderModel {
	definitions := []struct {
		id, name  string
		context   int
		reasoning bool
		vision    bool
	}{
		{"auto", "Qoder Auto", 180_000, true, true},
		{"ultimate", "Qoder Ultimate", 1_000_000, true, true},
		{"performance", "Qoder Performance", 1_000_000, true, true},
		{"efficient", "Qoder Efficient", 180_000, false, true},
		{"lite", "Qoder Lite", 180_000, false, false},
	}
	models := make([]qoderModel, 0, len(definitions))
	for _, definition := range definitions {
		models = append(models, qoderModel{
			ID: definition.id, Name: definition.name, ContextWindow: definition.context,
			MaxTokens: 32_768, Reasoning: definition.reasoning, Vision: definition.vision,
			Source: "system", Config: map[string]any{
				"key": definition.id, "is_reasoning": definition.reasoning,
				"max_output_tokens": 32_768, "source": "system",
			},
		})
	}
	return models
}

type qoderToolState struct {
	id, name string
	args     strings.Builder
}

func processQoderEventStream(ctx context.Context, reader io.Reader, assembler *anthropicResponseAssembler) (qoderStreamUsage, error) {
	usage := qoderStreamUsage{}
	tools := make(map[int]*qoderToolState)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), int(qoderMaxBodyBytes))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		inner, err := qoderEnvelopeBody([]byte(data))
		if err != nil {
			var syntaxError *json.SyntaxError
			if errors.As(err, &syntaxError) {
				LogDebugEvent("stream_frame_skipped", "component", "qoder", "request_id", requestLogID(ctx),
					"reason", "malformed_envelope", "error", err)
				continue
			}
			return usage, err
		}
		if len(inner) == 0 || string(inner) == "[DONE]" {
			continue
		}
		var event struct {
			Usage struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				TotalTokens      int64 `json:"total_tokens"`
				PromptDetails    struct {
					CachedTokens     int64 `json:"cached_tokens"`
					CacheWriteTokens int64 `json:"cache_write_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(inner, &event); err != nil {
			LogDebugEvent("stream_frame_skipped", "component", "qoder", "request_id", requestLogID(ctx),
				"reason", "malformed_payload", "error", err)
			continue
		}
		if event.Usage.PromptTokens > 0 || event.Usage.CompletionTokens > 0 {
			usage.cacheRead = event.Usage.PromptDetails.CachedTokens
			usage.cacheWrite = event.Usage.PromptDetails.CacheWriteTokens
			usage.input = max(int64(0), event.Usage.PromptTokens-usage.cacheRead-usage.cacheWrite)
			usage.output = event.Usage.CompletionTokens
			assembler.contextTokens = int(usage.input)
			assembler.cacheReadTokens = int(usage.cacheRead)
			assembler.cacheWriteTokens = int(usage.cacheWrite)
			if event.Usage.CompletionTokens > 0 {
				assembler.outputTokens = int(event.Usage.CompletionTokens)
			}
		}
		for _, choice := range event.Choices {
			delta := choice.Delta
			if reasoning := qoderStripThinkingTags(delta.ReasoningContent); reasoning != "" {
				assembler.nativeReasoning = true
				if err := assembler.flushInlineContent(); err != nil {
					return usage, err
				}
				if assembler.request.thinkingEnabled {
					err = assembler.addThinking(reasoning)
				} else {
					err = assembler.addText(reasoning)
				}
				if err != nil {
					return usage, err
				}
			}
			if delta.Content != "" {
				content := delta.Content
				if assembler.nativeReasoning {
					content = qoderStripThinkingTags(content)
				}
				if err := assembler.addAssistantContent(content); err != nil {
					return usage, err
				}
			}
			for _, toolCall := range delta.ToolCalls {
				state := tools[toolCall.Index]
				if state == nil {
					state = &qoderToolState{}
					tools[toolCall.Index] = state
				}
				if toolCall.ID != "" {
					state.id = toolCall.ID
				}
				if toolCall.Function.Name != "" {
					state.name = toolCall.Function.Name
				}
				state.args.WriteString(toolCall.Function.Arguments)
			}
			switch choice.FinishReason {
			case "length":
				assembler.stopReason = "max_tokens"
			case "content_filter":
				assembler.stopReason = "refusal"
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, fmt.Errorf("read Qoder event stream: %w", err)
	}
	indexes := make([]int, 0, len(tools))
	for index := range tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		state := tools[index]
		id := strings.TrimSpace(state.id)
		if id == "" {
			id = "toolu_" + qoderStableID("qoder-tool", state.name, state.args.String(), fmt.Sprintf("%d", index))
		}
		if err := assembler.addToolUse(id, state.name, state.args.String()); err != nil {
			return usage, err
		}
	}
	if usage.input == 0 {
		usage.input = int64(assembler.request.inputTokens)
	}
	if usage.output > 0 {
		assembler.outputTokens = int(usage.output)
	} else {
		usage.output = int64(assembler.outputTokens)
	}
	if err := assembler.finish(); err != nil {
		return usage, err
	}
	return usage, nil
}

func qoderEnvelopeBody(data []byte) ([]byte, error) {
	var envelope struct {
		StatusCodeValue int             `json:"statusCodeValue"`
		Body            json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("decode Qoder SSE envelope: %w", err)
	}
	if envelope.StatusCodeValue != 0 && envelope.StatusCodeValue != http.StatusOK {
		body := strings.TrimSpace(string(envelope.Body))
		return nil, &qoderStreamError{
			status: envelope.StatusCodeValue,
			body:   body,
			queue:  qoderQueueFromJSON(envelope.Body, 0),
		}
	}
	if len(envelope.Body) == 0 || string(envelope.Body) == "null" {
		return nil, nil
	}
	var body string
	if json.Unmarshal(envelope.Body, &body) == nil {
		return []byte(body), nil
	}
	return envelope.Body, nil
}

func qoderAnthropicErrorType(err error) string {
	var streamErr *qoderStreamError
	if errors.As(err, &streamErr) && streamErr.queue != nil && streamErr.queue.IsQueued {
		return "rate_limit_error"
	}
	return "api_error"
}

func logQoderQueue(ctx context.Context, model string, err error) {
	var streamErr *qoderStreamError
	if !errors.As(err, &streamErr) || streamErr.queue == nil {
		return
	}
	queue := streamErr.queue
	LogWarnEvent("model_queued", "component", "qoder", "request_id", requestLogID(ctx),
		"model", model, "queue_type", queue.QueueType, "queue_count", queue.QueueCount,
		"retry_after_seconds", queue.RetryAfterSeconds, "wait_seconds", queue.WaitTime,
		"service_available", queue.ServiceAvailable)
}

func qoderQueueFromJSON(raw []byte, depth int) *qoderQueueInfo {
	if depth > 6 || len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return qoderQueueFromValue(value, depth)
}

func qoderQueueFromValue(value any, depth int) *qoderQueueInfo {
	if depth > 6 {
		return nil
	}
	switch typed := value.(type) {
	case string:
		return qoderQueueFromJSON([]byte(typed), depth+1)
	case map[string]any:
		if queued, _ := typed["isQueued"].(bool); queued {
			raw, _ := json.Marshal(typed)
			var info qoderQueueInfo
			if json.Unmarshal(raw, &info) == nil {
				return &info
			}
		}
		for _, key := range []string{"message", "body", "error", "data"} {
			if nested, exists := typed[key]; exists {
				if info := qoderQueueFromValue(nested, depth+1); info != nil {
					return info
				}
			}
		}
	}
	return nil
}

func qoderStripThinkingTags(value string) string {
	value = strings.ReplaceAll(value, "<thinking>", "")
	value = strings.ReplaceAll(value, "</thinking>", "")
	return value
}

func closeQoderResponse(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
}

func drainQoderResponse(ctx context.Context, response *http.Response) string {
	if response == nil || response.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, qoderMaxErrorBytes))
	_ = response.Body.Close()
	DebugHTTPBody(fmt.Sprintf("qoder response request_id=%s status=%d", requestLogID(ctx), response.StatusCode), body)
	return strings.TrimSpace(string(body))
}
