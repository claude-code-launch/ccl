package oauthproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Chat Completions front. A Chat request is rewritten as a Responses request,
// served by the backend's Responses handler, and the Responses output is
// rendered back as Chat Completions. Backends therefore implement one inbound
// protocol, and every subscription is reachable over Chat.

const chatFrontMaxBodyBytes = int64(64 << 20)

type chatFrontRequest struct {
	Model               string           `json:"model"`
	Messages            []map[string]any `json:"messages"`
	Tools               []map[string]any `json:"tools"`
	ToolChoice          any              `json:"tool_choice"`
	ParallelToolCalls   *bool            `json:"parallel_tool_calls"`
	Stream              bool             `json:"stream"`
	StreamOptions       map[string]any   `json:"stream_options"`
	MaxTokens           int              `json:"max_tokens"`
	MaxCompletionTokens int              `json:"max_completion_tokens"`
	ReasoningEffort     string           `json:"reasoning_effort"`
	ResponseFormat      map[string]any   `json:"response_format"`
	Temperature         *float64         `json:"temperature"`
	TopP                *float64         `json:"top_p"`
	Stop                any              `json:"stop"`
	User                string           `json:"user"`
	ServiceTier         string           `json:"service_tier"`
	PromptCacheKey      string           `json:"prompt_cache_key"`
}

// chatToResponsesRequest rewrites a Chat Completions body as a Responses body.
func chatToResponsesRequest(raw []byte) ([]byte, *chatFrontRequest, error) {
	var request chatFrontRequest
	if err := decodeProtocolJSON(raw, &request); err != nil {
		return nil, nil, fmt.Errorf("invalid Chat Completions request: %w", err)
	}
	if strings.TrimSpace(request.Model) == "" {
		return nil, nil, fmt.Errorf("model is required")
	}
	if len(request.Messages) == 0 {
		return nil, nil, fmt.Errorf("messages must not be empty")
	}
	input := make([]map[string]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer", "user":
			content, err := chatFrontContent(message["content"], "input_text")
			if err != nil {
				return nil, nil, err
			}
			if len(content) > 0 {
				input = append(input, map[string]any{"type": "message", "role": role, "content": content})
			}
		case "assistant":
			if reasoning, _ := message["reasoning_content"].(string); reasoning != "" {
				input = append(input, map[string]any{
					"type": "reasoning", "id": assemblerItemID(cclReasoningIDPrefix),
					"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
				})
			}
			content, err := chatFrontContent(message["content"], "output_text")
			if err != nil {
				return nil, nil, err
			}
			if len(content) > 0 {
				input = append(input, map[string]any{"type": "message", "role": "assistant", "content": content})
			}
			calls, _ := message["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				id, _ := call["id"].(string)
				name, _ := function["name"].(string)
				if id == "" || name == "" {
					return nil, nil, fmt.Errorf("assistant tool_calls require id and function.name")
				}
				arguments, _ := function["arguments"].(string)
				if strings.TrimSpace(arguments) == "" {
					arguments = "{}"
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": arguments})
			}
		case "tool":
			id, _ := message["tool_call_id"].(string)
			if id == "" {
				return nil, nil, fmt.Errorf("tool messages require tool_call_id")
			}
			var output any = ""
			switch content := message["content"].(type) {
			case string:
				output = content
			case []any:
				parts, err := chatFrontContent(content, "input_text")
				if err != nil {
					return nil, nil, err
				}
				output = parts
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": id, "output": output})
		default:
			return nil, nil, fmt.Errorf("unsupported Chat Completions message role %q", role)
		}
	}
	body := map[string]any{"model": request.Model, "input": input, "stream": request.Stream}
	if len(request.Tools) > 0 {
		tools := make([]map[string]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			function, ok := tool["function"].(map[string]any)
			if tool["type"] != "function" || !ok {
				return nil, nil, fmt.Errorf("only function tools are supported over Chat Completions")
			}
			converted := map[string]any{"type": "function", "name": function["name"]}
			for _, key := range []string{"description", "parameters", "strict"} {
				if value, exists := function[key]; exists {
					converted[key] = value
				}
			}
			tools = append(tools, converted)
		}
		body["tools"] = tools
	}
	switch choice := request.ToolChoice.(type) {
	case string:
		body["tool_choice"] = choice
	case map[string]any:
		function, _ := choice["function"].(map[string]any)
		body["tool_choice"] = map[string]any{"type": "function", "name": function["name"]}
	}
	if request.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *request.ParallelToolCalls
	}
	if limit := max(request.MaxCompletionTokens, request.MaxTokens); limit > 0 {
		body["max_output_tokens"] = limit
	}
	if effort := strings.TrimSpace(request.ReasoningEffort); effort != "" {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	if request.ResponseFormat["type"] == "json_schema" {
		schema, _ := request.ResponseFormat["json_schema"].(map[string]any)
		format := map[string]any{"type": "json_schema"}
		for _, key := range []string{"name", "description", "schema", "strict"} {
			if value, exists := schema[key]; exists {
				format[key] = value
			}
		}
		body["text"] = map[string]any{"format": format}
	}
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		body["top_p"] = *request.TopP
	}
	if request.Stop != nil {
		body["stop"] = request.Stop
	}
	for key, value := range map[string]string{"user": request.User, "service_tier": request.ServiceTier, "prompt_cache_key": request.PromptCacheKey} {
		if value != "" {
			body[key] = value
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("encode Responses request: %w", err)
	}
	return encoded, &request, nil
}

// chatFrontContent converts Chat message content into Responses content parts.
func chatFrontContent(value any, textType string) ([]any, error) {
	switch content := value.(type) {
	case nil:
		return nil, nil
	case string:
		if content == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": textType, "text": content}}, nil
	case []any:
		parts := make([]any, 0, len(content))
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid Chat Completions content part")
			}
			switch part["type"] {
			case "text", "input_text", "output_text":
				if text, _ := part["text"].(string); text != "" {
					parts = append(parts, map[string]any{"type": textType, "text": text})
				}
			case "refusal":
				if text, _ := part["refusal"].(string); text != "" {
					parts = append(parts, map[string]any{"type": textType, "text": text})
				}
			case "image_url":
				url, _ := part["image_url"].(string)
				if nested, ok := part["image_url"].(map[string]any); ok {
					url, _ = nested["url"].(string)
				}
				parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
			default:
				return nil, fmt.Errorf("unsupported Chat Completions content part %q", part["type"])
			}
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("message content must be a string or an array of parts")
	}
}

// chatCompletionsFront serves Chat Completions through a Responses handler.
func chatCompletionsFront(key string, responses http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if !inferenceRequestAllowed(writer, request, key) {
			return
		}
		raw, err := readInboundBody(writer, request, chatFrontMaxBodyBytes)
		if err != nil {
			writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		body, chat, err := chatToResponsesRequest(raw)
		if err != nil {
			writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		inner := request.Clone(request.Context())
		inner.Body = io.NopCloser(bytes.NewReader(body))
		inner.ContentLength = int64(len(body))
		inner.URL.Path = "/v1/responses"
		includeUsage, _ := chat.StreamOptions["include_usage"].(bool)
		translator := newChatFrontWriter(writer, chat.Model, includeUsage)
		responses(translator, inner)
		translator.finish()
	}
}

// chatFrontWriter receives what a Responses handler writes and renders it as
// Chat Completions: stream events become chunks as they arrive, and a JSON
// body is converted once the handler returns.
type chatFrontWriter struct {
	target       http.ResponseWriter
	header       http.Header
	status       int
	wroteHeader  bool
	streaming    bool
	model        string
	id           string
	created      int64
	includeUsage bool
	pending      bytes.Buffer
	body         bytes.Buffer
	toolIndexes  map[int]int
	sawToolCall  bool
	sentRole     bool
	done         bool
}

func newChatFrontWriter(target http.ResponseWriter, model string, includeUsage bool) *chatFrontWriter {
	return &chatFrontWriter{
		target: target, header: make(http.Header), status: http.StatusOK, model: model,
		id: "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", ""), created: time.Now().Unix(),
		includeUsage: includeUsage, toolIndexes: make(map[int]int),
	}
}

func (w *chatFrontWriter) Header() http.Header { return w.header }

func (w *chatFrontWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.streaming = status == http.StatusOK && strings.HasPrefix(w.header.Get("Content-Type"), "text/event-stream")
	if !w.streaming {
		return
	}
	for key, values := range w.header {
		w.target.Header()[key] = values
	}
	w.target.WriteHeader(status)
}

func (w *chatFrontWriter) Flush() {
	if flusher, ok := w.target.(http.Flusher); ok && w.streaming {
		flusher.Flush()
	}
}

func (w *chatFrontWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !w.streaming {
		if int64(w.body.Len()+len(data)) > chatFrontMaxBodyBytes {
			return 0, fmt.Errorf("response exceeds the CCL Chat Completions limit")
		}
		return w.body.Write(data)
	}
	w.pending.Write(data)
	for {
		buffered := w.pending.Bytes()
		end := bytes.Index(buffered, []byte("\n\n"))
		if end < 0 {
			break
		}
		event := append([]byte(nil), buffered[:end]...)
		w.pending.Next(end + 2)
		if err := w.streamEvent(event); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

func (w *chatFrontWriter) chunk(delta map[string]any, finish any, usage map[string]any) error {
	payload := map[string]any{
		"id": w.id, "object": "chat.completion.chunk", "created": w.created, "model": w.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if delta == nil {
		payload["choices"] = []any{}
	}
	if usage != nil {
		payload["usage"] = usage
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.target, "data: %s\n\n", raw); err != nil {
		return err
	}
	w.Flush()
	return nil
}

func (w *chatFrontWriter) ensureRole() error {
	if w.sentRole {
		return nil
	}
	w.sentRole = true
	return w.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)
}

func (w *chatFrontWriter) streamEvent(raw []byte) error {
	var data []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(value, []byte(" "))...)
		}
	}
	if len(data) == 0 || w.done {
		return nil
	}
	var event map[string]any
	if decodeProtocolJSON(data, &event) != nil {
		return nil
	}
	kind, _ := event["type"].(string)
	switch kind {
	case "response.created":
		return w.ensureRole()
	case "response.output_text.delta":
		if err := w.ensureRole(); err != nil {
			return err
		}
		return w.chunk(map[string]any{"content": event["delta"]}, nil, nil)
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if err := w.ensureRole(); err != nil {
			return err
		}
		return w.chunk(map[string]any{"reasoning_content": event["delta"]}, nil, nil)
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		if item["type"] != "function_call" {
			return nil
		}
		if err := w.ensureRole(); err != nil {
			return err
		}
		index := len(w.toolIndexes)
		w.toolIndexes[chatFrontInt(event["output_index"])] = index
		w.sawToolCall = true
		return w.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "id": item["call_id"], "type": "function",
			"function": map[string]any{"name": item["name"], "arguments": ""},
		}}}, nil, nil)
	case "response.function_call_arguments.delta":
		index, ok := w.toolIndexes[chatFrontInt(event["output_index"])]
		if !ok {
			return nil
		}
		return w.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "function": map[string]any{"arguments": event["delta"]},
		}}}, nil, nil)
	case "response.completed", "response.incomplete":
		response, _ := event["response"].(map[string]any)
		if err := w.ensureRole(); err != nil {
			return err
		}
		if err := w.chunk(map[string]any{}, chatFrontFinishReason(response, w.sawToolCall), nil); err != nil {
			return err
		}
		if w.includeUsage {
			if err := w.chunk(nil, nil, chatFrontUsage(response["usage"])); err != nil {
				return err
			}
		}
		return w.end()
	case "response.failed", "error":
		failure, _ := event["error"].(map[string]any)
		if response, ok := event["response"].(map[string]any); ok {
			failure, _ = response["error"].(map[string]any)
		}
		message, _ := failure["message"].(string)
		if message == "" {
			message = "the provider reported a stream error"
		}
		errorType, _ := failure["code"].(string)
		if errorType == "" {
			errorType = "api_error"
		}
		raw, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": errorType, "code": nil, "param": nil}})
		if _, err := fmt.Fprintf(w.target, "data: %s\n\n", raw); err != nil {
			return err
		}
		return w.end()
	}
	return nil
}

func (w *chatFrontWriter) end() error {
	if w.done {
		return nil
	}
	w.done = true
	_, err := io.WriteString(w.target, "data: [DONE]\n\n")
	w.Flush()
	return err
}

// finish converts a buffered JSON body, or closes a stream the handler left
// without a terminal event.
func (w *chatFrontWriter) finish() {
	if w.streaming {
		_ = w.end()
		return
	}
	for key, values := range w.header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		w.target.Header()[key] = values
	}
	body := w.body.Bytes()
	if w.status == http.StatusOK {
		var response map[string]any
		if decodeProtocolJSON(body, &response) == nil && response["object"] == "response" {
			if converted, err := json.Marshal(chatFrontCompletion(response, w.id, w.created, w.model)); err == nil {
				body = converted
			}
		}
	}
	w.target.WriteHeader(w.status)
	_, _ = w.target.Write(body)
}

func chatFrontInt(value any) int {
	switch number := value.(type) {
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	case float64:
		return int(number)
	case int:
		return number
	default:
		return 0
	}
}

func chatFrontFinishReason(response map[string]any, sawToolCall bool) string {
	if response["status"] == "incomplete" {
		details, _ := response["incomplete_details"].(map[string]any)
		if details["reason"] == "content_filter" {
			return "content_filter"
		}
		return "length"
	}
	if sawToolCall {
		return "tool_calls"
	}
	return "stop"
}

func chatFrontUsage(value any) map[string]any {
	usage, _ := value.(map[string]any)
	input := chatFrontInt(usage["input_tokens"])
	output := chatFrontInt(usage["output_tokens"])
	result := map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		result["prompt_tokens_details"] = map[string]any{"cached_tokens": chatFrontInt(details["cached_tokens"])}
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		result["completion_tokens_details"] = map[string]any{"reasoning_tokens": chatFrontInt(details["reasoning_tokens"])}
	}
	return result
}

// chatFrontCompletion renders a finished Responses object as a chat.completion.
func chatFrontCompletion(response map[string]any, id string, created int64, model string) map[string]any {
	var text, reasoning strings.Builder
	var calls []any
	output, _ := response["output"].([]any)
	for _, rawItem := range output {
		item, _ := rawItem.(map[string]any)
		switch item["type"] {
		case "message":
			content, _ := item["content"].([]any)
			for _, rawPart := range content {
				part, _ := rawPart.(map[string]any)
				if value, _ := part["text"].(string); value != "" {
					text.WriteString(value)
				} else if value, _ := part["refusal"].(string); value != "" {
					text.WriteString(value)
				}
			}
		case "reasoning":
			summary, _ := item["summary"].([]any)
			for _, rawPart := range summary {
				part, _ := rawPart.(map[string]any)
				value, _ := part["text"].(string)
				reasoning.WriteString(value)
			}
		case "function_call":
			calls = append(calls, map[string]any{
				"id": item["call_id"], "type": "function",
				"function": map[string]any{"name": item["name"], "arguments": item["arguments"]},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
		if text.Len() == 0 {
			message["content"] = nil
		}
	}
	return map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": chatFrontFinishReason(response, len(calls) > 0),
		}},
		"usage": chatFrontUsage(response["usage"]),
	}
}
