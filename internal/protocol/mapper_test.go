package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/claude-code-launch/ccl/internal/protocol"
)

func TestConvertRequest(t *testing.T) {
	antReq := &protocol.AnthropicRequest{
		Model:  "claude-3-5-sonnet",
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

func TestConvertRequestStripsLeadingBillingHeaderAndIncludesStreamUsage(t *testing.T) {
	antReq := &protocol.AnthropicRequest{
		Model:     "claude-3-5-sonnet",
		System:    "x-anthropic-billing-header: cch=abc;\n\nYou are useful.",
		Stream:    true,
		MaxTokens: 10,
		Messages: []protocol.AnthropicMessage{{
			Role:    "user",
			Content: "Hello",
		}},
	}

	oaReq, err := protocol.ConvertRequest(antReq)
	if err != nil {
		t.Fatalf("ConvertRequest failed: %v", err)
	}
	if oaReq.Messages[0].Content != "You are useful." {
		t.Fatalf("billing header was not stripped: %+v", oaReq.Messages[0].Content)
	}
	if oaReq.StreamOptions == nil || !oaReq.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options.include_usage not injected: %+v", oaReq.StreamOptions)
	}
}

func TestMapModel(t *testing.T) {
	// Case 1: Configuration is explicit, so bypass mapping entirely.
	if m := protocol.MapModel("claude-3-5-sonnet", "my-custom-model", nil); m != "my-custom-model" {
		t.Errorf("Expected configuration override 'my-custom-model', got '%s'", m)
	}

	// Case 1b: Configuration is comma-separated pool, should select qwen3.6-plus for Sonnet.
	// qwen3.6-plus is a known sonnet-tier model in the registry with more capabilities
	// than heuristic-only matches. deepseek-v4-pro is classified as opus by heuristic ("pro").
	poolConfig := "bailian-glm-5.1,gemini-3.5-flash,qwen3.6-plus,deepseek-v4-flash,deepseek-v4-pro"
	if m := protocol.MapModel("claude-3-5-sonnet", poolConfig, nil); m != "qwen3.6-plus" {
		t.Errorf("Expected pool selection 'qwen3.6-plus' for Sonnet, got '%s'", m)
	}

	// Case 1c: Configuration is comma-separated pool, should select deepseek-v4-pro for Opus.
	// deepseek-v4-pro is not in the registry but heuristic classifies it as opus ("pro" keyword).
	if m := protocol.MapModel("claude-3-opus", poolConfig, nil); m != "deepseek-v4-pro" {
		t.Errorf("Expected pool selection 'deepseek-v4-pro' for Opus, got '%s'", m)
	}

	// Case 1d: Configuration is comma-separated pool, should select deepseek-v4-flash for Haiku.
	// Both gemini-3.5-flash and deepseek-v4-flash are heuristic haiku matches ("flash"),
	// but deepseek-v4-flash appears later in the pool. The first heuristic match is gemini-3.5-flash.
	// However, since no registry haiku models exist in the pool, the first heuristic match wins.
	if m := protocol.MapModel("claude-3-5-haiku", poolConfig, nil); m != "gemini-3.5-flash" {
		t.Errorf("Expected pool selection 'gemini-3.5-flash' for Haiku, got '%s'", m)
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

	// Case 4: Sonnet model tier requested, gpt-4o available.
	// gpt-4o is a known sonnet-tier model with 4 capabilities (tool_use, streaming, vision, structured_output),
	// while deepseek-chat has only 2 capabilities. The registry prefers models with more capabilities.
	modelsList3 := []string{"gpt-4o-mini", "deepseek-chat", "deepseek-reasoner", "gpt-4o"}
	if m := protocol.MapModel("claude-3-5-sonnet-latest", "", modelsList3); m != "gpt-4o" {
		t.Errorf("Expected Sonnet mapping 'gpt-4o', got '%s'", m)
	}

	// Case 5: Haiku model tier requested, gpt-4o-mini available.
	modelsListHaiku := []string{"gpt-4o-mini", "deepseek-reasoner", "gpt-4o"}
	if m := protocol.MapModel("claude-3-5-haiku-20241022", "", modelsListHaiku); m != "gpt-4o-mini" {
		t.Errorf("Expected Haiku mapping 'gpt-4o-mini', got '%s'", m)
	}

	// Case 6: Sonnet tier with custom non-standard models (like your gateway).
	// qwen3.6-plus is a known sonnet-tier model in the registry, so it's preferred over
	// heuristic-only matches like deepseek-v4-pro (which heuristic classifies as opus anyway).
	customModels := []string{"deepseek-v4-flash", "deepseek-v4-pro", "qwen3.6-plus", "bailian-glm-5.1", "gemini-3.5-flash"}
	if m := protocol.MapModel("claude-3-5-sonnet-20241022", "", customModels); m != "qwen3.6-plus" {
		t.Errorf("Expected custom Sonnet mapping 'qwen3.6-plus', got '%s'", m)
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

func TestConvertRequestStopToolChoiceAndMixedToolUse(t *testing.T) {
	antReq := &protocol.AnthropicRequest{
		Model:         "claude-3-5-sonnet",
		StopSequences: []string{"END"},
		ToolChoice:    &protocol.AnthropicToolChoice{Type: "none"},
		Messages: []protocol.AnthropicMessage{
			{
				Role: "assistant",
				Content: []any{
					map[string]any{
						"type": "text",
						"text": "I will call a tool.",
					},
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_1",
						"name":  "get_weather",
						"input": map[string]any{"location": "Tokyo"},
					},
				},
			},
		},
	}

	oaReq, err := protocol.ConvertRequest(antReq)
	if err != nil {
		t.Fatalf("ConvertRequest failed: %v", err)
	}

	if len(oaReq.Stop) != 1 || oaReq.Stop[0] != "END" {
		t.Fatalf("stop sequences not mapped: %+v", oaReq.Stop)
	}
	if oaReq.ToolChoice != "none" {
		t.Fatalf("tool_choice none not mapped: %+v", oaReq.ToolChoice)
	}
	if len(oaReq.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(oaReq.Messages))
	}
	msg := oaReq.Messages[0]
	if msg.Content != "I will call a tool." {
		t.Fatalf("assistant text content was not preserved: %+v", msg.Content)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool_use not mapped: %+v", msg.ToolCalls)
	}
}

func TestConvertRequestImageBlock(t *testing.T) {
	antReq := &protocol.AnthropicRequest{
		Model: "claude-3-5-sonnet",
		Messages: []protocol.AnthropicMessage{
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       "abc123",
						},
					},
				},
			},
		},
	}

	oaReq, err := protocol.ConvertRequest(antReq)
	if err != nil {
		t.Fatalf("ConvertRequest failed: %v", err)
	}
	if len(oaReq.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(oaReq.Messages))
	}
	parts, ok := oaReq.Messages[0].Content.([]protocol.OpenAIMessagePart)
	if !ok {
		t.Fatalf("expected OpenAI content parts, got %T", oaReq.Messages[0].Content)
	}
	if len(parts) != 1 || parts[0].Type != "image_url" || parts[0].ImageURL == nil ||
		parts[0].ImageURL.URL != "data:image/png;base64,abc123" {
		t.Fatalf("image block not mapped correctly: %+v", parts)
	}
}

func TestConvertResponseInvalidToolArguments(t *testing.T) {
	oaResp := &protocol.OpenAIResponse{
		ID:    "chatcmpl-tool",
		Model: "gpt-4o",
		Choices: []protocol.OpenAIChoice{
			{
				Message: protocol.OpenAIMessage{
					ToolCalls: []protocol.OpenAIToolCall{
						{
							ID:   "call_1",
							Type: "function",
							Function: protocol.OpenAIFunctionCall{
								Name:      "bad_args",
								Arguments: "{not-json",
							},
						},
					},
				},
				FinishReason: "tool_calls",
			},
		},
	}

	antResp, err := protocol.ConvertResponse(oaResp)
	if err != nil {
		t.Fatalf("ConvertResponse failed: %v", err)
	}
	if len(antResp.Content) != 1 || string(antResp.Content[0].Input) != "{}" {
		t.Fatalf("invalid tool arguments should map to empty object: %+v", antResp.Content)
	}
}

func TestConvertResponseContentArrayRefusalAndFunctionCall(t *testing.T) {
	oaResp := &protocol.OpenAIResponse{
		ID:    "chatcmpl-mixed",
		Model: "gpt-4o",
		Choices: []protocol.OpenAIChoice{{
			Message: protocol.OpenAIMessage{
				Content: []any{
					map[string]any{"type": "output_text", "text": "Hello"},
					map[string]any{"type": "refusal", "refusal": "No"},
				},
				Refusal: "Top-level refusal",
				FunctionCall: &protocol.OpenAIFunctionCall{
					Name:      "legacy_tool",
					Arguments: `{"ok":true}`,
				},
			},
			FinishReason: "function_call",
		}},
	}

	antResp, err := protocol.ConvertResponse(oaResp)
	if err != nil {
		t.Fatalf("ConvertResponse failed: %v", err)
	}
	if antResp.StopReason != "tool_use" {
		t.Fatalf("expected tool_use stop reason, got %q", antResp.StopReason)
	}
	if len(antResp.Content) != 4 {
		t.Fatalf("expected 4 content blocks, got %+v", antResp.Content)
	}
	if antResp.Content[0].Text != "Hello" || antResp.Content[1].Text != "No" || antResp.Content[2].Text != "Top-level refusal" {
		t.Fatalf("text/refusal blocks not mapped: %+v", antResp.Content)
	}
	if antResp.Content[3].Type != "tool_use" || antResp.Content[3].Name != "legacy_tool" || string(antResp.Content[3].Input) != `{"ok":true}` {
		t.Fatalf("legacy function_call not mapped: %+v", antResp.Content[3])
	}
}

func TestConvertRequestToResponses(t *testing.T) {
	antReq := &protocol.AnthropicRequest{
		Model:     "claude-3-5-sonnet",
		System:    "You are useful.",
		MaxTokens: 128,
		Stream:    true,
		Messages: []protocol.AnthropicMessage{
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": "text", "text": "Look"},
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       "abc",
						},
					},
				},
			},
			{
				Role: "assistant",
				Content: []any{
					map[string]any{"type": "text", "text": "Calling tool"},
					map[string]any{
						"type":  "tool_use",
						"id":    "call_1",
						"name":  "read_file",
						"input": map[string]any{"path": "README.md"},
					},
				},
			},
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "call_1",
						"content":     "ok",
					},
				},
			},
		},
		Tools: []protocol.AnthropicTool{{
			Name:        "read_file",
			Description: "Read a file",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
		ToolChoice: &protocol.AnthropicToolChoice{Type: "tool", Name: "read_file"},
	}

	respReq, err := protocol.ConvertRequestToResponses(antReq)
	if err != nil {
		t.Fatalf("ConvertRequestToResponses failed: %v", err)
	}
	if respReq.Instructions != "You are useful." || respReq.MaxOutputTokens != 128 || !respReq.Stream {
		t.Fatalf("basic fields not mapped: %+v", respReq)
	}
	if respReq.Store == nil || *respReq.Store {
		t.Fatalf("Responses proxy should default store=false: %+v", respReq.Store)
	}
	if len(respReq.Input) != 4 {
		t.Fatalf("expected 4 input items, got %+v", respReq.Input)
	}
	if respReq.Input[1].Type != "message" || respReq.Input[1].Role != "assistant" {
		t.Fatalf("assistant text should remain a message before function call: %+v", respReq.Input[1])
	}
	// Assistant history must use output_text (OpenAI rejects input_text on assistant).
	parts, ok := respReq.Input[1].Content.([]protocol.ResponsesContentPart)
	if !ok || len(parts) != 1 || parts[0].Type != "output_text" {
		t.Fatalf("assistant text part type should be output_text: %+v", respReq.Input[1].Content)
	}
	userParts, ok := respReq.Input[0].Content.([]protocol.ResponsesContentPart)
	if !ok || len(userParts) < 1 || userParts[0].Type != "input_text" {
		t.Fatalf("user text part type should be input_text: %+v", respReq.Input[0].Content)
	}
	if respReq.Input[2].Type != "function_call" || respReq.Input[2].CallID != "call_1" || respReq.Input[2].Name != "read_file" {
		t.Fatalf("tool_use not mapped to function_call: %+v", respReq.Input[2])
	}
	if respReq.Input[2].Status != "completed" {
		t.Fatalf("function_call replay should set status=completed: %+v", respReq.Input[2])
	}
	if respReq.Input[3].Type != "function_call_output" || respReq.Input[3].Output != "ok" {
		t.Fatalf("tool_result not mapped to function_call_output: %+v", respReq.Input[3])
	}
	if len(respReq.Tools) != 1 || respReq.Tools[0].Type != "function" || respReq.Tools[0].Name != "read_file" {
		t.Fatalf("tools not mapped: %+v", respReq.Tools)
	}
	choice, ok := respReq.ToolChoice.(map[string]string)
	if !ok || choice["type"] != "function" || choice["name"] != "read_file" {
		t.Fatalf("tool_choice not mapped: %+v", respReq.ToolChoice)
	}
}

func TestConvertRequestToResponsesDropsThinkingAndNormalizesArgs(t *testing.T) {
	antReq := &protocol.AnthropicRequest{
		Model: "claude-3-5-sonnet",
		Messages: []protocol.AnthropicMessage{
			{
				Role: "assistant",
				Content: []any{
					map[string]any{"type": "thinking", "thinking": "secret chain of thought"},
					map[string]any{"type": "text", "text": "done"},
					map[string]any{
						"type":  "tool_use",
						"id":    "call_x",
						"name":  "run",
						"input": "not-json",
					},
				},
			},
		},
	}

	respReq, err := protocol.ConvertRequestToResponses(antReq)
	if err != nil {
		t.Fatalf("ConvertRequestToResponses failed: %v", err)
	}
	if len(respReq.Input) != 2 {
		t.Fatalf("expected message + function_call (thinking dropped), got %+v", respReq.Input)
	}
	parts, ok := respReq.Input[0].Content.([]protocol.ResponsesContentPart)
	if !ok || len(parts) != 1 || parts[0].Type != "output_text" || parts[0].Text != "done" {
		t.Fatalf("assistant text mapping wrong: %+v", respReq.Input[0])
	}
	if respReq.Input[1].Type != "function_call" || !json.Valid([]byte(respReq.Input[1].Arguments)) {
		t.Fatalf("function args should be valid JSON: %+v", respReq.Input[1])
	}
}

func TestConvertResponsesResponse(t *testing.T) {
	resp := &protocol.OpenAIResponsesResponse{
		ID:     "resp_123",
		Model:  "gpt-5",
		Status: "completed",
		Output: []protocol.ResponsesOutputItem{
			{
				Type: "message",
				Content: []protocol.ResponsesOutputPart{
					{Type: "output_text", Text: "Hello"},
				},
			},
			{
				Type:      "function_call",
				CallID:    "call_1",
				Name:      "read_file",
				Arguments: `{"path":"README.md"}`,
			},
		},
		Usage: protocol.OpenAIResponsesUsage{InputTokens: 3, OutputTokens: 4},
	}

	antResp, err := protocol.ConvertResponsesResponse(resp)
	if err != nil {
		t.Fatalf("ConvertResponsesResponse failed: %v", err)
	}
	if antResp.StopReason != "tool_use" {
		t.Fatalf("expected tool_use stop reason, got %q", antResp.StopReason)
	}
	if len(antResp.Content) != 2 || antResp.Content[0].Text != "Hello" || antResp.Content[1].Name != "read_file" {
		t.Fatalf("content not mapped: %+v", antResp.Content)
	}
	if antResp.Usage.InputTokens != 3 || antResp.Usage.OutputTokens != 4 {
		t.Fatalf("usage not mapped: %+v", antResp.Usage)
	}
}

func TestReasoningContentMapping(t *testing.T) {
	// 1. Test request conversion mapping "thinking" -> "reasoning_content"
	antReq := &protocol.AnthropicRequest{
		Model: "claude-3-5-sonnet",
		Messages: []protocol.AnthropicMessage{
			{
				Role: "assistant",
				Content: []any{
					map[string]any{
						"type":     "thinking",
						"thinking": "Let me think about it.",
					},
					map[string]any{
						"type": "text",
						"text": "The answer is 42.",
					},
				},
			},
		},
	}

	oaReq, err := protocol.ConvertRequest(antReq)
	if err != nil {
		t.Fatalf("ConvertRequest failed: %v", err)
	}

	if len(oaReq.Messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(oaReq.Messages))
	}

	msg := oaReq.Messages[0]
	if msg.ReasoningContent != "Let me think about it." {
		t.Errorf("Expected ReasoningContent 'Let me think about it.', got '%s'", msg.ReasoningContent)
	}

	if msg.Content != "The answer is 42." {
		t.Errorf("Expected Content 'The answer is 42.', got '%v'", msg.Content)
	}

	// 2. Test response conversion mapping "reasoning_content" -> "thinking"
	oaResp := &protocol.OpenAIResponse{
		ID:    "chatcmpl-123",
		Model: "gpt-4o",
		Choices: []protocol.OpenAIChoice{
			{
				Index: 0,
				Message: protocol.OpenAIMessage{
					Role:             "assistant",
					Content:          "The answer is 42.",
					ReasoningContent: "Thinking process details.",
				},
				FinishReason: "stop",
			},
		},
	}

	antResp, err := protocol.ConvertResponse(oaResp)
	if err != nil {
		t.Fatalf("ConvertResponse failed: %v", err)
	}

	if len(antResp.Content) != 2 {
		t.Fatalf("Expected 2 content blocks, got %d", len(antResp.Content))
	}

	if antResp.Content[0].Type != "thinking" || antResp.Content[0].Thinking != "Thinking process details." {
		t.Errorf("Expected first content block to be 'thinking' with 'Thinking process details.', got %+v", antResp.Content[0])
	}

	if antResp.Content[1].Type != "text" || antResp.Content[1].Text != "The answer is 42." {
		t.Errorf("Expected second content block to be 'text' with 'The answer is 42.', got %+v", antResp.Content[1])
	}
}
