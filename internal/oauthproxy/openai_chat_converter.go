package oauthproxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// chatCompletionsConvertedRequest is the CCL-owned wire representation of one
// Responses request destined for an OpenAI Chat Completions upstream.
type chatCompletionsConvertedRequest struct {
	adapterRequest
	body  []byte
	model string
}

// convertResponsesToChatCompletions translates a Responses request into an
// OpenAI Chat Completions request.
func convertResponsesToChatCompletions(raw []byte) (*chatCompletionsConvertedRequest, error) {
	request, err := parseResponsesRequest(raw)
	if err != nil {
		return nil, err
	}
	upstreamModel := stripContextModelSuffix(request.Model)

	messages := make([]map[string]any, 0, len(request.Turns)+1)
	if strings.TrimSpace(request.System) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": request.System})
	}
	for _, turn := range request.Turns {
		messages = append(messages, chatConvertTurn(turn)...)
	}

	body := map[string]any{
		"model":    upstreamModel,
		"messages": messages,
		"stream":   request.Stream,
	}
	if request.MaxOutputTokens > 0 {
		body["max_tokens"] = request.MaxOutputTokens
	}
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	} else if request.TopP != nil {
		body["top_p"] = *request.TopP
	}
	if len(request.Stop) > 0 {
		body["stop"] = request.Stop
	}
	if request.User != "" {
		body["user"] = request.User
	}
	if request.thinkingEnabled() {
		body["reasoning_effort"] = request.Effort
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
		if request.ParallelToolCalls != nil {
			body["parallel_tool_calls"] = *request.ParallelToolCalls
		}
	}
	if format := structuredOutputSchema(request.Format); format != nil {
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": format}
	}
	// Ask the upstream to include usage in the final streaming chunk so token
	// statistics survive even when no dedicated usage event is emitted. Only
	// valid on a streaming request — some upstreams reject stream_options on a
	// non-stream request ("stream_options should be set along with stream = true").
	if request.Stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI Chat Completions request: %w", err)
	}
	return &chatCompletionsConvertedRequest{
		adapterRequest: request.adapter(upstreamModel),
		body:           encoded,
		model:          upstreamModel,
	}, nil
}

// chatConvertTurn renders one conversation turn into zero or more OpenAI
// messages. Tool results are emitted first (as role:"tool"), then the turn
// body, so OpenAI's "tool must follow the assistant tool_calls" invariant holds.
func chatConvertTurn(turn conversationTurn) []map[string]any {
	assistant := turn.Role == "assistant"
	var toolResults []map[string]any
	var contentParts []any
	var reasoningParts []string
	var toolCalls []map[string]any

	for _, part := range turn.Parts {
		switch part.Kind {
		case partToolResult:
			if assistant {
				continue
			}
			text := part.outputText()
			images := part.outputImages()
			if len(images) > 0 {
				if text != "" {
					text += "\n"
				}
				text += "[image attached in the following user message]"
			}
			toolResults = append(toolResults, map[string]any{
				"role": "tool", "tool_call_id": part.CallID, "content": text,
			})
			// Chat tool messages accept text only. Keep images in a user message
			// after all tool replies and label their originating call.
			if len(images) > 0 {
				contentParts = append(contentParts, map[string]any{"type": "text", "text": "Images from tool result " + part.CallID + ":"})
				for _, image := range images {
					contentParts = append(contentParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": image.imageURL()}})
				}
			}
		case partReasoning:
			// Only assistant reasoning is honored; anything else is dropped to
			// prevent reasoning-content injection.
			if assistant && part.Text != "" {
				reasoningParts = append(reasoningParts, part.Text)
			}
		case partText:
			contentParts = append(contentParts, map[string]any{"type": "text", "text": part.Text})
		case partImage:
			contentParts = append(contentParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": part.imageURL()}})
		case partToolCall:
			if assistant && part.CallID != "" {
				toolCalls = append(toolCalls, map[string]any{
					"id":   part.CallID,
					"type": "function",
					"function": map[string]any{
						"name":      part.Name,
						"arguments": string(part.Arguments),
					},
				})
			}
		}
	}

	out := toolResults
	if assistant {
		message := map[string]any{"role": "assistant"}
		if len(contentParts) > 0 {
			message["content"] = contentParts
		} else {
			message["content"] = ""
		}
		if len(reasoningParts) > 0 {
			message["reasoning_content"] = strings.Join(reasoningParts, "")
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		return append(out, message)
	}
	if len(contentParts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": contentParts})
	}
	return out
}

// chatTool renders one flattened tool as an OpenAI function tool.
func chatTool(tool conversationTool) map[string]any {
	parameters := tool.Parameters
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

// ensureObjectSchema guarantees a parameter schema is an object with a
// properties field, which the OpenAI function-calling contract requires.
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

// chatToolChoice maps a tool choice onto OpenAI Chat's tool_choice. The boolean
// is false when no tool_choice should be sent.
func chatToolChoice(choice conversationToolChoice) (any, bool) {
	switch choice.Mode {
	case toolChoiceAuto, toolChoiceRequired, toolChoiceNone:
		// "none" is sent explicitly: omitting it would let the upstream default
		// to "auto", contradicting the caller's request to disable tools.
		return choice.Mode, true
	case toolChoiceTool:
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}, true
	default:
		return nil, false
	}
}
