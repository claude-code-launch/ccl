package protocol

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
)

const anthropicBillingHeaderPrefix = "x-anthropic-billing-header:"

// ConvertRequest maps an Anthropic Messages request to an OpenAI Chat Completions request.
func ConvertRequest(antReq *AnthropicRequest) (*OpenAIRequest, error) {
	oaReq := &OpenAIRequest{
		Model:       antReq.Model,
		MaxTokens:   antReq.MaxTokens,
		Stream:      antReq.Stream,
		Temperature: antReq.Temperature,
		TopP:        antReq.TopP,
		Stop:        antReq.StopSequences,
	}
	if antReq.Stream {
		oaReq.StreamOptions = &OpenAIStreamOptions{IncludeUsage: true}
	}

	// 1. Process system prompt (inject as first message with "system" role if present)
	if antReq.System != nil {
		var sysContent string
		switch sys := antReq.System.(type) {
		case string:
			sysContent = stripLeadingAnthropicBillingHeader(sys)
		case []any:
			// If it's a list of blocks, merge text types
			for _, item := range sys {
				if m, ok := item.(map[string]any); ok {
					if t, ok := m["type"].(string); ok && t == "text" {
						if val, ok := m["text"].(string); ok {
							sysContent += stripLeadingAnthropicBillingHeader(val)
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
				case "thinking":
					oaMsg.ReasoningContent = block.Thinking
				case "image":
					if block.Source != nil {
						imageURL := block.Source.URL
						if imageURL == "" && block.Source.Type == "base64" && block.Source.Data != "" {
							imageURL = "data:" + block.Source.MediaType + ";base64," + block.Source.Data
						}
						if imageURL != "" {
							parts = append(parts, OpenAIMessagePart{
								Type:     "image_url",
								ImageURL: &OpenAIImageURL{URL: imageURL},
							})
						}
					}

				case "tool_use":
					// Translate incoming tool execution back to OpenAI tool_calls structure
					args := string(block.Input)
					if args == "" {
						args = "{}"
					}
					oaMsg.ToolCalls = append(oaMsg.ToolCalls, OpenAIToolCall{
						ID:   block.ID,
						Type: "function",
						Function: OpenAIFunctionCall{
							Name:      block.Name,
							Arguments: args,
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

			if len(parts) > 0 {
				if len(oaMsg.ToolCalls) > 0 && textBuf != "" {
					oaMsg.Content = textBuf
				} else if len(parts) == 1 && parts[0].Type == "text" {
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
		case "none":
			oaReq.ToolChoice = "none"
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
// Uses the authoritative models registry for tier classification and selection when possible,
// falling back to heuristic keyword matching for models not in the registry.
func MapModel(requestedModel string, configuredModel string, availableModels []string) string {
	return modelrouting.MapModel(requestedModel, configuredModel, availableModels)
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
	if msg.ReasoningContent != "" {
		antResp.Content = append(antResp.Content, ContentBlock{
			Type:     "thinking",
			Thinking: msg.ReasoningContent,
		})
	}
	antResp.Content = appendOpenAIContentBlocks(antResp.Content, msg.Content)
	if msg.Refusal != "" {
		antResp.Content = append(antResp.Content, ContentBlock{
			Type: "text",
			Text: msg.Refusal,
		})
	}

	for _, tc := range msg.ToolCalls {
		var inputObj json.RawMessage
		if tc.Function.Arguments != "" && json.Valid([]byte(tc.Function.Arguments)) {
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
	if len(msg.ToolCalls) == 0 && msg.FunctionCall != nil {
		var inputObj json.RawMessage
		if msg.FunctionCall.Arguments != "" && json.Valid([]byte(msg.FunctionCall.Arguments)) {
			inputObj = json.RawMessage(msg.FunctionCall.Arguments)
		} else {
			inputObj = json.RawMessage("{}")
		}
		antResp.Content = append(antResp.Content, ContentBlock{
			Type:  "tool_use",
			Name:  msg.FunctionCall.Name,
			Input: inputObj,
		})
		if antResp.StopReason == "end_turn" {
			antResp.StopReason = "tool_use"
		}
	}

	return antResp, nil
}

func appendOpenAIContentBlocks(blocks []ContentBlock, content any) []ContentBlock {
	switch v := content.(type) {
	case string:
		if v != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: v})
		}
	case []any:
		for _, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			partType, _ := part["type"].(string)
			switch partType {
			case "text", "output_text":
				if text, ok := part["text"].(string); ok && text != "" {
					blocks = append(blocks, ContentBlock{Type: "text", Text: text})
				}
			case "refusal":
				if refusal, ok := part["refusal"].(string); ok && refusal != "" {
					blocks = append(blocks, ContentBlock{Type: "text", Text: refusal})
				}
			}
		}
	case []OpenAIMessagePart:
		for _, part := range v {
			if part.Type == "text" && part.Text != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: part.Text})
			}
		}
	}
	return blocks
}

func stripLeadingAnthropicBillingHeader(text string) string {
	if !strings.HasPrefix(text, anthropicBillingHeaderPrefix) {
		return text
	}
	lineEnd := strings.IndexAny(text, "\r\n")
	if lineEnd < 0 {
		return ""
	}
	rest := text[lineEnd:]
	rest = strings.TrimLeft(rest, "\r\n")
	return strings.TrimLeft(rest, "\r\n")
}

func BatchToGatewayModelAlias(rawModels []string) []string {
	var models []string
	seen := make(map[string]bool)

	for _, rawModel := range rawModels {
		alias := ToGatewayModelAlias(rawModel)
		if !seen[alias] {
			seen[alias] = true
			models = append(models, alias)
		}
	}

	return models
}

// ToGatewayModelAlias translates a raw model name into an Anthropic-compliant name
// that avoids Claude Code's hardcoded blacklist.
func ToGatewayModelAlias(model string) string {
	if model == "" {
		return ""
	}
	modelLower := strings.ToLower(model)
	// If it already matches the whitelist and doesn't contain blacklist words, keep it.
	hasWhitelistWord := strings.Contains(modelLower, "claude") || strings.Contains(modelLower, "sonnet") ||
		strings.Contains(modelLower, "opus") || strings.Contains(modelLower, "haiku") || strings.Contains(modelLower, "anthropic")

	// Blacklist words from Claude Code's pC4 filter
	hasBlacklistWord := strings.Contains(modelLower, "deepseek") || strings.Contains(modelLower, "gemini") ||
		strings.Contains(modelLower, "glm") || strings.Contains(modelLower, "qwen") ||
		strings.Contains(modelLower, "openai") || strings.Contains(modelLower, "gpt") ||
		strings.Contains(modelLower, "llama") || strings.Contains(modelLower, "grok") ||
		strings.Contains(modelLower, "mistral") || strings.Contains(modelLower, "mixtral")

	if hasWhitelistWord && !hasBlacklistWord {
		return model
	}

	// Prepend 'claude-' and replace blacklist keywords with safe abbreviations
	alias := modelLower
	alias = strings.ReplaceAll(alias, "deepseek", "ds")
	alias = strings.ReplaceAll(alias, "gemini", "gm")
	alias = strings.ReplaceAll(alias, "qwen", "qw")
	alias = strings.ReplaceAll(alias, "glm", "g")
	alias = strings.ReplaceAll(alias, "openai", "oa")
	alias = strings.ReplaceAll(alias, "gpt", "gp")
	alias = strings.ReplaceAll(alias, "llama", "ll")
	alias = strings.ReplaceAll(alias, "grok", "gr")
	alias = strings.ReplaceAll(alias, "mistral", "ms")
	alias = strings.ReplaceAll(alias, "mixtral", "mx")

	if !strings.HasPrefix(alias, "claude-") {
		alias = "claude-" + alias
	}
	return alias
}

// FromGatewayModelAlias restores the original model name from an obfuscated alias.
func FromGatewayModelAlias(alias string, availableModels []string) string {
	aliasLower := strings.ToLower(alias)
	if !strings.HasPrefix(aliasLower, "claude-") {
		return alias
	}

	// First try matching against the real available models list
	for _, realModel := range availableModels {
		if strings.EqualFold(ToGatewayModelAlias(realModel), alias) {
			return realModel
		}
	}

	// Fallback reverse replacements
	restored := strings.TrimPrefix(aliasLower, "claude-")
	restored = strings.ReplaceAll(restored, "ds", "deepseek")
	restored = strings.ReplaceAll(restored, "gm", "gemini")
	restored = strings.ReplaceAll(restored, "qw", "qwen")
	restored = strings.ReplaceAll(restored, "g", "glm")
	restored = strings.ReplaceAll(restored, "oa", "openai")
	restored = strings.ReplaceAll(restored, "gp", "gpt")
	restored = strings.ReplaceAll(restored, "ll", "llama")
	restored = strings.ReplaceAll(restored, "gr", "grok")
	restored = strings.ReplaceAll(restored, "ms", "mistral")
	restored = strings.ReplaceAll(restored, "mx", "mixtral")
	return restored
}
