package oauthproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
)

func TestGeminiSignedReplayKeepsNativeCallIDs(t *testing.T) {
	for _, nativeID := range []string{"toolu_native", ""} {
		t.Run("native="+nativeID, func(t *testing.T) {
			part := `{"functionCall":{"name":"exec","args":{}},"thoughtSignature":"opaque+/="}`
			if nativeID != "" {
				part = `{"functionCall":{"id":"toolu_native","name":"exec","args":{}},"thoughtSignature":"opaque+/="}`
			}
			signature := geminiPartSignaturePrefix + base64.StdEncoding.EncodeToString([]byte(part))
			body, _ := json.Marshal(map[string]any{"model": "gemini-3-flash", "messages": []any{
				map[string]any{"role": "user", "content": "run"},
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "signature": signature}}},
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "old-client-id", "name": "exec", "input": map[string]any{}}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "old-client-id", "content": "ok"}}},
			}})
			converted, err := convertAnthropicToGemini(body)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(converted.geminiBody, "contents.1.parts.0").Raw; got != part {
				t.Fatalf("signed part changed: %s", got)
			}
			result := gjson.GetBytes(converted.geminiBody, "contents.2.parts.0.functionResponse")
			if got := result.Get("id").String(); got != nativeID || (nativeID == "" && result.Get("id").Exists()) {
				t.Fatalf("result ID = %q, want native ID %q", got, nativeID)
			}
		})
	}
}

func TestGeminiResponsePreservesNativeToolID(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		body := `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"toolu_native","name":"exec","args":{}},"thoughtSignature":"opaque+/="}]},"finishReason":"STOP"}]}`
		assembler := newAnthropicResponseAssembler(&anthropicAdapterRequest{}, nil)
		var err error
		if streaming {
			err = processGeminiStream(strings.NewReader("data: "+body+"\n\n"), assembler)
		} else {
			err = processGeminiNonStream([]byte(body), assembler)
		}
		if err != nil {
			t.Fatal(err)
		}
		var id string
		for _, block := range assembler.contentBlocks() {
			if block.Type == "tool_use" {
				id = block.ID
			}
		}
		if id != "toolu_native" {
			t.Fatalf("stream=%v tool ID = %q", streaming, id)
		}
	}
}

func TestGeminiRuntimeContinuesOnClaudeWithGeminiToolHistory(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:fetchAvailableModels" || r.URL.Path == "/v1internal:loadCodeAssist" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			if gjson.GetBytes(body, "model").String() != "gemini-3-flash" {
				t.Error("first request used the wrong model")
			}
			fmt.Fprint(w, `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"exec","args":{"input":"first"}},"thoughtSignature":"opaque+/="},{"functionCall":{"name":"exec","args":{"input":"second"}}}]},"finishReason":"STOP"}]}}`)
			return
		}
		if gjson.GetBytes(body, "model").String() != "claude-sonnet-4-6" {
			t.Error("continuation used the wrong model")
		}
		pending := map[string]bool{}
		results := 0
		for _, content := range gjson.GetBytes(body, "request.contents").Array() {
			for _, part := range content.Get("parts").Array() {
				if part.Get("thoughtSignature").Exists() && (!part.Get("thought").Bool() || part.Get("text").String() == "") {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"message":"messages.1.content.0.thinking.thinking: Field required"}}`)
					return
				}
				if call := part.Get("functionCall"); call.Exists() {
					id := call.Get("id").String()
					if id == "" || pending[id] {
						w.WriteHeader(http.StatusBadRequest)
						fmt.Fprint(w, `{"error":{"message":"messages.1.content.0.tool_use.id: Field required"}}`)
						return
					}
					pending[id] = true
				}
				if result := part.Get("functionResponse"); result.Exists() {
					id := result.Get("id").String()
					if !pending[id] {
						t.Error("tool result did not match its call ID")
					}
					delete(pending, id)
					results++
				}
			}
		}
		if len(pending) != 0 || results != 2 {
			t.Error("parallel calls/results were lost")
		}
		fmt.Fprint(w, `{"response":{"candidates":[{"content":{"parts":[{"text":"continued on sonnet"}]},"finishReason":"STOP"}]}}`)
	}))
	defer upstream.Close()
	previousDaily, previousProd := antigravityBaseURLDaily, antigravityBaseURLProd
	antigravityBaseURLDaily, antigravityBaseURLProd = upstream.URL, upstream.URL
	t.Cleanup(func() { antigravityBaseURLDaily, antigravityBaseURLProd = previousDaily, previousProd })
	home := t.TempDir()
	t.Setenv("HOME", home)
	credential := writeGeminiCredential(t, home, "fixture.json")
	runtime, err := StartOAuth(context.Background(), ProviderGemini, "gemini-3-flash,claude-sonnet-4-6", filepath.Base(credential))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	post := func(model string, messages []any) []byte {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 1024, "messages": messages, "tools": []any{map[string]any{"name": "exec", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}}}}})
		req, _ := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/messages", bytes.NewReader(body))
		req.Header.Set("X-Api-Key", runtime.APIKey())
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("model=%s status=%d response=%s", model, response.StatusCode, raw)
		}
		return raw
	}
	messages := []any{map[string]any{"role": "user", "content": "run two calls"}}
	first := post("gemini-3-flash", messages)
	var toolUses, fullBlocks, results []any
	for _, block := range gjson.GetBytes(first, "content").Array() {
		fullBlocks = append(fullBlocks, json.RawMessage(block.Raw))
		// Codex replays calls and outputs across model switches without the
		// previous provider's opaque thinking/signature blocks.
		if block.Get("type").String() == "tool_use" {
			toolUses = append(toolUses, json.RawMessage(block.Raw))
			results = append(results, map[string]any{"type": "tool_result", "tool_use_id": block.Get("id").String(), "content": "ok"})
		}
	}
	if len(toolUses) != 2 || len(fullBlocks) < 3 {
		t.Fatalf("missing parallel calls: %s", first)
	}
	codexHistory := append(append([]any{}, messages...), map[string]any{"role": "assistant", "content": toolUses}, map[string]any{"role": "user", "content": results}, map[string]any{"role": "user", "content": "hi"})
	if raw := post("claude-sonnet-4-6", codexHistory); !strings.Contains(string(raw), "continued on sonnet") || calls.Load() != 2 {
		t.Fatalf("model switch without thinking blocks failed: calls=%d response=%s", calls.Load(), raw)
	}
	// Claude Code preserves signed thinking blocks alongside tool_use blocks.
	claudeHistory := append(append([]any{}, messages...), map[string]any{"role": "assistant", "content": fullBlocks}, map[string]any{"role": "user", "content": results}, map[string]any{"role": "user", "content": "hi"})
	if raw := post("claude-sonnet-4-6", claudeHistory); !strings.Contains(string(raw), "continued on sonnet") || calls.Load() != 3 {
		t.Fatalf("model switch with signed thinking blocks failed: calls=%d response=%s", calls.Load(), raw)
	}
}

func TestGeminiSignedReplayGroupedCompanionsAndTurnBoundaries(t *testing.T) {
	part1 := `{"functionCall":{"name":"exec_one","args":{"n":1}},"thoughtSignature":"sig-1"}`
	part2 := `{"functionCall":{"id":"native-2","name":"exec_two","args":{"n":2}},"thoughtSignature":"sig-2"}`
	sig1 := geminiPartSignaturePrefix + base64.StdEncoding.EncodeToString([]byte(part1))
	sig2 := geminiPartSignaturePrefix + base64.StdEncoding.EncodeToString([]byte(part2))
	textPart := `{"text":"signed answer","thoughtSignature":"sig-text"}`
	textSig := geminiPartSignaturePrefix + base64.StdEncoding.EncodeToString([]byte(textPart))

	body, _ := json.Marshal(map[string]any{
		"model": "gemini-3-flash",
		"tools": []any{
			map[string]any{"name": "exec_one", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "exec_two", "input_schema": map[string]any{"type": "object"}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "run both"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "signature": sig1},
				map[string]any{"type": "thinking", "signature": sig2},
				map[string]any{"type": "tool_use", "id": "client-1", "name": "exec_one", "input": map[string]any{"n": 1}},
				map[string]any{"type": "tool_use", "id": "native-2", "name": "exec_two", "input": map[string]any{"n": 2}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "client-1", "content": "ok-1"},
				map[string]any{"type": "tool_result", "tool_use_id": "native-2", "content": "ok-2"},
			}},
			// Truncated signed text block without its companion in the previous turn must
			// not swallow the first text block of the next assistant turn after a user message.
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "signature": textSig},
			}},
			map[string]any{"role": "user", "content": "next question"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "fresh answer"},
			}},
			map[string]any{"role": "user", "content": "follow up"},
		},
	})
	converted, err := convertAnthropicToGemini(body)
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(converted.geminiBody, "contents.1.parts").Array()
	if len(parts) != 2 || parts[0].Raw != part1 || parts[1].Raw != part2 {
		t.Fatalf("grouped companions were not deduplicated: %s", gjson.GetBytes(converted.geminiBody, "contents.1.parts").Raw)
	}
	res0 := gjson.GetBytes(converted.geminiBody, "contents.2.parts.0.functionResponse")
	res1 := gjson.GetBytes(converted.geminiBody, "contents.2.parts.1.functionResponse")
	if res0.Get("id").Exists() || res1.Get("id").String() != "native-2" {
		t.Fatalf("unexpected functionResponse IDs: %s / %s", res0.Raw, res1.Raw)
	}
	if got := gjson.GetBytes(converted.geminiBody, "contents.5.parts.0.text").String(); got != "fresh answer" {
		t.Fatalf("next assistant turn text was swallowed: %q (contents=%s)", got, gjson.GetBytes(converted.geminiBody, "contents").Raw)
	}
}

func TestGeminiClaudeStreamedThinkingSignatureRoundTrip(t *testing.T) {
	for _, sigPart := range []string{
		`{"thought":true,"text":"","thoughtSignature":"sig-opus"}`,
		`{"thoughtSignature":"sig-opus"}`,
	} {
		sse := strings.Join([]string{
			`data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"step 1, "}]}}]}}`,
			`data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"step 2"}]}}]}}`,
			`data: {"response":{"candidates":[{"content":{"parts":[` + sigPart + `]}}]}}`,
			`data: {"response":{"candidates":[{"content":{"parts":[{"text":"final answer"}]},"finishReason":"STOP"}]}}`,
			"",
		}, "\n\n")
		assembler := newAnthropicResponseAssembler(&anthropicAdapterRequest{
			upstreamModel:     "claude-opus-4-6-thinking",
			clientModel:       "claude-opus-4-6-thinking",
			thinkingEnabled:   true,
			thinkingSignature: "gemini",
		}, nil)
		if err := processGeminiStream(strings.NewReader(sse), assembler); err != nil {
			t.Fatal(err)
		}
		blocks := assembler.contentBlocks()
		if len(blocks) != 2 || blocks[0].Type != "thinking" || blocks[0].Thinking != "step 1, step 2" || !strings.HasPrefix(blocks[0].Signature, geminiPartSignaturePrefix) {
			t.Fatalf("unexpected blocks: %+v", blocks)
		}
		rawBlocks, err := json.Marshal(blocks)
		if err != nil {
			t.Fatal(err)
		}
		replayReq, _ := json.Marshal(map[string]any{
			"model": "claude-opus-4-6-thinking",
			"messages": []any{
				map[string]any{"role": "user", "content": "think"},
				map[string]any{"role": "assistant", "content": json.RawMessage(rawBlocks)},
				map[string]any{"role": "user", "content": "follow up"},
			},
		})
		converted, err := convertAnthropicToAntigravity(replayReq)
		if err != nil {
			t.Fatal(err)
		}
		parts := gjson.GetBytes(converted.geminiBody, "contents.1.parts").Array()
		if len(parts) != 2 || !parts[0].Get("thought").Bool() || parts[0].Get("text").String() != "step 1, step 2" || parts[0].Get("thoughtSignature").String() != "sig-opus" || parts[1].Get("text").String() != "final answer" {
			t.Fatalf("unexpected replayed parts: %s", gjson.GetBytes(converted.geminiBody, "contents.1.parts").Raw)
		}
	}

	emptyThoughtSig := geminiPartSignaturePrefix + base64.StdEncoding.EncodeToString([]byte(`{"thought":true,"text":"","thoughtSignature":"sig-split"}`))
	emptyStandaloneSig := geminiPartSignaturePrefix + base64.StdEncoding.EncodeToString([]byte(`{"thought":true,"text":"","thoughtSignature":"sig-empty"}`))
	legacyReq, _ := json.Marshal(map[string]any{
		"model": "claude-opus-4-6-thinking",
		"messages": []any{
			map[string]any{"role": "user", "content": "think"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "recovered thought", "signature": "gemini"},
				map[string]any{"type": "thinking", "thinking": "", "signature": emptyThoughtSig},
				map[string]any{"type": "thinking", "thinking": "", "signature": emptyStandaloneSig},
				map[string]any{"type": "text", "text": "done"},
			}},
			map[string]any{"role": "user", "content": "next"},
		},
	})
	converted, err := convertAnthropicToAntigravity(legacyReq)
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(converted.geminiBody, "contents.1.parts").Array()
	if len(parts) != 2 || parts[0].Get("text").String() != "recovered thought" || parts[0].Get("thoughtSignature").String() != "sig-split" || parts[1].Get("text").String() != "done" {
		t.Fatalf("unexpected legacy recovery parts: %s", gjson.GetBytes(converted.geminiBody, "contents.1.parts").Raw)
	}
}

func TestGeminiClaudeConcurrentToolUseAndSplitTurnNormalization(t *testing.T) {
	thoughtSig := geminiPartSignaturePrefix + base64.StdEncoding.EncodeToString([]byte(`{"thought":true,"text":"plan two reads","thoughtSignature":"sig-claude"}`))
	req, _ := json.Marshal(map[string]any{
		"model":    "claude-opus-4-6-thinking",
		"thinking": map[string]any{"type": "adaptive"},
		"tools": []any{
			map[string]any{"name": "Read", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "Bash", "input_schema": map[string]any{"type": "object"}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "inspect project"},
			// Assistant blocks split across consecutive messages (thinking + text + 2 concurrent tool_use calls).
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "plan two reads", "signature": thoughtSig},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "Reading both files concurrently."},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_read_1", "name": "Read", "input": map[string]any{"file_path": "a.go"}},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_bash_2", "name": "Bash", "input": map[string]any{"command": "ls"}},
			}},
			// System reminder + image-bearing tool_result + missing second tool_result split across user turns.
			map[string]any{"role": "system", "content": "<system-reminder>hook note</system-reminder>"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_read_1",
					"content": []any{
						map[string]any{"type": "text", "text": "file a"},
						map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}},
					},
				},
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_orphan", "content": "stale"},
			}},
		},
	})
	converted, err := convertAnthropicToAntigravity(req)
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(converted.geminiBody, "generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatalf("missing includeThoughts in thinkingConfig: %s", gjson.GetBytes(converted.geminiBody, "generationConfig").Raw)
	}
	contents := gjson.GetBytes(converted.geminiBody, "contents").Array()
	if len(contents) != 3 || contents[0].Get("role").String() != "user" || contents[1].Get("role").String() != "model" || contents[2].Get("role").String() != "user" {
		t.Fatalf("expected strict user/model/user alternation, got: %s", gjson.GetBytes(converted.geminiBody, "contents").Raw)
	}
	modelParts := contents[1].Get("parts").Array()
	if len(modelParts) != 4 || !modelParts[0].Get("thought").Bool() || modelParts[1].Get("text").String() != "Reading both files concurrently." || modelParts[2].Get("functionCall.id").String() != "toolu_read_1" || modelParts[3].Get("functionCall.id").String() != "toolu_bash_2" {
		t.Fatalf("unexpected model parts ordering: %s", contents[1].Get("parts").Raw)
	}
	userParts := contents[2].Get("parts").Array()
	// Both functionResponse parts must come first (in call order, with missing toolu_bash_2 synthesized and toolu_orphan dropped), followed by reminder text and image inline_data.
	if len(userParts) != 4 || userParts[0].Get("functionResponse.id").String() != "toolu_read_1" || userParts[1].Get("functionResponse.id").String() != "toolu_bash_2" || userParts[2].Get("text").String() != "<system-reminder>hook note</system-reminder>" || userParts[3].Get("inline_data.data").String() != "AAAA" {
		t.Fatalf("unexpected user parts ordering/repair: %s", contents[2].Get("parts").Raw)
	}
}

func TestGeminiThinkingOnlyResponseAndHistoryRecovery(t *testing.T) {
	// 1. A stream that finishes with only thought parts promotes the thought text
	// into a visible text block so Claude Code does not render an empty turn.
	sse := strings.Join([]string{
		`data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"继续执行修复步骤"}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"","thoughtSignature":"sig-argon"}]},"finishReason":"STOP"}]}}`,
		"",
	}, "\n\n")
	assembler := newAnthropicResponseAssembler(&anthropicAdapterRequest{
		upstreamModel:     "gemini-4-argon-high",
		clientModel:       "gemini-4-argon",
		thinkingEnabled:   true,
		thinkingSignature: "gemini",
	}, nil)
	if err := processGeminiStream(strings.NewReader(sse), assembler); err != nil {
		t.Fatal(err)
	}
	blocks := assembler.contentBlocks()
	if len(blocks) != 2 || blocks[0].Type != "thinking" || blocks[1].Type != "text" || blocks[1].Text != "继续执行修复步骤" {
		t.Fatalf("expected thinking + promoted text block, got: %+v", blocks)
	}

	// 2. Historical thinking-only turns and interrupted tool calls followed by "继续"
	// are repaired on Gemini routes.
	thoughtOnlySig := geminiPartSignaturePrefix + base64.StdEncoding.EncodeToString([]byte(`{"thought":true,"text":"baked only","thoughtSignature":"sig-baked"}`))
	req, _ := json.Marshal(map[string]any{
		"model":      "gemini-4-argon",
		"max_tokens": 4096,
		"tools": []any{
			map[string]any{"name": "Bash", "input_schema": map[string]any{"type": "object"}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "start"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_interrupted", "name": "Bash", "input": map[string]any{"command": "ls"}},
			}},
			map[string]any{"role": "user", "content": "继续"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "baked only", "signature": thoughtOnlySig},
			}},
			map[string]any{"role": "user", "content": "继续"},
		},
	})
	catalog, err := parseGeminiCatalog([]byte(geminiCatalogFixture))
	if err != nil {
		t.Fatal(err)
	}
	converted, err := convertAnthropicToAntigravityWithCatalog(req, catalog)
	if err != nil {
		t.Fatal(err)
	}
	_, body, err := catalog.resolve(converted.clientModel, "", converted.geminiBody)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "generationConfig.maxOutputTokens").Int(); got != 32768 {
		t.Fatalf("maxOutputTokens = %d, want 32768", got)
	}
	contents := gjson.GetBytes(body, "contents").Array()
	if len(contents) != 3 {
		t.Fatalf("contents len = %d, want 3 (user, model, merged user): %s", len(contents), gjson.GetBytes(body, "contents").Raw)
	}
	if got := contents[2].Get("parts.0.functionResponse.id").String(); got != "toolu_interrupted" {
		t.Fatalf("missing synthesized functionResponse for interrupted tool call: %s", contents[2].Raw)
	}
}
