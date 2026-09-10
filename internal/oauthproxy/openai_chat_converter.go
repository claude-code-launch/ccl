package oauthproxy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// chatCompletionsConvertedRequest is the CCL-owned wire representation of one
// Anthropic Messages request destined for an OpenAI Chat Completions upstream.
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
	Temperature   *float64 `json:"temperature"`
	TopP          float64  `json:"top_p"`
	StopSequences []string `json:"stop_sequences"`
	User          string   `json:"user"`
}

// convertAnthropicToChatCompletions translates an Anthropic Messages request into
// an OpenAI Chat Completions request. The translation preserves the established
// Claude-to-Chat compatibility behavior.
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
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
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
		if disabled := gjson.GetBytes(request.ToolChoice, "disable_parallel_tool_use"); disabled.Exists() {
			body["parallel_tool_calls"] = !disabled.Bool()
		}
	}
	format, err := messagesStructuredOutput(request.OutputConfig)
	if err != nil {
		return nil, err
	}
	if format != nil {
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": format}
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

// claudeCodeGitPRLine strips the git-status boilerplate line Claude Code injects
// into its system prompt: "Main branch (you will usually use this for PRs): <branch>".
// It duplicates the "Current branch" line and is one of the Claude Code fingerprints
// that upstream gateways reject.
var claudeCodeGitPRLine = regexp.MustCompile(`(?m)^[ \t]*Main branch \(you will usually use this for PRs\):.*(?:\r?\n)?`)

// claudeCodeIdentityLine is the Claude Code self-identification line prepended to
// (or split out of) its system prompt. It carries no role/behavior information and
// is a Claude Code fingerprint rather than a user instruction.
const claudeCodeIdentityLine = "You are Claude Code, Anthropic's official CLI for Claude."

// isClaudeCodeChatFingerprint reports whether a system text block consists solely
// of Claude Code attribution that must not be forwarded upstream. The billing header
// and the identity line are fingerprints (not user content); forwarding them lets an
// upstream gateway identify the request as proxied Claude Code — WorkBuddy rejects
// such bodies with code 11128 ("unapproved channel").
func isClaudeCodeChatFingerprint(text string) bool {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "x-anthropic-billing-header:") {
		return true
	}
	return strings.TrimSpace(text) == claudeCodeIdentityLine
}

// sanitizeChatSystemText removes Claude Code attribution and git boilerplate from a
// system text block so it reads as plain instructions to the upstream model.
func sanitizeChatSystemText(text string) string {
	text = claudeCodeGitPRLine.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, claudeCodeIdentityLine, "")
	return text
}

// chatSystemContent flattens an Anthropic system prompt (string or block array)
// into OpenAI system content parts. Empty text and Claude Code attribution
// fingerprints are dropped.
func chatSystemContent(raw json.RawMessage) []any {
	if len(raw) == 0 {
		return nil
	}
	var direct string
	if json.Unmarshal(raw, &direct) == nil {
		return chatSystemParts(direct)
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
		text := metadataString(block, "text")
		if text == "" || isClaudeCodeChatFingerprint(text) {
			continue
		}
		text = sanitizeChatSystemText(text)
		if strings.TrimSpace(text) == "" {
			continue
		}
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	return parts
}

func chatSystemParts(text string) []any {
	if strings.TrimSpace(text) == "" || isClaudeCodeChatFingerprint(text) {
		return nil
	}
	text = sanitizeChatSystemText(text)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []any{map[string]any{"type": "text", "text": text}}
}

// chatContentBlock is a normalized view of one Anthropic content block after
// parsing, carrying only the fields the Chat Completions translator needs.
type chatContentBlock struct {
	blockType  string
	text       string
	thinking   string
	toolUseID  string
	toolUseIn  json.RawMessage
	toolResult string
	toolImages []string
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
	if err := decodeProtocolJSON(raw, &blocks); err != nil {
		return nil, fmt.Errorf("message content must be a string or content block array")
	}
	result := make([]chatContentBlock, 0, len(blocks))
	for index, block := range blocks {
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
			input := gjson.GetBytes(raw, fmt.Sprintf("%d.input", index))
			inputJSON := json.RawMessage(`{}`)
			if input.IsObject() {
				inputJSON = json.RawMessage(input.Raw)
			}
			result = append(result, chatContentBlock{
				blockType: "tool_use",
				toolUseID: metadataString(block, "id"),
				toolUseIn: inputJSON,
				text:      metadataString(block, "name"),
			})
		case "tool_result":
			var images []string
			for _, item := range sliceValue(block["content"]) {
				part := mapValue(item)
				if metadataString(part, "type") == "image" {
					if imageURL := chatImageURL(part); imageURL != "" {
						images = append(images, imageURL)
					}
				}
			}
			result = append(result, chatContentBlock{
				blockType:  "tool_result",
				toolUseID:  metadataString(block, "tool_use_id"),
				toolResult: chatToolResultContent(block["content"]),
				toolImages: images,
			})
		}
	}
	return result, nil
}

// chatImageURL preserves URL images and renders base64 images as data URLs.
func chatImageURL(block map[string]any) string {
	source, _ := block["source"].(map[string]any)
	if metadataString(source, "type") == "url" {
		return metadataString(source, "url")
	}
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
// concatenated; images are referenced here and attached to the following user
// message by chatConvertMessage.
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
			case "image":
				if chatImageURL(block) != "" {
					parts = append(parts, "[image attached in the following user message]")
				} else {
					parts = append(parts, "[image omitted: invalid image source]")
				}
			case "image_url", "input_image":
				parts = append(parts, "[image omitted: unsupported image block]")
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
	var contentParts []any
	var reasoningParts []string
	var toolCalls []map[string]any

	for _, block := range blocks {
		switch block.blockType {
		case "tool_result":
			if !assistant {
				toolResults = append(toolResults, map[string]any{
					"role":         "tool",
					"tool_call_id": block.toolUseID,
					"content":      block.toolResult,
				})
				// Chat tool messages accept text only. Keep images in a user
				// message after all tool replies and label their originating call.
				if len(block.toolImages) > 0 {
					contentParts = append(contentParts, map[string]any{"type": "text", "text": "Images from tool result " + block.toolUseID + ":"})
					for _, imageURL := range block.toolImages {
						contentParts = append(contentParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
					}
				}
			}
		case "thinking":
			// Only assistant reasoning is honored; user/system thinking is dropped
			// to prevent reasoning-content injection.
			if assistant {
				reasoningParts = append(reasoningParts, block.thinking)
			}
		case "text":
			contentParts = append(contentParts, map[string]any{"type": "text", "text": block.text})
		case "image":
			contentParts = append(contentParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": block.imageURL}})
		case "tool_use":
			if assistant && block.toolUseID != "" {
				arguments := block.toolUseIn
				toolCalls = append(toolCalls, map[string]any{
					"id":   block.toolUseID,
					"type": "function",
					"function": map[string]any{
						"name":      block.text,
						"arguments": string(arguments),
					},
				})
			}
		}
	}

	out = append(out, toolResults...)

	if assistant {
		message := map[string]any{"role": "assistant"}
		content := contentParts
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
		content := contentParts
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
	if decodeProtocolJSON(schema, &value) != nil || value == nil {
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
