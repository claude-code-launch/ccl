package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/klauspost/compress/zstd"
)

func fixtureGateway(t *testing.T, handler http.HandlerFunc, protocol string, options Options) *Gateway {
	t.Helper()
	return fixtureProviderGateway(t, handler, provider.Provider{Name: "chosen", Type: protocol, Model: "model-a"}, options)
}

func fixtureProviderGateway(t *testing.T, handler http.HandlerFunc, configured provider.Provider, options Options) *Gateway {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	var runtime *oauthproxy.Runtime
	var err error
	if configured.Type == "openai_responses" {
		runtime, err = oauthproxy.StartOpenAIResponsesAPI(context.Background(), upstream.URL+"/v1", "fake-provider-key", "model-a")
	} else {
		runtime, err = oauthproxy.StartOpenAIChatAPI(context.Background(), upstream.URL+"/v1", "fake-provider-key", "model-a")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)
	g, err := New(context.Background(), runtime, configured, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g
}

// Fixture metadata changes use the same immutable-publication discipline as
// production refreshes; mutating a map returned to HTTP readers would race.
func editFixtureModels(g *Gateway, edit func([]Model) []Model) {
	g.catalogMu.Lock()
	defer g.catalogMu.Unlock()
	next := *g.currentLocked()
	next.models = edit(cloneCatalogModels(next.models))
	g.models = next.models
	g.modelSnapshot.Store(&next)
}

func postResponses(t *testing.T, g *Gateway, payload map[string]any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(payload)
	request, _ := http.NewRequest(http.MethodPost, g.BaseURL()+"/responses", bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer fake-native-key")
	request.Header.Set("Chatgpt-Account-Id", "fake-native-account")
	request.Header.Set("Cookie", "native-cookie=must-stay-native")
	if !g.options.Desktop {
		request.Header.Set("Authorization", "Bearer "+g.Token())
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}
func responseObject(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", response.StatusCode, raw)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("invalid Responses JSON %s: %v", raw, err)
	}
	return result
}
func writeChatText(w http.ResponseWriter, r *http.Request, text string) {
	writeChatFixture(w, r, map[string]any{"content": text}, "stop", 3)
}

func writeChatTool(w http.ResponseWriter, r *http.Request, id, name string, args map[string]any) {
	writeChatFixture(w, r, map[string]any{"tool_calls": []any{chatFixtureTool(id, name, args)}}, "tool_calls", 3)
}

func chatFixtureTool(id, name string, args map[string]any) map[string]any {
	return map[string]any{"index": 0, "id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": textValue(args)}}
}

func writeChatFixture(w http.ResponseWriter, r *http.Request, message map[string]any, reason string, inputTokens int) {
	message["role"] = "assistant"
	choice := map[string]any{"index": 0, "finish_reason": reason}
	usage := map[string]any{"prompt_tokens": inputTokens, "completion_tokens": 2, "total_tokens": inputTokens + 2}
	response := map[string]any{"id": "chat_fixture", "choices": []any{choice}, "usage": usage}
	if r.Header.Get("Accept") == "text/event-stream" {
		choice["delta"] = message
		w.Header().Set("Content-Type", "text/event-stream")
		raw, _ := json.Marshal(response)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
		return
	}
	choice["message"] = message
	writeJSON(w, 200, response)
}

func writeChatDelta(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", text)
}

func finishChatFixture(w http.ResponseWriter) {
	_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
}

func TestCodexReplacementListenerKeepsEndpoint(t *testing.T) {
	g := fixtureGateway(t, func(http.ResponseWriter, *http.Request) {}, "openai", Options{Desktop: true})
	base := g.BaseURL()
	u, _ := url.Parse(base)
	options := Options{Desktop: true, ListenAddress: u.Host}
	if _, err := New(context.Background(), nil, provider.Provider{Name: "chosen"}, options); err == nil {
		t.Fatal("occupied endpoint silently moved to another port")
	}
	g.Close()
	replacement, err := New(context.Background(), g.runtime, provider.Provider{Name: "chosen", Type: "openai"}, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replacement.Close)
	if replacement.BaseURL() != base {
		t.Fatalf("replacement endpoint changed: %s -> %s", base, replacement.BaseURL())
	}
	for _, address := range []string{"0.0.0.0:1234", "localhost:1234", "[::1]:1234", "127.0.0.1:0", "127.0.0.1:http", "127.0.0.1:65536"} {
		if _, err := New(context.Background(), nil, provider.Provider{Name: "chosen"}, Options{ListenAddress: address}); err == nil {
			t.Fatalf("unsafe or unstable listener accepted: %s", address)
		}
	}
}

func TestCodexCloseReleasesListenerBeforeServeStarts(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	g := &Gateway{listener: listener, server: &http.Server{}, client: &http.Client{}}
	// Model the scheduling window between New returning and Serve registering
	// its listener. http.Server.Close cannot close an unregistered listener.
	g.Close()
	replacement, err := net.Listen("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Close retained the replacement endpoint: %v", err)
	}
	_ = replacement.Close()
}

func TestCodexBridgeRelaysRateLimitBeforeResponseStarts(t *testing.T) {
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		writeJSON(w, 429, map[string]any{"type": "error", "error": map[string]any{"type": "rate_limit_error", "message": "model temporarily resource limited (retry after 2s)"}})
	}, "openai", Options{})
	response := postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "input": "fixture", "stream": true})
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 429 || response.Header.Get("Retry-After") != "2" || !strings.Contains(string(raw), "rate_limit_error") || strings.Contains(string(raw), "response.created") {
		t.Fatalf("retryable HTTP rejection became a disconnected Responses stream: %d %s", response.StatusCode, raw)
	}
}

func TestCodexGatewayPublishesGPTEffortForUnadvertisedModels(t *testing.T) {
	g := fixtureGateway(t, func(http.ResponseWriter, *http.Request) {}, "openai", Options{})
	models := g.Models()
	if len(models) != 1 {
		t.Fatalf("models: %+v", models)
	}
	levels, _ := models[0]["supported_reasoning_levels"].([]protocol.ReasoningLevel)
	if len(levels) != 6 || levels[0].Effort != "low" || levels[5].Effort != "ultra" || models[0]["default_reasoning_level"] != "low" || levels[1].Description == "" {
		t.Fatalf("picker ladder: %+v", models[0])
	}
}

func TestCodexBridgeCompactionTriggerReturnsReplayableSummary(t *testing.T) {
	var phase atomic.Int32
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body["messages"])
		switch phase.Add(1) {
		case 1:
			if !strings.Contains(string(raw), "keep-this-goal") {
				t.Errorf("summary request dropped the conversation: %s", raw)
			}
			writeChatText(w, r, "kept-goal")
		default:
			if !strings.Contains(string(raw), "Conversation summary:") || !strings.Contains(string(raw), "kept-goal") || strings.Contains(string(raw), "keep-this-goal") {
				t.Errorf("compaction was not replayed: %s", raw)
			}
			writeChatText(w, r, "continued")
		}
	}, "openai", Options{})
	first := responseObject(t, postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "input": []any{
		map[string]any{"type": "message", "role": "user", "content": "keep-this-goal"},
		map[string]any{"type": "compaction_trigger"},
	}}))
	output, _ := first["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %+v", first["output"])
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "compaction" || item["encrypted_content"] == "" {
		t.Fatalf("compaction item = %+v", item)
	}
	second := responseObject(t, postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "input": []any{
		item,
		map[string]any{"type": "message", "role": "user", "content": "continue"},
	}}))
	if second["status"] != "completed" || phase.Load() != 2 {
		t.Fatalf("replay status=%v phase=%d", second["status"], phase.Load())
	}
}

func TestCodexCompactionSummarizesToolHistoryWithoutLiveCalls(t *testing.T) {
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		for _, want := range []string{"read_file", "file-contents", "patch-contents", "patch-result", "tool_search", "shipping_eta", "Return only the summary"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("summary lost %q: %s", want, raw)
			}
		}
		if body["tools"] != nil || strings.Contains(string(raw), `"tool_calls"`) || strings.Contains(string(raw), `"role":"tool"`) {
			t.Errorf("summary requires live tool definitions: %s", raw)
		}
		messages, _ := body["messages"].([]any)
		last, _ := messages[len(messages)-1].(map[string]any)
		if last["role"] != "user" || !strings.Contains(textValue(last["content"]), "Return only the summary") {
			t.Error("summary request is not the final user instruction")
		}
		writeChatText(w, r, "summarized tools")
	}, "openai", Options{})
	result := responseObject(t, postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "input": []any{
		map[string]any{"role": "user", "content": "inspect and patch"},
		map[string]any{"type": "tool_search_call", "call_id": "call_search", "execution": "client", "arguments": map[string]any{"query": "shipping_eta"}},
		map[string]any{"type": "tool_search_output", "call_id": "call_search", "execution": "client", "tools": []any{largeToolCatalog()[1]}},
		map[string]any{"type": "function_call", "call_id": "call_read", "name": "read_file", "arguments": `{"path":"file.txt"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_read", "output": "file-contents"},
		map[string]any{"type": "custom_tool_call", "call_id": "call_patch", "name": "apply_patch", "input": "patch-contents"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_patch", "output": "patch-result"},
		map[string]any{"type": "compaction_trigger"},
	}}))
	if output := result["output"].([]any); len(output) != 1 || output[0].(map[string]any)["type"] != "compaction" {
		t.Fatalf("compaction response: %+v", result)
	}
}

func TestCodexBridgeToolsContinueWithoutNativeCredentialLeak(t *testing.T) {
	var attempts atomic.Int32
	var seenToolResult atomic.Bool
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-provider-key" || r.Header.Get("Cookie") != "" || r.Header.Get("Chatgpt-Account-Id") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("native credentials crossed the CCL route")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "model-a" {
			t.Error("client alias reached provider")
		}
		if attempts.Add(1) == 1 {
			writeChatTool(w, r, "call_1", "read_file", map[string]any{"path": "local.txt"})
			return
		}
		serialized, _ := json.Marshal(body["messages"])
		if strings.Contains(string(serialized), "tool-result-fixture") && strings.Contains(string(serialized), "call_1") {
			seenToolResult.Store(true)
		}
		writeChatText(w, r, "continued")
	}, "openai", Options{Desktop: true})
	tools := []any{map[string]any{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}}
	first := responseObject(t, postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "input": "read the fixture", "tools": tools}))
	output := first["output"].([]any)
	call := output[0].(map[string]any)
	if call["type"] != "function_call" || call["name"] != "read_file" || call["call_id"] != "call_1" {
		t.Fatalf("invalid tool response: %v", call)
	}
	second := responseObject(t, postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "previous_response_id": first["id"], "input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "tool-result-fixture"}}, "tools": tools}))
	if !seenToolResult.Load() || second["status"] != "completed" {
		t.Fatal("tool continuation lost history")
	}
}

func TestCodexBridgeFlushesBeforeProviderCompletes(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatDelta(w, "first-delta")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		finishChatFixture(w)
	}, "openai", Options{Desktop: true})
	response := postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "input": "fixture", "stream": true, "tools": largeToolCatalog()})
	delta := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "first-delta") {
				delta <- scanner.Text()
				return
			}
		}
		delta <- ""
	}()
	select {
	case value := <-delta:
		if value == "" {
			t.Fatal("missing incremental delta")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Responses stream was buffered until provider completion")
	}
}

func TestCodexNativeRoutesAndUnknownCCLAlias(t *testing.T) {
	var nativeRequests, customRequests atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeRequests.Add(1)
		if r.Header.Get("Authorization") != "Bearer fake-native-key" {
			t.Error("native auth lost")
		}
		writeJSON(w, 200, map[string]any{"route": "native"})
	}))
	defer native.Close()
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		customRequests.Add(1)
		writeChatText(w, r, "custom")
	}, "openai", Options{Desktop: true, OpenAIURL: native.URL, ChatGPTURL: native.URL})
	responseObject(t, postResponses(t, g, map[string]any{"model": "native-model", "input": "fixture"}))
	response := postResponses(t, g, map[string]any{"model": "ccl/missing/model", "input": "fixture"})
	if response.StatusCode != http.StatusNotFound || nativeRequests.Load() != 1 || customRequests.Load() != 0 {
		t.Fatal("routing allow-list was not enforced")
	}
	request, _ := http.NewRequest(http.MethodGet, g.BaseURL()+"/responses", nil)
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "Upgrade")
	ws, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	ws.Body.Close()
	if ws.StatusCode != 426 {
		t.Fatal("missing HTTP fallback")
	}
}

func TestCodexCompressedRequestsAndInterruptedStreams(t *testing.T) {
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatDelta(w, "partial")
	}, "openai", Options{Desktop: true})
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	compressed := encoder.EncodeAll([]byte(`{"model":"ccl/chosen/model-a","input":"fixture","stream":true}`), nil)
	request, _ := http.NewRequest(http.MethodPost, g.BaseURL()+"/responses", bytes.NewReader(compressed))
	request.Header.Set("Content-Encoding", "zstd")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(raw), "response.failed") || strings.Contains(string(raw), "response.completed") {
		t.Fatalf("interrupted stream was reported as complete: %s", raw)
	}
}

func TestCodexNativeResponsesKeepStreamingAndCompaction(t *testing.T) {
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-provider-key" || r.Header.Get("Chatgpt-Account-Id") != "" {
			t.Error("wrong native Responses authorization")
		}
		if strings.HasSuffix(r.URL.Path, "/compact") {
			writeJSON(w, 200, map[string]any{"object": "response.compaction", "output": []any{}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}, "openai_responses", Options{Desktop: true})
	result := responseObject(t, postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "input": "fixture"}))
	if result["id"] != "resp_fixture" {
		t.Fatal("native response altered")
	}
	raw := strings.NewReader(`{"model":"ccl/chosen/model-a","input":"fixture"}`)
	request, _ := http.NewRequest(http.MethodPost, g.BaseURL()+"/responses/compact", raw)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if result := responseObject(t, response); result["object"] != "response.compaction" {
		t.Fatal("native compaction not routed")
	}
}

func fixtureResponseUsage(input int) map[string]any {
	return map[string]any{"input_tokens": input, "output_tokens": 2, "total_tokens": input + 2,
		"input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}
}

func writeNativeFixture(w http.ResponseWriter, output []any, inputTokens int) {
	events := bridgeEvents{writer: w}
	response := map[string]any{"id": randomID("resp_"), "object": "response", "created_at": time.Now().Unix(),
		"model": "native-fixture", "status": "in_progress", "output": []any{}}
	_ = events.emit("response.created", map[string]any{"response": response})
	for i, item := range output {
		_ = events.emit("response.output_item.added", map[string]any{"output_index": i, "item": item})
		_ = events.emit("response.output_item.done", map[string]any{"output_index": i, "item": item})
	}
	response["status"], response["output"], response["usage"] = "completed", output, fixtureResponseUsage(inputTokens)
	_ = events.emit("response.completed", map[string]any{"response": response})
}
