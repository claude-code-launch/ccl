package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/klauspost/compress/zstd"
)

func fixtureGateway(t *testing.T, handler http.HandlerFunc, protocol string, options Options) *Gateway {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	var runtime *oauthproxy.Runtime
	var err error
	if protocol == "openai_responses" {
		runtime, err = oauthproxy.StartOpenAIResponsesAPI(context.Background(), upstream.URL+"/v1", "fake-provider-key", "model-a")
	} else {
		runtime, err = oauthproxy.StartAnthropicAPI(context.Background(), upstream.URL+"/v1", "fake-provider-key", "model-a", "")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)
	g, err := New(context.Background(), runtime, provider.Provider{Name: "chosen", Type: protocol, Model: "model-a"}, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g
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
func writeMessage(w http.ResponseWriter, content []map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "content": content, "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 3, "output_tokens": 2}})
}

func TestCodexBridgeToolsContinueWithoutNativeCredentialLeak(t *testing.T) {
	var attempts atomic.Int32
	var seenToolResult atomic.Bool
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Chatgpt-Account-Id") != "" || r.Header.Get("X-Api-Key") != "fake-provider-key" {
			t.Error("native credentials crossed the CCL route")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "model-a" {
			t.Error("client alias reached provider")
		}
		if attempts.Add(1) == 1 {
			writeMessage(w, []map[string]any{{"type": "tool_use", "id": "call_1", "name": toolWireName("", "read_file"), "input": map[string]any{"path": "local.txt"}}})
			return
		}
		serialized, _ := json.Marshal(body["messages"])
		if strings.Contains(string(serialized), "tool-result-fixture") && strings.Contains(string(serialized), "call_1") {
			seenToolResult.Store(true)
		}
		writeMessage(w, []map[string]any{{"type": "text", "text": "continued"}})
	}, "anthropic", Options{Desktop: true})
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
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"first-delta\"}}\n\n")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}, "anthropic", Options{Desktop: true})
	response := postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "input": "fixture", "stream": true})
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
		writeMessage(w, []map[string]any{{"type": "text", "text": "custom"}})
	}, "anthropic", Options{Desktop: true, OpenAIURL: native.URL, ChatGPTURL: native.URL})
	responseObject(t, postResponses(t, g, map[string]any{"model": "native-model", "input": "fixture"}))
	response := postResponses(t, g, map[string]any{"model": "ccl/missing/model", "input": "fixture"})
	if response.StatusCode != 400 || nativeRequests.Load() != 1 || customRequests.Load() != 0 {
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
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{}}\n\n")
	}, "anthropic", Options{Desktop: true})
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
