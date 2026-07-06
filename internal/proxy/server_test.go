package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/proxy"
)

func startHTTPServer(t *testing.T, handler http.Handler) string {
	t.Helper()

	server := &http.Server{Handler: handler}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	go func() {
		_ = server.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
	})

	return ln.Addr().String()
}

func TestProxyServerUnary(t *testing.T) {
	// 1. Create a mock target endpoint server simulating OpenAI.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mock-api-key" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Read and decode OpenAI request
		var oaReq protocol.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&oaReq); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		if len(oaReq.Messages) < 1 {
			http.Error(w, "No messages", http.StatusBadRequest)
			return
		}

		// Simulating response
		oaResp := protocol.OpenAIResponse{
			ID:    "chatcmpl-mock",
			Model: oaReq.Model,
			Choices: []protocol.OpenAIChoice{
				{
					Index: 0,
					Message: protocol.OpenAIMessage{
						Role:    "assistant",
						Content: "Simulated OpenAI response for: " + oaReq.Messages[0].Content.(string),
					},
					FinishReason: "stop",
				},
			},
			Usage: protocol.OpenAIUsage{
				PromptTokens:     10,
				CompletionTokens: 15,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(oaResp)
	})

	upstreamAddr := startHTTPServer(t, mux)

	// 2. Initialize and start local CC Proxy Server
	p := provider.Provider{
		Name:     "mock-openai",
		Type:     "openai",
		Endpoint: "http://" + upstreamAddr + "/v1",
		APIKey:   "mock-api-key",
		Model:    "gpt-4o",
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxyServer := proxy.NewServer("127.0.0.1:0", p, logger)
	if err := proxyServer.Start(); err != nil {
		t.Fatalf("Failed to start proxy: %v", err)
	}
	defer proxyServer.Stop()

	// 3. Make client request using Anthropic structure to local proxy
	antReq := protocol.AnthropicRequest{
		Model: "claude-3-5-sonnet",
		Messages: []protocol.AnthropicMessage{
			{
				Role:    "user",
				Content: "Hello",
			},
		},
		Stream: false,
	}

	reqBody, _ := json.Marshal(antReq)
	req, err := http.NewRequest("POST", "http://"+proxyServer.Addr()+"/v1/messages", bytes.NewBuffer(reqBody))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to execute request to proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected OK, got %d. Body: %s", resp.StatusCode, string(body))
	}

	var antResp protocol.AnthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&antResp); err != nil {
		t.Fatalf("Failed to decode Anthropic response: %v", err)
	}

	if antResp.Model != "gpt-4o" {
		t.Errorf("Expected model 'gpt-4o', got '%s'", antResp.Model)
	}

	if len(antResp.Content) != 1 || antResp.Content[0].Text != "Simulated OpenAI response for: Hello" {
		t.Errorf("Unexpected response content: %+v", antResp.Content)
	}
}

func TestProxyServerResponsesUnary(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mock-api-key" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		var req protocol.OpenAIResponsesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if req.Model != "gpt-5" {
			http.Error(w, "model mismatch", http.StatusBadRequest)
			return
		}
		if req.Store == nil || *req.Store {
			http.Error(w, "expected store=false", http.StatusBadRequest)
			return
		}
		if len(req.Input) != 1 || req.Input[0].Type != "message" {
			http.Error(w, "input mismatch", http.StatusBadRequest)
			return
		}

		resp := protocol.OpenAIResponsesResponse{
			ID:     "resp_mock",
			Model:  req.Model,
			Status: "completed",
			Output: []protocol.ResponsesOutputItem{{
				Type: "message",
				Content: []protocol.ResponsesOutputPart{
					{Type: "output_text", Text: "Responses hello"},
				},
			}},
			Usage: protocol.OpenAIResponsesUsage{InputTokens: 5, OutputTokens: 6},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	upstreamAddr := startHTTPServer(t, mux)
	p := provider.Provider{
		Name:     "mock-openai-responses",
		Type:     "openai_responses",
		Endpoint: "http://" + upstreamAddr + "/v1",
		APIKey:   "mock-api-key",
		Model:    "gpt-5",
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxyServer := proxy.NewServer("127.0.0.1:0", p, logger)
	if err := proxyServer.Start(); err != nil {
		t.Fatalf("Failed to start proxy: %v", err)
	}
	defer proxyServer.Stop()

	antReq := protocol.AnthropicRequest{
		Model: "claude-3-5-sonnet",
		Messages: []protocol.AnthropicMessage{{
			Role:    "user",
			Content: "Hello",
		}},
	}

	reqBody, _ := json.Marshal(antReq)
	resp, err := http.Post("http://"+proxyServer.Addr()+"/v1/messages", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		t.Fatalf("Failed to execute request to proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected OK, got %d. Body: %s", resp.StatusCode, string(body))
	}

	var antResp protocol.AnthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&antResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if antResp.Model != "gpt-5" || len(antResp.Content) != 1 || antResp.Content[0].Text != "Responses hello" {
		t.Fatalf("unexpected response: %+v", antResp)
	}
	if antResp.Usage.InputTokens != 5 || antResp.Usage.OutputTokens != 6 {
		t.Fatalf("usage mismatch: %+v", antResp.Usage)
	}
}

func TestProxyServerModelsUsesDiscoveredModels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat"},{"id":"gpt-4o"}]}`))
	})

	upstreamAddr := startHTTPServer(t, mux)

	p := provider.Provider{
		Name:     "dynamic-models",
		Type:     "openai",
		Endpoint: "http://" + upstreamAddr + "/v1",
		APIKey:   "mock-key",
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxyServer := proxy.NewServer("127.0.0.1:0", p, logger)
	if err := proxyServer.Start(); err != nil {
		t.Fatalf("Failed to start proxy: %v", err)
	}
	defer proxyServer.Stop()

	resp, err := http.Get("http://" + proxyServer.Addr() + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	defer resp.Body.Close()

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode models response: %v", err)
	}

	got := make(map[string]bool)
	for _, model := range payload.Data {
		got[model.ID] = true
	}
	if !got["claude-ds-chat"] || !got["claude-gp-4o"] {
		t.Fatalf("expected discovered model aliases, got: %+v", payload.Data)
	}
}

func TestProxyServerResponsesStreaming(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		chunks := []string{
			`event: response.created
data: {"id":"resp_stream","model":"gpt-5","status":"in_progress"}`,
			`event: response.output_item.added
data: {"output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`,
			`event: response.output_text.delta
data: {"output_index":0,"content_index":0,"delta":"Hello"}`,
			`event: response.output_text.delta
data: {"output_index":0,"content_index":0,"delta":" world"}`,
			`event: response.completed
data: {"response":{"id":"resp_stream","model":"gpt-5","status":"completed","usage":{"input_tokens":7,"output_tokens":8}}}`,
			`data: [DONE]`,
		}

		for _, chunk := range chunks {
			_, _ = fmt.Fprint(w, chunk+"\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	upstreamAddr := startHTTPServer(t, mux)
	p := provider.Provider{
		Name:     "mock-openai-responses-stream",
		Type:     "openai_responses",
		Endpoint: "http://" + upstreamAddr + "/v1",
		APIKey:   "mock-key",
		Model:    "gpt-5",
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxyServer := proxy.NewServer("127.0.0.1:0", p, logger)
	if err := proxyServer.Start(); err != nil {
		t.Fatalf("Failed to start proxy: %v", err)
	}
	defer proxyServer.Stop()

	antReq := protocol.AnthropicRequest{
		Model: "claude-3-5-sonnet",
		Messages: []protocol.AnthropicMessage{{
			Role:    "user",
			Content: "Hello",
		}},
		Stream: true,
	}
	reqBody, _ := json.Marshal(antReq)
	req, err := http.NewRequest("POST", "http://"+proxyServer.Addr()+"/v1/messages", bytes.NewBuffer(reqBody))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("request proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected OK, got %d. Body: %s", resp.StatusCode, string(body))
	}

	var events []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if text := scanner.Text(); text != "" {
			events = append(events, text)
		}
	}

	var hasStart, hasText, hasUsage bool
	messageStopCount := 0
	for _, ev := range events {
		if strings.Contains(ev, `"type":"message_start"`) {
			hasStart = true
		}
		if strings.Contains(ev, `"text":"Hello"`) || strings.Contains(ev, `"text":" world"`) {
			hasText = true
		}
		if strings.Contains(ev, `"input_tokens":7`) && strings.Contains(ev, `"output_tokens":8`) {
			hasUsage = true
		}
		if strings.Contains(ev, `"type":"message_stop"`) {
			messageStopCount++
		}
	}
	if !hasStart || !hasText || !hasUsage {
		t.Fatalf("missing expected events: start=%v text=%v usage=%v events=%v", hasStart, hasText, hasUsage, events)
	}
	if messageStopCount != 1 {
		t.Fatalf("expected exactly one message_stop, got %d events=%v", messageStopCount, events)
	}
}

func TestProxyServerStreaming(t *testing.T) {
	// 1. Create a mock target endpoint server simulating OpenAI Streaming.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
			http.Error(w, "missing include_usage", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		chunks := []string{
			`data: {"id":"chatcmpl-stream","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-stream","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-stream","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-stream","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}

		for _, chunk := range chunks {
			_, _ = fmt.Fprint(w, chunk+"\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	upstreamAddr := startHTTPServer(t, mux)

	// 2. Initialize and start local CC Proxy Server
	p := provider.Provider{
		Name:     "mock-openai-stream",
		Type:     "openai",
		Endpoint: "http://" + upstreamAddr + "/v1",
		APIKey:   "mock-key",
		Model:    "gpt-4-turbo",
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxyServer := proxy.NewServer("127.0.0.1:0", p, logger)
	if err := proxyServer.Start(); err != nil {
		t.Fatalf("Failed to start proxy: %v", err)
	}
	defer proxyServer.Stop()

	// 3. Make client request using Anthropic structure with stream: true
	antReq := protocol.AnthropicRequest{
		Model: "claude-3-5-sonnet",
		Messages: []protocol.AnthropicMessage{
			{
				Role:    "user",
				Content: "Hello",
			},
		},
		Stream: true,
	}

	reqBody, _ := json.Marshal(antReq)
	req, err := http.NewRequest("POST", "http://"+proxyServer.Addr()+"/v1/messages", bytes.NewBuffer(reqBody))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to execute request to proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected OK, got %d", resp.StatusCode)
	}

	var events []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		text := scanner.Text()
		if text != "" {
			events = append(events, text)
		}
	}

	// We expect events to contain message_start, content_block_start, content_block_delta etc.
	var hasMessageStart, hasContentDelta bool
	messageStopCount := 0
	for _, ev := range events {
		if strings.Contains(ev, `"type":"message_start"`) {
			hasMessageStart = true
		}
		if strings.Contains(ev, `"text":"Hello"`) || strings.Contains(ev, `"text":" world"`) {
			hasContentDelta = true
		}
		if strings.Contains(ev, `"type":"message_stop"`) {
			messageStopCount++
		}
	}

	if !hasMessageStart {
		t.Error("Missing message_start event in translated stream")
	}
	if !hasContentDelta {
		t.Error("Missing content_block_delta text tokens in translated stream")
	}
	if messageStopCount != 1 {
		t.Errorf("Expected exactly one message_stop event, got %d", messageStopCount)
	}
}

func TestStreamTransformerMultipleToolCallsByIndexAndDelayedUsage(t *testing.T) {
	st := &proxy.StreamTransformer{}
	chunks := []string{
		`data: {"id":"chatcmpl-tools","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"first"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-tools","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_1","type":"function","function":{"name":"second"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-tools","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"b\":2}"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-tools","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-tools","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":22}}`,
		`data: [DONE]`,
	}

	var events []string
	for _, chunk := range chunks {
		translated, err := st.TranslateChunk(chunk)
		if err != nil {
			t.Fatalf("TranslateChunk failed: %v", err)
		}
		formatted := proxy.FormatEvents(translated)
		if formatted != "" {
			events = append(events, formatted)
		}
	}
	merged := strings.Join(events, "")

	if !strings.Contains(merged, `"id":"call_0"`) || !strings.Contains(merged, `"id":"call_1"`) {
		t.Fatalf("tool calls not started separately: %s", merged)
	}
	if !strings.Contains(merged, `"partial_json":"{\"b\":2}"`) || !strings.Contains(merged, `"partial_json":"{\"a\":1}"`) {
		t.Fatalf("tool argument deltas missing: %s", merged)
	}
	if strings.Count(merged, `"type":"message_delta"`) != 1 || strings.Count(merged, `"type":"message_stop"`) != 1 {
		t.Fatalf("expected one message_delta and one message_stop: %s", merged)
	}
	if !strings.Contains(merged, `"stop_reason":"tool_use"`) || !strings.Contains(merged, `"input_tokens":11`) || !strings.Contains(merged, `"output_tokens":22`) {
		t.Fatalf("delayed message_delta did not include final stop/usage: %s", merged)
	}
}

func TestProxyServerStreamingReasoning(t *testing.T) {
	// 1. Create a mock target endpoint server simulating OpenAI Streaming with reasoning_content.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		chunks := []string{
			`data: {"id":"chatcmpl-stream-reasoning","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-stream-reasoning","choices":[{"index":0,"delta":{"reasoning_content":"Let me "},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-stream-reasoning","choices":[{"index":0,"delta":{"reasoning_content":"think"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-stream-reasoning","choices":[{"index":0,"delta":{"content":"Hello!"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-stream-reasoning","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}

		for _, chunk := range chunks {
			_, _ = fmt.Fprint(w, chunk+"\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	upstreamAddr := startHTTPServer(t, mux)

	// 2. Initialize and start local CC Proxy Server
	p := provider.Provider{
		Name:     "mock-openai-stream-reasoning",
		Type:     "openai",
		Endpoint: "http://" + upstreamAddr + "/v1",
		APIKey:   "mock-key",
		Model:    "deepseek-reasoner",
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxyServer := proxy.NewServer("127.0.0.1:0", p, logger)
	if err := proxyServer.Start(); err != nil {
		t.Fatalf("Failed to start proxy: %v", err)
	}
	defer proxyServer.Stop()

	// 3. Make client request using Anthropic structure with stream: true
	antReq := protocol.AnthropicRequest{
		Model: "claude-3-5-sonnet",
		Messages: []protocol.AnthropicMessage{
			{
				Role:    "user",
				Content: "Hello",
			},
		},
		Stream: true,
	}

	reqBody, _ := json.Marshal(antReq)
	req, err := http.NewRequest("POST", "http://"+proxyServer.Addr()+"/v1/messages", bytes.NewBuffer(reqBody))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to execute request to proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected OK, got %d", resp.StatusCode)
	}

	var events []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		text := scanner.Text()
		if text != "" {
			events = append(events, text)
		}
	}

	// We expect events to contain message_start, content_block_start (thinking), content_block_delta (thinking_delta) etc.
	var hasMessageStart, hasThinkingDelta, hasContentDelta bool
	messageStopCount := 0
	for _, ev := range events {
		if strings.Contains(ev, `"type":"message_start"`) {
			hasMessageStart = true
		}
		if strings.Contains(ev, `"thinking_delta"`) && (strings.Contains(ev, `"thinking":"Let me "`) || strings.Contains(ev, `"thinking":"think"`)) {
			hasThinkingDelta = true
		}
		if strings.Contains(ev, `"text_delta"`) && strings.Contains(ev, `"text":"Hello!"`) {
			hasContentDelta = true
		}
		if strings.Contains(ev, `"type":"message_stop"`) {
			messageStopCount++
		}
	}

	if !hasMessageStart {
		t.Error("Missing message_start event in translated stream")
	}
	if !hasThinkingDelta {
		t.Error("Missing content_block_delta thinking tokens in translated stream")
	}
	if !hasContentDelta {
		t.Error("Missing content_block_delta text tokens in translated stream")
	}
	if messageStopCount != 1 {
		t.Errorf("Expected exactly one message_stop event, got %d", messageStopCount)
	}
}
