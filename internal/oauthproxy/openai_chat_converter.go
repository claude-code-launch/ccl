package oauthproxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// chatCompletionsConvertedRequest is the CCL-owned wire representation of one
// Anthropic Messages request destined for an OpenAI Chat Completions upstream.
// No CPA translator or executor participates in it.
type chatCompletionsConvertedRequest struct {
	anthropicAdapterRequest
	body  []byte
	model string
}

// chatAnthropicRequest extends the shared Messages request shape with the
// top-level fields only the Chat Completions protocol consumes. They live here
// rather than on anthropicMessagesRequest so other adapters are unaffected.
type chatAnthropicRequest struct {
	anthropicMessagesRequest
	Temperature   float64  `json:"temperature"`
	TopP          float64  `json:"top_p"`
	StopSequences []string `json:"stop_sequences"`
	User          string   `json:"user"`
}

// convertAnthropicToChatCompletions translates an Anthropic Messages request into
// an OpenAI Chat Completions request. The translation follows the same rules as
// CLIProxyAPI's Claude->OpenAI Chat translator so behavior stays identical after
// the migration.
func convertAnthropicToChatCompletions(raw []byte) (*chatCompletionsConvertedRequest, error) {
	var request chatAnthropicRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid Anthropic Messages request: %w", err)
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if len(request.Messages) == 0 {
		return nil, fmt.Errorf("messages must not be empty")
	}

	upstreamModel := stripContextModelSuffix(request.Model)

	messages := make([]map[string]any, 0, len(request.Messages)+1)
	if system := chatSystemContent(request.System); len(system) > 0 {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	for _, message := range request.Messages {
		converted, err := chatConvertMessage(message)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}

	body := map[string]any{
		"model":    upstreamModel,
		"messages": messages,
		"stream":   request.Stream,
	}
	if request.MaxTokens > 0 {
		body["max_tokens"] = request.MaxTokens
	}
	if request.Temperature > 0 {
		body["temperature"] = request.Temperature
	} else if request.TopP > 0 {
		body["top_p"] = request.TopP
	}
	if len(request.StopSequences) > 0 {
		body["stop"] = request.StopSequences
	}
	if request.User != "" {
		body["user"] = request.User
	}
	if effort := chatReasoningEffort(request.Thinking, request.OutputConfig); effort != "" {
		body["reasoning_effort"] = effort
	}
	if len(request.Tools) > 0 {
		tools := make([]map[string]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, chatTool(tool))
		}
		body["tools"] = tools
		if choice, ok := chatToolChoice(request.ToolChoice); ok {
			body["tool_choice"] = choice
		}
	}
	// Ask the upstream to include usage in the final streaming chunk so token
	// statistics survive even when no dedicated usage event is emitted. Only
	// valid on a streaming request — some upstreams reject stream_options on a
	// non-stream request (e.g. "stream_options should be set along with
	// stream = true"), which breaks auto-compact (Claude Code sends stream=false).
	if request.Stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI Chat Completions request: %w", err)
	}

	thinkingEnabled := request.Thinking != nil && !strings.EqualFold(strings.TrimSpace(request.Thinking.Type), "disabled")
	return &chatCompletionsConvertedRequest{
		anthropicAdapterRequest: anthropicAdapterRequest{
			upstreamModel:     upstreamModel,
			clientModel:       request.Model,
			stream:            request.Stream,
			thinkingEnabled:   thinkingEnabled,
			thinkingSignature: "ccl-openai-chat-signature-unavailable",
			maxTokens:         request.MaxTokens,
			inputTokens:       estimateApproxTokensBytes(raw),
		},
		body:  encoded,
		model: upstreamModel,
	}, nil
}

// chatReasoningEffort maps an Anthropic thinking request onto OpenAI's
// reasoning_effort. A nil thinking block leaves the field unset so the upstream
// keeps its own default rather than forcing a reasoning tier.
func chatReasoningEffort(thinking *anthropicThinking, output *anthropicOutput) string {
	if thinking == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(thinking.Type)) {
	case "disabled":
		return ""
	case "adaptive", "auto":
		if output != nil {
			if effort := strings.ToLower(strings.TrimSpace(output.Effort)); effort != "" {
				return effort
			}
		}
		return "xhigh"
	case "enabled":
		switch {
		case thinking.BudgetTokens <= 0:
			return "medium"
		case thinking.BudgetTokens <= 2_000:
			return "low"
		case thinking.BudgetTokens <= 10_000:
			return "medium"
		case thinking.BudgetTokens <= 32_000:
			return "high"
		default:
			return "xhigh"
		}
	default:
		return ""
	}
}

// chatSystemContent flattens an Anthropic system prompt (string or block array)
// into OpenAI system content parts. Empty text is dropped.
func chatSystemContent(raw json.RawMessage) []any {
	if len(raw) == 0 {
		return nil
	}
	var direct string
	if json.Unmarshal(raw, &direct) == nil {
		if strings.TrimSpace(direct) == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": direct}}
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		if !strings.EqualFold(metadataString(block, "type"), "text") {
			continue
		}
		if text := metadataString(block, "text"); text != "" {
			parts = append(parts, map[string]any{"type": "text", "text": text})
		}
	}
	return parts
}

// chatContentBlock is a normalized view of one Anthropic content block after
// parsing, carrying only the fields the Chat Completions translator needs.
type chatContentBlock struct {
	blockType  string
	text       string
	thinking   string
	toolUseID  string
	toolUseIn  map[string]any
	toolResult string
	imageURL   string
}

// chatParseContent decodes an Anthropic content field (string or block array)
// into ordered normalized blocks.
func chatParseContent(raw json.RawMessage) ([]chatContentBlock, error) {
	var direct string
	if json.Unmarshal(raw, &direct) == nil {
		if strings.TrimSpace(direct) == "" {
			return nil, nil
		}
		return []chatContentBlock{{blockType: "text", text: direct}}, nil
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("message content must be a string or content block array")
	}
	result := make([]chatContentBlock, 0, len(blocks))
	for _, block := range blocks {
		switch strings.ToLower(metadataString(block, "type")) {
		case "text":
			if text := metadataString(block, "text"); text != "" {
				result = append(result, chatContentBlock{blockType: "text", text: text})
			}
		case "thinking":
			if thinking := metadataString(block, "thinking"); thinking != "" {
				result = append(result, chatContentBlock{blockType: "thinking", thinking: thinking})
			}
		case "image":
			if url := chatImageURL(block); url != "" {
				result = append(result, chatContentBlock{blockType: "image", imageURL: url})
			}
		case "tool_use":
			input, _ := block["input"].(map[string]any)
			if input == nil {
				input = map[string]any{}
			}
			result = append(result, chatContentBlock{
				blockType: "tool_use",
				toolUseID: metadataString(block, "id"),
				toolUseIn: input,
				text:      metadataString(block, "name"),
			})
		case "tool_result":
			result = append(result, chatContentBlock{
				blockType:  "tool_result",
				toolUseID:  metadataString(block, "tool_use_id"),
				toolResult: chatToolResultContent(block["content"]),
			})
		}
	}
	return result, nil
}

// chatImageURL renders an Anthropic base64 image block as an OpenAI data URL.
func chatImageURL(block map[string]any) string {
	source, _ := block["source"].(map[string]any)
	mediaType := metadataString(source, "media_type")
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	data := metadataString(source, "data")
	if data == "" {
		return ""
	}
	return "data:" + mediaType + ";base64," + data
}

// chatToolResultContent stringifies a tool_result content field. Text is
// concatenated; images and other unsupported blocks are marked as omitted, the
// same collapse CLIProxyAPI applies for image-less upstreams.
func chatToolResultContent(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(metadataString(block, "type")) {
			case "text":
				if text := metadataString(block, "text"); text != "" {
					parts = append(parts, text)
				}
			case "image", "image_url", "input_image":
				parts = append(parts, "[image omitted: unsupported by upstream]")
			}
		}
		return strings.Join(parts, "\n")
	default:
		raw, _ := json.Marshal(typed)
		return string(raw)
	}
}

// chatConvertMessage renders one Anthropic message into zero or more OpenAI
// messages. Tool results are emitted first (as role:"tool"), then the message
// body, so OpenAI's "tool must follow the assistant tool_calls" invariant holds.
func chatConvertMessage(message anthropicMessage) ([]map[string]any, error) {
	blocks, err := chatParseContent(message.Content)
	if err != nil {
		return nil, err
	}
	role := strings.ToLower(strings.TrimSpace(message.Role))
	assistant := role == "assistant"

	out := make([]map[string]any, 0, 2)

	// Historical system reminders surface as a user message.
	if role == "system" {
		var parts []string
		for _, block := range blocks {
			if block.blockType == "text" {
				parts = append(parts, block.text)
			}
		}
		if len(parts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": strings.Join(parts, "\n")})
		}
		return out, nil
	}

	// Tool results precede their message body, matching OpenAI ordering.
	var toolResults []map[string]any
	var textParts []string
	var reasoningParts []string
	var toolCalls []map[string]any
	var imageParts []any

	for _, block := range blocks {
		switch block.blockType {
		case "tool_result":
			if !assistant {
				toolResults = append(toolResults, map[string]any{
					"role":         "tool",
					"tool_call_id": block.toolUseID,
					"content":      block.toolResult,
				})
			}
		case "thinking":
			// Only assistant reasoning is honored; user/system thinking is dropped
			// to prevent reasoning-content injection.
			if assistant {
				reasoningParts = append(reasoningParts, block.thinking)
			}
		case "text":
			textParts = append(textParts, block.text)
		case "image":
			imageParts = append(imageParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": block.imageURL}})
		case "tool_use":
			if assistant && block.toolUseID != "" {
				arguments := block.toolUseIn
				argsJSON, _ := json.Marshal(arguments)
				toolCalls = append(toolCalls, map[string]any{
					"id":   block.toolUseID,
					"type": "function",
					"function": map[string]any{
						"name":      block.text,
						"arguments": string(argsJSON),
					},
				})
			}
		}
	}

	out = append(out, toolResults...)

	if assistant {
		message := map[string]any{"role": "assistant"}
		content := make([]any, 0, len(textParts)+len(imageParts))
		for _, part := range textParts {
			content = append(content, map[string]any{"type": "text", "text": part})
		}
		content = append(content, imageParts...)
		if len(content) > 0 {
			message["content"] = content
		} else {
			message["content"] = ""
		}
		if len(reasoningParts) > 0 {
			message["reasoning_content"] = strings.Join(reasoningParts, "")
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		out = append(out, message)
	} else {
		content := make([]any, 0, len(textParts)+len(imageParts))
		for _, part := range textParts {
			content = append(content, map[string]any{"type": "text", "text": part})
		}
		content = append(content, imageParts...)
		if len(content) > 0 {
			out = append(out, map[string]any{"role": "user", "content": content})
		}
	}
	return out, nil
}

// chatTool renders one Anthropic tool as an OpenAI function tool.
func chatTool(tool anthropicTool) map[string]any {
	parameters := tool.InputSchema
	if len(parameters) == 0 {
		parameters = json.RawMessage(`{"type":"object","properties":{}}`)
	} else {
		parameters = ensureObjectSchema(parameters)
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  parameters,
		},
	}
}

// ensureObjectSchema guarantees an input_schema is an object with a properties
// field, which the OpenAI function-calling contract requires.
func ensureObjectSchema(schema json.RawMessage) json.RawMessage {
	var value map[string]any
	if json.Unmarshal(schema, &value) != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	if _, ok := value["properties"]; !ok {
		value["properties"] = map[string]any{}
	}
	if _, ok := value["type"]; !ok {
		value["type"] = "object"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return encoded
}

// chatToolChoice maps an Anthropic tool_choice onto OpenAI's tool_choice. The
// boolean is false when no tool_choice should be sent.
func chatToolChoice(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var direct string
	if json.Unmarshal(raw, &direct) == nil {
		switch strings.ToLower(strings.TrimSpace(direct)) {
		case "auto":
			return "auto", true
		case "any", "required":
			return "required", true
		case "none":
			// Omitting tool_choice would let the upstream default to "auto",
			// contradicting the caller's explicit request to disable tools.
			return "none", true
		default:
			return "auto", true
		}
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) != nil {
		return "auto", true
	}
	switch strings.ToLower(strings.TrimSpace(choice.Type)) {
	case "auto":
		return "auto", true
	case "any", "required":
		return "required", true
	case "none":
		return "none", true
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}, true
	default:
		return "auto", true
	}
}
