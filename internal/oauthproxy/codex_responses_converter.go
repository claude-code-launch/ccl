package oauthproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const codexResponsesNameLimit = 64

// codexResponsesConvertedRequest is the CCL-owned wire representation of one
// Anthropic Messages request. No CPA translator or executor participates in it.
type codexResponsesConvertedRequest struct {
	anthropicAdapterRequest
	body      []byte
	model     string
	sessionID string
}

func convertAnthropicToCodexResponses(raw []byte) (*codexResponsesConvertedRequest, error) {
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

	upstreamModel := stripContextModelSuffix(request.Model)
	originalToShort, shortToOriginal := codexToolNameMaps(request.Tools)
	input := make([]any, 0, len(request.Messages)+1)
	if system := codexSystemContent(request.System); len(system) > 0 {
		input = append(input, map[string]any{
			"type": "message", "role": "developer", "content": system,
		})
	}
	for _, message := range request.Messages {
		items, err := codexMessageItems(message, originalToShort)
		if err != nil {
			return nil, err
		}
		input = append(input, items...)
	}

	reasoning := map[string]any{"effort": codexReasoningEffort(request.Thinking, request.OutputConfig)}
	if request.Thinking != nil && !strings.EqualFold(strings.TrimSpace(request.Thinking.Type), "disabled") {
		reasoning["summary"] = "auto"
	}
	body := map[string]any{
		"model":               upstreamModel,
		"instructions":        "",
		"input":               input,
		"parallel_tool_calls": codexParallelToolCalls(request.ToolChoice),
		"reasoning":           reasoning,
		"stream":              true,
		"store":               false,
		"include":             []string{"reasoning.encrypted_content"},
	}
	if tier := codexServiceTier(request.ServiceTier, request.Speed); tier != "" {
		body["service_tier"] = tier
	}
	if len(request.Tools) > 0 {
		tools, err := codexTools(request.Tools, originalToShort)
		if err != nil {
			return nil, err
		}
		body["tools"] = tools
		body["tool_choice"] = codexToolChoice(request.ToolChoice, originalToShort)
	}
	sessionID, promptCacheKey := codexRequestIdentity(request.Metadata)
	body["prompt_cache_key"] = promptCacheKey
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode Codex Responses request: %w", err)
	}
	thinkingEnabled := request.Thinking != nil && !strings.EqualFold(strings.TrimSpace(request.Thinking.Type), "disabled")
	return &codexResponsesConvertedRequest{
		anthropicAdapterRequest: anthropicAdapterRequest{
			upstreamModel:     upstreamModel,
			clientModel:       request.Model,
			stream:            request.Stream,
			thinkingEnabled:   thinkingEnabled,
			thinkingSignature: "ccl-codex-signature-unavailable",
			maxTokens:         request.MaxTokens,
			inputTokens:       estimateApproxTokensBytes(raw),
			toolNameMap:       shortToOriginal,
		},
		body: encoded, model: upstreamModel, sessionID: sessionID,
	}, nil
}

func codexRequestIdentity(metadata *anthropicRequestMetadata) (sessionID, promptCacheKey string) {
	if metadata != nil {
		userID := strings.TrimSpace(metadata.UserID)
		if userID != "" {
			promptCacheKey = userID
			if match := kiroSessionUUIDPattern.FindStringSubmatch(userID); len(match) == 2 {
				sessionID = match[1]
			}
		}
	}
	if sessionID == "" {
		sessionID = uuidString()
	}
	if promptCacheKey == "" {
		promptCacheKey = sessionID
	}
	return sessionID, promptCacheKey
}

func codexSystemContent(raw json.RawMessage) []any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil
		}
		return []any{map[string]any{"type": "input_text", "text": text}}
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	content := make([]any, 0, len(blocks))
	for _, block := range blocks {
		if block["type"] == "text" {
			if value, _ := block["text"].(string); value != "" {
				content = append(content, map[string]any{"type": "input_text", "text": value})
			}
		}
	}
	return content
}

func codexMessageItems(message anthropicMessage, toolNames map[string]string) ([]any, error) {
	role := strings.ToLower(strings.TrimSpace(message.Role))
	if role != "assistant" {
		role = "user"
	}
	var stringContent string
	if json.Unmarshal(message.Content, &stringContent) == nil {
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		return []any{map[string]any{"type": "message", "role": role, "content": []any{
			map[string]any{"type": contentType, "text": stringContent},
		}}}, nil
	}

	var blocks []map[string]any
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil, fmt.Errorf("invalid %s message content: %w", role, err)
	}
	result := make([]any, 0, len(blocks))
	content := make([]any, 0, len(blocks))
	flush := func() {
		if len(content) == 0 {
			return
		}
		result = append(result, map[string]any{"type": "message", "role": role, "content": content})
		content = make([]any, 0, len(blocks))
	}
	for _, block := range blocks {
		kind, _ := block["type"].(string)
		switch kind {
		case "text":
			text, _ := block["text"].(string)
			partType := "input_text"
			if role == "assistant" {
				partType = "output_text"
			}
			content = append(content, map[string]any{"type": partType, "text": text})
		case "image":
			if imageURL := codexDataURL(block["source"]); imageURL != "" {
				content = append(content, map[string]any{"type": "input_image", "image_url": imageURL})
			}
		case "document":
			if fileData := codexPDFDataURL(block["source"]); fileData != "" {
				content = append(content, map[string]any{"type": "input_file", "file_data": fileData, "filename": "document.pdf"})
			}
		case "thinking", "redacted_thinking":
			if role != "assistant" {
				continue
			}
			signature, _ := block["signature"].(string)
			if signature == "" {
				signature, _ = block["data"].(string)
			}
			if codexReplayableSignature(signature) {
				flush()
				result = append(result, map[string]any{"type": "reasoning", "summary": []any{}, "content": nil, "encrypted_content": signature})
			}
		case "tool_use":
			flush()
			name, _ := block["name"].(string)
			if short := toolNames[name]; short != "" {
				name = short
			} else {
				name = codexShortName(name)
			}
			arguments, err := json.Marshal(block["input"])
			if err != nil {
				return nil, fmt.Errorf("encode tool %s input: %w", name, err)
			}
			if string(arguments) == "null" {
				arguments = []byte("{}")
			}
			result = append(result, map[string]any{
				"type": "function_call", "call_id": codexShortCallID(stringValue(block["id"])),
				"name": name, "arguments": string(arguments),
			})
		case "tool_result":
			flush()
			result = append(result, map[string]any{
				"type": "function_call_output", "call_id": codexShortCallID(stringValue(block["tool_use_id"])),
				"output": codexToolResultOutput(block["content"]),
			})
		}
	}
	flush()
	return result, nil
}

func codexDataURL(source any) string {
	value, _ := source.(map[string]any)
	if value == nil {
		return ""
	}
	if strings.EqualFold(stringValue(value["type"]), "url") {
		return stringValue(value["url"])
	}
	data := stringValue(value["data"])
	if data == "" {
		data = stringValue(value["base64"])
	}
	if data == "" {
		return ""
	}
	mediaType := stringValue(value["media_type"])
	if mediaType == "" {
		mediaType = stringValue(value["mime_type"])
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return "data:" + mediaType + ";base64," + data
}

func codexPDFDataURL(source any) string {
	value, _ := source.(map[string]any)
	if value == nil || !strings.EqualFold(stringValue(value["media_type"]), "application/pdf") {
		return ""
	}
	return codexDataURL(source)
}

func codexToolResultOutput(raw any) any {
	if text, ok := raw.(string); ok {
		return text
	}
	blocks, _ := raw.([]any)
	output := make([]any, 0, len(blocks))
	for _, item := range blocks {
		block, _ := item.(map[string]any)
		if block == nil {
			continue
		}
		switch stringValue(block["type"]) {
		case "text":
			output = append(output, map[string]any{"type": "input_text", "text": stringValue(block["text"])})
		case "image":
			if value := codexDataURL(block["source"]); value != "" {
				output = append(output, map[string]any{"type": "input_image", "image_url": value})
			}
		}
	}
	if len(output) == 0 {
		return ""
	}
	return output
}

func codexTools(tools []anthropicTool, names map[string]string) ([]any, error) {
	result := make([]any, 0, len(tools))
	for _, tool := range tools {
		name := names[tool.Name]
		if name == "" {
			name = codexShortName(tool.Name)
		}
		parameters := map[string]any{}
		if len(tool.InputSchema) > 0 && string(tool.InputSchema) != "null" {
			if err := json.Unmarshal(tool.InputSchema, &parameters); err != nil {
				return nil, fmt.Errorf("invalid input_schema for tool %s: %w", tool.Name, err)
			}
		}
		if stringValue(parameters["type"]) == "" {
			parameters["type"] = "object"
		}
		if parameters["type"] == "object" && parameters["properties"] == nil {
			parameters["properties"] = map[string]any{}
		}
		delete(parameters, "$schema")
		result = append(result, map[string]any{
			"type": "function", "name": name, "description": tool.Description,
			"parameters": parameters, "strict": false,
		})
	}
	return result, nil
}

func codexToolNameMaps(tools []anthropicTool) (map[string]string, map[string]string) {
	originalToShort := make(map[string]string, len(tools))
	shortToOriginal := make(map[string]string, len(tools))
	used := make(map[string]bool, len(tools))
	for _, tool := range tools {
		candidate := codexShortName(tool.Name)
		if used[candidate] {
			digest := sha256.Sum256([]byte(tool.Name))
			suffix := "_" + hex.EncodeToString(digest[:4])
			limit := codexResponsesNameLimit - len(suffix)
			if len(candidate) > limit {
				candidate = candidate[:limit]
			}
			candidate += suffix
		}
		used[candidate] = true
		originalToShort[tool.Name] = candidate
		shortToOriginal[candidate] = tool.Name
	}
	return originalToShort, shortToOriginal
}

func codexShortName(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= codexResponsesNameLimit {
		return value
	}
	if strings.HasPrefix(value, "mcp__") {
		if index := strings.LastIndex(value, "__"); index > 0 {
			value = "mcp__" + value[index+2:]
		}
	}
	if len(value) > codexResponsesNameLimit {
		value = value[:codexResponsesNameLimit]
	}
	return value
}

func codexShortCallID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= codexResponsesNameLimit {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	suffix := "_" + hex.EncodeToString(digest[:4])
	return value[:codexResponsesNameLimit-len(suffix)] + suffix
}

func codexToolChoice(raw json.RawMessage, names map[string]string) any {
	if len(raw) == 0 || string(raw) == "null" {
		return "auto"
	}
	var choice map[string]any
	if json.Unmarshal(raw, &choice) != nil {
		return "auto"
	}
	switch strings.ToLower(stringValue(choice["type"])) {
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name := stringValue(choice["name"])
		if short := names[name]; short != "" {
			name = short
		}
		return map[string]any{"type": "function", "name": name}
	default:
		return "auto"
	}
}

func codexParallelToolCalls(raw json.RawMessage) bool {
	var choice map[string]any
	if json.Unmarshal(raw, &choice) == nil {
		if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok {
			return !disabled
		}
	}
	return true
}

func codexReasoningEffort(thinking *anthropicThinking, output *anthropicOutput) string {
	if thinking == nil {
		return "medium"
	}
	switch strings.ToLower(strings.TrimSpace(thinking.Type)) {
	case "disabled":
		return "none"
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
		return "medium"
	}
}

func codexServiceTier(tier, speed string) string {
	if strings.EqualFold(strings.TrimSpace(speed), "fast") {
		return "priority"
	}
	switch normalized := strings.ToLower(strings.TrimSpace(tier)); normalized {
	case "priority", "flex", "default", "auto":
		return normalized
	default:
		return ""
	}
}

func codexReplayableSignature(signature string) bool {
	signature = strings.TrimSpace(signature)
	return signature != "" && signature != "ccl-codex-signature-unavailable"
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
