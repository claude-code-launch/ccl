package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// anthropicPassthroughService is a pure native-Anthropic Messages passthrough:
// no protocol conversion, only credential resolution and 401-refresh-once. It
// backs models.dev native-Anthropic gateways and Copilot native-Messages models
// (static key).
type anthropicPassthroughService struct {
	apiKey     string
	authorizer chatAuthorizer
	models     []string
	client     *http.Client
	usage      *UsageTracker
	baseURL    string
}

// newAnthropicPassthroughService builds a native-Messages passthrough against
// baseURL. The caller's Anthropic-Beta header passes through untouched.
func newAnthropicPassthroughService(apiKey, baseURL string, models []string, authorizer chatAuthorizer, usage *UsageTracker) *anthropicPassthroughService {
	return &anthropicPassthroughService{
		apiKey: apiKey, baseURL: baseURL, models: models, authorizer: authorizer,
		usage: usage,
		client: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
			ResponseHeaderTimeout: 90 * time.Second,
		}},
	}
}

func (s *anthropicPassthroughService) handler() http.Handler {
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

func (s *anthropicPassthroughService) authorized(request *http.Request) bool {
	if request.Header.Get("x-api-key") == s.apiKey {
		return true
	}
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) == s.apiKey
}

func (s *anthropicPassthroughService) handleModels(writer http.ResponseWriter, request *http.Request) {
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

func (s *anthropicPassthroughService) handleCountTokens(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
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
	response, err := s.forward(requestCtx, request, "/v1/messages/count_tokens", raw)
	if err != nil {
		LogErrorEvent("request_failed", "component", "anthropic_passthrough", "request_id", requestID,
			"returned_status", http.StatusBadGateway, "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.forwardUpstreamError(writer, response)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(writer, io.LimitReader(response.Body, chatMaxResponseBytes))
}

func (s *anthropicPassthroughService) handleMessages(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	LogDebugEvent("request_received", "component", "anthropic_passthrough", "request_id", requestID,
		"path", request.URL.Path, "method", request.Method)
	if !s.authorized(request) {
		LogWarnEvent("request_rejected", "component", "anthropic_passthrough", "request_id", requestID,
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
	stream := anthropicRequestStreams(raw)
	fallbackModel := stripContextModelSuffix(gjson.GetBytes(raw, "model").String())
	response, err := s.forward(requestCtx, request, "/v1/messages?beta=true", raw)
	if err != nil {
		LogErrorEvent("request_failed", "component", "anthropic_passthrough", "request_id", requestID,
			"returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", err)
		writeAnthropicError(writer, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		LogUpstreamEvent(response.StatusCode, "request_failed", "component", "anthropic_passthrough", "request_id", requestID,
			"status", response.StatusCode, "duration", logDuration(started))
		s.forwardUpstreamError(writer, response)
		return
	}
	if stream {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		s.streamCopy(writer, response.Body, fallbackModel)
	} else {
		writer.Header().Set("Content-Type", "application/json")
		body, readErr := io.ReadAll(io.LimitReader(response.Body, chatMaxResponseBytes))
		if len(body) > 0 {
			s.recordJSONUsage(body, fallbackModel)
			_, _ = writer.Write(body)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			LogWarnEvent("response_read_failed", "component", "anthropic_passthrough", "request_id", requestID,
				"error", readErr)
		}
	}
	LogDebugEvent("request_complete", "component", "anthropic_passthrough", "request_id", requestID,
		"status", http.StatusOK, "stream", stream, "duration", logDuration(started))
}

// forward sends one upstream request, resolving the Bearer token. On a 401 from
// a stale OAuth token it refreshes once and retries before surfacing the failure.
func (s *anthropicPassthroughService) forward(ctx context.Context, incoming *http.Request, path string, body []byte) (*http.Response, error) {
	response, err := s.forwardOnce(ctx, incoming, path, body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized && s.authorizer != nil && s.authorizer.isOAuth() {
		_ = response.Body.Close()
		if _, authErr := s.authorizer.authorize(ctx, true); authErr != nil {
			return nil, fmt.Errorf("refresh OAuth token: %w", authErr)
		}
		response, err = s.forwardOnce(ctx, incoming, path, body)
		if err != nil {
			return nil, err
		}
	}
	return response, nil
}

func (s *anthropicPassthroughService) forwardOnce(ctx context.Context, incoming *http.Request, path string, body []byte) (*http.Response, error) {
	token, err := s.authorizer.authorize(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("authorize upstream: %w", err)
	}
	target := strings.TrimRight(s.baseURL, "/") + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, values := range incoming.Header {
		switch http.CanonicalHeaderKey(key) {
		case "Authorization", "X-Api-Key", "Host", "Content-Length", "Connection", "Accept-Encoding":
			continue
		}
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if request.Header.Get("Anthropic-Version") == "" {
		request.Header.Set("Anthropic-Version", "2023-06-01")
	}
	LogDebugEvent("upstream_request", "component", "anthropic_passthrough", "request_id", requestLogID(ctx),
		"method", http.MethodPost, "endpoint", SafeLogEndpoint(target), "body_bytes", len(body))
	started := time.Now()
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call upstream: %w", err)
	}
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "anthropic_passthrough", "request_id", requestLogID(ctx),
		"status", response.StatusCode, "retry_after", response.Header.Get("Retry-After"),
		"upstream_request_id", firstHeader(response.Header, "X-Request-Id", "Request-Id"),
		"content_type", response.Header.Get("Content-Type"), "duration", logDuration(started))
	return response, nil
}

// forwardUpstreamError relays a non-2xx upstream response verbatim so Claude Code
// sees Anthropic's own error JSON and status rather than a rewritten shape.
func (s *anthropicPassthroughService) forwardUpstreamError(writer http.ResponseWriter, response *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(response.Body, chatMaxErrorBytes))
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		writer.Header().Set("Content-Type", contentType)
	} else {
		writer.Header().Set("Content-Type", "application/json")
	}
	if retryAfter := response.Header.Get("Retry-After"); retryAfter != "" {
		writer.Header().Set("Retry-After", retryAfter)
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(body)
}

// streamCopy relays an SSE stream to the client, flushing after every chunk so
// tokens surface as the upstream emits them. Relay bytes are scanned for usage
// events so the passthrough records token totals like the other CCL data planes.
func (s *anthropicPassthroughService) streamCopy(writer http.ResponseWriter, body io.Reader, fallbackModel string) {
	flusher, _ := writer.(http.Flusher)
	scanner := &passthroughUsageScanner{}
	buffer := make([]byte, 32*1024)
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			scanner.feed(buffer[:n])
			if _, writeErr := writer.Write(buffer[:n]); writeErr != nil {
				scanner.flush()
				s.recordStreamUsage(scanner, fallbackModel)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				LogWarnEvent("stream_copy_failed", "component", "anthropic_passthrough", "error", err)
			}
			scanner.flush()
			s.recordStreamUsage(scanner, fallbackModel)
			return
		}
	}
}

// passthroughUsageScanner extracts Anthropic usage totals from relayed SSE
// bytes without altering them.
type passthroughUsageScanner struct {
	pending []byte
	model   string
	// output is kept as the high-water mark because message_delta reports the
	// cumulative output token count, not a per-event delta.
	input, output, cacheRead, cacheWrite int64
	sawUsage                             bool
}

func (u *passthroughUsageScanner) feed(chunk []byte) {
	u.pending = append(u.pending, chunk...)
	for {
		index := bytes.IndexByte(u.pending, '\n')
		if index < 0 {
			break
		}
		line := u.pending[:index]
		u.pending = append([]byte(nil), u.pending[index+1:]...)
		u.scanLine(line)
	}
}

// flush processes a trailing line that was not newline-terminated when the
// stream ended, so a final usage event is not lost.
func (u *passthroughUsageScanner) flush() {
	if len(u.pending) > 0 {
		line := u.pending
		u.pending = nil
		u.scanLine(line)
	}
}

func (u *passthroughUsageScanner) scanLine(line []byte) {
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	event := gjson.ParseBytes(payload)
	switch event.Get("type").String() {
	case "message_start":
		if u.model == "" {
			u.model = event.Get("message.model").String()
		}
		// Gate on usage.Exists(): gateways that omit message_start usage must
		// not produce a phantom zero-token entry in the usage tracker.
		if usage := event.Get("message.usage"); usage.Exists() {
			u.input = usage.Get("input_tokens").Int()
			u.cacheRead = usage.Get("cache_read_input_tokens").Int()
			u.cacheWrite = usage.Get("cache_creation_input_tokens").Int()
			u.sawUsage = true
		}
	case "message_delta":
		if output := event.Get("usage.output_tokens").Int(); output > u.output {
			u.output = output
		}
		if event.Get("usage").Exists() {
			u.sawUsage = true
		}
	}
}

// recordJSONUsage records usage from a non-streaming Anthropic Messages
// response body.
func (s *anthropicPassthroughService) recordJSONUsage(body []byte, fallbackModel string) {
	if s.usage == nil {
		return
	}
	usage := gjson.GetBytes(body, "usage")
	if !usage.Exists() {
		return
	}
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		model = fallbackModel
	}
	s.usage.Add(model,
		usage.Get("input_tokens").Int(),
		usage.Get("output_tokens").Int(),
		usage.Get("cache_read_input_tokens").Int(),
		usage.Get("cache_creation_input_tokens").Int())
}

func (s *anthropicPassthroughService) recordStreamUsage(scanner *passthroughUsageScanner, fallbackModel string) {
	if s.usage == nil || !scanner.sawUsage {
		return
	}
	model := scanner.model
	if model == "" {
		model = fallbackModel
	}
	s.usage.Add(model, scanner.input, scanner.output, scanner.cacheRead, scanner.cacheWrite)
}

// anthropicRequestStreams reports whether an Anthropic Messages request body
// asks for a streaming response.
func anthropicRequestStreams(raw []byte) bool {
	var request struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(raw, &request) != nil {
		return false
	}
	return request.Stream
}
