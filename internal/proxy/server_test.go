package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/haiboyuwen/cc/internal/protocol"
	"github.com/haiboyuwen/cc/internal/provider"
	"github.com/haiboyuwen/cc/internal/proxy"
	"go.uber.org/zap"
)

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

	mockServer := &http.Server{
		Addr:    "127.0.0.1:4567",
		Handler: mux,
	}

	go func() {
		_ = mockServer.ListenAndServe()
	}()
	defer mockServer.Shutdown(context.Background())

	// Wait for mock server to spin up
	time.Sleep(100 * time.Millisecond)

	// 2. Initialize and start local CC Proxy Server
	p := provider.Provider{
		Name:     "mock-openai",
		Type:     "openai",
		Endpoint: "http://127.0.0.1:4567/v1",
		APIKey:   "mock-api-key",
		Model:    "gpt-4o",
	}

	logger, _ := zap.NewDevelopment()
	proxyServer := proxy.NewServer("127.0.0.1:3457", p, logger)
	if err := proxyServer.Start(); err != nil {
		t.Fatalf("Failed to start proxy: %v", err)
	}
	defer proxyServer.Stop()

	time.Sleep(100 * time.Millisecond)

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
	req, err := http.NewRequest("POST", "http://127.0.0.1:3457/v1/messages", bytes.NewBuffer(reqBody))
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

func TestProxyServerStreaming(t *testing.T) {
	// 1. Create a mock target endpoint server simulating OpenAI Streaming.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
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

	mockServer := &http.Server{
		Addr:    "127.0.0.1:4568",
		Handler: mux,
	}

	go func() {
		_ = mockServer.ListenAndServe()
	}()
	defer mockServer.Shutdown(context.Background())

	time.Sleep(100 * time.Millisecond)

	// 2. Initialize and start local CC Proxy Server
	p := provider.Provider{
		Name:     "mock-openai-stream",
		Type:     "openai",
		Endpoint: "http://127.0.0.1:4568/v1",
		APIKey:   "mock-key",
		Model:    "gpt-4-turbo",
	}

	logger, _ := zap.NewDevelopment()
	proxyServer := proxy.NewServer("127.0.0.1:3458", p, logger)
	if err := proxyServer.Start(); err != nil {
		t.Fatalf("Failed to start proxy: %v", err)
	}
	defer proxyServer.Stop()

	time.Sleep(100 * time.Millisecond)

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
	req, err := http.NewRequest("POST", "http://127.0.0.1:3458/v1/messages", bytes.NewBuffer(reqBody))
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
	var hasMessageStart, hasContentDelta, hasMessageStop bool
	for _, ev := range events {
		if strings.Contains(ev, `"type":"message_start"`) {
			hasMessageStart = true
		}
		if strings.Contains(ev, `"text":"Hello"`) || strings.Contains(ev, `"text":" world"`) {
			hasContentDelta = true
		}
		if strings.Contains(ev, `"type":"message_stop"`) {
			hasMessageStop = true
		}
	}

	if !hasMessageStart {
		t.Error("Missing message_start event in translated stream")
	}
	if !hasContentDelta {
		t.Error("Missing content_block_delta text tokens in translated stream")
	}
	if !hasMessageStop {
		t.Error("Missing message_stop event in translated stream")
	}
}
