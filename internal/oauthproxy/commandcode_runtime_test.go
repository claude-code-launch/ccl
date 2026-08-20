package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConvertAnthropicToCommandCodeMapsBlocksAndEnvelope(t *testing.T) {
	payload := `{
		"model": "claude-sonnet-4-6[1m]",
		"max_tokens": 999999,
		"stream": false,
		"temperature": 0.4,
		"system": [{"type": "text", "text": "be brief"}],
		"tools": [{"name": "lookup", "description": "look things up", "input_schema": {"type": "object", "properties": {"q": {"type": "string"}}}}],
		"tool_choice": {"type": "tool", "name": "lookup"},
		"messages": [
			{"role": "user", "content": [
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGk="}},
				{"type": "text", "text": "find it"}
			]},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "let me think"},
				{"type": "tool_use", "id": "tu_1", "name": "lookup", "input": {"q": "x"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tu_1", "content": [{"type": "text", "text": "found"}]}
			]}
		]
	}`
	converted, err := convertAnthropicToCommandCode([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if converted.upstreamModel != "claude-sonnet-4-6" || converted.clientModel != "claude-sonnet-4-6[1m]" {
		t.Fatalf("model mapping upstream=%q client=%q", converted.upstreamModel, converted.clientModel)
	}
	if converted.stream {
		t.Fatal("client stream flag should carry through unforced")
	}
	if converted.inputTokens <= 0 {
		t.Fatal("inputTokens should be estimated from the request body")
	}

	params := converted.body["params"].(map[string]any)
	if params["model"] != "claude-sonnet-4-6" {
		t.Fatalf("upstream model = %v", params["model"])
	}
	if params["max_tokens"] != commandcodeMaxTokensCap {
		t.Fatalf("max_tokens = %v, want cap %d", params["max_tokens"], commandcodeMaxTokensCap)
	}
	// CommandCode always streams; non-streaming clients are served by
	// accumulating the NDJSON locally.
	if params["stream"] != true {
		t.Fatalf("params.stream = %v, want true", params["stream"])
	}
	if params["system"] != "be brief" {
		t.Fatalf("system = %v", params["system"])
	}
	if params["temperature"] != 0.4 {
		t.Fatalf("temperature = %v", params["temperature"])
	}
	tools := params["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", params["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "lookup" || tool["type"] != "function" {
		t.Fatalf("tool = %v", tool)
	}
	if choice := params["tool_choice"].(map[string]any); choice["type"] != "tool" || choice["name"] != "lookup" {
		t.Fatalf("tool_choice = %v", params["tool_choice"])
	}

	messages := params["messages"].([]map[string]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %v", params["messages"])
	}
	userParts := messages[0]["content"].([]map[string]any)
	if userParts[0]["type"] != "image" || userParts[0]["image"] != "data:image/png;base64,aGk=" {
		t.Fatalf("image part = %v", userParts[0])
	}
	if userParts[1]["type"] != "text" || userParts[1]["text"] != "find it" {
		t.Fatalf("text part = %v", userParts[1])
	}
	assistantParts := messages[1]["content"].([]map[string]any)
	if len(assistantParts) != 1 {
		t.Fatalf("thinking must be dropped, got %v", assistantParts)
	}
	call := assistantParts[0]
	if call["type"] != "tool-call" || call["toolCallId"] != "tu_1" || call["toolName"] != "lookup" {
		t.Fatalf("tool-call part = %v", call)
	}
	input, ok := call["input"].(map[string]any)
	if !ok || input["q"] != "x" {
		t.Fatalf("tool-call input = %v", call["input"])
	}
	resultParts := messages[2]["content"].([]map[string]any)
	result := resultParts[0]
	if result["type"] != "tool-result" || result["toolCallId"] != "tu_1" || result["toolName"] != "lookup" {
		t.Fatalf("tool-result part = %v", result)
	}
	output, ok := result["output"].(map[string]any)
	if !ok || output["value"] != "found" {
		t.Fatalf("tool-result output = %v", result["output"])
	}

	config := converted.body["config"].(map[string]any)
	if config["workingDir"] != "/" {
		t.Fatalf("config workingDir = %v", config["workingDir"])
	}
	if environment, ok := config["environment"].(string); !ok || !strings.Contains(environment, "Node.js") {
		t.Fatalf("config environment = %v", config["environment"])
	}
	if converted.body["permissionMode"] != "standard" {
		t.Fatalf("permissionMode = %v", converted.body["permissionMode"])
	}
}

func TestConvertAnthropicToCommandCodeRejectsMissingModel(t *testing.T) {
	if _, err := convertAnthropicToCommandCode([]byte(`{"messages":[]}`)); err == nil {
		t.Fatal("expected an error for a request without a model")
	}
}

func TestCommandCodeMappedStatus(t *testing.T) {
	tests := []struct {
		upstream int
		want     int
	}{
		{http.StatusBadRequest, http.StatusBadRequest},
		{http.StatusUnauthorized, http.StatusUnauthorized},
		{http.StatusPaymentRequired, http.StatusTooManyRequests},
		{http.StatusForbidden, http.StatusUnauthorized},
		{http.StatusNotFound, http.StatusNotFound},
		{http.StatusTooManyRequests, http.StatusTooManyRequests},
		{http.StatusUnprocessableEntity, http.StatusBadRequest},
		{http.StatusInternalServerError, http.StatusBadGateway},
		{http.StatusBadGateway, http.StatusBadGateway},
		{http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{http.StatusTeapot, http.StatusBadGateway},
	}
	for _, test := range tests {
		if got := commandcodeMappedStatus(test.upstream); got != test.want {
			t.Errorf("commandcodeMappedStatus(%d) = %d, want %d", test.upstream, got, test.want)
		}
	}
}

func TestCommandCodeErrorMessageFormats(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{"error message", `{"error":{"message":"boom"}}`, http.StatusBadRequest, "boom"},
		{"plain message", `{"message":"plain boom"}`, http.StatusBadRequest, "plain boom"},
		{"empty body", "", http.StatusInternalServerError, "CC API error (500)"},
		{"json without message", `{"foo":1}`, http.StatusBadRequest, "CC API error (400)"},
		{"long non-json clipped", strings.Repeat("x", 300), http.StatusInternalServerError, strings.Repeat("x", 200)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := commandcodeErrorMessage(test.body, test.status); got != test.want {
				t.Fatalf("commandcodeErrorMessage = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCommandCodeProjectSlug(t *testing.T) {
	tests := []struct {
		sessionID string
		want      string
	}{
		// 0xa3f2 % 16 == 2 -> "backend" out of the slug name pool.
		{"a3f2419c", "users-dev-projects-backend-a3f2"},
		// Unparseable hex keeps the default name.
		{"zzzz1234", "users-dev-projects-app-zzzz"},
		{"", "users-dev-projects-app"},
	}
	for _, test := range tests {
		if got := commandcodeProjectSlug(test.sessionID); got != test.want {
			t.Errorf("commandcodeProjectSlug(%q) = %q, want %q", test.sessionID, got, test.want)
		}
	}
}

func TestCommandCodeCatalogAndSupportsModel(t *testing.T) {
	catalog := CommandCodeModelCatalog()
	if len(catalog) != len(commandcodeModelCatalog) {
		t.Fatalf("catalog has %d models, want %d", len(catalog), len(commandcodeModelCatalog))
	}
	for _, info := range catalog {
		if info.DisplayName == "" {
			t.Errorf("catalog entry %q has no display name", info.ID)
		}
		if !CommandCodeSupportsModel(info.ID) {
			t.Errorf("CommandCodeSupportsModel(%q) = false", info.ID)
		}
		if !CommandCodeSupportsModel(strings.ToUpper(info.ID)) {
			t.Errorf("CommandCodeSupportsModel should be case-insensitive for %q", info.ID)
		}
	}
	for _, unsupported := range []string{"not-a-model", " ", "claude-opus-99"} {
		if CommandCodeSupportsModel(unsupported) {
			t.Errorf("CommandCodeSupportsModel(%q) = true, want false", unsupported)
		}
	}
}

func TestProcessCommandCodeStreamEmitsAnthropicSSE(t *testing.T) {
	request := &anthropicAdapterRequest{clientModel: "claude-sonnet-4-6", toolNameMap: map[string]string{}}
	assembler := newAnthropicResponseAssembler(request, nil)
	ndjson := strings.Join([]string{
		`: comment line`,
		`{"type":"start"}`,
		`{"type":"text-delta","text":"Hello"}`,
		`{"type":"reasoning-delta","text":"thinking out loud"}`,
		`{"type":"tool-call","toolCallId":"cc_1","toolName":"search","input":{"q":"z"}}`,
		`{"type":"finish-step","finishReason":"tool-calls","usage":{"inputTokens":10,"outputTokens":2,"cachedInputTokens":4}}`,
		`{"type":"finish","finishReason":"length","totalUsage":{"inputTokens":11,"outputTokens":3,"cachedInputTokens":5}}`,
		`[DONE]`,
	}, "\n") + "\n"
	if err := processCommandCodeStream(strings.NewReader(ndjson), assembler); err != nil {
		t.Fatal(err)
	}
	response := assembler.response()
	blocks := response["content"].([]kiroResponseBlock)
	var thinking, text, toolName string
	var toolInput map[string]any
	for _, block := range blocks {
		switch block.Type {
		case "thinking":
			thinking = block.Thinking
		case "text":
			text = block.Text
		case "tool_use":
			toolName = block.Name
			if block.Input != nil {
				toolInput = *block.Input
			}
		}
	}
	if thinking != "thinking out loud" || text != "Hello" {
		t.Fatalf("thinking=%q text=%q", thinking, text)
	}
	if toolName != "search" || toolInput["q"] != "z" {
		t.Fatalf("tool name=%q input=%v", toolName, toolInput)
	}
	// Explicit length mapping wins over the hasToolUse-derived tool_use reason.
	if response["stop_reason"] != "max_tokens" {
		t.Fatalf("stop_reason=%v", response["stop_reason"])
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != 11 || usage["output_tokens"] != 3 || usage["cache_read_input_tokens"] != 5 {
		t.Fatalf("usage=%v", usage)
	}
}

func TestProcessCommandCodeStreamStringToolInputPassthrough(t *testing.T) {
	request := &anthropicAdapterRequest{clientModel: "claude-sonnet-4-6", toolNameMap: map[string]string{}}
	assembler := newAnthropicResponseAssembler(request, nil)
	ndjson := strings.Join([]string{
		`{"type":"tool-call","toolCallId":"cc_2","toolName":"apply_patch","input":"{\"path\":\"a.go\"}"}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
	}, "\n") + "\n"
	if err := processCommandCodeStream(strings.NewReader(ndjson), assembler); err != nil {
		t.Fatal(err)
	}
	blocks := assembler.response()["content"].([]kiroResponseBlock)
	if len(blocks) != 1 || blocks[0].Input == nil || (*blocks[0].Input)["path"] != "a.go" {
		t.Fatalf("blocks = %v", blocks)
	}
}

func TestProcessCommandCodeNonStreamAccumulatesJSON(t *testing.T) {
	request := &anthropicAdapterRequest{clientModel: "claude-sonnet-4-6", toolNameMap: map[string]string{}}
	assembler := newAnthropicResponseAssembler(request, nil)
	body := "{\"type\":\"text-delta\",\"text\":\"non-stream answer\"}\n" +
		"{\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":7,\"outputTokens\":2}}\n"
	if err := processCommandCodeNonStream([]byte(body), assembler); err != nil {
		t.Fatal(err)
	}
	response := assembler.response()
	if response["model"] != "claude-sonnet-4-6" || response["stop_reason"] != "end_turn" {
		t.Fatalf("response = %v", response)
	}
	blocks := response["content"].([]kiroResponseBlock)
	if len(blocks) != 1 || blocks[0].Text != "non-stream answer" {
		t.Fatalf("blocks = %v", blocks)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != 7 || usage["output_tokens"] != 2 {
		t.Fatalf("usage=%v", usage)
	}
}

// commandcodeTestUpstream is an httptest handler implementing the Command Code
// data plane surface: identity endpoints 200, generate serves NDJSON.
func commandcodeTestUpstream(t *testing.T, fingerprintCalls, lifecycleCalls, generateCalls *atomic.Int32, onGenerate func(writer http.ResponseWriter)) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/alpha/fingerprint/record":
			fingerprintCalls.Add(1)
			writer.WriteHeader(http.StatusOK)
		case "/alpha/lifecycle-events":
			lifecycleCalls.Add(1)
			writer.WriteHeader(http.StatusOK)
		case "/alpha/generate":
			generateCalls.Add(1)
			onGenerate(writer)
		default:
			http.NotFound(writer, request)
		}
	})
}

func TestCommandCodeEndToEndStreamingWithInitHandshake(t *testing.T) {
	var fingerprintCalls, lifecycleCalls, generateCalls atomic.Int32
	upstream := httptest.NewServer(commandcodeTestUpstream(t, &fingerprintCalls, &lifecycleCalls, &generateCalls,
		func(writer http.ResponseWriter) {
			writer.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(writer, "{\"type\":\"text-delta\",\"text\":\"Hello from Command Code\"}\n")
			_, _ = io.WriteString(writer, "{\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":12,\"outputTokens\":5,\"cachedInputTokens\":3}}\n")
		}))
	defer upstream.Close()
	runtime, err := StartCommandCodeAPI(context.Background(), upstream.URL, "cmd-key", "")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	if got := len(runtime.Models()); got != len(commandcodeModelCatalog) {
		t.Fatalf("runtime models = %d, want %d", got, len(commandcodeModelCatalog))
	}

	body := postClaudeMessage(t, context.Background(), runtime, "claude-sonnet-4-6")
	for _, want := range []string{"message_start", "Hello from Command Code", "message_stop", `"input_tokens":12`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q: %s", want, body)
		}
	}
	// The handshake runs before the first generate and is not repeated for this
	// session's subsequent request.
	_ = postClaudeMessage(t, context.Background(), runtime, "claude-sonnet-4-6")
	if generateCalls.Load() != 2 {
		t.Fatalf("generate calls = %d, want 2", generateCalls.Load())
	}
	if fingerprintCalls.Load() != 1 || lifecycleCalls.Load() != 1 {
		t.Fatalf("init counts fingerprint=%d lifecycle=%d, want 1 each", fingerprintCalls.Load(), lifecycleCalls.Load())
	}
}

func TestCommandCodeGenerateCarriesIdentityHeaders(t *testing.T) {
	var fingerprintCalls, generateCalls atomic.Int32
	var gotAuth, gotVersion, gotSession, gotSlug, gotTraceparent string
	verified := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/alpha/fingerprint/record":
			fingerprintCalls.Add(1)
			writer.WriteHeader(http.StatusOK)
		case "/alpha/lifecycle-events":
			writer.WriteHeader(http.StatusOK)
		case "/alpha/generate":
			generateCalls.Add(1)
			gotAuth = request.Header.Get("Authorization")
			gotVersion = request.Header.Get("x-command-code-version")
			gotSession = request.Header.Get("x-session-id")
			gotSlug = request.Header.Get("x-project-slug")
			gotTraceparent = request.Header.Get("traceparent")
			verified <- struct{}{}
			_, _ = io.WriteString(writer, "{\"type\":\"text-delta\",\"text\":\"ok\"}\n{\"type\":\"finish\",\"finishReason\":\"stop\"}\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()
	runtime, err := StartCommandCodeAPI(context.Background(), upstream.URL, "cmd-key", "")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	_ = postClaudeMessage(t, context.Background(), runtime, "claude-sonnet-4-6")
	<-verified
	if gotAuth != "Bearer cmd-key" {
		t.Errorf("authorization = %q", gotAuth)
	}
	for header, value := range map[string]string{
		"x-command-code-version": gotVersion,
		"x-session-id":           gotSession,
		"x-project-slug":         gotSlug,
		"traceparent":            gotTraceparent,
	} {
		if strings.TrimSpace(value) == "" {
			t.Errorf("%s header missing", header)
		}
	}
	if fingerprintCalls.Load() != 1 || generateCalls.Load() != 1 {
		t.Errorf("fingerprint calls=%d generate calls=%d, want 1 each", fingerprintCalls.Load(), generateCalls.Load())
	}
}

func TestCommandCodeEndToEndNonStreaming(t *testing.T) {
	var fingerprintCalls, lifecycleCalls, generateCalls atomic.Int32
	upstream := httptest.NewServer(commandcodeTestUpstream(t, &fingerprintCalls, &lifecycleCalls, &generateCalls,
		func(writer http.ResponseWriter) {
			_, _ = io.WriteString(writer, "{\"type\":\"text-delta\",\"text\":\"non-stream answer\"}\n")
			_, _ = io.WriteString(writer, "{\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":7,\"outputTokens\":2}}\n")
		}))
	defer upstream.Close()
	runtime, err := StartCommandCodeAPI(context.Background(), upstream.URL, "cmd-key", "")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()

	payload := strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	request, err := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/messages", payload)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	for _, want := range []string{`"type":"message"`, "non-stream answer", `"input_tokens":7`, `"output_tokens":2`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("response missing %q: %s", want, body)
		}
	}
}

func TestCommandCodeErrorMappingAndRetryAfter(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		want      int
		wantType  string
		retryFlag bool
	}{
		{name: "payment required maps to rate limit", status: http.StatusPaymentRequired, want: http.StatusTooManyRequests, wantType: "rate_limit_error"},
		{name: "forbidden maps to unauthorized", status: http.StatusForbidden, want: http.StatusUnauthorized, wantType: "authentication_error"},
		{name: "rate limit carries retry after", status: http.StatusTooManyRequests, want: http.StatusTooManyRequests, wantType: "rate_limit_error", retryFlag: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var fingerprintCalls, lifecycleCalls, generateCalls atomic.Int32
			upstream := httptest.NewServer(commandcodeTestUpstream(t, &fingerprintCalls, &lifecycleCalls, &generateCalls,
				func(writer http.ResponseWriter) {
					writer.WriteHeader(test.status)
					_, _ = io.WriteString(writer, `{"error":{"message":"boom"}}`)
				}))
			defer upstream.Close()
			runtime, err := StartCommandCodeAPI(context.Background(), upstream.URL, "cmd-key", "")
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Stop()

			payload := strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			request, _ := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/messages", payload)
			request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != test.want {
				t.Fatalf("status = %d, want %d; body=%s", response.StatusCode, test.want, body)
			}
			if !strings.Contains(string(body), "boom") || !strings.Contains(string(body), test.wantType) {
				t.Fatalf("body missing message or type %q: %s", test.wantType, body)
			}
			if test.retryFlag {
				if got := response.Header.Get("Retry-After"); got != commandcodeRetryAfter {
					t.Fatalf("Retry-After = %q, want %q", got, commandcodeRetryAfter)
				}
			} else if got := response.Header.Get("Retry-After"); got != "" {
				t.Fatalf("unexpected Retry-After %q", got)
			}
		})
	}
}

func TestCommandCodeInitFailureIsAdvisory(t *testing.T) {
	var fingerprintCalls, generateCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/alpha/fingerprint/record":
			fingerprintCalls.Add(1)
			http.Error(writer, "identity down", http.StatusInternalServerError)
		case "/alpha/lifecycle-events":
			http.Error(writer, "identity down", http.StatusInternalServerError)
		case "/alpha/generate":
			generateCalls.Add(1)
			_, _ = io.WriteString(writer, "{\"type\":\"text-delta\",\"text\":\"answer despite init failure\"}\n{\"type\":\"finish\",\"finishReason\":\"stop\"}\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()
	runtime, err := StartCommandCodeAPI(context.Background(), upstream.URL, "cmd-key", "")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	body := postClaudeMessage(t, context.Background(), runtime, "claude-sonnet-4-6")
	if !strings.Contains(body, "answer despite init failure") {
		t.Fatalf("generate should proceed after a failed handshake: %s", body)
	}
	if fingerprintCalls.Load() != 1 || generateCalls.Load() != 1 {
		t.Fatalf("fingerprint calls=%d generate calls=%d", fingerprintCalls.Load(), generateCalls.Load())
	}
}

func TestCommandCodeModelsEndpointAuthAndCatalog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.NotFound(writer, request)
	}))
	defer upstream.Close()
	runtime, err := StartCommandCodeAPI(context.Background(), upstream.URL, "cmd-key", "")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()

	unauthorized, err := http.Get(runtime.Endpoint() + "/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /models status = %d, want 401", unauthorized.StatusCode)
	}

	request, _ := http.NewRequest(http.MethodGet, runtime.Endpoint()+"/models", nil)
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), commandcodeModelCatalog[0].id) {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode models list: %v", err)
	}
	if len(list.Data) != len(commandcodeModelCatalog) {
		t.Fatalf("models list has %d entries, want %d", len(list.Data), len(commandcodeModelCatalog))
	}
}

func TestCommandCodeProbeInitReportsStatusAndPreview(t *testing.T) {
	mux := http.NewServeMux()
	var whoamiAbort atomic.Bool
	var whoamiCalls, fingerprintCalls atomic.Int32
	mux.HandleFunc("/alpha/whoami", func(writer http.ResponseWriter, request *http.Request) {
		whoamiCalls.Add(1)
		if whoamiAbort.Load() {
			// Drop the connection so the probe takes the fingerprint fallback.
			panic(http.ErrAbortHandler)
		}
		if request.Header.Get("Authorization") != "Bearer probe-key" {
			http.Error(writer, `{"error":{"message":"invalid token"}}`, http.StatusUnauthorized)
			return
		}
		if request.Header.Get("x-command-code-version") == "" {
			t.Errorf("probe missing x-command-code-version")
		}
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/alpha/fingerprint/record", func(writer http.ResponseWriter, request *http.Request) {
		fingerprintCalls.Add(1)
		if request.Header.Get("Authorization") != "Bearer probe-key" {
			http.Error(writer, `{"error":{"message":"invalid token"}}`, http.StatusUnauthorized)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	// A conclusive whoami answer short-circuits: 2xx with the right key, and a
	// 401 surfaces the invalid-key body without touching the fallback.
	status, preview, err := CommandCodeProbeInit(context.Background(), server.URL, "probe-key", 5*time.Second)
	if err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("probe status = %d, want 200", status)
	}
	status, preview, err = CommandCodeProbeInit(context.Background(), server.URL, "wrong-key", 5*time.Second)
	if err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if status != http.StatusUnauthorized || !strings.Contains(preview, "invalid token") {
		t.Fatalf("probe status=%d preview=%q", status, preview)
	}
	if fingerprintCalls.Load() != 0 {
		t.Fatalf("fingerprint fallback ran %d times on conclusive whoami answers", fingerprintCalls.Load())
	}

	// A transport failure on /alpha/whoami falls back to the fingerprint
	// handshake, which keeps protocol detection working against filtered routes.
	whoamiAbort.Store(true)
	status, _, err = CommandCodeProbeInit(context.Background(), server.URL, "probe-key", 5*time.Second)
	if err != nil {
		t.Fatalf("fallback probe error: %v", err)
	}
	if status != http.StatusOK || fingerprintCalls.Load() != 1 {
		t.Fatalf("fallback probe status=%d fingerprint calls=%d, want 200 and 1", status, fingerprintCalls.Load())
	}

	_, _, err = CommandCodeProbeInit(context.Background(), "http://127.0.0.1:1", "probe-key", 2*time.Second)
	if err == nil {
		t.Fatal("probe against a dead endpoint should fail")
	}
}
