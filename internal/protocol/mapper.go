package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ConvertRequest maps an Anthropic Messages request to an OpenAI Chat Completions request.
func ConvertRequest(antReq *AnthropicRequest) (*OpenAIRequest, error) {
	oaReq := &OpenAIRequest{
		Model:       antReq.Model,
		MaxTokens:   antReq.MaxTokens,
		Stream:      antReq.Stream,
		Temperature: antReq.Temperature,
		TopP:        antReq.TopP,
	}

	// 1. Process system prompt (inject as first message with "system" role if present)
	if antReq.System != nil {
		var sysContent string
		switch sys := antReq.System.(type) {
		case string:
			sysContent = sys
		case []any:
			// If it's a list of blocks, merge text types
			for _, item := range sys {
				if m, ok := item.(map[string]any); ok {
					if t, ok := m["type"].(string); ok && t == "text" {
						if val, ok := m["text"].(string); ok {
							sysContent += val
						}
					}
				}
			}
		}

		if sysContent != "" {
			oaReq.Messages = append(oaReq.Messages, OpenAIMessage{
				Role:    "system",
				Content: sysContent,
			})
		}
	}

	// 2. Map messages
	for _, antMsg := range antReq.Messages {
		oaMsg := OpenAIMessage{
			Role: antMsg.Role,
		}

		// Handle Content (either string or array of blocks)
		switch content := antMsg.Content.(type) {
		case string:
			oaMsg.Content = content
		case []any:
			var parts []OpenAIMessagePart
			var textBuf string

			for _, blockItem := range content {
				blockBytes, err := json.Marshal(blockItem)
				if err != nil {
					continue
				}

				var block ContentBlock
				if err := json.Unmarshal(blockBytes, &block); err != nil {
					continue
				}

				switch block.Type {
				case "text":
					textBuf += block.Text
					parts = append(parts, OpenAIMessagePart{
						Type: "text",
						Text: block.Text,
					})

				case "image":
					// Try to handle image mapping if encountered, otherwise skip
					// OpenAI expects base64 or URL. We leave empty or map to Type/ImageURL
					// Standard Claude Code is CLI-based so it rarely sends images.

				case "tool_use":
					// Translate incoming tool execution back to OpenAI tool_calls structure
					oaMsg.ToolCalls = append(oaMsg.ToolCalls, OpenAIToolCall{
						ID:   block.ID,
						Type: "function",
						Function: OpenAIFunctionCall{
							Name:      block.Name,
							Arguments: string(block.Input),
						},
					})

				case "tool_result":
					// Claude Code says: I ran tool X, here is the result.
					// In OpenAI, this is a separate message with role="tool"
					// and tool_call_id pointing to the original ID.
					var resText string
					switch res := block.Content.(type) {
					case string:
						resText = res
					case []any:
						// If complex blocks inside tool result, serialize or flatten
						for _, b := range res {
							if bm, ok := b.(map[string]any); ok {
								if t, ok := bm["type"].(string); ok && t == "text" {
									if txt, ok := bm["text"].(string); ok {
										resText += txt
									}
								}
							}
						}
					}

					// We immediately append a separate tool message for OpenAI
					oaReq.Messages = append(oaReq.Messages, OpenAIMessage{
						Role:       "tool",
						ToolCallID: block.ToolUseID,
						Content:    resText,
					})
				}
			}

			// If it's normal text-only blocks, compress to string content for better compatibility
			if len(parts) > 0 && len(oaMsg.ToolCalls) == 0 {
				if len(parts) == 1 && parts[0].Type == "text" {
					oaMsg.Content = parts[0].Text
				} else {
					oaMsg.Content = parts
				}
			}
		}

		// Only append if it wasn't a standalone tool result (which has already been handled and appended as role="tool")
		if oaMsg.Content != nil || len(oaMsg.ToolCalls) > 0 {
			oaReq.Messages = append(oaReq.Messages, oaMsg)
		}
	}

	// 3. Map tools definitions
	if len(antReq.Tools) > 0 {
		for _, antTool := range antReq.Tools {
			oaReq.Tools = append(oaReq.Tools, OpenAITool{
				Type: "function",
				Function: OpenAIFunctionDef{
					Name:        antTool.Name,
					Description: antTool.Description,
					Parameters:  antTool.InputSchema,
				},
			})
		}
	}

	// 4. Map tool choice
	if antReq.ToolChoice != nil {
		switch antReq.ToolChoice.Type {
		case "any", "auto":
			oaReq.ToolChoice = "auto"
		case "tool":
			type toolStruct struct {
				Type     string            `json:"type"`
				Function OpenAIFunctionDef `json:"function"`
			}
			oaReq.ToolChoice = toolStruct{
				Type: "function",
				Function: OpenAIFunctionDef{
					Name: antReq.ToolChoice.Name,
				},
			}
		}
	}

	return oaReq, nil
}

// MapModel intelligently translates the requested Anthropic model name into the best available OpenAI gateway counterpart.
func MapModel(requestedModel string, configuredModel string, availableModels []string) string {
	// If the user explicitly configured a single model (no commas), use it directly.
	if configuredModel != "" && !strings.Contains(configuredModel, ",") {
		return configuredModel
	}

	// If the user provided a comma-separated list of models in configuration,
	// we treat it as a constrained pool of available models.
	var modelPool []string
	if configuredModel != "" && strings.Contains(configuredModel, ",") {
		parts := strings.Split(configuredModel, ",")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part != "" {
				modelPool = append(modelPool, part)
			}
		}
	} else {
		modelPool = availableModels
	}

	// If the requested model is already a pool member, return it directly.
	// This handles per-tier env vars (ANTHROPIC_DEFAULT_SONNET_MODEL, etc.)
	// that already map tiers to specific backend models.
	for _, model := range modelPool {
		if strings.EqualFold(model, requestedModel) {
			return model
		}
	}

	requestedModel = strings.ToLower(requestedModel)

	// Helper matching function
	containsAny := func(s string, keywords ...string) bool {
		for _, kw := range keywords {
			if strings.Contains(s, kw) {
				return true
			}
		}
		return false
	}

	// Find models in pool
	findInAvailable := func(candidates ...string) string {
		for _, cand := range candidates {
			for _, avail := range modelPool {
				if strings.EqualFold(avail, cand) || strings.Contains(strings.ToLower(avail), strings.ToLower(cand)) {
					return avail
				}
			}
		}
		return ""
	}

	// 2. Map based on model tier requested by Claude Code
	isHaikuTier := containsAny(requestedModel, "haiku")
	isOpusTier := containsAny(requestedModel, "opus", "4.8", "4.7")
	isSonnetTier := (containsAny(requestedModel, "sonnet", "3-5", "3.5") && !isHaikuTier) || requestedModel == ""

	if isOpusTier {
		// Prefer strongest reasoning / smart models
		if match := findInAvailable("deepseek-reasoner", "deepseek-v4-pro", "o1", "o3-mini", "gpt-4o", "claude-3-opus"); match != "" {
			return match
		}
		// Run a keyword heuristic if standard candidates are not found in modelPool
		for _, avail := range modelPool {
			availLower := strings.ToLower(avail)
			if containsAny(availLower, "reasoner", "reasoning", "o1", "o3", "max", "opus", "pro") {
				return avail
			}
		}
		// Second heuristic try: general high performance keyword
		for _, avail := range modelPool {
			availLower := strings.ToLower(avail)
			if containsAny(availLower, "plus") && !containsAny(availLower, "flash", "mini", "lite") {
				return avail
			}
		}
		// Fallback
		if len(modelPool) > 0 {
			return modelPool[0]
		}
		return "deepseek-reasoner"
	}

	if isSonnetTier {
		// Prefer flagship standard models or good chat models
		if match := findInAvailable("deepseek-v4-pro", "qwen3.6-plus", "deepseek-chat", "gpt-4o", "claude-3-5-sonnet", "gpt-4"); match != "" {
			return match
		}
		// Run a keyword heuristic if standard candidates are not found in modelPool
		for _, avail := range modelPool {
			availLower := strings.ToLower(avail)
			// Match "pro", "plus", "chat", "standard", "v4", "v3", etc. but exclude low-tier or reasoning variants
			if containsAny(availLower, "pro", "plus", "chat", "standard", "v4", "v3", "glm-5", "flash") && !containsAny(availLower, "mini", "lite", "reasoner", "reasoning") {
				return avail
			}
		}
		// Fallback to first available model that is not low tier if possible
		for _, avail := range modelPool {
			availLower := strings.ToLower(avail)
			if !containsAny(availLower, "mini", "lite") {
				return avail
			}
		}
		// Fallback
		if len(modelPool) > 0 {
			return modelPool[0]
		}
		return "deepseek-chat"
	}

	if isHaikuTier {
		// Prefer fast, cost-effective models
		if match := findInAvailable("deepseek-v4-flash", "gpt-4o-mini", "gpt-3.5-turbo", "claude-3-5-haiku"); match != "" {
			return match
		}
		// Run a keyword heuristic if standard candidates are not found in modelPool
		for _, avail := range modelPool {
			availLower := strings.ToLower(avail)
			if containsAny(availLower, "mini", "flash", "lite", "haiku", "turbo", "fast") {
				return avail
			}
		}
		// Fallback
		if len(modelPool) > 0 {
			return modelPool[len(modelPool)-1] // return last model which is usually smaller
		}
		return "gpt-4o-mini"
	}

	// Global default if no tier could be resolved
	if len(modelPool) > 0 {
		// Just pick deepseek-chat or the first available model
		if match := findInAvailable("deepseek-chat", "gpt-4o", "qwen3.6-plus"); match != "" {
			return match
		}
		return modelPool[0]
	}

	return "deepseek-chat" // Reasonable general fallback
}

// ConvertResponse translates a standard OpenAI Chat Completions response to Anthropic style Messages.
func ConvertResponse(oaResp *OpenAIResponse) (*AnthropicResponse, error) {
	if len(oaResp.Choices) == 0 {
		return nil, fmt.Errorf("empty choice returned from OpenAI response")
	}

	choice := oaResp.Choices[0]
	antResp := &AnthropicResponse{
		ID:    oaResp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: oaResp.Model,
		Usage: AnthropicUsage{
			InputTokens:  oaResp.Usage.PromptTokens,
			OutputTokens: oaResp.Usage.CompletionTokens,
		},
	}

	// Translate finish reason
	switch choice.FinishReason {
	case "stop":
		antResp.StopReason = "end_turn"
	case "tool_calls":
		antResp.StopReason = "tool_use"
	case "length":
		antResp.StopReason = "max_tokens"
	default:
		antResp.StopReason = "end_turn"
	}

	// Process message content or tool calls
	msg := choice.Message
	if contentStr, ok := msg.Content.(string); ok && contentStr != "" {
		antResp.Content = append(antResp.Content, ContentBlock{
			Type: "text",
			Text: contentStr,
		})
	}

	for _, tc := range msg.ToolCalls {
		var inputObj json.RawMessage
		if tc.Function.Arguments != "" {
			inputObj = json.RawMessage(tc.Function.Arguments)
		} else {
			inputObj = json.RawMessage("{}")
		}

		antResp.Content = append(antResp.Content, ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: inputObj,
		})
	}

	return antResp, nil
}
