package oauthproxy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	kiroMaxTextFieldBytes = 400_000
	// This is a conservative local estimate, not Kiro's advertised context
	// window. The estimator intentionally excludes base64 media and differs from
	// the upstream tokenizer, so keep ample room below its near-100% rejection.
	kiroMaxEstimatedContentTokens = 160_000
)

type kiroTextLimitStats struct {
	truncated    int
	droppedBytes int
	largestBytes int
}

type kiroRequestBudgetStats struct {
	originalBytes          int
	finalBytes             int
	originalTokens         int
	finalTokens            int
	droppedImages          int
	droppedHistoryMessages int
	truncatedTexts         int
	droppedTextBytes       int
	droppedTools           int
}

func kiroUserMessagesNewestFirst(conversationState map[string]any) []map[string]any {
	var messages []map[string]any
	if current, ok := conversationState["currentMessage"].(map[string]any); ok {
		if userMessage, ok := current["userInputMessage"].(map[string]any); ok {
			messages = append(messages, userMessage)
		}
	}
	if history, ok := conversationState["history"].([]any); ok {
		for index := len(history) - 1; index >= 0; index-- {
			entry, ok := history[index].(map[string]any)
			if !ok {
				continue
			}
			if userMessage, ok := entry["userInputMessage"].(map[string]any); ok {
				messages = append(messages, userMessage)
			}
		}
	}
	return messages
}

// deduplicateKiroInlineMedia keeps the most recent copy of an identical image.
// Claude Code can repeat screenshots in tool results across many turns.
func deduplicateKiroInlineMedia(conversationState map[string]any) (dropped int) {
	seen := make(map[[32]byte]struct{})
	for _, message := range kiroUserMessagesNewestFirst(conversationState) {
		images, ok := message["images"].([]any)
		if !ok || len(images) == 0 {
			continue
		}
		retained := make([]any, 0, len(images))
		for index := len(images) - 1; index >= 0; index-- {
			key := kiroImageDigest(images[index])
			if _, exists := seen[key]; exists {
				dropped++
				continue
			}
			seen[key] = struct{}{}
			retained = append(retained, images[index])
		}
		for left, right := 0, len(retained)-1; left < right; left, right = left+1, right-1 {
			retained[left], retained[right] = retained[right], retained[left]
		}
		if len(retained) == 0 {
			delete(message, "images")
		} else {
			message["images"] = retained
		}
	}
	return dropped
}

func kiroImageDigest(value any) [32]byte {
	if image, ok := value.(map[string]any); ok {
		if source, ok := image["source"].(map[string]any); ok {
			if encoded, ok := source["bytes"].(string); ok {
				if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
					return sha256.Sum256(decoded)
				}
				return sha256.Sum256([]byte(encoded))
			}
		}
	}
	raw, _ := json.Marshal(value)
	return sha256.Sum256(raw)
}

// normalizeKiroHistory merges adjacent messages with the same role and ensures
// that the current user message is preceded by an assistant history message.
func normalizeKiroHistory(history []any) []any {
	normalized := make([]any, 0, len(history)+1)
	for _, entry := range history {
		kind, message := kiroHistoryMessage(entry)
		if kind == "" {
			continue
		}
		if len(normalized) > 0 {
			previousKind, previous := kiroHistoryMessage(normalized[len(normalized)-1])
			if previousKind == kind {
				mergeKiroHistoryMessage(previous, message, kind)
				continue
			}
		}
		normalized = append(normalized, entry)
	}
	if len(normalized) > 0 {
		if kind, _ := kiroHistoryMessage(normalized[len(normalized)-1]); kind == "user" {
			normalized = append(normalized, map[string]any{
				"assistantResponseMessage": map[string]any{"content": "OK"},
			})
		}
	}
	return normalized
}

func kiroHistoryMessage(entry any) (string, map[string]any) {
	container, ok := entry.(map[string]any)
	if !ok {
		return "", nil
	}
	if message, ok := container["userInputMessage"].(map[string]any); ok {
		return "user", message
	}
	if message, ok := container["assistantResponseMessage"].(map[string]any); ok {
		return "assistant", message
	}
	return "", nil
}

func mergeKiroHistoryMessage(destination, source map[string]any, kind string) {
	destination["content"] = joinKiroContent(metadataString(destination, "content"), metadataString(source, "content"))
	if kind == "user" {
		appendKiroSliceField(destination, source, "images")
		mergeKiroMessageContext(destination, source)
		return
	}
	appendKiroSliceField(destination, source, "toolUses")
}

func joinKiroContent(left, right string) string {
	switch {
	case left == "":
		return right
	case right == "":
		return left
	default:
		return left + "\n" + right
	}
}

func appendKiroSliceField(destination, source map[string]any, field string) {
	values, _ := destination[field].([]any)
	additional, _ := source[field].([]any)
	if len(additional) > 0 {
		destination[field] = append(values, additional...)
	}
}

func mergeKiroMessageContext(destination, source map[string]any) {
	sourceContext, _ := source["userInputMessageContext"].(map[string]any)
	if len(sourceContext) == 0 {
		return
	}
	destinationContext, _ := destination["userInputMessageContext"].(map[string]any)
	if destinationContext == nil {
		destinationContext = map[string]any{"envState": kiroEnvironmentState()}
		destination["userInputMessageContext"] = destinationContext
	}
	for _, field := range []string{"toolResults", "tools"} {
		appendKiroSliceField(destinationContext, sourceContext, field)
	}
}

// normalizeKiroToolPairing removes orphaned or duplicate tool results and tool
// uses. Amazon Q rejects a conversation when any tool use is left unpaired.
func normalizeKiroToolPairing(conversationState map[string]any) (droppedUses, droppedResults int) {
	history, _ := conversationState["history"].([]any)
	seenUses := make(map[string]bool)
	pairedUses := make(map[string]bool)

	for _, entry := range history {
		kind, message := kiroHistoryMessage(entry)
		switch kind {
		case "assistant":
			for _, toolUse := range kiroAnySlice(message["toolUses"]) {
				if id := metadataString(kiroAnyMap(toolUse), "toolUseId"); id != "" {
					seenUses[id] = true
				}
			}
		case "user":
			context, _ := message["userInputMessageContext"].(map[string]any)
			if context == nil {
				continue
			}
			results := kiroAnySlice(context["toolResults"])
			filtered := results[:0]
			for _, result := range results {
				id := metadataString(kiroAnyMap(result), "toolUseId")
				if id == "" || !seenUses[id] || pairedUses[id] {
					droppedResults++
					continue
				}
				pairedUses[id] = true
				filtered = append(filtered, result)
			}
			setKiroSliceField(context, "toolResults", filtered)
		}
	}

	currentContext := kiroCurrentMessageContext(conversationState)
	if currentContext != nil {
		results := kiroAnySlice(currentContext["toolResults"])
		filtered := results[:0]
		for _, result := range results {
			id := metadataString(kiroAnyMap(result), "toolUseId")
			if id == "" || !seenUses[id] || pairedUses[id] {
				droppedResults++
				continue
			}
			pairedUses[id] = true
			filtered = append(filtered, result)
		}
		setKiroSliceField(currentContext, "toolResults", filtered)
	}

	retainedUseIDs := make(map[string]bool)
	for _, entry := range history {
		kind, message := kiroHistoryMessage(entry)
		if kind != "assistant" {
			continue
		}
		toolUses := kiroAnySlice(message["toolUses"])
		filtered := toolUses[:0]
		for _, toolUse := range toolUses {
			id := metadataString(kiroAnyMap(toolUse), "toolUseId")
			if id == "" || !pairedUses[id] || retainedUseIDs[id] {
				droppedUses++
				continue
			}
			retainedUseIDs[id] = true
			filtered = append(filtered, toolUse)
		}
		setKiroSliceField(message, "toolUses", filtered)
	}
	return droppedUses, droppedResults
}

func kiroCurrentMessageContext(conversationState map[string]any) map[string]any {
	current, _ := conversationState["currentMessage"].(map[string]any)
	userMessage, _ := current["userInputMessage"].(map[string]any)
	context, _ := userMessage["userInputMessageContext"].(map[string]any)
	return context
}

func kiroAnyMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func kiroAnySlice(value any) []any {
	result, _ := value.([]any)
	return result
}

func setKiroSliceField(container map[string]any, field string, values []any) {
	if len(values) == 0 {
		delete(container, field)
		return
	}
	container[field] = values
}

// enforceKiroContentBudget keeps text/tool history below Kiro's effective
// context threshold. Inline media bytes are deliberately excluded: observed
// 30 MB / 100-image requests succeed, while small requests fail when text usage
// crosses 200K tokens. Old complete turns are removed before current content.
func enforceKiroContentBudget(body map[string]any, protectedHistoryPrefix, limit int) kiroRequestBudgetStats {
	stats := kiroRequestBudgetStats{
		originalBytes:  kiroJSONSize(body),
		originalTokens: estimateKiroContentTokens(body),
	}
	stats.finalBytes = stats.originalBytes
	stats.finalTokens = stats.originalTokens
	if limit <= 0 || stats.finalTokens <= limit {
		return stats
	}

	conversationState, _ := body["conversationState"].(map[string]any)
	history, _ := conversationState["history"].([]any)
	protectedHistoryPrefix = min(max(protectedHistoryPrefix, 0), len(history))

	// Discard complete oldest turns. Keep the synthetic system pair and
	// any assistant entry referenced by a current tool_result until the end.
	for stats.finalTokens > limit {
		history, _ = conversationState["history"].([]any)
		start, end, ok := oldestDroppableKiroHistoryTurn(conversationState, history, protectedHistoryPrefix)
		if !ok {
			break
		}
		stats.droppedImages += countKiroHistoryImages(history[start:end])
		history = append(append([]any(nil), history[:start]...), history[end:]...)
		stats.droppedHistoryMessages += end - start
		if len(history) == 0 {
			delete(conversationState, "history")
		} else {
			conversationState["history"] = history
		}
		stats.finalBytes = kiroJSONSize(body)
		stats.finalTokens = estimateKiroContentTokens(body)
	}

	// Tool pairs can become orphaned when an old turn is removed. Pair first,
	// then remove messages that became empty and restore strict role alternation.
	stats.droppedHistoryMessages += normalizeKiroBudgetHistory(conversationState)
	stats.finalBytes = kiroJSONSize(body)
	stats.finalTokens = estimateKiroContentTokens(body)

	// Finally shrink the largest remaining textual fields. This covers a
	// current tool_result containing several individually-valid 400 KB chunks.
	for stats.finalTokens > limit {
		container, field, value := largestKiroBudgetText(conversationState)
		if container == nil || len(value) <= 1_024 {
			break
		}
		valueTokens := max(estimateKiroTokens(value), 1)
		targetTokens := max(256, valueTokens-(stats.finalTokens-limit)-1_024)
		target := max(1_024, len(value)*targetTokens/valueTokens)
		truncated, dropped := truncateKiroText(value, target)
		if dropped <= 0 {
			break
		}
		container[field] = truncated
		stats.truncatedTexts++
		stats.droppedTextBytes += dropped
		stats.finalBytes = kiroJSONSize(body)
		stats.finalTokens = estimateKiroContentTokens(body)
	}

	// Tool descriptions and definitions are optional context. Extremely large
	// schemas should not make the entire request unusable.
	if stats.finalTokens > limit {
		context := kiroCurrentMessageContext(conversationState)
		for _, value := range kiroAnySlice(context["tools"]) {
			delete(kiroAnyMap(value), "description")
		}
		stats.finalBytes = kiroJSONSize(body)
		stats.finalTokens = estimateKiroContentTokens(body)
		for stats.finalTokens > limit {
			tools := kiroAnySlice(context["tools"])
			if len(tools) == 0 {
				break
			}
			context["tools"] = append([]any(nil), tools[:len(tools)-1]...)
			stats.droppedTools++
			if len(tools) == 1 {
				delete(context, "tools")
			}
			stats.finalBytes = kiroJSONSize(body)
			stats.finalTokens = estimateKiroContentTokens(body)
		}
	}
	return stats
}

func estimateKiroContentTokens(body map[string]any) int {
	state, _ := body["conversationState"].(map[string]any)
	total := 0
	addText := func(value string) {
		total += estimateKiroTokens(value)
	}
	addJSON := func(value any) {
		raw, _ := json.Marshal(value)
		total += estimateKiroTokens(string(raw))
	}
	addMessage := func(kind string, message map[string]any) {
		addText(metadataString(message, "content"))
		switch kind {
		case "assistant":
			for _, toolUse := range kiroAnySlice(message["toolUses"]) {
				addJSON(toolUse)
			}
		case "user":
			context, _ := message["userInputMessageContext"].(map[string]any)
			for _, resultValue := range kiroAnySlice(context["toolResults"]) {
				result := kiroAnyMap(resultValue)
				addText(metadataString(result, "toolUseId"))
				for _, contentValue := range kiroAnySlice(result["content"]) {
					addText(metadataString(kiroAnyMap(contentValue), "text"))
				}
			}
		}
	}

	for _, entry := range kiroAnySlice(state["history"]) {
		kind, message := kiroHistoryMessage(entry)
		addMessage(kind, message)
	}
	addMessage("user", kiroCurrentUserMessage(state))
	context := kiroCurrentMessageContext(state)
	for _, tool := range kiroAnySlice(context["tools"]) {
		addJSON(tool)
	}
	if additional := body["additionalModelRequestFields"]; additional != nil {
		addJSON(additional)
	}
	return total
}

func countKiroHistoryImages(history []any) int {
	count := 0
	for _, entry := range history {
		kind, message := kiroHistoryMessage(entry)
		if kind == "user" {
			count += len(kiroAnySlice(message["images"]))
		}
	}
	return count
}

func normalizeKiroBudgetHistory(conversationState map[string]any) int {
	dropped := 0
	for range 2 {
		normalizeKiroToolPairing(conversationState)
		history := kiroAnySlice(conversationState["history"])
		filtered := make([]any, 0, len(history))
		for _, entry := range history {
			kind, message := kiroHistoryMessage(entry)
			if !kiroHistoryMessageHasContent(kind, message) {
				dropped++
				continue
			}
			filtered = append(filtered, entry)
		}
		filtered = normalizeKiroHistory(filtered)
		if len(filtered) == 0 {
			delete(conversationState, "history")
		} else {
			conversationState["history"] = filtered
		}
	}
	normalizeKiroToolPairing(conversationState)

	// A current message made only of orphaned tool results can become empty
	// after pairing normalization. Kiro requires a meaningful user input.
	current := kiroCurrentUserMessage(conversationState)
	if strings.TrimSpace(metadataString(current, "content")) == "" &&
		len(kiroAnySlice(current["images"])) == 0 &&
		len(kiroAnySlice(kiroCurrentMessageContext(conversationState)["toolResults"])) == 0 {
		current["content"] = "Continue."
	}
	return dropped
}

func kiroHistoryMessageHasContent(kind string, message map[string]any) bool {
	if message == nil {
		return false
	}
	if strings.TrimSpace(metadataString(message, "content")) != "" {
		return true
	}
	switch kind {
	case "user":
		if len(kiroAnySlice(message["images"])) > 0 {
			return true
		}
		context, _ := message["userInputMessageContext"].(map[string]any)
		return len(kiroAnySlice(context["toolResults"])) > 0
	case "assistant":
		return len(kiroAnySlice(message["toolUses"])) > 0
	default:
		return false
	}
}

func validateKiroConversationState(conversationState map[string]any) error {
	history := kiroAnySlice(conversationState["history"])
	seenUses := make(map[string]bool)
	pairedUses := make(map[string]bool)
	for index, entry := range history {
		kind, message := kiroHistoryMessage(entry)
		expected := "user"
		if index%2 == 1 {
			expected = "assistant"
		}
		if kind != expected {
			return fmt.Errorf("Kiro history role %d is %q, want %q", index, kind, expected)
		}
		if !kiroHistoryMessageHasContent(kind, message) {
			return fmt.Errorf("Kiro history message %d is empty", index)
		}
		switch kind {
		case "assistant":
			for _, value := range kiroAnySlice(message["toolUses"]) {
				id := metadataString(kiroAnyMap(value), "toolUseId")
				if id == "" || seenUses[id] {
					return fmt.Errorf("Kiro history has invalid tool use at message %d", index)
				}
				seenUses[id] = true
			}
		case "user":
			context, _ := message["userInputMessageContext"].(map[string]any)
			for _, value := range kiroAnySlice(context["toolResults"]) {
				id := metadataString(kiroAnyMap(value), "toolUseId")
				if id == "" || !seenUses[id] || pairedUses[id] {
					return fmt.Errorf("Kiro history has orphaned tool result at message %d", index)
				}
				pairedUses[id] = true
			}
		}
	}
	if len(history)%2 != 0 {
		return fmt.Errorf("Kiro history must end with an assistant message")
	}

	current := kiroCurrentUserMessage(conversationState)
	if strings.TrimSpace(metadataString(current, "content")) == "" &&
		len(kiroAnySlice(current["images"])) == 0 &&
		len(kiroAnySlice(kiroCurrentMessageContext(conversationState)["toolResults"])) == 0 {
		return fmt.Errorf("Kiro current user message is empty")
	}
	for _, value := range kiroAnySlice(kiroCurrentMessageContext(conversationState)["toolResults"]) {
		id := metadataString(kiroAnyMap(value), "toolUseId")
		if id == "" || !seenUses[id] || pairedUses[id] {
			return fmt.Errorf("Kiro current message has orphaned tool result")
		}
		pairedUses[id] = true
	}
	for id := range seenUses {
		if !pairedUses[id] {
			return fmt.Errorf("Kiro history has unpaired tool use %q", id)
		}
	}
	return nil
}

func kiroJSONSize(value any) int {
	raw, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(raw)
}

func kiroCurrentUserMessage(conversationState map[string]any) map[string]any {
	current, _ := conversationState["currentMessage"].(map[string]any)
	userMessage, _ := current["userInputMessage"].(map[string]any)
	return userMessage
}

func oldestDroppableKiroHistoryTurn(conversationState map[string]any, history []any, protectedPrefix int) (int, int, bool) {
	protectedToolUseIDs := make(map[string]bool)
	for _, result := range kiroAnySlice(kiroCurrentMessageContext(conversationState)["toolResults"]) {
		if id := metadataString(kiroAnyMap(result), "toolUseId"); id != "" {
			protectedToolUseIDs[id] = true
		}
	}
	isProtected := func(entry any) bool {
		kind, message := kiroHistoryMessage(entry)
		if kind != "assistant" {
			return false
		}
		for _, toolUse := range kiroAnySlice(message["toolUses"]) {
			if protectedToolUseIDs[metadataString(kiroAnyMap(toolUse), "toolUseId")] {
				return true
			}
		}
		return false
	}

	for start := protectedPrefix; start < len(history); {
		end := min(start+2, len(history))
		if end-start == 2 {
			firstKind, _ := kiroHistoryMessage(history[start])
			secondKind, _ := kiroHistoryMessage(history[start+1])
			if firstKind == secondKind || firstKind == "" || secondKind == "" {
				end = start + 1
			} else if firstKind == "user" && secondKind == "assistant" {
				// A Claude agent turn may contain several assistant tool_use →
				// user tool_result cycles. Treat the whole chain as one unit so
				// budget trimming never leaves the next tool_result orphaned.
				for end < len(history) && kiroHistoryUserHasToolResults(history[end]) {
					end++
					if end < len(history) {
						if kind, _ := kiroHistoryMessage(history[end]); kind == "assistant" {
							end++
						}
					}
				}
			}
		}
		protected := false
		for index := start; index < end; index++ {
			protected = protected || isProtected(history[index])
		}
		if !protected {
			return start, end, true
		}
		start = end
	}
	return 0, 0, false
}

func kiroHistoryUserHasToolResults(entry any) bool {
	kind, message := kiroHistoryMessage(entry)
	if kind != "user" {
		return false
	}
	context, _ := message["userInputMessageContext"].(map[string]any)
	return len(kiroAnySlice(context["toolResults"])) > 0
}

func largestKiroBudgetText(conversationState map[string]any) (map[string]any, string, string) {
	var largestContainer map[string]any
	largestField, largestValue := "", ""
	consider := func(container map[string]any, field string) {
		value, _ := container[field].(string)
		if len(value) > len(largestValue) {
			largestContainer, largestField, largestValue = container, field, value
		}
	}
	considerMessage := func(message map[string]any, includeToolResults bool) {
		consider(message, "content")
		if !includeToolResults {
			return
		}
		context, _ := message["userInputMessageContext"].(map[string]any)
		for _, resultValue := range kiroAnySlice(context["toolResults"]) {
			for _, contentValue := range kiroAnySlice(kiroAnyMap(resultValue)["content"]) {
				consider(kiroAnyMap(contentValue), "text")
			}
		}
	}
	considerMessage(kiroCurrentUserMessage(conversationState), true)
	for _, entry := range kiroAnySlice(conversationState["history"]) {
		kind, message := kiroHistoryMessage(entry)
		considerMessage(message, kind == "user")
	}
	return largestContainer, largestField, largestValue
}

// limitKiroTextFields applies Amazon Q's per-content-field size limit. Keeping
// both ends retains command context as well as the final error/output summary.
func limitKiroTextFields(conversationState map[string]any, limit int) kiroTextLimitStats {
	var stats kiroTextLimitStats
	limitMessage := func(message map[string]any, includeToolResults bool) {
		limitKiroStringField(message, "content", limit, &stats)
		if !includeToolResults {
			return
		}
		context, _ := message["userInputMessageContext"].(map[string]any)
		for _, resultValue := range kiroAnySlice(context["toolResults"]) {
			result := kiroAnyMap(resultValue)
			for _, contentValue := range kiroAnySlice(result["content"]) {
				limitKiroStringField(kiroAnyMap(contentValue), "text", limit, &stats)
			}
		}
	}

	if current, ok := conversationState["currentMessage"].(map[string]any); ok {
		if userMessage, ok := current["userInputMessage"].(map[string]any); ok {
			limitMessage(userMessage, true)
		}
	}
	if history, ok := conversationState["history"].([]any); ok {
		for _, entry := range history {
			kind, message := kiroHistoryMessage(entry)
			limitMessage(message, kind == "user")
		}
	}
	return stats
}

func limitKiroStringField(container map[string]any, field string, limit int, stats *kiroTextLimitStats) {
	if container == nil {
		return
	}
	value, ok := container[field].(string)
	if !ok {
		return
	}
	stats.largestBytes = max(stats.largestBytes, len(value))
	truncated, dropped := truncateKiroText(value, limit)
	if dropped == 0 {
		return
	}
	container[field] = truncated
	stats.truncated++
	stats.droppedBytes += dropped
}

func truncateKiroText(value string, limit int) (string, int) {
	if limit <= 0 {
		return "", len(value)
	}
	if len(value) <= limit {
		return value, 0
	}

	// Calculate the marker twice so its dropped-byte count remains accurate even
	// when the number of digits changes the retained budget.
	dropped := len(value) - limit
	var result string
	for range 2 {
		marker := fmt.Sprintf("\n\n...[ccl truncated %d bytes for Kiro content limit]...\n\n", dropped)
		if len(marker) >= limit {
			return truncateKiroUTF8Prefix(marker, limit), len(value)
		}
		retained := limit - len(marker)
		head := truncateKiroUTF8Prefix(value, retained/2)
		tail := truncateKiroUTF8Suffix(value, retained-len(head))
		dropped = len(value) - len(head) - len(tail)
		result = head + marker + tail
	}
	if len(result) > limit {
		result = truncateKiroUTF8Prefix(result, limit)
	}
	return result, dropped
}

func truncateKiroUTF8Prefix(value string, limit int) string {
	if limit >= len(value) {
		return value
	}
	if limit <= 0 {
		return ""
	}
	for limit > 0 && value[limit]&0xc0 == 0x80 {
		limit--
	}
	return value[:limit]
}

func truncateKiroUTF8Suffix(value string, limit int) string {
	if limit >= len(value) {
		return value
	}
	if limit <= 0 {
		return ""
	}
	start := len(value) - limit
	for start < len(value) && value[start]&0xc0 == 0x80 {
		start++
	}
	return value[start:]
}
