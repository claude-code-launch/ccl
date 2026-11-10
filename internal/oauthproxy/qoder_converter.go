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
	adapter     *adapterRequest
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

func convertResponsesToQoder(raw []byte, catalog []qoderModel) (*qoderConvertedRequest, error) {
	request, err := parseResponsesRequest(raw)
	if err != nil {
		return nil, err
	}
	model, ok := qoderSelectModel(request.Model, catalog)
	if !ok {
		return nil, fmt.Errorf("Qoder has no available models")
	}
	messages, lastUser := qoderTransformTurns(request.Turns)
	if system := strings.TrimSpace(request.System); system != "" {
		messages = append([]any{map[string]any{"role": "system", "content": system}}, messages...)
	}
	tools, err := qoderTransformTools(request.Tools)
	if err != nil {
		return nil, err
	}
	maxTokens := request.MaxOutputTokens
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
	parameters := map[string]any{"max_tokens": maxTokens}
	if levels, _ := qoderReasoningLevels(model.Config); len(levels) > 0 {
		thinkingOff := strings.EqualFold(request.Effort, "none")
		if matched := qoderMatchEffort(levels, request.Effort); matched != "" && !thinkingOff {
			parameters["enable_thinking"] = true
			parameters["reasoning_effort"] = matched
		}
	}
	recordID := qoderStableID("qoder-record", request.Model, string(raw), fmt.Sprintf("mt=%d", maxTokens))
	sessionSeed := firstNonEmptyString(strings.TrimSpace(request.CacheKey), strings.TrimSpace(request.User), uuid.NewString())
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
		"parameters":       parameters,
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
	adapter := request.adapter(model.ID)
	adapter.maxTokens = maxTokens
	adapter.toolNameMap = map[string]string{}
	return &qoderConvertedRequest{
		clientModel: request.Model,
		model:       model,
		stream:      request.Stream,
		body:        body,
		adapter:     &adapter,
		inputTokens: adapter.inputTokens,
	}, nil
}

func qoderSelectModel(requested string, catalog []qoderModel) (qoderModel, bool) {
	base := stripContextModelSuffix(strings.TrimSpace(requested))
	for _, model := range catalog {
		if strings.EqualFold(model.ID, base) {
			return model, true
		}
	}
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

func qoderTransformTools(tools []conversationTool) ([]any, error) {
	transformed := make([]any, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			return nil, fmt.Errorf("tool name is required")
		}
		var parameters any = map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.Parameters) > 0 && string(tool.Parameters) != "null" {
			if err := json.Unmarshal(tool.Parameters, &parameters); err != nil {
				return nil, fmt.Errorf("tool %s has invalid parameters: %w", name, err)
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

func qoderTransformTurns(turns []conversationTurn) ([]any, string) {
	transformed := make([]any, 0, len(turns)+2)
	lastUser := ""
	for _, turn := range turns {
		switch turn.Role {
		case "user":
			mapped, userText := qoderTransformUserTurn(turn)
			transformed = append(transformed, mapped...)
			if strings.TrimSpace(userText) != "" {
				lastUser = userText
			}
		case "developer", "system":
			var texts []string
			for _, part := range turn.Parts {
				if part.Kind == partText && strings.TrimSpace(part.Text) != "" {
					texts = append(texts, part.Text)
				}
			}
			if len(texts) > 0 {
				transformed = append(transformed, map[string]any{
					"role":    "user",
					"content": strings.Join(texts, "\n"),
				})
			}
		case "assistant":
			transformed = append(transformed, qoderTransformAssistantTurn(turn))
		}
	}
	return transformed, lastUser
}

func qoderTransformUserTurn(turn conversationTurn) ([]any, string) {
	messages := make([]any, 0, len(turn.Parts))
	parts := make([]any, 0, len(turn.Parts))
	var toolImages []any
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
	for _, part := range turn.Parts {
		switch part.Kind {
		case partText:
			parts = append(parts, map[string]any{"type": "text", "text": part.Text})
			allText.WriteString(part.Text)
		case partImage:
			if imageURL := part.imageURL(); imageURL != "" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
			}
		case partToolResult:
			flushUser()
			content := part.outputText()
			var toolText []string
			hasImage := false
			// Tool messages carry text. Preserve screenshots as a subsequent
			// user message, after all tool replies, like the Chat adapter.
			for _, sub := range part.Output {
				if sub.Kind == partText {
					toolText = append(toolText, sub.Text)
				}
				if sub.Kind == partImage {
					if imageURL := sub.imageURL(); imageURL != "" {
						hasImage = true
						toolText = append(toolText, "[image attached in the following user message]")
						toolImages = append(toolImages, map[string]any{"type": "text", "text": "Image from tool result " + part.CallID + ":"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
					}
				}
			}
			if hasImage {
				content = strings.Join(toolText, "\n")
			}
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": part.CallID,
				"content":      content,
			})
		}
	}
	parts = append(parts, toolImages...)
	flushUser()
	if len(messages) == 0 {
		messages = append(messages, map[string]any{"role": "user", "content": ""})
	}
	return messages, allText.String()
}

func qoderTransformAssistantTurn(turn conversationTurn) any {
	var content strings.Builder
	toolCalls := make([]qoderToolCall, 0)
	for _, part := range turn.Parts {
		switch part.Kind {
		case partText:
			content.WriteString(part.Text)
		case partReasoning:
			content.WriteString("<thinking>")
			content.WriteString(part.Text)
			content.WriteString("</thinking>\n\n")
		case partToolCall:
			arguments := strings.TrimSpace(string(part.Arguments))
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			call := qoderToolCall{ID: part.CallID, Type: "function"}
			call.Function.Name = part.Name
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
	return message
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
