package oauthproxy

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCommandCodePreservesToolJSONNumbers(t *testing.T) {
	const numbers = `{"id":9007199254740993,"values":[1e1000,0.123456789012345678901]}`
	raw := `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":` + numbers + `}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"ok"}]}],"tools":[{"name":"f","input_schema":{"type":"object","properties":{"value":{"const":` + numbers + `}}}}]}`
	c, err := convertAnthropicToCommandCode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(c.body)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"params.messages.0.content.0.input", "params.tools.0.input_schema.properties.value.const"} {
		for _, field := range []string{"id", "values.0", "values.1"} {
			if got, want := gjson.GetBytes(body, path+"."+field).Raw, gjson.Get(numbers, field).Raw; got != want {
				t.Fatalf("%s.%s changed to %s; want %s", path, field, got, want)
			}
		}
	}
}

func TestGeminiGenerationLimitsPreserved(t *testing.T) {
	for _, maxTokens := range []int{17, 32000} {
		request := fmt.Sprintf(`{"model":"gemini","max_tokens":%d,"stop_sequences":["END","STOP"],"messages":[{"role":"user","content":"hi"}]}`, maxTokens)
		c, err := convertAnthropicToGemini([]byte(request))
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(c.geminiBody, "generationConfig.maxOutputTokens").Int(); got != int64(maxTokens) {
			t.Fatalf("maxOutputTokens=%d", got)
		}
		if got := gjson.GetBytes(c.geminiBody, "generationConfig.stopSequences").Raw; got != `["END","STOP"]` {
			t.Fatalf("stopSequences=%s", got)
		}
	}
	if _, err := convertAnthropicToGemini([]byte(`{"model":"gemini","stop_sequences":[123],"messages":[{"role":"user","content":"hi"}]}`)); err == nil {
		t.Fatal("invalid stops accepted")
	}
}

func TestGeminiToolNamesAreUniqueAndOrderIndependent(t *testing.T) {
	names := []string{"1tool", "_1tool", "a/b", "a_b", strings.Repeat("x", 64) + "A", strings.Repeat("x", 64) + "B"}
	var first map[string]string
	for _, reverse := range []bool{false, true} {
		var tools []any
		for index := range names {
			if reverse {
				index = len(names) - 1 - index
			}
			tools = append(tools, map[string]any{"name": names[index], "input_schema": map[string]any{"type": "object"}})
		}
		raw, _ := json.Marshal(map[string]any{"tools": tools})
		mapping, err := buildGeminiToolNames(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			wire := mapping.byOriginal[name]
			if wire == "" || wire != sanitizeFunctionName(wire) || mapping.byWire[wire] != name {
				t.Fatalf("broken bijection for %q: %+v", name, mapping)
			}
			if first != nil && first[name] != wire {
				t.Fatalf("mapping changed with order for %q", name)
			}
		}
		first = mapping.byOriginal
	}
}

func TestGeminiCollidingToolsSignedRoundTrip(t *testing.T) {
	tools := []any{
		map[string]any{"name": "1tool", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"name": "_1tool", "input_schema": map[string]any{"type": "object"}},
	}
	user := map[string]any{"role": "user", "content": "use the tools"}
	raw, _ := json.Marshal(map[string]any{"model": "gemini", "tools": tools, "messages": []any{user}})
	initial, err := convertAnthropicToGemini(raw)
	if err != nil {
		t.Fatal(err)
	}
	var nativeParts []any
	for _, declaration := range gjson.GetBytes(initial.geminiBody, "tools.0.functionDeclarations").Array() {
		nativeParts = append(nativeParts, map[string]any{"functionCall": map[string]any{"name": declaration.Get("name").String(), "args": json.RawMessage(`{"id":9007199254740993}`)}, "thoughtSignature": "opaque-" + declaration.Get("name").String()})
	}
	response, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": nativeParts}, "finishReason": "STOP"}}})
	a := newAnthropicResponseAssembler(&initial.anthropicAdapterRequest, nil)
	if err := processGeminiNonStream(response, a); err != nil {
		t.Fatal(err)
	}
	messages := []any{user}
	var results []any
	for _, block := range a.contentBlocks() {
		// Exercise signature and tool_use companions split across messages.
		messages = append(messages, map[string]any{"role": "assistant", "content": []any{block}})
		if block.Type == "tool_use" {
			results = append(results, map[string]any{"type": "tool_result", "tool_use_id": block.ID, "content": "ok"})
		}
	}
	messages = append(messages, map[string]any{"role": "user", "content": results})
	// Introduce a new tool whose native name collides with a signed alias.
	wire0 := gjson.GetBytes(initial.geminiBody, "tools.0.functionDeclarations.0.name").String()
	tools = append(tools, map[string]any{"name": wire0, "input_schema": map[string]any{"type": "object"}})
	raw, _ = json.Marshal(map[string]any{"model": "gemini", "tools": tools, "messages": messages, "tool_choice": map[string]any{"type": "tool", "name": "1tool"}})
	replay, err := convertAnthropicToGemini(raw)
	if err != nil {
		t.Fatal(err)
	}
	var calls, replies []string
	for _, content := range gjson.GetBytes(replay.geminiBody, "contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if part.Get("functionCall").Exists() {
				calls = append(calls, part.Raw)
			}
			if part.Get("functionResponse").Exists() {
				replies = append(replies, part.Get("functionResponse.name").String())
			}
		}
	}
	if len(calls) != 2 || len(replies) != 2 {
		t.Fatalf("missing calls/results: %s", replay.geminiBody)
	}
	for index, native := range nativeParts {
		want, _ := json.Marshal(native)
		if calls[index] != string(want) || replies[index] != gjson.GetBytes(want, "functionCall.name").String() {
			t.Fatalf("signed call or result changed: %s; reply=%s", calls[index], replies[index])
		}
	}
	seen := map[string]bool{}
	for _, declaration := range gjson.GetBytes(replay.geminiBody, "tools.0.functionDeclarations").Array() {
		name := declaration.Get("name").String()
		if seen[name] {
			t.Fatalf("duplicate declaration %s", name)
		}
		seen[name] = true
	}
	if got := gjson.GetBytes(replay.geminiBody, "toolConfig.functionCallingConfig.allowedFunctionNames.0").String(); got != wire0 || replay.toolNameMap[got] != "1tool" {
		t.Fatalf("tool choice or reverse mapping changed: %s", got)
	}
}

func TestGeminiBlockedResponsesAreRefusals(t *testing.T) {
	for _, body := range []string{
		`{"candidates":[{"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":0}}`,
		`{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":0}}`,
		`{"response":{"promptFeedback":{"blockReason":"BLOCKLIST"}},"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":0}}`,
	} {
		for _, stream := range []bool{false, true} {
			w := httptest.NewRecorder()
			a := newAnthropicResponseAssembler(&anthropicAdapterRequest{inputTokens: 777}, nil)
			var err error
			if stream {
				a.writer = w
				err = processGeminiStream(strings.NewReader("data: "+body+"\n\n"), a)
			} else {
				err = processGeminiNonStream([]byte(body), a)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !a.finished || a.resolvedStopReason() != "refusal" || len(a.contentBlocks()) != 1 || !strings.Contains(a.contentBlocks()[0].Text, "blocked") {
				t.Fatalf("lost refusal: %v", a.response())
			}
			if in, out := a.tokenTotals(); in != 10 || out != 0 {
				t.Fatalf("usage=%d/%d", in, out)
			}
			if stream && !strings.Contains(w.Body.String(), `"stop_reason":"refusal"`) {
				t.Fatal("SSE refusal missing")
			}
		}
	}
	for _, body := range []string{`{}`, `{"candidates":[]}`, `{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}`} {
		a := newAnthropicResponseAssembler(&anthropicAdapterRequest{}, nil)
		if err := processGeminiNonStream([]byte(body), a); err == nil || a.finished {
			t.Fatalf("invalid response accepted: %s", body)
		}
	}
}

func TestResponsesReasoningTracksEverySummaryPart(t *testing.T) {
	for _, mode := range []string{"done-only", "mixed", "terminal-only"} {
		t.Run(mode, func(t *testing.T) {
			var stream strings.Builder
			emit := func(event map[string]any) { raw, _ := json.Marshal(event); fmt.Fprintf(&stream, "data: %s\n\n", raw) }
			item := map[string]any{"type": "reasoning", "id": "r1", "summary": []any{map[string]any{"type": "summary_text", "text": "first"}, map[string]any{"type": "summary_text", "text": "second"}}, "encrypted_content": "opaque"}
			if mode != "terminal-only" {
				emit(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "r1"}})
				if mode == "mixed" {
					for _, delta := range []string{"fi", "rst"} {
						emit(map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": "r1", "summary_index": 0, "delta": delta})
					}
				}
				for index, text := range []string{"first", "second"} {
					emit(map[string]any{"type": "response.reasoning_summary_text.done", "item_id": "r1", "summary_index": index, "text": text})
					emit(map[string]any{"type": "response.reasoning_summary_part.done", "item_id": "r1", "summary_index": index, "part": map[string]any{"text": text}})
				}
				emit(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
			}
			emit(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{item, map[string]any{"type": "reasoning", "id": "r2", "summary": []any{map[string]any{"type": "summary_text", "text": "third"}}}}}})
			a := newAnthropicResponseAssembler(&anthropicAdapterRequest{}, nil)
			if err := processCodexResponsesStream(strings.NewReader(stream.String()), a); err != nil {
				t.Fatal(err)
			}
			blocks := a.contentBlocks()
			if len(blocks) != 2 || blocks[0].Thinking != "first\n\nsecond" || blocks[1].Thinking != "third" {
				t.Fatalf("missing/duplicated summaries: %+v", blocks)
			}
			if blocks[0].Signature == "" || blocks[1].Signature == "" {
				t.Fatal("reasoning signatures missing")
			}
		})
	}
}
