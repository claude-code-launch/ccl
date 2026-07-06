package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenAIResponsesRequest matches the OpenAI Responses API payload shape.
type OpenAIResponsesRequest struct {
	Model           string                `json:"model"`
	Input           []ResponsesInputItem  `json:"input"`
	Instructions    string                `json:"instructions,omitempty"`
	MaxOutputTokens int                   `json:"max_output_tokens,omitempty"`
	Stream          bool                  `json:"stream,omitempty"`
	Temperature     *float64              `json:"temperature,omitempty"`
	TopP            *float64              `json:"top_p,omitempty"`
	Tools           []OpenAIResponsesTool `json:"tools,omitempty"`
	ToolChoice      any                   `json:"tool_choice,omitempty"`
	Metadata        map[string]any        `json:"metadata,omitempty"`
	Store           *bool                 `json:"store,omitempty"`
}

type ResponsesInputItem struct {
	Type      string `json:"type"`
	Role      string `json:"role,omitempty"`
	Content   any    `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
	Status    string `json:"status,omitempty"`
}

type ResponsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type OpenAIResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type OpenAIResponsesResponse struct {
	ID     string                `json:"id"`
	Object string                `json:"object"`
	Model  string                `json:"model"`
	Status string                `json:"status"`
	Output []ResponsesOutputItem `json:"output"`
	Usage  OpenAIResponsesUsage  `json:"usage"`
}

type ResponsesOutputItem struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	Role      string                 `json:"role,omitempty"`
	Status    string                 `json:"status,omitempty"`
	Content   []ResponsesOutputPart  `json:"content,omitempty"`
	CallID    string                 `json:"call_id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Arguments string                 `json:"arguments,omitempty"`
	Summary   []ResponsesSummaryPart `json:"summary,omitempty"`
}

type ResponsesOutputPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ResponsesSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type OpenAIResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ProbeOpenAIResponsesSupport sends a minimal, real generation request to the
// /v1/responses endpoint to determine whether an OpenAI-compatible gateway
// implements the newer, agent-oriented Responses API ("openai(agent)") as
// opposed to only the legacy Chat Completions API ("openai(chat)"). Listing
// models alone (/v1/models) cannot distinguish these two, since both protocols
// commonly share the same model catalog — an actual call to /v1/responses is
// required. Returns true only when the upstream responds with a 2xx status.
func ProbeOpenAIResponsesSupport(endpoint, apiKey, model string, timeout time.Duration) bool {
	store := false
	body, err := json.Marshal(map[string]any{
		"model": model,
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "hi"}}},
		},
		"max_output_tokens": 1,
		"store":             store,
	})
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, NormalizeOpenAIResponsesURL(endpoint), bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func ConvertRequestToResponses(antReq *AnthropicRequest) (*OpenAIResponsesRequest, error) {
	store := false
	respReq := &OpenAIResponsesRequest{
		Model:           antReq.Model,
		MaxOutputTokens: antReq.MaxTokens,
		Stream:          antReq.Stream,
		Temperature:     antReq.Temperature,
		TopP:            antReq.TopP,
		Metadata:        antReq.Metadata,
		Store:           &store,
	}

	respReq.Instructions = stringifyAnthropicSystem(antReq.System)

	for _, antMsg := range antReq.Messages {
		items, err := convertAnthropicMessageToResponsesItems(antMsg)
		if err != nil {
			return nil, err
		}
		respReq.Input = append(respReq.Input, items...)
	}

	for _, antTool := range antReq.Tools {
		respReq.Tools = append(respReq.Tools, OpenAIResponsesTool{
			Type:        "function",
			Name:        antTool.Name,
			Description: antTool.Description,
			Parameters:  antTool.InputSchema,
		})
	}

	if antReq.ToolChoice != nil {
		respReq.ToolChoice = mapAnthropicToolChoiceToResponses(antReq.ToolChoice)
	}

	return respReq, nil
}

func convertAnthropicMessageToResponsesItems(msg AnthropicMessage) ([]ResponsesInputItem, error) {
	if text, ok := msg.Content.(string); ok {
		return []ResponsesInputItem{{
			Type:    "message",
			Role:    msg.Role,
			Content: []ResponsesContentPart{{Type: "input_text", Text: text}},
		}}, nil
	}

	blocks, ok := msg.Content.([]any)
	if !ok {
		return []ResponsesInputItem{{
			Type:    "message",
			Role:    msg.Role,
			Content: msg.Content,
		}}, nil
	}

	var items []ResponsesInputItem
	var contentParts []ResponsesContentPart
	flushMessage := func() {
		if len(contentParts) == 0 {
			return
		}
		items = append(items, ResponsesInputItem{
			Type:    "message",
			Role:    msg.Role,
			Content: contentParts,
		})
		contentParts = nil
	}

	for _, blockItem := range blocks {
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
			contentParts = append(contentParts, ResponsesContentPart{
				Type: "input_text",
				Text: block.Text,
			})
		case "image":
			if block.Source == nil {
				continue
			}
			imageURL := block.Source.URL
			if imageURL == "" && block.Source.Type == "base64" && block.Source.Data != "" {
				mediaType := block.Source.MediaType
				if mediaType == "" {
					mediaType = "image/png"
				}
				imageURL = "data:" + mediaType + ";base64," + block.Source.Data
			}
			if imageURL != "" {
				contentParts = append(contentParts, ResponsesContentPart{
					Type:     "input_image",
					ImageURL: imageURL,
				})
			}
		case "tool_use":
			flushMessage()
			args := string(block.Input)
			if args == "" {
				args = "{}"
			}
			items = append(items, ResponsesInputItem{
				Type:      "function_call",
				CallID:    block.ID,
				Name:      block.Name,
				Arguments: args,
			})
		case "tool_result":
			flushMessage()
			items = append(items, ResponsesInputItem{
				Type:   "function_call_output",
				CallID: block.ToolUseID,
				Output: stringifyToolResult(block.Content),
			})
		}
	}

	flushMessage()
	return items, nil
}

func ConvertResponsesResponse(resp *OpenAIResponsesResponse) (*AnthropicResponse, error) {
	if resp == nil {
		return nil, fmt.Errorf("nil OpenAI Responses response")
	}

	antResp := &AnthropicResponse{
		ID:    resp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: resp.Model,
		Usage: AnthropicUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}

	hasToolUse := false
	for _, item := range resp.Output {
		switch item.Type {
		case "reasoning":
			for _, summary := range item.Summary {
				if summary.Text != "" {
					antResp.Content = append(antResp.Content, ContentBlock{
						Type:     "thinking",
						Thinking: summary.Text,
					})
				}
			}
		case "message":
			for _, part := range item.Content {
				if (part.Type == "output_text" || part.Type == "text") && part.Text != "" {
					antResp.Content = append(antResp.Content, ContentBlock{
						Type: "text",
						Text: part.Text,
					})
				}
			}
		case "function_call":
			hasToolUse = true
			input := json.RawMessage("{}")
			if item.Arguments != "" && json.Valid([]byte(item.Arguments)) {
				input = json.RawMessage(item.Arguments)
			}
			antResp.Content = append(antResp.Content, ContentBlock{
				Type:  "tool_use",
				ID:    item.CallID,
				Name:  item.Name,
				Input: input,
			})
		}
	}

	if hasToolUse {
		antResp.StopReason = "tool_use"
	} else if resp.Status == "incomplete" {
		antResp.StopReason = "max_tokens"
	} else {
		antResp.StopReason = "end_turn"
	}

	return antResp, nil
}

func stringifyAnthropicSystem(system any) string {
	if system == nil {
		return ""
	}
	if text, ok := system.(string); ok {
		return stripLeadingAnthropicBillingHeader(text)
	}
	blocks, ok := system.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range blocks {
		if block, ok := item.(map[string]any); ok {
			if block["type"] == "text" {
				if text, ok := block["text"].(string); ok && text != "" {
					parts = append(parts, stripLeadingAnthropicBillingHeader(text))
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

func stringifyToolResult(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if block, ok := item.(map[string]any); ok {
				if block["type"] == "text" {
					if text, ok := block["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "")
		}
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	return string(raw)
}

func mapAnthropicToolChoiceToResponses(toolChoice *AnthropicToolChoice) any {
	switch toolChoice.Type {
	case "none":
		return "none"
	case "any":
		return "required"
	case "auto":
		return "auto"
	case "tool":
		return map[string]string{
			"type": "function",
			"name": toolChoice.Name,
		}
	default:
		return nil
	}
}
