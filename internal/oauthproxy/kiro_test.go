package oauthproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ugorji/go/codec"
)

func TestConvertAnthropicMessagesToKiro(t *testing.T) {
	longToolName := "mcp__example__" + strings.Repeat("very_long_tool_name_", 4)
	payload := []byte(`{
		"model":"claude-sonnet-4-6[1m]",
		"max_tokens":4096,
		"stream":true,
		"system":[{"type":"text","text":"Be concise."}],
		"thinking":{"type":"adaptive","budget_tokens":20000},
		"output_config":{"effort":"max"},
		"metadata":{"user_id":"user_account__session_0b4445e1-f5be-49e1-87ce-62bbc28ad705"},
		"tools":[{
			"name":"` + longToolName + `",
			"description":"Read a file",
			"input_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
		}],
		"messages":[
			{"role":"user","content":"Read the file"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"I should inspect it."},
				{"type":"tool_use","id":"toolu_1","name":"` + longToolName + `","input":{"path":"/tmp/a"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"contents"},
				{"type":"text","text":"Continue"}
			]}
		]
	}`)
	converted, err := convertAnthropicToKiro(payload)
	if err != nil {
		t.Fatal(err)
	}
	if converted.model != "claude-sonnet-4.6" {
		t.Fatalf("model = %q", converted.model)
	}
	if !converted.stream || !converted.thinkingEnabled || converted.maxTokens != 4096 {
		t.Fatalf("conversion flags = %+v", converted)
	}
	state := converted.body["conversationState"].(map[string]any)
	if state["conversationId"] != "0b4445e1-f5be-49e1-87ce-62bbc28ad705" {
		t.Fatalf("conversationId = %#v", state["conversationId"])
	}
	history := state["history"].([]any)
	if len(history) != 4 {
		t.Fatalf("history len = %d, history=%#v", len(history), history)
	}
	system := history[0].(map[string]any)["userInputMessage"].(map[string]any)["content"].(string)
	if !strings.Contains(system, "<thinking_mode>adaptive</thinking_mode>") || !strings.Contains(system, "Be concise.") {
		t.Fatalf("system = %q", system)
	}
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	contextValue := current["userInputMessageContext"].(map[string]any)
	if len(contextValue["toolResults"].([]any)) != 1 || len(contextValue["tools"].([]any)) != 1 {
		t.Fatalf("current context = %#v", contextValue)
	}
	additional := converted.body["additionalModelRequestFields"].(map[string]any)
	outputConfig := additional["output_config"].(map[string]any)
	if outputConfig["effort"] != "max" {
		t.Fatalf("reasoning fields = %#v", additional)
	}
	if len(converted.toolNameMap) != 1 {
		t.Fatalf("tool name map = %#v", converted.toolNameMap)
	}
}

func TestKiroInlineMediaLimitKeepsCurrentThenNewestHistory(t *testing.T) {
	numberedImages := func(prefix string, count int) []any {
		images := make([]any, count)
		for index := range images {
			images[index] = fmt.Sprintf("%s-%03d", prefix, index)
		}
		return images
	}
	oldest := map[string]any{"images": numberedImages("oldest", 50)}
	newest := map[string]any{"images": numberedImages("newest", 60)}
	current := map[string]any{"images": numberedImages("current", 30)}
	state := map[string]any{
		"currentMessage": map[string]any{"userInputMessage": current},
		"history": []any{
			map[string]any{"userInputMessage": oldest},
			map[string]any{"assistantResponseMessage": map[string]any{"content": "response"}},
			map[string]any{"userInputMessage": newest},
		},
	}

	kept, dropped := limitKiroInlineMedia(state, kiroMaxInlineMediaSegments)
	if kept != 100 || dropped != 40 {
		t.Fatalf("media result kept=%d dropped=%d", kept, dropped)
	}
	if got := len(current["images"].([]any)); got != 30 {
		t.Fatalf("current images = %d", got)
	}
	if got := len(newest["images"].([]any)); got != 60 {
		t.Fatalf("newest history images = %d", got)
	}
	oldestImages := oldest["images"].([]any)
	if len(oldestImages) != 10 || oldestImages[0] != "oldest-040" || oldestImages[9] != "oldest-049" {
		t.Fatalf("oldest retained images = %#v", oldestImages)
	}
}

func TestKiroInlineMediaDeduplicationKeepsNewestCopy(t *testing.T) {
	imageValue := func(value string) any {
		return map[string]any{
			"format": "png",
			"source": map[string]any{"bytes": base64.StdEncoding.EncodeToString([]byte(value))},
		}
	}
	oldest := map[string]any{"images": []any{imageValue("current-a"), imageValue("oldest-only")}}
	newest := map[string]any{"images": []any{imageValue("current-b"), imageValue("newest-only")}}
	current := map[string]any{"images": []any{imageValue("current-a"), imageValue("current-b")}}
	state := map[string]any{
		"currentMessage": map[string]any{"userInputMessage": current},
		"history": []any{
			map[string]any{"userInputMessage": oldest},
			map[string]any{"userInputMessage": newest},
		},
	}

	if dropped := deduplicateKiroInlineMedia(state); dropped != 2 {
		t.Fatalf("deduplicated images = %d", dropped)
	}
	if got := len(current["images"].([]any)); got != 2 {
		t.Fatalf("current images = %d", got)
	}
	if got := len(newest["images"].([]any)); got != 1 {
		t.Fatalf("newest history images = %d", got)
	}
	if got := len(oldest["images"].([]any)); got != 1 {
		t.Fatalf("oldest history images = %d", got)
	}
}

func TestKiroContentBudgetDropsOldestCompleteTurn(t *testing.T) {
	systemUser := map[string]any{"content": "system"}
	systemAssistant := map[string]any{"content": "ack"}
	oldUser := map[string]any{
		"content": strings.Repeat("u", 12_000),
		"images": []any{map[string]any{
			"format": "jpeg",
			"source": map[string]any{"bytes": strings.Repeat("x", 12_000)},
		}},
	}
	oldAssistant := map[string]any{"content": strings.Repeat("a", 12_000)}
	newUser := map[string]any{"content": "new user"}
	newAssistant := map[string]any{"content": "new assistant"}
	state := map[string]any{
		"history": []any{
			map[string]any{"userInputMessage": systemUser},
			map[string]any{"assistantResponseMessage": systemAssistant},
			map[string]any{"userInputMessage": oldUser},
			map[string]any{"assistantResponseMessage": oldAssistant},
			map[string]any{"userInputMessage": newUser},
			map[string]any{"assistantResponseMessage": newAssistant},
		},
		"currentMessage": map[string]any{"userInputMessage": map[string]any{
			"content":                 "continue",
			"userInputMessageContext": map[string]any{"envState": kiroEnvironmentState()},
		}},
	}
	body := map[string]any{"conversationState": state}
	originalTokens := estimateKiroContentTokens(body)
	limit := originalTokens - 4_500

	stats := enforceKiroContentBudget(body, 2, limit)
	if stats.droppedHistoryMessages != 2 || stats.droppedImages != 1 {
		t.Fatalf("budget stats = %+v", stats)
	}
	if stats.finalTokens > limit {
		t.Fatalf("final tokens = %d, limit = %d", stats.finalTokens, limit)
	}
	history := kiroAnySlice(state["history"])
	if len(history) != 4 {
		t.Fatalf("history length = %d, history=%#v", len(history), history)
	}
	if metadataString(kiroAnyMap(history[0].(map[string]any)["userInputMessage"]), "content") != "system" ||
		metadataString(kiroAnyMap(history[1].(map[string]any)["assistantResponseMessage"]), "content") != "ack" ||
		metadataString(kiroAnyMap(history[2].(map[string]any)["userInputMessage"]), "content") != "new user" ||
		metadataString(kiroAnyMap(history[3].(map[string]any)["assistantResponseMessage"]), "content") != "new assistant" {
		t.Fatalf("wrong history retained: %#v", history)
	}
}

func TestKiroContentBudgetDropsWholeToolChainAndKeepsValidAlternation(t *testing.T) {
	toolUse := map[string]any{
		"toolUseId": "toolu_old",
		"name":      "Read",
		"input":     map[string]any{"path": "/tmp/old"},
	}
	toolResult := map[string]any{
		"toolUseId": "toolu_old",
		"content":   []any{map[string]any{"text": strings.Repeat("r", 12_000)}},
		"status":    "success",
	}
	state := map[string]any{
		"history": []any{
			map[string]any{"userInputMessage": map[string]any{"content": "system"}},
			map[string]any{"assistantResponseMessage": map[string]any{"content": "ack"}},
			map[string]any{"userInputMessage": map[string]any{"content": strings.Repeat("u", 12_000)}},
			map[string]any{"assistantResponseMessage": map[string]any{
				"content":  " ",
				"toolUses": []any{toolUse},
			}},
			map[string]any{"userInputMessage": map[string]any{
				"content": "",
				"userInputMessageContext": map[string]any{
					"envState":    kiroEnvironmentState(),
					"toolResults": []any{toolResult},
				},
			}},
			map[string]any{"assistantResponseMessage": map[string]any{"content": strings.Repeat("a", 12_000)}},
			map[string]any{"userInputMessage": map[string]any{"content": "new user"}},
			map[string]any{"assistantResponseMessage": map[string]any{"content": "new assistant"}},
		},
		"currentMessage": map[string]any{"userInputMessage": map[string]any{
			"content":                 "continue",
			"userInputMessageContext": map[string]any{"envState": kiroEnvironmentState()},
		}},
	}
	body := map[string]any{"conversationState": state}
	originalTokens := estimateKiroContentTokens(body)

	stats := enforceKiroContentBudget(body, 2, originalTokens-5_000)
	if stats.droppedHistoryMessages != 4 {
		t.Fatalf("dropped history = %d, want complete four-message tool chain; stats=%+v", stats.droppedHistoryMessages, stats)
	}
	history := kiroAnySlice(state["history"])
	if len(history) != 4 {
		t.Fatalf("history length = %d, history=%#v", len(history), history)
	}
	wantRoles := []string{"user", "assistant", "user", "assistant"}
	for index, want := range wantRoles {
		got, message := kiroHistoryMessage(history[index])
		if got != want || !kiroHistoryMessageHasContent(got, message) {
			t.Fatalf("history[%d] role=%q meaningful=%t, want %q: %#v",
				index, got, kiroHistoryMessageHasContent(got, message), want, history)
		}
	}
}

func TestConvertAnthropicRequestDoesNotChargeMediaAgainstTextBudget(t *testing.T) {
	imageBlocks := make([]any, 8)
	for index := range imageBlocks {
		rawImage := make([]byte, 300_000)
		rawImage[0] = byte(index + 1)
		imageBlocks[index] = map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": "image/jpeg",
				"data":       base64.StdEncoding.EncodeToString(rawImage),
			},
		}
	}
	payload, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 1024,
		"system":     strings.Repeat("s", 200_000),
		"messages": []any{
			map[string]any{"role": "user", "content": imageBlocks},
			map[string]any{"role": "assistant", "content": strings.Repeat("a", 200_000)},
			map[string]any{"role": "user", "content": strings.Repeat("c", 100_000)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	converted, err := convertAnthropicToKiro(payload)
	if err != nil {
		t.Fatal(err)
	}
	if converted.originalBody <= 3_600_000 {
		t.Fatalf("fixture body = %d, expected a media-heavy request", converted.originalBody)
	}
	if converted.originalTokens > kiroMaxEstimatedContentTokens ||
		converted.finalTokens > kiroMaxEstimatedContentTokens {
		t.Fatalf("content tokens original=%d final=%d", converted.originalTokens, converted.finalTokens)
	}
	if converted.budgetMedia != 0 || converted.inlineMedia != 8 {
		t.Fatalf("budget dropped media=%d retained=%d", converted.budgetMedia, converted.inlineMedia)
	}
	state := kiroAnyMap(converted.body["conversationState"])
	current := kiroCurrentUserMessage(state)
	if metadataString(current, "content") != strings.Repeat("c", 100_000) {
		t.Fatal("content budget unexpectedly changed the current message")
	}
}

func TestConvertAnthropicRequestTrimsTextHistoryBelowKiroContextBudget(t *testing.T) {
	messages := make([]any, 0, 21)
	for index := 0; index < 10; index++ {
		messages = append(messages,
			map[string]any{"role": "user", "content": fmt.Sprintf("user-%02d-", index) + strings.Repeat("u", 50_000)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("assistant-%02d-", index) + strings.Repeat("a", 50_000)},
		)
	}
	messages = append(messages, map[string]any{"role": "user", "content": "CURRENT-" + strings.Repeat("c", 10_000)})
	payload, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 1024,
		"system":     "SYSTEM-" + strings.Repeat("s", 100_000),
		"messages":   messages,
	})
	if err != nil {
		t.Fatal(err)
	}

	converted, err := convertAnthropicToKiro(payload)
	if err != nil {
		t.Fatal(err)
	}
	if converted.originalTokens <= kiroMaxEstimatedContentTokens {
		t.Fatalf("fixture tokens = %d, expected over %d", converted.originalTokens, kiroMaxEstimatedContentTokens)
	}
	if converted.finalTokens > kiroMaxEstimatedContentTokens || converted.budgetHistory == 0 {
		t.Fatalf("budget final tokens=%d dropped history=%d", converted.finalTokens, converted.budgetHistory)
	}
	state := kiroAnyMap(converted.body["conversationState"])
	history := kiroAnySlice(state["history"])
	if len(history) < 2 ||
		!strings.HasPrefix(metadataString(kiroAnyMap(history[0].(map[string]any)["userInputMessage"]), "content"), "SYSTEM-") {
		t.Fatalf("protected system history missing: %#v", history)
	}
	if !strings.HasPrefix(metadataString(kiroCurrentUserMessage(state), "content"), "CURRENT-") {
		t.Fatal("current message was not preserved")
	}
}

func TestConvertAnthropicToolResultImagesHonorsKiroMediaLimit(t *testing.T) {
	toolResultImages := make([]any, kiroMaxInlineMediaSegments+5)
	for index := range toolResultImages {
		toolResultImages[index] = map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": "image/png",
				"data":       base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("image-%03d", index))),
			},
		}
	}
	payload, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 1024,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": "toolu_images",
				"content":     toolResultImages,
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	converted, err := convertAnthropicToKiro(payload)
	if err != nil {
		t.Fatal(err)
	}
	if converted.inlineMedia != kiroMaxInlineMediaSegments || converted.droppedMedia != 5 {
		t.Fatalf("converted media kept=%d dropped=%d", converted.inlineMedia, converted.droppedMedia)
	}
	state := converted.body["conversationState"].(map[string]any)
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	images := current["images"].([]any)
	if len(images) != kiroMaxInlineMediaSegments {
		t.Fatalf("current images = %d", len(images))
	}
	first := images[0].(map[string]any)["source"].(map[string]any)["bytes"]
	wantFirst := base64.StdEncoding.EncodeToString([]byte("image-005"))
	if first != wantFirst {
		t.Fatalf("first retained image = %#v, want %#v", first, wantFirst)
	}
	contextValue := current["userInputMessageContext"].(map[string]any)
	if _, exists := contextValue["toolResults"]; exists {
		t.Fatalf("orphaned tool result was retained: %#v", contextValue)
	}
}

func TestKiroImageOnlyToolResultUsesNonEmptyPlaceholder(t *testing.T) {
	text, images := extractKiroToolResult([]any{map[string]any{
		"type": "image",
		"source": map[string]any{
			"media_type": "image/png",
			"data":       "aW1hZ2U=",
		},
	}})
	if text != "[image attached]" || len(images) != 1 {
		t.Fatalf("tool result text=%q images=%d", text, len(images))
	}
}

func TestNormalizeKiroToolPairingDropsOrphansAndDuplicates(t *testing.T) {
	toolUse := func(id string) any {
		return map[string]any{"toolUseId": id, "name": "Read", "input": map[string]any{}}
	}
	toolResult := func(id string) any {
		return map[string]any{
			"toolUseId": id,
			"content":   []any{map[string]any{"text": "done"}},
			"status":    "success",
		}
	}
	assistant := map[string]any{
		"content":  " ",
		"toolUses": []any{toolUse("history"), toolUse("current"), toolUse("orphan")},
	}
	historyContext := map[string]any{
		"envState":    kiroEnvironmentState(),
		"toolResults": []any{toolResult("history"), toolResult("unknown")},
	}
	currentContext := map[string]any{
		"envState":    kiroEnvironmentState(),
		"toolResults": []any{toolResult("current"), toolResult("history")},
	}
	state := map[string]any{
		"history": []any{
			map[string]any{"assistantResponseMessage": assistant},
			map[string]any{"userInputMessage": map[string]any{
				"content":                 "",
				"userInputMessageContext": historyContext,
			}},
		},
		"currentMessage": map[string]any{"userInputMessage": map[string]any{
			"content":                 "continue",
			"userInputMessageContext": currentContext,
		}},
	}

	droppedUses, droppedResults := normalizeKiroToolPairing(state)
	if droppedUses != 1 || droppedResults != 2 {
		t.Fatalf("pairing result uses=%d results=%d", droppedUses, droppedResults)
	}
	if got := len(assistant["toolUses"].([]any)); got != 2 {
		t.Fatalf("assistant tool uses = %d", got)
	}
	if got := len(historyContext["toolResults"].([]any)); got != 1 {
		t.Fatalf("history tool results = %d", got)
	}
	if got := len(currentContext["toolResults"].([]any)); got != 1 {
		t.Fatalf("current tool results = %d", got)
	}
}

func TestLimitKiroTextFieldsKeepsHeadAndTail(t *testing.T) {
	longCurrent := "CURRENT-BEGIN-" + strings.Repeat("中", 160_000) + "-CURRENT-END"
	longAssistant := "ASSISTANT-BEGIN-" + strings.Repeat("a", kiroMaxTextFieldBytes) + "-ASSISTANT-END"
	longToolResult := "RESULT-BEGIN-" + strings.Repeat("r", kiroMaxTextFieldBytes) + "-RESULT-END"
	toolResultContent := map[string]any{"text": longToolResult}
	state := map[string]any{
		"currentMessage": map[string]any{"userInputMessage": map[string]any{
			"content": longCurrent,
			"userInputMessageContext": map[string]any{
				"toolResults": []any{map[string]any{
					"content": []any{toolResultContent},
				}},
			},
		}},
		"history": []any{
			map[string]any{"assistantResponseMessage": map[string]any{"content": longAssistant}},
		},
	}

	stats := limitKiroTextFields(state, kiroMaxTextFieldBytes)
	if stats.truncated != 3 || stats.droppedBytes <= 0 || stats.largestBytes != len(longCurrent) {
		t.Fatalf("text stats = %+v", stats)
	}
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)["content"].(string)
	assistant := state["history"].([]any)[0].(map[string]any)["assistantResponseMessage"].(map[string]any)["content"].(string)
	result := toolResultContent["text"].(string)
	for name, value := range map[string]string{
		"current":   current,
		"assistant": assistant,
		"result":    result,
	} {
		if len(value) > kiroMaxTextFieldBytes || !strings.Contains(value, "[ccl truncated ") {
			t.Fatalf("%s length=%d marker=%t", name, len(value), strings.Contains(value, "[ccl truncated "))
		}
	}
	if !strings.HasPrefix(current, "CURRENT-BEGIN-") || !strings.HasSuffix(current, "-CURRENT-END") {
		t.Fatalf("current did not retain head/tail: prefix=%q suffix=%q", current[:20], current[len(current)-20:])
	}
	if !utf8.ValidString(current) {
		t.Fatal("truncated content is not valid UTF-8")
	}
}

func TestConvertAnthropicImageCorrectsMIMEAndResizesLongSide(t *testing.T) {
	sourceImage := image.NewRGBA(image.Rect(0, 0, 2000, 20))
	for y := 0; y < sourceImage.Bounds().Dy(); y++ {
		for x := 0; x < sourceImage.Bounds().Dx(); x++ {
			sourceImage.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
		}
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, sourceImage); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 1024,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": "image/jpeg",
					"data":       base64.StdEncoding.EncodeToString(pngData.Bytes()),
				},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	converted, err := convertAnthropicToKiro(payload)
	if err != nil {
		t.Fatal(err)
	}
	if converted.resizedMedia != 1 || converted.correctedMedia != 1 {
		t.Fatalf("image stats resized=%d corrected=%d", converted.resizedMedia, converted.correctedMedia)
	}
	state := converted.body["conversationState"].(map[string]any)
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	convertedImage := current["images"].([]any)[0].(map[string]any)
	if convertedImage["format"] != "jpeg" {
		t.Fatalf("image format = %#v", convertedImage["format"])
	}
	encoded := convertedImage["source"].(map[string]any)["bytes"].(string)
	if len(encoded) > kiroImageMaxBase64Size {
		t.Fatalf("base64 size = %d", len(encoded))
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" || max(config.Width, config.Height) > kiroImageMaxLongSide {
		t.Fatalf("decoded image format=%q dimensions=%dx%d", format, config.Width, config.Height)
	}
}

func TestKiroEventStreamFrameDecoder(t *testing.T) {
	frame := encodeKiroTestFrame(t, "assistantResponseEvent", map[string]any{"content": "hello"})
	decoded, err := readKiroEventFrame(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.headers[":message-type"] != "event" ||
		decoded.headers[":event-type"] != "assistantResponseEvent" ||
		!strings.Contains(string(decoded.payload), "hello") {
		t.Fatalf("decoded frame = %#v payload=%s", decoded.headers, decoded.payload)
	}

	corrupt := append([]byte{}, frame...)
	corrupt[len(corrupt)-1] ^= 0xff
	if _, err := readKiroEventFrame(bytes.NewReader(corrupt)); err == nil ||
		!strings.Contains(err.Error(), "CRC") {
		t.Fatalf("corrupt frame error = %v", err)
	}
}

func TestKiroInlineThinkingIsConvertedToAnthropicBlock(t *testing.T) {
	request := &kiroConvertedRequest{
		anthropicAdapterRequest: anthropicAdapterRequest{
			upstreamModel:   "claude-haiku-4.5",
			clientModel:     "claude-haiku-4-5",
			thinkingEnabled: true,
			inputTokens:     1,
		},
		model: "claude-haiku-4.5",
	}
	assembler := newAnthropicResponseAssembler(&request.anthropicAdapterRequest, nil)
	for _, content := range []string{"<think", "ing>plan", "</thinking>answer"} {
		payload, _ := json.Marshal(map[string]any{"content": content})
		if err := assembler.processEvent("assistantResponseEvent", payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := assembler.finish(); err != nil {
		t.Fatal(err)
	}
	blocks := assembler.contentBlocks()
	if len(blocks) != 2 {
		t.Fatalf("blocks = %#v", blocks)
	}
	if blocks[0].Type != "thinking" || blocks[0].Thinking != "plan" || blocks[0].Signature == "" {
		t.Fatalf("thinking block = %#v", blocks[0])
	}
	if blocks[1].Type != "text" || blocks[1].Text != "answer" {
		t.Fatalf("text block = %#v", blocks[1])
	}
}

func TestKiroContextWindowMatchesAdvertisedPortalModels(t *testing.T) {
	for _, model := range []string{
		"claude-opus-5",
		"claude-sonnet-5",
		"claude-opus-4.8",
		"claude-opus-4.7",
		"claude-opus-4.6",
		"claude-sonnet-4.6",
	} {
		if got := kiroContextWindow(model); got != 1_000_000 {
			t.Errorf("%s context window = %d, want 1000000", model, got)
		}
	}
	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if got := kiroContextWindow(model); got != 272_000 {
			t.Errorf("%s context window = %d, want 272000", model, got)
		}
	}
	if got := kiroContextWindow("claude-haiku-4.5"); got != 200_000 {
		t.Errorf("fallback context window = %d, want 200000", got)
	}
}

func TestKiroNonStreamingToolResponseKeepsEmptyInput(t *testing.T) {
	request := &kiroConvertedRequest{
		anthropicAdapterRequest: anthropicAdapterRequest{
			upstreamModel: "claude-sonnet-4.6",
			clientModel:   "claude-sonnet-4-6",
			inputTokens:   1,
			toolNameMap:   map[string]string{},
		},
		model: "claude-sonnet-4.6",
	}
	assembler := newAnthropicResponseAssembler(&request.anthropicAdapterRequest, nil)
	payload, _ := json.Marshal(map[string]any{
		"name":      "NoArgs",
		"toolUseId": "toolu_empty",
		"input":     "",
		"stop":      true,
	})
	if err := assembler.processEvent("toolUseEvent", payload); err != nil {
		t.Fatal(err)
	}
	if err := assembler.finish(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(assembler.response())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"input":{}`)) || !bytes.Contains(raw, []byte(`"stop_reason":"tool_use"`)) {
		t.Fatalf("response = %s", raw)
	}
}

func TestKiroMessagesStreamingConversion(t *testing.T) {
	authDir := t.TempDir()
	credential := map[string]any{
		"type":          "kiro",
		"access_token":  "access-token",
		"refresh_token": "refresh-token",
		"profile_arn":   kiroBuilderProfileARN,
		"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"auth_method":   "idc",
		"client_id":     "client",
		"client_secret": "secret",
	}
	rawCredential, _ := json.Marshal(credential)
	if err := os.WriteFile(filepath.Join(authDir, "kiro-test.json"), rawCredential, 0o600); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer access-token" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["profileArn"] != kiroBuilderProfileARN {
			t.Errorf("profileArn = %#v", body["profileArn"])
		}
		state := body["conversationState"].(map[string]any)
		current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
		if current["modelId"] != "claude-sonnet-4.6" {
			t.Errorf("modelId = %#v", current["modelId"])
		}
		writer.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		for _, frame := range [][]byte{
			encodeKiroTestFrame(t, "reasoningContentEvent", map[string]any{"text": "think", "signature": "sig"}),
			encodeKiroTestFrame(t, "assistantResponseEvent", map[string]any{"content": "hello"}),
			encodeKiroTestFrame(t, "toolUseEvent", map[string]any{"name": "Read", "toolUseId": "toolu_1", "input": `{"path":`}),
			encodeKiroTestFrame(t, "toolUseEvent", map[string]any{"name": "Read", "toolUseId": "toolu_1", "input": `"/tmp/a"}`, "stop": true}),
			encodeKiroTestFrame(t, "contextUsageEvent", map[string]any{"contextUsagePercentage": 1.5}),
			encodeKiroTestFrame(t, "meteringEvent", map[string]any{"unit": "credit", "unitPlural": "credits", "usage": 0.25}),
		} {
			_, _ = writer.Write(frame)
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	pool := newKiroCredentialPool(authDir, nil, false, nil)
	service := &kiroService{
		apiKey: "local-key",
		models: []string{"claude-sonnet-4-6"},
		pool:   pool,
		client: upstream.Client(),
		upstreamURL: func(*kiroCredential) string {
			return upstream.URL
		},
	}
	server := httptest.NewServer(service.handler())
	defer server.Close()

	payload := `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"stream":true,
		"thinking":{"type":"adaptive","budget_tokens":10000},
		"tools":[{"name":"Read","description":"Read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],
		"messages":[{"role":"user","content":"read"}]
	}`
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/v1/messages", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("x-api-key", "local-key")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.StatusCode, body)
	}
	stream := string(body)
	for _, want := range []string{
		"event: message_start",
		`"type":"thinking_delta","thinking":"think"`,
		`"type":"signature_delta","signature":"sig"`,
		`"type":"text_delta","text":"hello"`,
		`"type":"tool_use"`,
		`"partial_json":"{\"path\":\"/tmp/a\"}"`,
		`"stop_reason":"tool_use"`,
		`"credit_usage":0.25`,
		"event: message_stop",
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream missing %q:\n%s", want, stream)
		}
	}
	if strings.Index(stream, "event: message_start") > strings.Index(stream, "event: content_block_start") {
		t.Fatalf("message_start emitted after content block:\n%s", stream)
	}
}

func TestKiroBuilderIDDeviceLoginPersistsCredential(t *testing.T) {
	oidc := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/client/register":
			_, _ = io.WriteString(writer, `{"clientId":"client","clientSecret":"secret"}`)
		case "/device_authorization":
			_, _ = io.WriteString(writer, `{
				"deviceCode":"device",
				"userCode":"ABCD-EFGH",
				"verificationUri":"https://example.test/verify",
				"verificationUriComplete":"https://example.test/verify?code=ABCD-EFGH",
				"expiresIn":60,
				"interval":1
			}`)
		case "/token":
			_, _ = io.WriteString(writer, `{"accessToken":"access","refreshToken":"refresh","expiresIn":3600}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer oidc.Close()

	originalEndpoint := kiroOIDCEndpoint
	originalFloor := kiroOAuthPollFloor
	kiroOIDCEndpoint = func(string) string { return oidc.URL }
	kiroOAuthPollFloor = time.Millisecond
	t.Cleanup(func() {
		kiroOIDCEndpoint = originalEndpoint
		kiroOAuthPollFloor = originalFloor
	})

	authDir := t.TempDir()
	result, err := loginKiro(context.Background(), authDir, LoginOptions{
		NoBrowser:    true,
		KiroAuthMode: KiroAuthModeBuilderID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != ProviderKiro || result.Backend != ProviderKiro {
		t.Fatalf("login result = %+v", result)
	}
	stored, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	var credential map[string]any
	if err := json.Unmarshal(stored, &credential); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"type":          "kiro",
		"access_token":  "access",
		"refresh_token": "refresh",
		"auth_method":   "idc",
		"client_id":     "client",
		"client_secret": "secret",
		"profile_arn":   kiroBuilderProfileARN,
	} {
		if credential[key] != want {
			t.Fatalf("%s = %#v, want %q", key, credential[key], want)
		}
	}
}

func TestKiroPortalLoginPersistsSocialCredentialAndWebSession(t *testing.T) {
	var portal *httptest.Server
	portal = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/signin":
			redirectURI := request.URL.Query().Get("redirect_uri")
			if request.URL.Query().Get("code_challenge_method") != "S256" ||
				request.URL.Query().Get("redirect_from") != "KiroIDE" ||
				redirectURI == "" {
				t.Errorf("signin query = %q", request.URL.RawQuery)
			}
			callback := redirectURI + "/oauth/callback?code=authorization-code&state=" +
				url.QueryEscape(request.URL.Query().Get("state")) + "&login_option=Google"
			http.Redirect(writer, request, callback, http.StatusFound)
		case "/oauth/token":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			if payload["code"] != "authorization-code" ||
				!strings.HasSuffix(payload["redirect_uri"].(string), "/oauth/callback?login_option=Google") ||
				strings.TrimSpace(payload["code_verifier"].(string)) == "" {
				t.Errorf("token payload = %#v", payload)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{
				"accessToken":"social-access",
				"refreshToken":"social-refresh",
				"expiresIn":3600,
				"profileArn":"`+kiroSocialProfileARN+`"
			}`)
		case "/home":
			cookies := make(map[string]string)
			for _, cookie := range request.Cookies() {
				cookies[cookie.Name] = cookie.Value
			}
			if cookies["AccessToken"] != "social-access" ||
				cookies["RefreshToken"] != "social-refresh" ||
				cookies["Idp"] != "Google" {
				t.Errorf("portal cookies = %#v", cookies)
			}
			_, _ = io.WriteString(writer, `<html><head>
				<meta name="csrf-token" content="portal-csrf">
				<meta name="user-id" content="google-user">
				<meta name="profile-arn" content="`+kiroSocialProfileARN+`">
				<meta name="idp" content="Google">
			</head></html>`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer portal.Close()

	originalAuthEndpoint := kiroAuthEndpoint
	originalSignInEndpoint := kiroPortalSignInEndpoint
	originalHomeEndpoint := kiroWebPortalHomeEndpoint
	originalBrowserOpener := kiroBrowserOpener
	kiroAuthEndpoint = func(string) string { return portal.URL }
	kiroPortalSignInEndpoint = portal.URL + "/signin"
	kiroWebPortalHomeEndpoint = portal.URL + "/home"
	kiroBrowserOpener = func(target string) error {
		response, err := http.Get(target)
		if err != nil {
			return err
		}
		return response.Body.Close()
	}
	t.Cleanup(func() {
		kiroAuthEndpoint = originalAuthEndpoint
		kiroPortalSignInEndpoint = originalSignInEndpoint
		kiroWebPortalHomeEndpoint = originalHomeEndpoint
		kiroBrowserOpener = originalBrowserOpener
	})

	authDir := t.TempDir()
	result, err := loginKiro(context.Background(), authDir, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	var credential map[string]any
	if err := json.Unmarshal(stored, &credential); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"type":          "kiro",
		"access_token":  "social-access",
		"refresh_token": "social-refresh",
		"auth_method":   "social",
		"provider":      "Google",
		"profile_arn":   kiroSocialProfileARN,
		"csrf_token":    "portal-csrf",
		"user_id":       "google-user",
		"region":        "us-east-1",
		"auth_region":   "us-east-1",
		"api_region":    "us-east-1",
	} {
		if credential[key] != want {
			t.Fatalf("%s = %#v, want %q", key, credential[key], want)
		}
	}
	if result.Provider != ProviderKiro || result.Backend != ProviderKiro ||
		filepath.Base(result.Path) != "kiro-google-user.json" {
		t.Fatalf("login result = %+v", result)
	}
}

func TestReadKiroInboundBodyRejectsOversizeWithoutSilentJSONTruncation(t *testing.T) {
	if kiroMaxInboundRequestBytes <= 32<<20 {
		t.Fatalf("Kiro inbound request limit = %d; must accommodate 1M-context requests", kiroMaxInboundRequestBytes)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(strings.Repeat("x", 65)))
	request.ContentLength = -1 // Exercise streaming/chunked bodies through MaxBytesReader.
	_, err := readAnthropicInboundBody(httptest.NewRecorder(), request, 64)
	if err == nil || !strings.Contains(err.Error(), "exceeds 64 bytes limit") {
		t.Fatalf("oversize error = %v", err)
	}

	valid := []byte(`{"model":"claude-sonnet-5","messages":[]}`)
	request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(valid))
	got, err := readAnthropicInboundBody(httptest.NewRecorder(), request, int64(len(valid)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, valid) {
		t.Fatalf("body = %q, want %q", got, valid)
	}
}

func TestStartKiroRuntimeExposesAnthropicModels(t *testing.T) {
	var modelRequests atomic.Int32
	modelUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		modelRequests.Add(1)
		if request.URL.Path != "/ListAvailableModels" || request.URL.Query().Get("origin") != "AI_EDITOR" {
			t.Errorf("model discovery URL = %s", request.URL.String())
		}
		if request.Header.Get("Authorization") != "Bearer access" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		if !strings.Contains(request.Header.Get("User-Agent"), "KiroIDE-0.9.2-") {
			t.Errorf("user-agent = %q", request.Header.Get("User-Agent"))
		}
		_, _ = io.WriteString(writer, `{"models":[{
			"modelId":"claude-sonnet-4.6",
			"modelName":"Claude Sonnet 4.6",
			"description":"1M context model",
			"tokenLimits":{"maxInputTokens":1000000,"maxOutputTokens":64000}
		},{"modelId":"gpt-5.6-terra","modelName":"GPT 5.6 Terra"}]}`)
	}))
	defer modelUpstream.Close()
	originalModelsEndpoint := kiroAvailableModelsEndpoint
	kiroAvailableModelsEndpoint = func(string) string {
		return modelUpstream.URL + "/ListAvailableModels?origin=AI_EDITOR"
	}
	t.Cleanup(func() { kiroAvailableModelsEndpoint = originalModelsEndpoint })

	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credential := []byte(`{
		"type":"kiro",
		"access_token":"access",
		"refresh_token":"refresh",
		"expires_at":"2099-01-01T00:00:00Z",
		"client_id":"client",
		"client_secret":"secret"
	}`)
	if err := os.WriteFile(filepath.Join(authDir, "kiro-test.json"), credential, 0o600); err != nil {
		t.Fatal(err)
	}
	proxyRuntime, err := StartOAuth(context.Background(), ProviderKiro, "claude-sonnet-4-6[1m]", "kiro-test.json")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := proxyRuntime.Endpoint()
	request, err := http.NewRequest(http.MethodGet, endpoint+"/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+proxyRuntime.APIKey())
	for attempt := 0; attempt < 2; attempt++ {
		response, err := http.DefaultClient.Do(request.Clone(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		for _, want := range []string{
			`"id":"claude-sonnet-4.6"`,
			`"id":"claude-sonnet-4-6[1m]"`,
			`"id":"gpt-5.6-terra"`,
			`"description":"1M context model"`,
			`"max_input_tokens":1000000`,
			`"max_output_tokens":64000`,
		} {
			if response.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
				t.Fatalf("models status=%d missing %q body=%s", response.StatusCode, want, body)
			}
		}
	}
	if modelRequests.Load() != 1 {
		t.Fatalf("ListAvailableModels requests = %d, want cached single request", modelRequests.Load())
	}
	proxyRuntime.Stop()
	if _, err := http.Get(endpoint + "/models"); err == nil {
		t.Fatal("Kiro runtime still accepts connections after Stop")
	}
}

func TestKiroAvailableModelsMergeAccounts(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/us-east-1" || request.URL.Query().Get("origin") != "AI_EDITOR" {
			t.Errorf("model discovery URL = %s", request.URL.String())
		}
		if strings.Contains(request.URL.RawQuery, "profileArn") || request.Header.Get("x-amzn-kiro-profile-arn") != "" {
			t.Errorf("model discovery sent profile ARN: URL=%s headers=%v", request.URL, request.Header)
		}
		switch request.Header.Get("Authorization") {
		case "Bearer access-a":
			_, _ = io.WriteString(writer, `{"models":[{"modelId":"claude-sonnet-5","modelName":"Claude Sonnet 5"}]}`)
		case "Bearer access-b":
			_, _ = io.WriteString(writer, `{"models":[{"modelId":"gpt-5.6-sol","modelName":"GPT 5.6 Sol"}]}`)
		default:
			http.Error(writer, "bad token", http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	authDir := t.TempDir()
	for name, token := range map[string]string{"kiro-a.json": "access-a", "kiro-b.json": "access-b"} {
		raw := []byte(fmt.Sprintf(`{
			"type":"kiro",
			"access_token":%q,
			"expires_at":"2099-01-01T00:00:00Z"
		}`, token))
		if err := os.WriteFile(filepath.Join(authDir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	service := &kiroService{
		models: kiroRuntimeModels("claude-sonnet-5"),
		pool:   newKiroCredentialPool(authDir, nil, false, nil),
		client: upstream.Client(),
		modelCatalog: newKiroModelCatalog(func(region string) string {
			return upstream.URL + "/" + region + "?origin=AI_EDITOR"
		}),
	}
	models, err := service.availableModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(models))
	for _, model := range models {
		got[model.ModelID] = true
	}
	for _, want := range []string{"claude-sonnet-5", "gpt-5.6-sol"} {
		if !got[want] {
			t.Fatalf("merged models missing %q: %+v", want, models)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("ListAvailableModels requests = %d, want one per credential", requests.Load())
	}
}

func TestKiroModelsRESTRegionDefaultsToUSEast(t *testing.T) {
	regions := kiroRESTRegionCandidates(&kiroCredential{authRegion: "eu-west-1", apiRegion: "ap-southeast-1"})
	if len(regions) != 2 || regions[0] != "us-east-1" || regions[1] != "eu-central-1" {
		t.Fatalf("REST region candidates = %v", regions)
	}
}

func TestKiroModelsRESTFallsBackAfterUSEast403(t *testing.T) {
	var usRequests atomic.Int32
	var euRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/us-east-1":
			usRequests.Add(1)
			http.Error(writer, `{"message":"Invalid token"}`, http.StatusForbidden)
		case "/eu-central-1":
			euRequests.Add(1)
			_, _ = io.WriteString(writer, `{"models":[{"modelId":"claude-sonnet-5"}]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	authDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(authDir, "kiro-test.json"), []byte(`{
		"type":"kiro",
		"access_token":"access",
		"expires_at":"2099-01-01T00:00:00Z"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &kiroService{
		pool:   newKiroCredentialPool(authDir, nil, false, nil),
		client: upstream.Client(),
		modelCatalog: newKiroModelCatalog(func(region string) string {
			return upstream.URL + "/" + region + "?origin=AI_EDITOR"
		}),
	}
	models, err := service.availableModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ModelID != "claude-sonnet-5" {
		t.Fatalf("fallback models = %+v", models)
	}
	if usRequests.Load() != 1 || euRequests.Load() != 1 {
		t.Fatalf("region requests us=%d eu=%d", usRequests.Load(), euRequests.Load())
	}
}

func TestKiroWebPortalModelsCBORIncludesCreditMetadata(t *testing.T) {
	var portalRequests atomic.Int32
	var qRequests atomic.Int32
	rate := 2.2
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/service/KiroWebPortalService/operation/ListAvailableModels" {
			http.NotFound(writer, request)
			return
		}
		portalRequests.Add(1)
		for header, want := range map[string]string{
			"Authorization":    "Bearer portal-access",
			"X-CSRF-Token":     "csrf-token",
			"Smithy-Protocol":  "rpc-v2-cbor",
			"Content-Type":     "application/cbor",
			"Accept":           "application/cbor",
			"X-Kiro-Userid":    "user-1",
			"X-Kiro-Visitorid": "visitor-1",
		} {
			if got := request.Header.Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
		for name, want := range map[string]string{
			"AccessToken":     "portal-access",
			"RefreshToken":    "portal-refresh",
			"Idp":             "Google",
			"UserId":          "user-1",
			"kiro-visitor-id": "visitor-1",
		} {
			cookie, err := request.Cookie(name)
			if err != nil || cookie.Value != want {
				t.Errorf("cookie %s = %#v, %v; want %q", name, cookie, err, want)
			}
		}
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var input kiroPortalModelsRequest
		if err := codec.NewDecoderBytes(raw, kiroCBORHandle).Decode(&input); err != nil {
			t.Errorf("decode portal request: %v", err)
			return
		}
		if input.CSRFToken != "csrf-token" || input.ProfileARN != "arn:aws:codewhisperer:us-east-1:123:profile/test" {
			t.Errorf("portal input = %+v", input)
		}
		var responseBody []byte
		err = codec.NewEncoderBytes(&responseBody, kiroCBORHandle).Encode(kiroPortalModelsResponse{
			Models: []kiroAvailableModel{{
				ModelID:             "claude-opus-5",
				ModelName:           "Claude Opus 5",
				Description:         "Experimental preview",
				RateMultiplier:      &rate,
				RateUnit:            "Credits",
				SupportedInputTypes: []string{"TEXT", "IMAGE"},
			}},
		})
		if err != nil {
			t.Error(err)
			return
		}
		writer.Header().Set("Content-Type", "application/cbor")
		_, _ = io.WriteString(writer, "data:application/cbor;base64,"+base64.StdEncoding.EncodeToString(responseBody))
	}))
	defer upstream.Close()

	authDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(authDir, "kiro-web.json"), []byte(`{
		"type":"kiro",
		"access_token":"portal-access",
		"refresh_token":"portal-refresh",
		"expires_at":"2099-01-01T00:00:00Z",
		"profile_arn":"arn:aws:codewhisperer:us-east-1:123:profile/test",
		"provider":"Google",
		"csrf_token":"csrf-token",
		"user_id":"user-1",
		"visitor_id":"visitor-1"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := newKiroModelCatalog(func(string) string {
		qRequests.Add(1)
		return upstream.URL + "/unexpected-q"
	})
	catalog.portalModelsEndpoint = upstream.URL + "/service/KiroWebPortalService/operation/ListAvailableModels"
	catalog.portalHomeEndpoint = upstream.URL + "/home"
	service := &kiroService{
		apiKey:       "local-key",
		models:       kiroRuntimeModels("claude-opus-5[1m]"),
		modelCatalog: catalog,
		pool:         newKiroCredentialPool(authDir, nil, false, nil),
		client:       upstream.Client(),
	}
	server := httptest.NewServer(service.handler())
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("x-api-key", "local-key")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	for _, want := range []string{
		`"id":"claude-opus-5"`,
		`"id":"claude-opus-5[1m]"`,
		`"rate_multiplier":2.2`,
		`"rate_unit":"Credits"`,
		`"supported_input_types":["TEXT","IMAGE"]`,
	} {
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Fatalf("models status=%d missing %q body=%s", response.StatusCode, want, body)
		}
	}
	if portalRequests.Load() != 1 || qRequests.Load() != 0 {
		t.Fatalf("model requests portal=%d q=%d", portalRequests.Load(), qRequests.Load())
	}
}

func TestKiroWebPortalSessionBootstrapsFromCookies(t *testing.T) {
	var homeRequests atomic.Int32
	var portalRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/home":
			homeRequests.Add(1)
			for name, want := range map[string]string{
				"AccessToken":  "portal-access",
				"RefreshToken": "portal-refresh",
				"Idp":          "Google",
			} {
				cookie, err := request.Cookie(name)
				if err != nil || cookie.Value != want {
					t.Errorf("bootstrap cookie %s = %#v, %v; want %q", name, cookie, err, want)
				}
			}
			_, _ = io.WriteString(writer, `<html><head>
				<meta name="csrf-token" content="fresh-csrf">
				<meta name="user-id" content="fresh-user">
				<meta name="idp" content="Google">
				<meta name="profile-arn" content="arn:aws:codewhisperer:us-east-1:123:profile/fresh">
			</head></html>`)
		case "/service/KiroWebPortalService/operation/ListAvailableModels":
			portalRequests.Add(1)
			if request.Header.Get("X-CSRF-Token") != "fresh-csrf" || request.Header.Get("X-Kiro-Userid") != "fresh-user" {
				t.Errorf("bootstrapped headers = %v", request.Header)
			}
			var input kiroPortalModelsRequest
			raw, _ := io.ReadAll(request.Body)
			if err := codec.NewDecoderBytes(raw, kiroCBORHandle).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			if input.ProfileARN != "arn:aws:codewhisperer:us-east-1:123:profile/fresh" {
				t.Errorf("bootstrapped input = %+v", input)
			}
			var responseBody []byte
			if err := codec.NewEncoderBytes(&responseBody, kiroCBORHandle).Encode(kiroPortalModelsResponse{
				Models: []kiroAvailableModel{{ModelID: "gpt-5.6-terra"}},
			}); err != nil {
				t.Error(err)
				return
			}
			_, _ = writer.Write(responseBody)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	authDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(authDir, "kiro-web.json"), []byte(`{
		"type":"kiro",
		"access_token":"portal-access",
		"refresh_token":"portal-refresh",
		"expires_at":"2099-01-01T00:00:00Z",
		"provider":"Google"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := newKiroModelCatalog(func(string) string { return upstream.URL + "/unexpected-q" })
	catalog.portalHomeEndpoint = upstream.URL + "/home"
	catalog.portalModelsEndpoint = upstream.URL + "/service/KiroWebPortalService/operation/ListAvailableModels"
	service := &kiroService{
		modelCatalog: catalog,
		pool:         newKiroCredentialPool(authDir, nil, false, nil),
		client:       upstream.Client(),
	}
	models, err := service.availableModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ModelID != "gpt-5.6-terra" {
		t.Fatalf("bootstrapped models = %+v", models)
	}
	if homeRequests.Load() != 1 || portalRequests.Load() != 1 {
		t.Fatalf("portal requests home=%d models=%d", homeRequests.Load(), portalRequests.Load())
	}
}

func TestKiroWebPortalFailureFallsBackToAmazonQCatalog(t *testing.T) {
	var portalRequests atomic.Int32
	var qRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/home":
			_, _ = io.WriteString(writer, `<html><head></head></html>`)
		case "/service/KiroWebPortalService/operation/ListAvailableModels":
			portalRequests.Add(1)
			http.Error(writer, "expired web session", http.StatusForbidden)
		case "/us-east-1":
			qRequests.Add(1)
			_, _ = io.WriteString(writer, `{"models":[{"modelId":"claude-sonnet-5","modelName":"Claude Sonnet 5"}]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	authDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(authDir, "kiro-web.json"), []byte(`{
		"type":"kiro",
		"access_token":"access",
		"refresh_token":"refresh",
		"expires_at":"2099-01-01T00:00:00Z",
		"provider":"Google",
		"csrf_token":"expired-csrf"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := newKiroModelCatalog(func(region string) string {
		return upstream.URL + "/" + region + "?origin=AI_EDITOR"
	})
	catalog.portalHomeEndpoint = upstream.URL + "/home"
	catalog.portalModelsEndpoint = upstream.URL + "/service/KiroWebPortalService/operation/ListAvailableModels"
	service := &kiroService{
		modelCatalog: catalog,
		pool:         newKiroCredentialPool(authDir, nil, false, nil),
		client:       upstream.Client(),
	}
	models, err := service.availableModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ModelID != "claude-sonnet-5" {
		t.Fatalf("fallback models = %+v", models)
	}
	if portalRequests.Load() != 1 || qRequests.Load() != 1 {
		t.Fatalf("fallback requests portal=%d q=%d", portalRequests.Load(), qRequests.Load())
	}
}

func TestKiroExpiredIDCTokenRefreshesAndPersists(t *testing.T) {
	oidc := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/token" {
			http.NotFound(writer, request)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload["grantType"] != "refresh_token" || payload["refreshToken"] != "old-refresh" {
			t.Errorf("refresh payload = %#v", payload)
		}
		_, _ = io.WriteString(writer, `{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}`)
	}))
	defer oidc.Close()
	originalEndpoint := kiroOIDCEndpoint
	kiroOIDCEndpoint = func(string) string { return oidc.URL }
	t.Cleanup(func() { kiroOIDCEndpoint = originalEndpoint })

	authDir := t.TempDir()
	path := filepath.Join(authDir, "kiro-expired.json")
	if err := os.WriteFile(path, []byte(`{
		"type":"kiro",
		"access_token":"old-access",
		"refresh_token":"old-refresh",
		"expires_at":"2020-01-01T00:00:00Z",
		"auth_method":"idc",
		"client_id":"client",
		"client_secret":"secret"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := newKiroCredentialPool(authDir, nil, false, nil)
	loaded, err := pool.load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load = %#v, %v", loaded, err)
	}
	refreshed, err := pool.usableCredential(context.Background(), loaded[0], false)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.accessToken != "new-access" || refreshed.refreshToken != "new-refresh" {
		t.Fatalf("refreshed = %+v", refreshed)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stored, []byte(`"access_token":"new-access"`)) ||
		!bytes.Contains(stored, []byte(`"refresh_token":"new-refresh"`)) {
		t.Fatalf("stored credential = %s", stored)
	}
}

func TestKiroExpiredSocialTokenRefreshesAndPersists(t *testing.T) {
	authServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/refreshToken" {
			http.NotFound(writer, request)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload["refreshToken"] != "old-social-refresh" {
			t.Errorf("refresh payload = %#v", payload)
		}
		if !strings.HasPrefix(request.Header.Get("User-Agent"), "KiroIDE-") {
			t.Errorf("user-agent = %q", request.Header.Get("User-Agent"))
		}
		_, _ = io.WriteString(writer, `{
			"accessToken":"new-social-access",
			"refreshToken":"new-social-refresh",
			"profileArn":"`+kiroSocialProfileARN+`",
			"expiresIn":3600
		}`)
	}))
	defer authServer.Close()
	originalAuthEndpoint := kiroAuthEndpoint
	kiroAuthEndpoint = func(string) string { return authServer.URL }
	t.Cleanup(func() { kiroAuthEndpoint = originalAuthEndpoint })

	authDir := t.TempDir()
	path := filepath.Join(authDir, "kiro-social-expired.json")
	if err := os.WriteFile(path, []byte(`{
		"type":"kiro",
		"access_token":"old-social-access",
		"refresh_token":"old-social-refresh",
		"expires_at":"2020-01-01T00:00:00Z",
		"auth_method":"social",
		"provider":"Google"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := newKiroCredentialPool(authDir, nil, false, nil)
	loaded, err := pool.load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load = %#v, %v", loaded, err)
	}
	refreshed, err := pool.usableCredential(context.Background(), loaded[0], false)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.accessToken != "new-social-access" ||
		refreshed.refreshToken != "new-social-refresh" ||
		refreshed.profileARN != kiroSocialProfileARN {
		t.Fatalf("refreshed social credential = %+v", refreshed)
	}
}

func encodeKiroTestFrame(t *testing.T, eventType string, payload any) []byte {
	t.Helper()
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	headers := append(encodeKiroTestHeader(":message-type", "event"), encodeKiroTestHeader(":event-type", eventType)...)
	totalLength := kiroEventPreludeSize + len(headers) + len(payloadRaw) + 4
	frame := make([]byte, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], headers)
	copy(frame[12+len(headers):], payloadRaw)
	binary.BigEndian.PutUint32(frame[totalLength-4:], crc32.ChecksumIEEE(frame[:totalLength-4]))
	return frame
}

func encodeKiroTestHeader(name, value string) []byte {
	raw := make([]byte, 0, 1+len(name)+1+2+len(value))
	raw = append(raw, byte(len(name)))
	raw = append(raw, name...)
	raw = append(raw, 7)
	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(value)))
	raw = append(raw, length...)
	raw = append(raw, value...)
	return raw
}

func TestKiroAssemblerTokenTotalsUsesContextOverride(t *testing.T) {
	assembler := newAnthropicResponseAssembler(&anthropicAdapterRequest{inputTokens: 50}, nil)
	assembler.outputTokens = 30
	if input, output := assembler.tokenTotals(); input != 50 || output != 30 {
		t.Fatalf("tokenTotals() = (%d, %d), want (50, 30)", input, output)
	}

	// contextUsageEvent overrides the request's estimated input tokens with the
	// server-reported context size.
	assembler.contextTokens = 4096
	if input, output := assembler.tokenTotals(); input != 4096 || output != 30 {
		t.Fatalf("tokenTotals() with contextTokens set = (%d, %d), want (4096, 30)", input, output)
	}
}

func TestKiroAssemblerUsageMatchesTokenTotals(t *testing.T) {
	assembler := newAnthropicResponseAssembler(&anthropicAdapterRequest{inputTokens: 12}, nil)
	assembler.outputTokens = 7
	usage := assembler.usage()
	if usage["input_tokens"] != 12 || usage["output_tokens"] != 7 {
		t.Fatalf("usage() = %+v, want input_tokens=12 output_tokens=7", usage)
	}
}

func TestKiroServiceRecordKiroUsageKeyedByClientModel(t *testing.T) {
	service := &kiroService{usage: NewUsageTracker()}
	converted := &kiroConvertedRequest{
		anthropicAdapterRequest: anthropicAdapterRequest{
			upstreamModel: "claude-sonnet-4-6",
			clientModel:   "claude-sonnet-4-6[1m]",
			inputTokens:   100,
		},
		model: "claude-sonnet-4-6",
	}
	assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, nil)
	assembler.outputTokens = 40

	service.recordKiroUsage(converted, assembler)

	totals, ok := service.usage.Snapshot()
	if !ok || len(totals) != 1 {
		t.Fatalf("expected one recorded model, got %+v (ok=%t)", totals, ok)
	}
	got := totals[0]
	if got.Model != "claude-sonnet-4-6[1m]" {
		t.Fatalf("expected usage keyed by clientModel alias, got %q", got.Model)
	}
	if got.InputTokens != 100 || got.OutputTokens != 40 || got.Requests != 1 {
		t.Fatalf("unexpected recorded totals: %+v", got)
	}
}

func TestKiroServiceRecordKiroUsageNilTrackerIsNoop(t *testing.T) {
	service := &kiroService{}
	converted := &kiroConvertedRequest{anthropicAdapterRequest: anthropicAdapterRequest{clientModel: "claude-sonnet-4-6", inputTokens: 10}}
	assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, nil)
	// Must not panic when the service has no usage tracker.
	service.recordKiroUsage(converted, assembler)
}
