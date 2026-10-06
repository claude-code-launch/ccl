package oauthproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	zedMaxBodyBytes  = int64(128 << 20)
	zedMaxErrorBytes = int64(1 << 20)
	// zedMaxStreamLine bounds one NDJSON event. Events are small deltas, so this
	// is generous headroom rather than a working limit.
	zedMaxStreamLine = 16 << 20
)

// zedWire identifies which provider protocol Zed's envelope carries, and with it
// which CCL data plane serves the model.
type zedWire int

const (
	zedWireAnthropic zedWire = iota
	zedWireResponses
	zedWireChat
	zedWireGoogle
)

// provider is the value of the envelope's "provider" field.
func (w zedWire) provider() string {
	switch w {
	case zedWireResponses:
		return "open_ai"
	case zedWireChat:
		return "x_ai"
	case zedWireGoogle:
		return "google"
	}
	return "anthropic"
}

func (w zedWire) String() string {
	switch w {
	case zedWireResponses:
		return "responses"
	case zedWireChat:
		return "chat"
	case zedWireGoogle:
		return "google"
	}
	return "anthropic"
}

// zedCompletionBody is Zed's request envelope: the upstream provider's native
// request nested under provider_request.
type zedCompletionBody struct {
	Provider        string          `json:"provider"`
	Model           string          `json:"model"`
	ProviderRequest json.RawMessage `json:"provider_request"`
}

// zedGateway is the inner hop of the two-hop Zed path. CCL's existing outer
// services (Anthropic passthrough, Responses, Chat, and the Zed Gemini
// service) speak each provider's native protocol to this loopback gateway as if
// it were the provider; the gateway wraps those requests in Zed's envelope,
// owns LLM-token refresh, and unwraps Zed's NDJSON event stream.
//
// Like the Copilot gateway it must NOT run retryUpstream: the outer service
// already owns the 429/5xx fast retry, and nesting would retry 3x3 times.
type zedGateway struct {
	endpoint string
	key      string
	session  *zedSession
	models   map[string]zedModel
	server   *http.Server
	done     chan struct{}
}

func startZedGateway(parent context.Context, session *zedSession, catalog []zedModel) (*zedGateway, error) {
	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("start Zed gateway listener: %w", err)
	}
	key, err := sessionAPIKey()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("generate Zed gateway key: %w", err)
	}
	models := make(map[string]zedModel, len(catalog))
	for _, model := range catalog {
		models[strings.ToLower(model.ID)] = model
	}
	gateway := &zedGateway{
		endpoint: "http://" + listener.Addr().String(),
		key:      key,
		session:  session,
		models:   models,
		done:     make(chan struct{}),
	}
	gateway.server = &http.Server{
		Handler:           http.HandlerFunc(gateway.serveHTTP),
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return parent },
	}
	go func() {
		err := gateway.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			LogErrorf("Zed gateway stopped endpoint=%q error=%v", gateway.endpoint, err)
		}
		close(gateway.done)
	}()
	return gateway, nil
}

func (g *zedGateway) Stop() {
	if g == nil || g.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeStopTimeout)
	_ = g.server.Shutdown(ctx)
	cancel()
	waitClosed(g.done, runtimeStopTimeout)
}

// zedGatewayRoute maps a loopback path to its wire. The trailing "/google" path
// is internal to CCL: Zed's Gemini service posts native Gemini requests there.
func zedGatewayRoute(path string) (wire zedWire, count bool, ok bool) {
	switch strings.TrimPrefix(path, "/v1") {
	case "/messages":
		return zedWireAnthropic, false, true
	case "/messages/count_tokens":
		return zedWireAnthropic, true, true
	case "/responses":
		return zedWireResponses, false, true
	case "/chat/completions":
		return zedWireChat, false, true
	case "/google":
		return zedWireGoogle, false, true
	}
	return 0, false, false
}

// zedRequestError is a request CCL refuses before contacting Zed.
type zedRequestError struct {
	status  int
	message string
}

func (e *zedRequestError) Error() string { return e.message }

func (g *zedGateway) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	ctx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	if strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) != g.key {
		LogWarnEvent("request_rejected", "component", "zed", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusUnauthorized, "reason", "invalid_gateway_key")
		zedWriteFailure(writer, zedWireAnthropic, zedCloudFailure{status: http.StatusUnauthorized, message: "invalid gateway key"})
		return
	}
	wire, count, ok := zedGatewayRoute(request.URL.Path)
	if !ok {
		zedWriteFailure(writer, zedWireAnthropic, zedCloudFailure{status: http.StatusNotFound, message: "unsupported Zed gateway path"})
		return
	}
	if request.Method != http.MethodPost {
		zedWriteFailure(writer, wire, zedCloudFailure{status: http.StatusMethodNotAllowed, message: "unsupported method"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, zedMaxBodyBytes))
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		zedWriteFailure(writer, wire, zedCloudFailure{status: status, message: "read request body: " + err.Error()})
		return
	}
	model, providerRequest, err := g.prepare(wire, count, body)
	if err != nil {
		status := http.StatusBadRequest
		var requestErr *zedRequestError
		if errors.As(err, &requestErr) {
			status = requestErr.status
		}
		LogWarnEvent("request_rejected", "component", "zed", "request_id", requestID, "wire", wire.String(),
			"status", status, "reason", "request_conversion", "error", err)
		zedWriteFailure(writer, wire, zedCloudFailure{status: status, message: err.Error()})
		return
	}
	g.complete(ctx, writer, wire, count, model, providerRequest, requestID, started)
}

// prepare resolves the catalog model for a request and rewrites the body into
// the exact provider request Zed's own client would send.
func (g *zedGateway) prepare(wire zedWire, count bool, body []byte) (zedModel, []byte, error) {
	requested := strings.TrimPrefix(strings.TrimSpace(gjson.GetBytes(body, "model").String()), "models/")
	requested = stripContextModelSuffix(requested)
	if requested == "" {
		return zedModel{}, nil, &zedRequestError{status: http.StatusBadRequest, message: "model is required"}
	}
	model, ok := g.models[strings.ToLower(requested)]
	if !ok {
		return zedModel{}, nil, &zedRequestError{status: http.StatusNotFound,
			message: fmt.Sprintf("model %q is not available on this Zed account", requested)}
	}
	if modelWire, _ := zedModelWire(model); modelWire != wire {
		return zedModel{}, nil, &zedRequestError{status: http.StatusBadRequest,
			message: fmt.Sprintf("model %q is not served over the %s protocol", requested, wire)}
	}
	var providerRequest []byte
	var err error
	switch wire {
	case zedWireAnthropic:
		providerRequest, err = zedAnthropicProviderRequest(body, model, count)
	case zedWireResponses:
		providerRequest, err = zedResponsesProviderRequest(body, model)
	case zedWireChat:
		providerRequest, err = zedChatProviderRequest(body, model)
	case zedWireGoogle:
		providerRequest, err = zedGoogleProviderRequest(body, model)
	}
	if err != nil {
		return zedModel{}, nil, &zedRequestError{status: http.StatusBadRequest, message: err.Error()}
	}
	return model, providerRequest, nil
}

func zedDecodeFields(body []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("invalid request JSON: %w", err)
	}
	if fields == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	return fields, nil
}

func zedSetField(fields map[string]json.RawMessage, key string, value any) {
	encoded, _ := json.Marshal(value)
	fields[key] = encoded
}

// zedAnthropicRequestFields is the set of top-level Messages fields Zed's cloud
// accepts. Anything else Claude Code sends (stream, metadata, service_tier,
// context_management, ...) is dropped: the cloud owns streaming and betas, and
// its typed request would otherwise reject or mis-handle them.
var zedAnthropicRequestFields = []string{
	"model", "max_tokens", "messages", "tools", "thinking", "tool_choice", "system",
	"cache_control", "stop_sequences", "speed", "temperature", "top_k", "top_p", "output_config",
}

func zedAnthropicProviderRequest(body []byte, model zedModel, count bool) ([]byte, error) {
	fields, err := zedDecodeFields(body)
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(zedAnthropicRequestFields))
	for _, key := range zedAnthropicRequestFields {
		if value, ok := fields[key]; ok && string(value) != "null" {
			out[key] = value
		}
	}
	zedSetField(out, "model", model.ID)
	if !model.SupportsFastMode {
		delete(out, "speed")
	}
	if !model.SupportsThinking {
		delete(out, "thinking")
	}
	if _, ok := out["max_tokens"]; !ok {
		// Zed's typed request requires max_tokens even for token counting.
		maxTokens := 1
		if !count {
			maxTokens = model.MaxOutputTokens
			if maxTokens <= 0 {
				maxTokens = 8192
			}
		}
		zedSetField(out, "max_tokens", maxTokens)
	}
	return json.Marshal(out)
}

func zedResponsesProviderRequest(body []byte, model zedModel) ([]byte, error) {
	fields, err := zedDecodeFields(body)
	if err != nil {
		return nil, err
	}
	// client_metadata is a Codex-backend extension; Zed proxies to the public
	// Responses API, which rejects it.
	delete(fields, "client_metadata")
	zedSetField(fields, "model", model.ID)
	zedSetField(fields, "stream", true)
	if !model.SupportsFastMode {
		delete(fields, "service_tier")
	}
	if !model.SupportsThinking {
		delete(fields, "reasoning")
		delete(fields, "include")
	}
	if !model.SupportsParallelToolCalls {
		delete(fields, "parallel_tool_calls")
	}
	return json.Marshal(fields)
}

func zedChatProviderRequest(body []byte, model zedModel) ([]byte, error) {
	fields, err := zedDecodeFields(body)
	if err != nil {
		return nil, err
	}
	zedSetField(fields, "model", model.ID)
	zedSetField(fields, "stream", true)
	// Zed asks xAI for max_completion_tokens, the non-deprecated spelling.
	if value, ok := fields["max_tokens"]; ok {
		if _, exists := fields["max_completion_tokens"]; !exists {
			fields["max_completion_tokens"] = value
		}
		delete(fields, "max_tokens")
	}
	if !model.SupportsParallelToolCalls {
		delete(fields, "parallel_tool_calls")
	}
	return json.Marshal(fields)
}

// zedGoogleProviderRequest turns the Gemini request ccl builds for Antigravity
// into the public Gemini request Zed forwards: the model travels in the body as
// "models/<id>", and thinking levels use the public API's uppercase enum.
func zedGoogleProviderRequest(body []byte, model zedModel) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("invalid request JSON")
	}
	out, err := sjson.SetBytes(body, "model", "models/"+model.ID)
	if err != nil {
		return nil, err
	}
	if !model.SupportsThinking {
		out, err = sjson.DeleteBytes(out, "generationConfig.thinkingConfig")
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	if level := gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingLevel"); level.Exists() {
		out, err = sjson.SetBytes(out, "generationConfig.thinkingConfig.thinkingLevel", zedGeminiThinkingLevel(level.String()))
		if err != nil {
			return nil, err
		}
	}
	if gjson.GetBytes(out, "generationConfig.thinkingConfig").Exists() {
		out, err = sjson.SetBytes(out, "generationConfig.thinkingConfig.includeThoughts", true)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// zedGeminiThinkingLevel clamps Claude's effort names to Gemini's thinking
// levels; anything above "high" has no Gemini equivalent and maps to HIGH.
func zedGeminiThinkingLevel(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal":
		return "MINIMAL"
	case "low":
		return "LOW"
	case "medium":
		return "MEDIUM"
	}
	return "HIGH"
}

// complete sends one provider request through Zed and relays the answer.
func (g *zedGateway) complete(ctx context.Context, writer http.ResponseWriter, wire zedWire, count bool, model zedModel, providerRequest []byte, requestID string, started time.Time) {
	path := "/completions"
	if count {
		path = "/count_tokens"
	}
	envelope, err := json.Marshal(zedCompletionBody{Provider: wire.provider(), Model: model.ID, ProviderRequest: providerRequest})
	if err != nil {
		zedWriteFailure(writer, wire, zedCloudFailure{status: http.StatusInternalServerError, message: "encode Zed request: " + err.Error()})
		return
	}
	LogDebugEvent("upstream_request", "component", "zed", "request_id", requestID, "wire", wire.String(),
		"model", model.ID, "path", path, "body_bytes", len(envelope), "credential", g.session.credential.fileName)
	DebugHTTPBody(fmt.Sprintf("zed request request_id=%s path=%s", requestID, path), envelope)

	response, err := g.session.doLLM(ctx, func(string) (*http.Request, error) {
		request, err := newZedCloudRequest(ctx, http.MethodPost, path, bytes.NewReader(envelope))
		if err != nil {
			return nil, err
		}
		if !count {
			request.Header.Set(zedClientStatusHeader, "true")
			request.Header.Set(zedClientStreamEndHeader, "true")
		}
		return request, nil
	})
	if err != nil {
		failure := zedFailureFromError(err)
		LogErrorEvent("request_failed", "component", "zed", "request_id", requestID, "wire", wire.String(),
			"model", model.ID, "returned_status", failure.status, "duration", logDuration(started), "error", err)
		zedWriteFailure(writer, wire, failure)
		return
	}
	defer response.Body.Close()
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "zed", "request_id", requestID,
		"wire", wire.String(), "model", model.ID, "status", response.StatusCode,
		"retry_after", response.Header.Get("Retry-After"), "duration", logDuration(started))

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, zedMaxErrorBytes))
		DebugHTTPBody(fmt.Sprintf("zed response request_id=%s status=%d", requestID, response.StatusCode), raw)
		zedWriteFailure(writer, wire, zedParseCloudFailure(response.StatusCode, response.Header, raw))
		return
	}
	if count {
		g.relayTokenCount(writer, response)
		return
	}
	g.stream(writer, wire, response, requestID, started, model)
}

func (g *zedGateway) relayTokenCount(writer http.ResponseWriter, response *http.Response) {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, zedMaxErrorBytes))
	tokens := gjson.GetBytes(raw, "tokens")
	if !tokens.Exists() {
		zedWriteFailure(writer, zedWireAnthropic, zedCloudFailure{status: http.StatusBadGateway, message: "Zed token count response has no tokens"})
		return
	}
	zedWriteJSON(writer, http.StatusOK, map[string]any{"input_tokens": tokens.Int()})
}

// stream converts Zed's NDJSON events into the SSE the outer service expects.
func (g *zedGateway) stream(writer http.ResponseWriter, wire zedWire, response *http.Response, requestID string, started time.Time, model zedModel) {
	flusher, _ := writer.(http.Flusher)
	header := writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), zedMaxStreamLine)
	sawStop := false
	var failure *zedCloudFailure
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		parsed := parseZedStreamLine(line)
		if parsed.failure != nil {
			failure = parsed.failure
			break
		}
		if parsed.event == nil {
			continue
		}
		if wire == zedWireAnthropic && gjson.GetBytes(parsed.event, "type").String() == "message_stop" {
			sawStop = true
		}
		if err := zedWriteEvent(writer, flusher, wire, parsed.event); err != nil {
			return // the client went away
		}
	}
	if failure == nil {
		if err := scanner.Err(); err != nil {
			failure = &zedCloudFailure{status: http.StatusBadGateway, code: "stream_read_error", message: "read Zed stream: " + err.Error()}
		} else if wire == zedWireAnthropic && !sawStop {
			// Claude Code treats a stream without message_stop as a dropped
			// connection; saying so explicitly gives it a retryable error.
			failure = &zedCloudFailure{status: http.StatusBadGateway, code: "stream_ended_unexpectedly", message: "Zed stream ended before the response completed"}
		}
	}
	if failure != nil {
		LogWarnEvent("stream_failed", "component", "zed", "request_id", requestID, "wire", wire.String(),
			"model", model.ID, "code", failure.code, "status", failure.status, "duration", logDuration(started), "error", failure.message)
		_ = zedWriteStreamFailure(writer, flusher, wire, *failure)
		return
	}
	if wire == zedWireChat {
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	LogDebugEvent("request_complete", "component", "zed", "request_id", requestID, "wire", wire.String(),
		"model", model.ID, "status", http.StatusOK, "duration", logDuration(started))
}

// zedStreamLine is one decoded NDJSON line: an upstream event, a status
// message, or a failure. Zed wraps lines as {"event":...} / {"status":...} when
// the client asks for status messages; older servers send bare events, which
// are passed through untouched.
type zedStreamLine struct {
	event   []byte
	status  string
	failure *zedCloudFailure
}

func parseZedStreamLine(line []byte) zedStreamLine {
	root := gjson.ParseBytes(line)
	if !root.IsObject() {
		return zedStreamLine{}
	}
	// Zed's wrapper is an externally tagged enum, so a wrapped line has exactly
	// one key. Requiring that keeps a bare provider event that merely contains an
	// "event" or "status" field from being mistaken for a wrapper.
	var key string
	var value gjson.Result
	fields := 0
	root.ForEach(func(name, field gjson.Result) bool {
		fields++
		key, value = name.String(), field
		return fields < 2
	})
	if fields == 1 {
		switch key {
		case "event":
			return zedStreamLine{event: []byte(value.Raw)}
		case "status":
			return parseZedStatus(value)
		}
	}
	return zedStreamLine{event: append([]byte(nil), line...)}
}

func parseZedStatus(status gjson.Result) zedStreamLine {
	if status.Type == gjson.String {
		return zedStreamLine{status: status.String()}
	}
	if !status.IsObject() {
		return zedStreamLine{}
	}
	if failed := status.Get("failed"); failed.Exists() {
		failure := zedParseCloudFailure(http.StatusBadGateway, nil, []byte(failed.Raw))
		if failure.status == http.StatusBadGateway {
			switch {
			case strings.Contains(failure.code, "rate_limit"):
				failure.status = http.StatusTooManyRequests
			case strings.Contains(failure.code, "overloaded"):
				failure.status = 529
			case strings.Contains(failure.code, "payment"):
				failure.status = http.StatusPaymentRequired
			}
		}
		return zedStreamLine{status: "failed", failure: &failure}
	}
	if status.Get("queued").Exists() {
		return zedStreamLine{status: "queued"}
	}
	return zedStreamLine{}
}

// zedWriteEvent writes one upstream event as an SSE record. Anthropic and
// Responses streams name their events; Chat and Gemini streams are data-only.
func zedWriteEvent(writer io.Writer, flusher http.Flusher, wire zedWire, payload []byte) error {
	var record bytes.Buffer
	if wire == zedWireAnthropic || wire == zedWireResponses {
		if name := gjson.GetBytes(payload, "type").String(); name != "" {
			record.WriteString("event: ")
			record.WriteString(name)
			record.WriteByte('\n')
		}
	}
	record.WriteString("data: ")
	record.Write(payload)
	record.WriteString("\n\n")
	if _, err := writer.Write(record.Bytes()); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// zedCloudFailure is a normalized Zed failure, whether it arrived as an HTTP
// error body or as a failed status line inside a stream.
type zedCloudFailure struct {
	status     int
	code       string
	message    string
	retryAfter string
}

// zedParseCloudFailure interprets Zed's {code,message,upstream_status,retry_after}
// error body the way Zed's own client does: an upstream_http_* code carries the
// upstream provider's real status, which is what drives retry decisions.
func zedParseCloudFailure(status int, header http.Header, body []byte) zedCloudFailure {
	failure := zedCloudFailure{status: status, message: zedErrorMessage(body)}
	var parsed struct {
		Code           string   `json:"code"`
		UpstreamStatus *int     `json:"upstream_status"`
		RetryAfter     *float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		failure.code = parsed.Code
		if strings.HasPrefix(parsed.Code, "upstream_http_") {
			if parsed.UpstreamStatus != nil && *parsed.UpstreamStatus >= 100 && *parsed.UpstreamStatus < 600 {
				failure.status = *parsed.UpstreamStatus
			} else if !strings.HasSuffix(parsed.Code, "_error") {
				if n, err := strconv.Atoi(strings.TrimPrefix(parsed.Code, "upstream_http_")); err == nil && n >= 100 && n < 600 {
					failure.status = n
				}
			}
		}
		if parsed.RetryAfter != nil && *parsed.RetryAfter > 0 {
			failure.retryAfter = strconv.Itoa(int(math.Ceil(*parsed.RetryAfter)))
		}
	}
	if header != nil {
		if value := strings.TrimSpace(header.Get("Retry-After")); value != "" {
			failure.retryAfter = value
		}
		if minimum := strings.TrimSpace(header.Get(zedMinimumVersionHeader)); minimum != "" {
			failure.message = strings.TrimSpace(fmt.Sprintf("Zed requires client version %s or newer (ccl identifies as Zed %s); update ccl. %s",
				minimum, zedClientVersion, failure.message))
		}
	}
	if failure.message == "" {
		if status == http.StatusPaymentRequired {
			failure.message = "payment required to use this language model; please upgrade your Zed plan"
		} else {
			failure.message = http.StatusText(status)
		}
	}
	return failure
}

// zedFailureFromError converts a token-acquisition or transport error into the
// failure to relay.
func zedFailureFromError(err error) zedCloudFailure {
	var httpErr *zedHTTPError
	if errors.As(err, &httpErr) {
		failure := zedParseCloudFailure(httpErr.status, nil, []byte(httpErr.body))
		if httpErr.retryAfter != "" {
			failure.retryAfter = httpErr.retryAfter
		}
		if httpErr.status == http.StatusUnauthorized {
			failure.message = fmt.Sprintf("Zed rejected the stored credentials; run `ccl oauth %s` again (%s)", ProviderZed, failure.message)
		}
		return failure
	}
	return zedCloudFailure{status: http.StatusBadGateway, message: err.Error()}
}

func zedGoogleStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	}
	return "INTERNAL"
}

func zedWriteJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

// zedWriteFailure writes an HTTP error in the error dialect of the wire, so the
// outer service that issued the request can read the message.
func zedWriteFailure(writer http.ResponseWriter, wire zedWire, failure zedCloudFailure) {
	if failure.retryAfter != "" {
		writer.Header().Set("Retry-After", failure.retryAfter)
	}
	switch wire {
	case zedWireAnthropic:
		writeAnthropicError(writer, failure.status, anthropicErrorType(failure.status), failure.message)
	case zedWireGoogle:
		zedWriteJSON(writer, failure.status, map[string]any{"error": map[string]any{
			"code": failure.status, "message": failure.message, "status": zedGoogleStatus(failure.status),
		}})
	default:
		zedWriteJSON(writer, failure.status, map[string]any{"error": map[string]any{
			"message": failure.message, "type": anthropicErrorType(failure.status), "code": failure.code,
		}})
	}
}

// zedWriteStreamFailure reports a failure that happened after the 200 response
// started, as the terminal error event of the wire's stream dialect.
func zedWriteStreamFailure(writer http.ResponseWriter, flusher http.Flusher, wire zedWire, failure zedCloudFailure) error {
	var payload map[string]any
	switch wire {
	case zedWireAnthropic:
		payload = map[string]any{"type": "error", "error": map[string]any{
			"type": anthropicErrorType(failure.status), "message": failure.message,
		}}
	case zedWireResponses:
		payload = map[string]any{"type": "error", "code": failure.code, "message": failure.message}
	case zedWireGoogle:
		payload = map[string]any{"error": map[string]any{
			"code": failure.status, "message": failure.message, "status": zedGoogleStatus(failure.status),
		}}
	default:
		payload = map[string]any{"error": map[string]any{
			"message": failure.message, "type": anthropicErrorType(failure.status), "code": failure.code,
		}}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return zedWriteEvent(writer, flusher, wire, encoded)
}
