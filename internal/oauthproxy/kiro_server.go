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
}

type kiroUpstreamError struct {
	status int
	body   string
}

func (e *kiroUpstreamError) Error() string {
	return fmt.Sprintf("Kiro upstream returned HTTP %d: %s", e.status, e.body)
}

func startKiroOAuthWithFiles(parent context.Context, modelSpec string, credentialFiles []string, restrictToFiles bool, resolver func() ([]string, error)) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	pool := newKiroCredentialPool(authDir, credentialFiles, restrictToFiles, resolver)
	credentials, err := pool.load()
	if err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		if restrictToFiles && len(credentialFiles) == 0 {
			return nil, fmt.Errorf("OAuth group for %s has no credentials; edit it with `ccl oauth group` or run `ccl oauth sync`", ProviderKiro)
		}
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
	service := &kiroService{
		apiKey:       apiKey,
		models:       models,
		modelCatalog: newKiroModelCatalog(kiroAvailableModelsEndpoint),
		pool:         pool,
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
	Debugf("runtime start oauth provider=kiro backend=kiro protocol=anthropic port=%s credential_files=%d restricted=%t model_count=%d",
		listener.Addr().String(), len(credentialFiles), restrictToFiles, len(models))
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
		writeKiroError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodGet {
		writeKiroError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	models, err := s.availableModels(request.Context())
	if err != nil {
		writeKiroError(writer, http.StatusBadGateway, "api_error", "Unable to load available models from Kiro: "+err.Error())
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
	firstID, lastID := kiroModelBounds(ids)
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
		writeKiroError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		writeKiroError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	raw, err := readKiroInboundBody(writer, request, kiroMaxInboundRequestBytes)
	if err != nil {
		writeKiroError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"input_tokens": estimateKiroTokensBytes(raw)})
}

func (s *kiroService) handleMessages(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeKiroError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		writeKiroError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	Debugf("kiro messages inbound path=%q content_length=%d transfer_encoding_count=%d content_type=%q content_encoding=%q",
		request.URL.RequestURI(), request.ContentLength, len(request.TransferEncoding),
		request.Header.Get("Content-Type"), request.Header.Get("Content-Encoding"))
	raw, err := readKiroInboundBody(writer, request, kiroMaxInboundRequestBytes)
	if err != nil {
		writeKiroError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	Debugf("kiro messages body bytes=%d", len(raw))
	if len(bytes.TrimSpace(raw)) == 0 {
		writeKiroError(writer, http.StatusBadRequest, "invalid_request_error", "invalid Anthropic Messages request: request body is empty")
		return
	}
	converted, err := convertAnthropicToKiro(raw)
	if err != nil {
		writeKiroError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if converted.droppedMedia > 0 || converted.dedupedMedia > 0 ||
		converted.resizedMedia > 0 || converted.correctedMedia > 0 {
		Debugf("kiro inline media normalized kept=%d capped=%d deduplicated=%d resized=%d mime_corrected=%d limit=%d",
			converted.inlineMedia, converted.droppedMedia, converted.dedupedMedia,
			converted.resizedMedia, converted.correctedMedia, kiroMaxInlineMediaSegments)
	}
	if converted.droppedToolUses > 0 || converted.droppedToolRuns > 0 {
		Debugf("kiro tool pairing normalized dropped_uses=%d dropped_results=%d",
			converted.droppedToolUses, converted.droppedToolRuns)
	}
	if converted.truncatedTexts > 0 {
		Debugf("kiro content fields truncated fields=%d dropped_bytes=%d largest_original_bytes=%d limit=%d",
			converted.truncatedTexts, converted.droppedText, converted.largestText, kiroMaxTextFieldBytes)
	}
	upstream, err := s.callUpstream(request.Context(), converted)
	if err != nil {
		// Forward upstream client errors unchanged (status and Anthropic error
		// type) so Claude Code can apply its own handling: back off on 429,
		// re-authenticate on 401, shrink the request on 413. Collapsing them all
		// into 400 made every one of those look like a malformed request.
		var upstreamErr *kiroUpstreamError
		if errors.As(err, &upstreamErr) && upstreamErr.status >= 400 && upstreamErr.status < 500 {
			// err, not upstreamErr: the wrapper carries extra context such as a
			// failed token refresh.
			writeKiroError(writer, upstreamErr.status, kiroAnthropicErrorType(upstreamErr.status), err.Error())
			return
		}
		// Everything else is a proxy-side or upstream server failure.
		writeKiroError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer upstream.Body.Close()

	if converted.stream {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		assembler := newKiroAnthropicAssembler(converted, writer)
		if err := assembler.start(); err != nil {
			return
		}
		if err := processKiroEventStream(upstream.Body, assembler); err != nil {
			_ = assembler.emit("error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "api_error",
					"message": err.Error(),
				},
			})
		}
		return
	}

	assembler := newKiroAnthropicAssembler(converted, nil)
	if err := processKiroEventStream(upstream.Body, assembler); err != nil {
		writeKiroError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(assembler.response())
}

func readKiroInboundBody(writer http.ResponseWriter, request *http.Request, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("invalid Kiro request body limit")
	}
	if request.ContentLength > maxBytes {
		return nil, fmt.Errorf("Anthropic Messages request body exceeds %s limit", kiroRequestLimitLabel(maxBytes))
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxBytes)
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return nil, fmt.Errorf("Anthropic Messages request body exceeds %s limit", kiroRequestLimitLabel(maxBytes))
		}
		return nil, fmt.Errorf("read Anthropic Messages request body: %w", err)
	}
	return raw, nil
}

// drainKiroErrorBody reads a bounded prefix of an upstream error body and closes
// it, so the diagnostics survive after the response is discarded.
func drainKiroErrorBody(response *http.Response) string {
	if response == nil || response.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, kiroMaxUpstreamErrorBytes))
	_ = response.Body.Close()
	return strings.TrimSpace(string(body))
}

func kiroRequestLimitLabel(maxBytes int64) string {
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
		response, err := s.callUpstreamOnce(ctx, converted)
		if err == nil {
			return response, nil
		}
		if !isKiroRateLimitError(err) || attempt >= len(backoff) {
			return nil, err
		}
		delay := backoff[attempt]
		Debugf("kiro upstream rate limited model=%q retry=%d/%d wait=%s", converted.model, attempt+1, len(backoff), delay)
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
func (s *kiroService) callUpstreamOnce(ctx context.Context, converted *kiroConvertedRequest) (*http.Response, error) {
	credentials, err := s.pool.orderedCredentials()
	if err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("no usable Kiro credentials")
	}
	var lastErr error
	var rateLimitErr error
	for _, candidate := range credentials {
		credential, err := s.pool.usableCredential(ctx, candidate, false)
		if err != nil {
			lastErr = err
			continue
		}
		response, err := s.doUpstreamRequest(ctx, converted, credential)
		if err != nil {
			lastErr = err
			continue
		}
		if response.StatusCode == http.StatusUnauthorized {
			// Drain the rejected response before closing it: if the refresh then
			// fails we still have the upstream diagnostics to report.
			unauthorized := drainKiroErrorBody(response)
			refreshed, refreshErr := s.pool.usableCredential(ctx, credential, true)
			if refreshErr != nil {
				lastErr = fmt.Errorf("%w (credential refresh failed: %v)",
					&kiroUpstreamError{status: http.StatusUnauthorized, body: unauthorized}, refreshErr)
				continue
			}
			response, err = s.doUpstreamRequest(ctx, converted, refreshed)
			if err != nil {
				lastErr = err
				continue
			}
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		}
		upstreamErr := &kiroUpstreamError{status: response.StatusCode, body: drainKiroErrorBody(response)}
		if response.StatusCode == http.StatusBadRequest {
			return nil, upstreamErr
		}
		if response.StatusCode == http.StatusTooManyRequests {
			rateLimitErr = upstreamErr
		}
		lastErr = upstreamErr
	}
	// A rate limit is the retryable outcome, so it wins over whatever the last
	// credential happened to fail with.
	if rateLimitErr != nil {
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
	Debugf("kiro upstream request host=%q model=%q credential=%q body_bytes=%d media=%d budget_original_bytes=%d budget_final_bytes=%d budget_original_tokens=%d budget_final_tokens=%d budget_dropped_media=%d budget_dropped_history=%d budget_truncated_texts=%d budget_dropped_text_bytes=%d budget_dropped_tools=%d",
		request.URL.Host, converted.model, credential.fileName, len(raw), converted.inlineMedia,
		converted.originalBody, converted.finalBody, converted.originalTokens, converted.finalTokens,
		converted.budgetMedia, converted.budgetHistory,
		converted.budgetTexts, converted.budgetTextBytes, converted.budgetTools)
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Kiro upstream: %w", err)
	}
	Debugf("kiro upstream response status=%d model=%q credential=%q", response.StatusCode, converted.model, credential.fileName)
	return response, nil
}

// kiroAnthropicErrorType maps an HTTP status onto the error type the Anthropic
// Messages API uses for it, so a forwarded status stays self-consistent for
// clients that branch on error.type rather than on the status code.
func kiroAnthropicErrorType(status int) string {
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
	default:
		return "invalid_request_error"
	}
}

func writeKiroError(writer http.ResponseWriter, status int, errorType, message string) {
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

// kiroModelBounds reports the first and last model id of a page, or empty
// strings when the page is empty.
func kiroModelBounds(models []string) (string, string) {
	if len(models) == 0 {
		return "", ""
	}
	return models[0], models[len(models)-1]
}

func uuidString() string {
	return uuid.NewString()
}
