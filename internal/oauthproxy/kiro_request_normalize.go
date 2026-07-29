package oauthproxy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
)

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

	for _, entry := range history {
		kind, message := kiroHistoryMessage(entry)
		if kind != "assistant" {
			continue
		}
		toolUses := kiroAnySlice(message["toolUses"])
		filtered := toolUses[:0]
		for _, toolUse := range toolUses {
			id := metadataString(kiroAnyMap(toolUse), "toolUseId")
			if id == "" || !pairedUses[id] {
				droppedUses++
				continue
			}
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
