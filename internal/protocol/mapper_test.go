package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/haiboyuwen/claude-code-launch/internal/protocol"
)

func TestConvertRequest(t *testing.T) {
	antReq := &protocol.AnthropicRequest{
		Model: "claude-3-5-sonnet",
		System: "You are a helpful assistant.",
		Messages: []protocol.AnthropicMessage{
			{
				Role:    "user",
				Content: "Hello world",
			},
		},
		MaxTokens: 1000,
	}

	oaReq, err := protocol.ConvertRequest(antReq)
	if err != nil {
		t.Fatalf("ConvertRequest failed: %v", err)
	}

	if oaReq.Model != "claude-3-5-sonnet" {
		t.Errorf("Expected model 'claude-3-5-sonnet', got '%s'", oaReq.Model)
	}

	if len(oaReq.Messages) != 2 {
		t.Fatalf("Expected 2 messages (system + user), got %d", len(oaReq.Messages))
	}

	if oaReq.Messages[0].Role != "system" || oaReq.Messages[0].Content != "You are a helpful assistant." {
		t.Errorf("System message not mapped correctly")
	}

	if oaReq.Messages[1].Role != "user" || oaReq.Messages[1].Content != "Hello world" {
		t.Errorf("User message not mapped correctly")
	}
}

func TestMapModel(t *testing.T) {
	// Case 1: Configuration is explicit, so bypass mapping entirely.
	if m := protocol.MapModel("claude-3-5-sonnet", "my-custom-model", nil); m != "my-custom-model" {
		t.Errorf("Expected configuration override 'my-custom-model', got '%s'", m)
	}

	// Case 1b: Configuration is comma-separated pool, should select deepseek-v4-pro for Sonnet.
	poolConfig := "bailian-glm-5.1,gemini-3.5-flash,qwen3.6-plus,deepseek-v4-flash,deepseek-v4-pro"
	if m := protocol.MapModel("claude-3-5-sonnet", poolConfig, nil); m != "deepseek-v4-pro" {
		t.Errorf("Expected pool selection 'deepseek-v4-pro' for Sonnet, got '%s'", m)
	}

	// Case 1c: Configuration is comma-separated pool, should select deepseek-v4-pro for Opus.
	if m := protocol.MapModel("claude-3-opus", poolConfig, nil); m != "deepseek-v4-pro" {
		t.Errorf("Expected pool selection 'deepseek-v4-pro' for Opus, got '%s'", m)
	}

	// Case 1d: Configuration is comma-separated pool, should select deepseek-v4-flash for Haiku.
	if m := protocol.MapModel("claude-3-5-haiku", poolConfig, nil); m != "deepseek-v4-flash" {
		t.Errorf("Expected pool selection 'deepseek-v4-flash' for Haiku, got '%s'", m)
	}

	// Case 2: Opus model tier requested, deepseek-reasoner available.
	modelsList := []string{"gpt-4o-mini", "deepseek-chat", "deepseek-reasoner", "gpt-4o"}
	if m := protocol.MapModel("claude-3-opus-20240229", "", modelsList); m != "deepseek-reasoner" {
		t.Errorf("Expected Opus mapping 'deepseek-reasoner', got '%s'", m)
	}

	// Case 3: Opus model tier requested (latest Opus 4.8), o1 available.
	modelsList2 := []string{"gpt-4o-mini", "o1", "gpt-4o"}
	if m := protocol.MapModel("claude-4-opus-2026", "", modelsList2); m != "o1" {
		t.Errorf("Expected Opus mapping 'o1', got '%s'", m)
	}

	// Case 4: Sonnet model tier requested, deepseek-chat available.
	if m := protocol.MapModel("claude-3-5-sonnet-latest", "", modelsList); m != "deepseek-chat" {
		t.Errorf("Expected Sonnet mapping 'deepseek-chat', got '%s'", m)
	}

	// Case 5: Haiku model tier requested, gpt-4o-mini available.
	modelsListHaiku := []string{"gpt-4o-mini", "deepseek-reasoner", "gpt-4o"}
	if m := protocol.MapModel("claude-3-5-haiku-20241022", "", modelsListHaiku); m != "gpt-4o-mini" {
		t.Errorf("Expected Haiku mapping 'gpt-4o-mini', got '%s'", m)
	}

	// Case 6: Sonnet tier with custom non-standard models (like your gateway).
	customModels := []string{"deepseek-v4-flash", "deepseek-v4-pro", "qwen3.6-plus", "bailian-glm-5.1", "gemini-3.5-flash"}
	if m := protocol.MapModel("claude-3-5-sonnet-20241022", "", customModels); m != "deepseek-v4-pro" {
		t.Errorf("Expected custom Sonnet mapping 'deepseek-v4-pro', got '%s'", m)
	}

	// Case 7: Haiku tier with custom non-standard models (like your gateway).
	if m := protocol.MapModel("claude-3-5-haiku-20241022", "", customModels); m != "deepseek-v4-flash" {
		t.Errorf("Expected custom Haiku mapping 'deepseek-v4-flash', got '%s'", m)
	}
}

func TestConvertResponse(t *testing.T) {
	oaResp := &protocol.OpenAIResponse{
		ID:    "chatcmpl-123",
		Model: "gpt-4o",
		Choices: []protocol.OpenAIChoice{
			{
				Index: 0,
				Message: protocol.OpenAIMessage{
					Role:    "assistant",
					Content: "Hello human",
				},
				FinishReason: "stop",
			},
		},
		Usage: protocol.OpenAIUsage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}

	antResp, err := protocol.ConvertResponse(oaResp)
	if err != nil {
		t.Fatalf("ConvertResponse failed: %v", err)
	}

	if antResp.ID != "chatcmpl-123" {
		t.Errorf("ID mismatch: expected chatcmpl-123, got %s", antResp.ID)
	}

	if antResp.StopReason != "end_turn" {
		t.Errorf("StopReason mismatch: expected end_turn, got %s", antResp.StopReason)
	}

	if len(antResp.Content) != 1 || antResp.Content[0].Type != "text" || antResp.Content[0].Text != "Hello human" {
		t.Errorf("Content block mismatch")
	}

	if antResp.Usage.InputTokens != 10 || antResp.Usage.OutputTokens != 20 {
		t.Errorf("Usage metrics mismatch")
	}
}

func TestConvertToolCall(t *testing.T) {
	inputSchema := []byte(`{"type":"object","properties":{"location":{"type":"string"}}}`)
	antReq := &protocol.AnthropicRequest{
		Model: "claude-3-5-sonnet",
		Messages: []protocol.AnthropicMessage{
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type": "text",
						"text": "What is the weather in Tokyo?",
					},
				},
			},
		},
		Tools: []protocol.AnthropicTool{
			{
				Name:        "get_weather",
				Description: "Get local weather",
				InputSchema: inputSchema,
			},
		},
	}

	oaReq, err := protocol.ConvertRequest(antReq)
	if err != nil {
		t.Fatalf("ConvertRequest failed: %v", err)
	}

	if len(oaReq.Tools) != 1 {
		t.Fatalf("Expected 1 tool, got %d", len(oaReq.Tools))
	}

	tool := oaReq.Tools[0]
	if tool.Type != "function" || tool.Function.Name != "get_weather" || tool.Function.Description != "Get local weather" {
		t.Errorf("Tool definition conversion mismatch: %+v", tool)
	}

	var schema map[string]any
	if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
		t.Fatalf("Failed to parse tool parameters: %v", err)
	}

	if schema["type"] != "object" {
		t.Errorf("Schema type mismatch")
	}
}
