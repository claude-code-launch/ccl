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
	body              []byte
	model             string
	sessionID         string
	compaction        bool
	compactionReason  string
	compactionSignals string
	sourceMessages    int
	compactionTokens  int
	droppedItems      int
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
	compactionReason, compactionSignals := codexClassifyCompactionRequest(&request)
	compaction := compactionReason != ""

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

	reasoningEffort := codexReasoningEffort(request.Thinking, request.OutputConfig)
	if compaction {
		// Conversation summarization benefits much less from deep reasoning than a
		// normal coding turn. Avoid spending latency and output tokens on it.
		reasoningEffort = "low"
	}
	reasoning := map[string]any{"effort": reasoningEffort}
	if !compaction && request.Thinking != nil && !strings.EqualFold(strings.TrimSpace(request.Thinking.Type), "disabled") {
		reasoning["summary"] = "auto"
	}
	body := map[string]any{
		"model":               upstreamModel,
		"instructions":        "",
		"input":               input,
		"parallel_tool_calls": !compaction && codexParallelToolCalls(request.ToolChoice),
		"reasoning":           reasoning,
		"stream":              true,
		"store":               false,
		"include":             []string{"reasoning.encrypted_content"},
	}
	if tier := codexServiceTier(request.ServiceTier, request.Speed); tier != "" {
		body["service_tier"] = tier
	}
	if len(request.Tools) > 0 && !compaction {
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
		compaction: compaction, compactionReason: compactionReason,
		compactionSignals: compactionSignals, sourceMessages: len(request.Messages),
	}, nil
}

// codexClassifyCompactionRequest returns both the positive match reason and a
// privacy-safe list of prompt signals. The signals make prompt drift visible in
// DEBUG logs without recording the user's conversation text.
func codexClassifyCompactionRequest(request *anthropicMessagesRequest) (reason, signals string) {
	if request == nil {
		return "", "none"
	}
	if codexIsCompactionSystem(request.System) {
		return "system_summarizer", "system_summarizer"
	}
	// Claude Code 2.1.223 keeps its normal system prompt and puts the internal
	// compaction instructions in one of the final user messages. Require two
	// stable prompt phrases so a normal discussion about summaries cannot enter
	// the destructive history-trimming path.
	first := len(request.Messages) - 3
	if first < 0 {
		first = 0
	}
	seen := make(map[string]bool)
	addSignal := func(name string, matched bool) {
		if matched {
			seen[name] = true
		}
	}
	for index := len(request.Messages) - 1; index >= first; index-- {
		message := request.Messages[index]
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		content := strings.ToLower(string(message.Content))
		legacyDetailed := strings.Contains(content, "your task is to create a detailed summary of this conversation")
		legacyContinuation := strings.Contains(content, "this summary will be placed at the start of a continuing session")
		legacyTranscript := strings.Contains(content, "summarize this portion of a claude code session transcript")
		focusMarker := strings.Contains(content, "focus on:")
		sectionsMarker := strings.Contains(content, "your summary should include the following sections")
		primaryRequestMarker := strings.Contains(content, "primary request and intent:")
		plainTextMarker := strings.Contains(content, "respond with plain text only")
		additionalInstructionsMarker := strings.Contains(content, "additional summarization instructions")
		noToolsMarker := strings.Contains(content, "do not call any tools")
		compactCommandMarker := strings.Contains(content, "<command-name>/compact</command-name>") ||
			strings.Contains(content, `\u003ccommand-name\u003e/compact\u003c/command-name\u003e`)
		addSignal("legacy_detailed", legacyDetailed)
		addSignal("legacy_continuation", legacyContinuation)
		addSignal("legacy_transcript", legacyTranscript)
		addSignal("focus", focusMarker)
		addSignal("summary_sections", sectionsMarker)
		addSignal("primary_request", primaryRequestMarker)
		addSignal("plain_text_only", plainTextMarker)
		addSignal("additional_instructions", additionalInstructionsMarker)
		addSignal("no_tools", noToolsMarker)
		addSignal("compact_command", compactCommandMarker)
		if legacyDetailed && legacyContinuation {
			return "user_detailed_summary", joinCodexCompactionSignals(seen)
		}
		if legacyTranscript && focusMarker {
			return "user_transcript_summary", joinCodexCompactionSignals(seen)
		}
		// Claude Code prompt wording changes across builds. These three
		// structural labels describe the private compact protocol rather than a
		// normal user request to summarize text.
		if sectionsMarker && primaryRequestMarker && plainTextMarker &&
			(additionalInstructionsMarker || noToolsMarker) {
			return "user_structured_summary", joinCodexCompactionSignals(seen)
		}
	}
	return "", joinCodexCompactionSignals(seen)
}

func joinCodexCompactionSignals(seen map[string]bool) string {
	order := []string{
		"system_summarizer", "legacy_detailed", "legacy_continuation", "legacy_transcript", "focus",
		"summary_sections", "primary_request", "plain_text_only", "additional_instructions", "no_tools", "compact_command",
	}
	matched := make([]string, 0, len(seen))
	for _, name := range order {
		if seen[name] {
			matched = append(matched, name)
		}
	}
	if len(matched) == 0 {
		return "none"
	}
	return strings.Join(matched, ",")
}

func codexRequestFingerprint(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:6])
}

func codexIsCompactionSystem(raw json.RawMessage) bool {
	const marker = "tasked with summarizing conversations"
	for _, item := range codexSystemContent(raw) {
		block, _ := item.(map[string]any)
		if strings.Contains(strings.ToLower(stringValue(block["text"])), marker) {
			return true
		}
	}
	return false
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
