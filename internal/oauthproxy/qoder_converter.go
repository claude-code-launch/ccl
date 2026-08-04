package oauthproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/google/uuid"
)

type qoderModel struct {
	ID                 string
	Name               string
	ContextWindow      int
	MaxTokens          int
	PriceFactor        *float64
	Reasoning          bool
	Vision             bool
	IsNew              bool
	PromotionAvailable bool
	Source             string
	Config             map[string]any
}

type qoderConvertedRequest struct {
	clientModel string
	model       qoderModel
	stream      bool
	body        map[string]any
	anthropic   *anthropicAdapterRequest
	inputTokens int
}

type qoderToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func convertAnthropicToQoder(raw []byte, catalog []qoderModel) (*qoderConvertedRequest, error) {
	var request anthropicMessagesRequest
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
	model, ok := qoderSelectModel(request.Model, catalog)
	if !ok {
		return nil, fmt.Errorf("Qoder has no available models")
	}
	messages, lastUser, err := qoderTransformMessages(request.Messages)
	if err != nil {
		return nil, err
	}
	if system := extractKiroSystem(request.System); strings.TrimSpace(system) != "" {
		messages = append([]any{map[string]any{"role": "system", "content": system}}, messages...)
	}
	tools, err := qoderTransformTools(request.Tools)
	if err != nil {
		return nil, err
	}
	maxTokens := request.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 32_768
	}
	if model.MaxTokens > 0 && maxTokens > model.MaxTokens {
		maxTokens = model.MaxTokens
	}
	modelConfig := make(map[string]any, len(model.Config)+3)
	for key, value := range model.Config {
		modelConfig[key] = value
	}
	modelConfig["key"] = model.ID
	if _, exists := modelConfig["is_reasoning"]; !exists {
		modelConfig["is_reasoning"] = model.Reasoning
	}
	if _, exists := modelConfig["max_output_tokens"]; !exists {
		modelConfig["max_output_tokens"] = maxTokens
	}
	if _, exists := modelConfig["source"]; !exists {
		modelConfig["source"] = model.Source
	}
	recordID := qoderStableID("qoder-record", request.Model, string(raw), fmt.Sprintf("mt=%d", maxTokens))
	sessionSeed := ""
	if request.Metadata != nil {
		sessionSeed = strings.TrimSpace(request.Metadata.UserID)
	}
	if sessionSeed == "" {
		sessionSeed = uuid.NewString()
	}
	sessionID := qoderStableID("qoder-session", model.ID, sessionSeed) + "-" + sessionSeed
	body := map[string]any{
		"request_id":       uuid.NewString(),
		"request_set_id":   recordID,
		"chat_record_id":   recordID,
		"session_id":       sessionID,
		"stream":           true,
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"session_type":     "qodercli",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"code_language":    "",
		"chat_prompt":      "",
		"image_urls":       nil,
		"aliyun_user_type": "",
		"system":           "",
		"messages":         messages,
		"tools":            tools,
		"parameters":       map[string]any{"max_tokens": maxTokens},
		"chat_context": map[string]any{
			"chatPrompt": "",
			"imageUrls":  nil,
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"key": model.ID, "is_reasoning": model.Reasoning},
				"originalContent": lastUser,
			},
			"features": []any{},
			"text":     lastUser,
		},
		"model_config": modelConfig,
		"business": map[string]any{
			"product":  "cli",
			"version":  "1.0.0",
			"type":     "agent",
			"stage":    "start",
			"id":       uuid.NewString(),
			"name":     qoderPrefix(lastUser, 30),
			"begin_at": time.Now().UnixMilli(),
		},
	}
	thinkingEnabled := request.Thinking != nil &&
		(strings.EqualFold(request.Thinking.Type, "enabled") || strings.EqualFold(request.Thinking.Type, "adaptive"))
	anthropic := &anthropicAdapterRequest{
		upstreamModel:     model.ID,
		clientModel:       request.Model,
		stream:            request.Stream,
		thinkingEnabled:   thinkingEnabled,
		thinkingSignature: "qoder",
		maxTokens:         maxTokens,
		inputTokens:       estimateApproxTokensBytes(raw),
		toolNameMap:       map[string]string{},
	}
	return &qoderConvertedRequest{
		clientModel: request.Model,
		model:       model,
		stream:      request.Stream,
		body:        body,
		anthropic:   anthropic,
		inputTokens: anthropic.inputTokens,
	}, nil
}

func qoderSelectModel(requested string, catalog []qoderModel) (qoderModel, bool) {
	base := stripContextModelSuffix(strings.TrimSpace(requested))
	for _, model := range catalog {
		if strings.EqualFold(model.ID, base) {
			return model, true
		}
	}
	// Claude Code receives catalog display names for its /model UI. Resolve the
	// selected display alias back to Qoder's stable technical key here.
	for _, model := range catalog {
		if strings.EqualFold(strings.TrimSpace(model.Name), base) {
			return model, true
		}
	}
	ids := make([]string, 0, len(catalog))
	for _, model := range catalog {
		ids = append(ids, model.ID)
	}
	selected := modelrouting.MapModel(base, "", ids)
	for _, model := range catalog {
		if strings.EqualFold(model.ID, selected) {
			return model, true
		}
	}
	return qoderModel{}, false
}

func qoderTransformTools(tools []anthropicTool) ([]any, error) {
	transformed := make([]any, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			return nil, fmt.Errorf("tool name is required")
		}
		var parameters any = map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.InputSchema) > 0 && string(tool.InputSchema) != "null" {
			if err := json.Unmarshal(tool.InputSchema, &parameters); err != nil {
				return nil, fmt.Errorf("tool %s has invalid input_schema: %w", name, err)
			}
		}
		transformed = append(transformed, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": tool.Description,
				"parameters":  parameters,
			},
		})
	}
	return transformed, nil
}

func qoderTransformMessages(messages []anthropicMessage) ([]any, string, error) {
	transformed := make([]any, 0, len(messages)+2)
	lastUser := ""
	for _, message := range messages {
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system":
			// Claude Code's gateway-compatible Messages requests can carry the
			// system prompt as a message instead of the Anthropic top-level
			// `system` field. Qoder uses an OpenAI-style message list, so keep
			// that role rather than rejecting the request locally.
			content := extractKiroSystem(message.Content)
			if content != "" {
				transformed = append(transformed, map[string]any{"role": "system", "content": content})
			}
		case "user":
			mapped, userText, err := qoderTransformUserMessage(message.Content)
			if err != nil {
				return nil, "", err
			}
			transformed = append(transformed, mapped...)
			if strings.TrimSpace(userText) != "" {
				lastUser = userText
			}
		case "assistant":
			mapped, err := qoderTransformAssistantMessage(message.Content)
			if err != nil {
				return nil, "", err
			}
			transformed = append(transformed, mapped)
		default:
			return nil, "", fmt.Errorf("unsupported Anthropic message role %q", message.Role)
		}
	}
	return transformed, lastUser, nil
}

func qoderTransformUserMessage(raw json.RawMessage) ([]any, string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []any{map[string]any{"role": "user", "content": text}}, text, nil
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		Source    struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, "", fmt.Errorf("invalid Anthropic user content: %w", err)
	}
	messages := make([]any, 0, len(blocks))
	parts := make([]any, 0, len(blocks))
	var allText strings.Builder
	flushUser := func() {
		if len(parts) == 0 {
			return
		}
		content := any(parts)
		if len(parts) == 1 {
			if part, ok := parts[0].(map[string]any); ok && part["type"] == "text" {
				content = part["text"]
			}
		}
		messages = append(messages, map[string]any{"role": "user", "content": content})
		parts = nil
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": block.Text})
			allText.WriteString(block.Text)
		case "image":
			imageURL := strings.TrimSpace(block.Source.URL)
			if imageURL == "" && block.Source.Data != "" {
				imageURL = "data:" + block.Source.MediaType + ";base64," + block.Source.Data
			}
			if imageURL != "" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
			}
		case "tool_result":
			flushUser()
			content := qoderRawContentText(block.Content)
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": block.ToolUseID,
				"content":      content,
			})
		default:
			// Cache-control and other client-only blocks are intentionally omitted.
		}
	}
	flushUser()
	if len(messages) == 0 {
		messages = append(messages, map[string]any{"role": "user", "content": ""})
	}
	return messages, allText.String(), nil
}

func qoderTransformAssistantMessage(raw json.RawMessage) (any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return map[string]any{"role": "assistant", "content": text}, nil
	}
	var blocks []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("invalid Anthropic assistant content: %w", err)
	}
	var content strings.Builder
	toolCalls := make([]qoderToolCall, 0)
	for _, block := range blocks {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "thinking":
			content.WriteString("<thinking>")
			content.WriteString(block.Thinking)
			content.WriteString("</thinking>\n\n")
		case "tool_use":
			arguments := strings.TrimSpace(string(block.Input))
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			call := qoderToolCall{ID: block.ID, Type: "function"}
			call.Function.Name = block.Name
			call.Function.Arguments = arguments
			toolCalls = append(toolCalls, call)
		}
	}
	message := map[string]any{"role": "assistant"}
	if content.Len() > 0 {
		message["content"] = content.String()
	} else if len(toolCalls) > 0 {
		message["content"] = " "
	} else {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	return message, nil
}

func qoderRawContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var combined strings.Builder
		for _, block := range blocks {
			if block.Type == "text" {
				combined.WriteString(block.Text)
			}
		}
		if combined.Len() > 0 {
			return combined.String()
		}
	}
	return string(raw)
}

func qoderStableID(prefix string, values ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(prefix))
	for _, value := range values {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))[:16]
}

func qoderPrefix(value string, maximum int) string {
	characters := []rune(value)
	if maximum > 0 && len(characters) > maximum {
		characters = characters[:maximum]
	}
	return string(characters)
}
