package oauthproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	kiroMaxToolNameLength      = 63
	kiroMaxInlineMediaSegments = 100
)

var (
	kiroSessionUUIDPattern = regexp.MustCompile(`(?i)session_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
	kiroModernModelPattern = regexp.MustCompile(`(?i)^claude-(sonnet|opus|haiku)-([0-9]+)[-.]([0-9]+)(?:-[0-9]{8})?(?:-thinking|-latest)?$`)
	kiroLegacyModelPattern = regexp.MustCompile(`(?i)^claude-([0-9]+)-([0-9]+)-(sonnet|opus|haiku)(?:-[0-9]{8})?(?:-thinking|-latest)?$`)
)

type anthropicMessagesRequest struct {
	Model        string                    `json:"model"`
	MaxTokens    int                       `json:"max_tokens"`
	Messages     []anthropicMessage        `json:"messages"`
	Stream       bool                      `json:"stream"`
	System       json.RawMessage           `json:"system"`
	Tools        []anthropicTool           `json:"tools"`
	ToolChoice   json.RawMessage           `json:"tool_choice"`
	Thinking     *anthropicThinking        `json:"thinking"`
	OutputConfig *anthropicOutput          `json:"output_config"`
	Metadata     *anthropicRequestMetadata `json:"metadata"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicOutput struct {
	Effort string `json:"effort"`
}

type anthropicRequestMetadata struct {
	UserID string `json:"user_id"`
}

// anthropicAdapterRequest is the client-facing state shared by direct
// Anthropic-compatible adapters. Upstream-specific converted requests embed it.
type anthropicAdapterRequest struct {
	upstreamModel   string
	clientModel     string
	stream          bool
	thinkingEnabled bool
	// thinkingSignature identifies synthetic reasoning signatures emitted by
	// direct adapters when the upstream does not provide an Anthropic signature.
	thinkingSignature string
	maxTokens         int
	inputTokens       int
	toolNameMap       map[string]string
}

type kiroConvertedRequest struct {
	anthropicAdapterRequest
	body            map[string]any
	model           string
	inlineMedia     int
	droppedMedia    int
	dedupedMedia    int
	resizedMedia    int
	correctedMedia  int
	droppedToolUses int
	droppedToolRuns int
	emptyToolRuns   int
	truncatedTexts  int
	droppedText     int
	largestText     int
	originalBody    int
	finalBody       int
	originalTokens  int
	finalTokens     int
	budgetMedia     int
	budgetHistory   int
	budgetTexts     int
	budgetTextBytes int
	budgetTools     int
}

type kiroContent struct {
	text        string
	thinking    string
	images      []any
	toolResults []any
	toolUses    []any
	toolNames   []string
	// emptyResults counts tool results that arrived with no content at all.
	emptyResults int
}

// kiroEmptyToolResultText stands in for a tool result that carried no content.
//
// Forwarding an empty string makes a tool that legitimately produced no output
// indistinguishable from a result that was lost on the way, and the model then
// reports the tooling as broken instead of continuing.
const kiroEmptyToolResultText = "(no output)"

func convertAnthropicToKiro(raw []byte) (*kiroConvertedRequest, error) {
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
	if request.MaxTokens <= 0 {
		request.MaxTokens = 32_000
	}

	lastUser := -1
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if strings.EqualFold(strings.TrimSpace(request.Messages[index].Role), "user") {
			lastUser = index
			break
		}
	}
	if lastUser < 0 {
		return nil, fmt.Errorf("messages must contain a user message")
	}
	messages := request.Messages[:lastUser+1]
	model := mapKiroModel(request.Model)
	toolNameMap := make(map[string]string)
	tools, declaredTools, err := convertKiroTools(request.Tools, toolNameMap)
	if err != nil {
		return nil, err
	}

	thinkingEnabled := request.Thinking != nil &&
		(strings.EqualFold(request.Thinking.Type, "enabled") || strings.EqualFold(request.Thinking.Type, "adaptive"))
	history := make([]any, 0, len(messages)+2)
	systemText := extractKiroSystem(request.System)
	if thinkingEnabled {
		prefix := kiroThinkingPrefix(request.Thinking, request.OutputConfig)
		systemText = strings.TrimSpace(prefix + "\n" + systemText)
	}
	if systemText != "" {
		history = append(history,
			map[string]any{"userInputMessage": map[string]any{
				"content": systemText,
				"modelId": model,
				"origin":  "AI_EDITOR",
			}},
			map[string]any{"assistantResponseMessage": map[string]any{
				"content": "I will follow these instructions.",
			}},
		)
	}

	historicalToolNames := make(map[string]bool)
	emptyToolRuns := 0
	for _, message := range messages[:len(messages)-1] {
		content, err := parseKiroContent(message.Content, toolNameMap)
		if err != nil {
			return nil, err
		}
		emptyToolRuns += content.emptyResults
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "user":
			userMessage := map[string]any{
				"content": content.text,
				"modelId": model,
				"origin":  "AI_EDITOR",
			}
			if len(content.images) > 0 {
				userMessage["images"] = content.images
			}
			if len(content.toolResults) > 0 {
				userMessage["userInputMessageContext"] = map[string]any{
					"envState":    kiroEnvironmentState(),
					"toolResults": content.toolResults,
				}
			}
			history = append(history, map[string]any{"userInputMessage": userMessage})
		case "assistant":
			assistantText := content.text
			if content.thinking != "" {
				assistantText = strings.TrimSpace("<thinking>" + content.thinking + "</thinking>\n\n" + assistantText)
			}
			if assistantText == "" && len(content.toolUses) > 0 {
				assistantText = " "
			}
			assistantMessage := map[string]any{"content": assistantText}
			if len(content.toolUses) > 0 {
				assistantMessage["toolUses"] = content.toolUses
			}
			for _, name := range content.toolNames {
				historicalToolNames[name] = true
			}
			history = append(history, map[string]any{"assistantResponseMessage": assistantMessage})
		}
	}
	history = normalizeKiroHistory(history)

	currentContent, err := parseKiroContent(messages[len(messages)-1].Content, toolNameMap)
	if err != nil {
		return nil, err
	}
	emptyToolRuns += currentContent.emptyResults
	for name := range historicalToolNames {
		if declaredTools[name] {
			continue
		}
		tools = append(tools, kiroToolDefinition(name, "Tool used in conversation history", map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"required":             []any{},
			"additionalProperties": true,
		}))
	}

	contextValue := map[string]any{
		"envState": kiroEnvironmentState(),
	}
	if len(tools) > 0 {
		contextValue["tools"] = tools
	}
	if len(currentContent.toolResults) > 0 {
		contextValue["toolResults"] = currentContent.toolResults
	}
	currentMessage := map[string]any{
		"content":                 currentContent.text,
		"modelId":                 model,
		"origin":                  "AI_EDITOR",
		"userInputMessageContext": contextValue,
	}
	if len(currentContent.images) > 0 {
		currentMessage["images"] = currentContent.images
	}

	conversationState := map[string]any{
		"agentContinuationId": uuid.NewString(),
		"agentTaskType":       "vibe",
		"chatTriggerType":     "MANUAL",
		"conversationId":      kiroConversationID(request.Metadata),
		"currentMessage":      map[string]any{"userInputMessage": currentMessage},
	}
	if len(history) > 0 {
		conversationState["history"] = history
	}
	droppedToolUses, droppedToolRuns := normalizeKiroToolPairing(conversationState)
	textStats := limitKiroTextFields(conversationState, kiroMaxTextFieldBytes)
	dedupedMedia := deduplicateKiroInlineMedia(conversationState)
	inlineMedia, droppedMedia := limitKiroInlineMedia(conversationState, kiroMaxInlineMediaSegments)
	resizedMedia, correctedMedia := processKiroInlineMedia(conversationState)
	body := map[string]any{"conversationState": conversationState}
	if additional := kiroReasoningFields(&request, model); additional != nil {
		body["additionalModelRequestFields"] = additional
	}
	protectedHistoryPrefix := 0
	if systemText != "" {
		protectedHistoryPrefix = 2
	}
	budgetStats := enforceKiroContentBudget(body, protectedHistoryPrefix, kiroMaxEstimatedContentTokens)
	if err := validateKiroConversationState(conversationState); err != nil {
		return nil, err
	}
	inlineMedia = countKiroInlineMedia(conversationState)
	return &kiroConvertedRequest{
		anthropicAdapterRequest: anthropicAdapterRequest{
			upstreamModel:   model,
			clientModel:     request.Model,
			stream:          request.Stream,
			thinkingEnabled: thinkingEnabled,
			maxTokens:       request.MaxTokens,
			inputTokens:     estimateApproxTokensBytes(raw),
			toolNameMap:     toolNameMap,
		},
		body:            body,
		model:           model,
		inlineMedia:     inlineMedia,
		droppedMedia:    droppedMedia,
		dedupedMedia:    dedupedMedia,
		resizedMedia:    resizedMedia,
		correctedMedia:  correctedMedia,
		droppedToolUses: droppedToolUses,
		droppedToolRuns: droppedToolRuns,
		emptyToolRuns:   emptyToolRuns,
		truncatedTexts:  textStats.truncated,
		droppedText:     textStats.droppedBytes,
		largestText:     textStats.largestBytes,
		originalBody:    budgetStats.originalBytes,
		finalBody:       budgetStats.finalBytes,
		originalTokens:  budgetStats.originalTokens,
		finalTokens:     budgetStats.finalTokens,
		budgetMedia:     budgetStats.droppedImages,
		budgetHistory:   budgetStats.droppedHistoryMessages,
		budgetTexts:     budgetStats.truncatedTexts,
		budgetTextBytes: budgetStats.droppedTextBytes,
		budgetTools:     budgetStats.droppedTools,
	}, nil
}

func countKiroInlineMedia(conversationState map[string]any) int {
	count := 0
	for _, message := range kiroUserMessagesNewestFirst(conversationState) {
		count += len(kiroAnySlice(message["images"]))
	}
	return count
}

// limitKiroInlineMedia applies Kiro's request-wide inline media limit. Images in
// the current message are most relevant, followed by images in the newest
// historical user messages. Within one message, the newest images are retained.
func limitKiroInlineMedia(conversationState map[string]any, limit int) (kept, dropped int) {
	remaining := max(limit, 0)
	for _, message := range kiroUserMessagesNewestFirst(conversationState) {
		images, ok := message["images"].([]any)
		if !ok || len(images) == 0 {
			delete(message, "images")
			continue
		}
		switch {
		case remaining == 0:
			dropped += len(images)
			delete(message, "images")
		case len(images) > remaining:
			dropped += len(images) - remaining
			message["images"] = append([]any(nil), images[len(images)-remaining:]...)
			kept += remaining
			remaining = 0
		default:
			kept += len(images)
			remaining -= len(images)
		}
	}
	return kept, dropped
}

func mapKiroModel(model string) string {
	model = stripContextModelSuffix(strings.TrimSpace(model))
	model = strings.TrimPrefix(strings.ToLower(model), "kiro-")
	if match := kiroModernModelPattern.FindStringSubmatch(model); len(match) == 4 {
		return fmt.Sprintf("claude-%s-%s.%s", strings.ToLower(match[1]), match[2], match[3])
	}
	if match := kiroLegacyModelPattern.FindStringSubmatch(model); len(match) == 4 {
		return fmt.Sprintf("claude-%s-%s.%s", strings.ToLower(match[3]), match[1], match[2])
	}
	model = strings.TrimSuffix(model, "-thinking")
	model = strings.TrimSuffix(model, "-latest")
	if len(model) > 9 {
		if suffix := model[len(model)-8:]; allKiroDigits(suffix) && model[len(model)-9] == '-' {
			model = model[:len(model)-9]
		}
	}
	return model
}

func allKiroDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return value != ""
}

func extractKiroSystem(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func kiroThinkingPrefix(thinking *anthropicThinking, output *anthropicOutput) string {
	if thinking == nil {
		return ""
	}
	if strings.EqualFold(thinking.Type, "adaptive") {
		effort := "high"
		if output != nil && strings.TrimSpace(output.Effort) != "" {
			effort = normalizeKiroEffort(output.Effort)
		}
		return "<thinking_mode>adaptive</thinking_mode><thinking_effort>" + effort + "</thinking_effort>"
	}
	budget := thinking.BudgetTokens
	if budget <= 0 {
		budget = 20_000
	}
	if budget > 24_576 {
		budget = 24_576
	}
	return "<thinking_mode>enabled</thinking_mode><max_thinking_length>" + strconv.Itoa(budget) + "</max_thinking_length>"
}

func kiroReasoningFields(request *anthropicMessagesRequest, model string) map[string]any {
	if request.Thinking != nil && strings.EqualFold(request.Thinking.Type, "disabled") {
		return nil
	}
	supported := model == "claude-opus-4.6" || model == "claude-sonnet-4.6"
	if !supported {
		return nil
	}
	if model == "claude-opus-4.6" &&
		(request.Thinking == nil || !strings.EqualFold(request.Thinking.Type, "adaptive")) {
		return nil
	}
	if request.Thinking == nil && (request.OutputConfig == nil || strings.TrimSpace(request.OutputConfig.Effort) == "") {
		return nil
	}
	effort := "high"
	if request.OutputConfig != nil && strings.TrimSpace(request.OutputConfig.Effort) != "" {
		effort = normalizeKiroEffort(request.OutputConfig.Effort)
	} else if request.Thinking != nil {
		switch {
		case request.Thinking.BudgetTokens <= 4_000:
			effort = "low"
		case request.Thinking.BudgetTokens <= 16_000:
			effort = "medium"
		default:
			effort = "high"
		}
	}
	return map[string]any{"output_config": map[string]any{"effort": effort}}
}

func normalizeKiroEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	case "xhigh", "x-high", "x_high":
		return "high"
	default:
		return "high"
	}
}

func convertKiroTools(input []anthropicTool, nameMap map[string]string) ([]any, map[string]bool, error) {
	tools := make([]any, 0, len(input))
	declared := make(map[string]bool, len(input))
	for _, tool := range input {
		name := shortenKiroToolName(strings.TrimSpace(tool.Name), nameMap)
		if name == "" {
			continue
		}
		description := strings.TrimSpace(tool.Description)
		if description == "" {
			description = tool.Name
		}
		if len(description) > 10_000 {
			description = description[:10_000]
		}
		var schema any
		if len(tool.InputSchema) == 0 || json.Unmarshal(tool.InputSchema, &schema) != nil {
			schema = map[string]any{}
		}
		normalized := normalizeKiroSchema(schema, true)
		tools = append(tools, kiroToolDefinition(name, description, normalized))
		declared[name] = true
	}
	return tools, declared, nil
}

func kiroToolDefinition(name, description string, schema any) map[string]any {
	return map[string]any{"toolSpecification": map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": map[string]any{"json": schema},
	}}
}

func normalizeKiroSchema(value any, root bool) any {
	object, ok := value.(map[string]any)
	if !ok {
		if root {
			return map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"required":             []any{},
				"additionalProperties": true,
			}
		}
		return value
	}
	delete(object, "$schema")
	if root {
		if _, hasProperties := object["properties"]; !hasProperties {
			for _, key := range []string{"oneOf", "anyOf", "allOf"} {
				variants, _ := object[key].([]any)
				for _, variant := range variants {
					candidate, _ := variant.(map[string]any)
					if candidate["type"] != "object" {
						continue
					}
					for _, field := range []string{"properties", "required", "additionalProperties", "description"} {
						if value, exists := candidate[field]; exists {
							object[field] = value
						}
					}
					break
				}
				if _, hasProperties := object["properties"]; hasProperties {
					break
				}
			}
		}
		for _, key := range []string{"oneOf", "anyOf", "allOf"} {
			delete(object, key)
		}
		object["type"] = "object"
		if _, ok := object["properties"].(map[string]any); !ok {
			object["properties"] = map[string]any{}
		}
		if _, ok := object["required"].([]any); !ok {
			object["required"] = []any{}
		}
		if _, ok := object["additionalProperties"]; !ok {
			object["additionalProperties"] = true
		}
	} else {
		for _, key := range []string{"exclusiveMinimum", "exclusiveMaximum"} {
			if _, isNumber := object[key].(float64); isNumber {
				delete(object, key)
			}
		}
		for _, key := range []string{"minimum", "maximum"} {
			if value, isNumber := object[key].(float64); isNumber &&
				(value > 2_147_483_647 || value < -2_147_483_648) {
				delete(object, key)
			}
		}
	}
	if properties, ok := object["properties"].(map[string]any); ok {
		for key, property := range properties {
			properties[key] = normalizeKiroSchema(property, false)
		}
	}
	if items, ok := object["items"]; ok {
		object["items"] = normalizeKiroSchema(items, false)
	}
	return object
}

func shortenKiroToolName(name string, nameMap map[string]string) string {
	if len(name) <= kiroMaxToolNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:4])
	prefixLength := kiroMaxToolNameLength - len(suffix) - 1
	short := name[:prefixLength] + "_" + suffix
	nameMap[short] = name
	return short
}

func parseKiroContent(raw json.RawMessage, nameMap map[string]string) (kiroContent, error) {
	var direct string
	if json.Unmarshal(raw, &direct) == nil {
		return kiroContent{text: direct}, nil
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return kiroContent{}, fmt.Errorf("message content must be a string or content block array")
	}
	var result kiroContent
	var textParts []string
	var thinkingParts []string
	for _, block := range blocks {
		switch strings.ToLower(metadataString(block, "type")) {
		case "text":
			if text := metadataString(block, "text"); text != "" {
				textParts = append(textParts, text)
			}
		case "thinking":
			if thinking := metadataString(block, "thinking"); thinking != "" {
				thinkingParts = append(thinkingParts, thinking)
			}
		case "image":
			if image := kiroImageFromBlock(block); image != nil {
				result.images = append(result.images, image)
			}
		case "tool_result":
			toolUseID := metadataString(block, "tool_use_id")
			if toolUseID == "" {
				continue
			}
			resultText, resultImages := extractKiroToolResult(block["content"])
			if resultText == "" && len(resultImages) == 0 {
				resultText = kiroEmptyToolResultText
				result.emptyResults++
			}
			result.images = append(result.images, resultImages...)
			isError, _ := block["is_error"].(bool)
			status := "success"
			if isError {
				status = "error"
			}
			toolResult := map[string]any{
				"toolUseId": toolUseID,
				"content":   []any{map[string]any{"text": resultText}},
				"status":    status,
			}
			if isError {
				toolResult["isError"] = true
			}
			result.toolResults = append(result.toolResults, toolResult)
		case "tool_use":
			id := metadataString(block, "id")
			name := shortenKiroToolName(metadataString(block, "name"), nameMap)
			if id == "" || name == "" {
				continue
			}
			input := block["input"]
			if input == nil {
				input = map[string]any{}
			}
			result.toolUses = append(result.toolUses, map[string]any{
				"toolUseId": id,
				"name":      name,
				"input":     input,
			})
			result.toolNames = append(result.toolNames, name)
		}
	}
	result.text = strings.Join(textParts, "\n")
	result.thinking = strings.Join(thinkingParts, "")
	return result, nil
}

func extractKiroToolResult(value any) (string, []any) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case []any:
		parts := make([]string, 0, len(typed))
		var images []any
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text := metadataString(block, "text"); text != "" {
				parts = append(parts, text)
			}
			if strings.EqualFold(metadataString(block, "type"), "image") {
				if image := kiroImageFromBlock(block); image != nil {
					images = append(images, image)
				}
			}
		}
		text := strings.Join(parts, "\n")
		if text == "" && len(images) > 0 {
			text = "[image attached]"
		}
		return text, images
	case nil:
		return "", nil
	default:
		raw, _ := json.Marshal(typed)
		return string(raw), nil
	}
}

func kiroImageFromBlock(block map[string]any) any {
	source, _ := block["source"].(map[string]any)
	mediaType := metadataString(source, "media_type")
	format := strings.TrimPrefix(mediaType, "image/")
	if format == "jpg" {
		format = "jpeg"
	}
	data := metadataString(source, "data")
	if data == "" || (format != "jpeg" && format != "png" && format != "gif" && format != "webp") {
		return nil
	}
	return map[string]any{
		"format": format,
		"source": map[string]any{"bytes": data},
	}
}

func kiroConversationID(metadata *anthropicRequestMetadata) string {
	if metadata != nil {
		if match := kiroSessionUUIDPattern.FindStringSubmatch(metadata.UserID); len(match) == 2 {
			return match[1]
		}
		var value struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(metadata.UserID), &value) == nil {
			if _, err := uuid.Parse(value.SessionID); err == nil {
				return value.SessionID
			}
		}
	}
	return uuid.NewString()
}

func kiroWorkingDirectory() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "/"
	}
	return cwd
}

func kiroOperatingSystem() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	default:
		return runtime.GOOS
	}
}

func kiroEnvironmentState() map[string]any {
	return map[string]any{
		"operatingSystem":         kiroOperatingSystem(),
		"currentWorkingDirectory": kiroWorkingDirectory(),
	}
}

// estimateApproxTokens approximates the token count of text as one token per four
// runes. Counting runes in place keeps this allocation-free, which matters both
// for whole request bodies and for the per-delta calls on the streaming path.
func estimateApproxTokens(text string) int {
	if text == "" {
		return 0
	}
	return (utf8.RuneCountInString(text) + 3) / 4
}

// estimateApproxTokensBytes is estimateApproxTokens without forcing a []byte to
// string copy of the payload.
func estimateApproxTokensBytes(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	return (utf8.RuneCount(raw) + 3) / 4
}
