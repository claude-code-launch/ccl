package oauthproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/google/uuid"
)

const (
	kiroMaxToolNameLength      = 63
	kiroMaxInlineMediaSegments = 100
)

var (
	kiroSessionUUIDPattern = regexp.MustCompile(`(?i)session_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
	kiroModernModelPattern = regexp.MustCompile(`(?i)^claude-(sonnet|opus|haiku|fable)-([0-9]+)[-.]([0-9]+)(?:-[0-9]{8})?(?:-thinking|-latest)?$`)
	kiroLegacyModelPattern = regexp.MustCompile(`(?i)^claude-([0-9]+)-([0-9]+)-(sonnet|opus|haiku|fable)(?:-[0-9]{8})?(?:-thinking|-latest)?$`)
)

type kiroConvertedRequest struct {
	adapterRequest
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

func convertResponsesToKiro(raw []byte) (*kiroConvertedRequest, error) {
	request, err := parseResponsesRequest(raw)
	if err != nil {
		return nil, err
	}
	if request.MaxOutputTokens <= 0 {
		request.MaxOutputTokens = 32_000
	}

	lastUser := -1
	for index := len(request.Turns) - 1; index >= 0; index-- {
		if request.Turns[index].Role == "user" {
			lastUser = index
			break
		}
	}
	if lastUser < 0 {
		return nil, fmt.Errorf("input must contain a user message")
	}
	messages := request.Turns[:lastUser+1]
	model := mapKiroModel(request.Model)
	toolNameMap := make(map[string]string)
	tools, declaredTools, err := convertKiroTools(request.Tools, toolNameMap)
	if err != nil {
		return nil, err
	}

	thinkingEnabled := request.thinkingEnabled()
	history := make([]any, 0, len(messages)+2)
	systemText := strings.TrimSpace(request.System)
	if thinkingEnabled {
		prefix := kiroThinkingPrefix(request.Effort)
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
		content := parseKiroContent(message, toolNameMap)
		emptyToolRuns += content.emptyResults
		switch message.Role {
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

	currentContent := parseKiroContent(messages[len(messages)-1], toolNameMap)
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
		"conversationId":      kiroConversationID(request.CacheKey),
		"currentMessage":      map[string]any{"userInputMessage": currentMessage},
	}
	if len(history) > 0 {
		conversationState["history"] = history
	}
	droppedToolUses, droppedToolRuns := normalizeKiroToolPairing(conversationState)
	textStats := limitKiroTextFields(conversationState, kiroMaxTextFieldBytes)
	dedupedMedia := deduplicateKiroInlineMedia(conversationState)
	_, droppedMedia := limitKiroInlineMedia(conversationState, kiroMaxInlineMediaSegments)
	resizedMedia, correctedMedia := processKiroInlineMedia(conversationState)
	body := map[string]any{"conversationState": conversationState}
	if additional := kiroReasoningFields(request, model); additional != nil {
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
	inlineMedia := countKiroInlineMedia(conversationState)
	return &kiroConvertedRequest{
		adapterRequest:  kiroAdapter(request, model, toolNameMap),
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

func kiroAdapter(request *conversationRequest, model string, toolNameMap map[string]string) adapterRequest {
	adapter := request.adapter(model)
	adapter.toolNameMap = toolNameMap
	return adapter
}

// kiroThinkingPrefix asks for adaptive reasoning at the requested effort.
func kiroThinkingPrefix(effort string) string {
	return "<thinking_mode>adaptive</thinking_mode><thinking_effort>" + normalizeKiroEffort(effort) + "</thinking_effort>"
}

func kiroReasoningFields(request *conversationRequest, model string) map[string]any {
	if !request.thinkingEnabled() {
		return nil
	}
	if model != "claude-opus-4.6" && model != "claude-sonnet-4.6" {
		return nil
	}
	return map[string]any{"output_config": map[string]any{"effort": normalizeKiroEffort(request.Effort)}}
}

func normalizeKiroEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	case "minimal":
		return "low"
	case "ultra":
		return "max"
	case "xhigh", "x-high", "x_high":
		return "high"
	default:
		return "high"
	}
}

func convertKiroTools(input []conversationTool, nameMap map[string]string) ([]any, map[string]bool, error) {
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
		if len(tool.Parameters) == 0 || json.Unmarshal(tool.Parameters, &schema) != nil {
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

func parseKiroContent(turn conversationTurn, nameMap map[string]string) kiroContent {
	var result kiroContent
	var textParts []string
	var thinkingParts []string
	for _, part := range turn.Parts {
		switch part.Kind {
		case partText:
			if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
		case partReasoning:
			if part.Text != "" {
				thinkingParts = append(thinkingParts, part.Text)
			}
		case partImage:
			if image := kiroImageFromPart(part); image != nil {
				result.images = append(result.images, image)
			}
		case partToolResult:
			if part.CallID == "" {
				continue
			}
			resultText := part.outputText()
			var resultImages []any
			for _, piece := range part.outputImages() {
				if image := kiroImageFromPart(piece); image != nil {
					resultImages = append(resultImages, image)
				}
			}
			if resultText == "" && len(resultImages) > 0 {
				resultText = "[image attached]"
			}
			if resultText == "" && len(resultImages) == 0 {
				resultText = kiroEmptyToolResultText
				result.emptyResults++
			}
			result.images = append(result.images, resultImages...)
			result.toolResults = append(result.toolResults, map[string]any{
				"toolUseId": part.CallID,
				"content":   []any{map[string]any{"text": resultText}},
				"status":    "success",
			})
		case partToolCall:
			name := shortenKiroToolName(part.Name, nameMap)
			if part.CallID == "" || name == "" {
				continue
			}
			var input any = map[string]any{}
			if len(part.Arguments) > 0 {
				var decoded map[string]any
				if json.Unmarshal(part.Arguments, &decoded) == nil && decoded != nil {
					input = decoded
				}
			}
			result.toolUses = append(result.toolUses, map[string]any{
				"toolUseId": part.CallID,
				"name":      name,
				"input":     input,
			})
			result.toolNames = append(result.toolNames, name)
		}
	}
	result.text = strings.Join(textParts, "\n")
	result.thinking = strings.Join(thinkingParts, "")
	return result
}

func kiroImageFromPart(part conversationPart) any {
	format := strings.TrimPrefix(part.MediaType, "image/")
	if format == "jpg" {
		format = "jpeg"
	}
	if part.Data == "" || (format != "jpeg" && format != "png" && format != "gif" && format != "webp") {
		return nil
	}
	return map[string]any{
		"format": format,
		"source": map[string]any{"bytes": part.Data},
	}
}

// kiroConversationID keeps one upstream conversation per client session. A
// session key that is not itself a UUID is hashed into a stable one.
func kiroConversationID(sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return uuid.NewString()
	}
	if match := kiroSessionUUIDPattern.FindStringSubmatch(sessionKey); len(match) == 2 {
		return match[1]
	}
	if parsed, err := uuid.Parse(sessionKey); err == nil {
		return parsed.String()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("ccl-kiro-session:"+sessionKey)).String()
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

// estimateApproxTokens preserves the ASCII text-budget estimate but charges
// non-ASCII bytes more conservatively, avoiding the old CJK undercount without
// halving the retained English history. Upstream usage remains authoritative.
func estimateApproxTokens(text string) int {
	return estimateUnknownTextTokens(text)
}

// estimateApproxTokensBytes is estimateApproxTokens without forcing a []byte to
// string copy of the payload.
func estimateApproxTokensBytes(raw []byte) int {
	return estimateUnknownTextTokens(raw)
}

func estimateUnknownTextTokens[T ~string | ~[]byte](text T) int {
	ascii := 0
	for i := 0; i < len(text); i++ {
		if text[i] < 128 {
			ascii++
		}
	}
	return (ascii+3)/4 + (len(text)-ascii+1)/2
}
