package oauthproxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

const (
	zedTestUserID  = "42"
	zedTestToken   = "account-token"
	zedTestOrgID   = "org_personal"
	zedTestCredent = "zed-test.json"
)

type fakeZedCall struct {
	path     string
	header   http.Header
	envelope []byte
}

func (c fakeZedCall) providerRequest() gjson.Result {
	return gjson.GetBytes(c.envelope, "provider_request")
}

// fakeZedCloud is a stand-in for cloud.zed.dev: account profile, LLM-token mint,
// model catalog, completions, and token counting.
type fakeZedCloud struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	issued   int
	rejected map[string]bool
	calls    []fakeZedCall
	models   []map[string]any
	// profile replaces the default GET /client/users/me body. Tests set it, and
	// completion, before starting a runtime; the first request orders the access.
	profile map[string]any
	// completion overrides the default per-provider stream when it returns true.
	completion func(call fakeZedCall, writer http.ResponseWriter) bool
}

func zedTestModel(provider, id string, extra map[string]any) map[string]any {
	model := map[string]any{
		"provider": provider, "id": id, "display_name": id, "is_latest": true,
		"max_token_count": 200000, "max_output_tokens": 64000,
		"supports_tools": true, "supports_images": true, "supports_thinking": true,
		"supported_effort_levels": []any{},
	}
	for key, value := range extra {
		model[key] = value
	}
	return model
}

func newFakeZedCloud(t *testing.T) *fakeZedCloud {
	t.Helper()
	cloud := &fakeZedCloud{t: t, rejected: make(map[string]bool)}
	cloud.models = []map[string]any{
		zedTestModel("anthropic", "claude-sonnet-test", nil),
		zedTestModel("open_ai", "gpt-test", map[string]any{"supports_fast_mode": true, "supports_parallel_tool_calls": true}),
		zedTestModel("x_ai", "grok-test", map[string]any{"supports_thinking": false}),
		zedTestModel("google", "gemini-test", nil),
		zedTestModel("anthropic", "claude-fable-5-1", nil),
		zedTestModel("anthropic", "claude-disabled", map[string]any{"is_disabled": true, "disabled_reason": "plan"}),
		zedTestModel("mistral", "mistral-future", nil),
	}
	cloud.server = httptest.NewServer(cloud)
	t.Cleanup(cloud.server.Close)

	previous := zedCloudBaseURL
	zedCloudBaseURL = cloud.server.URL
	t.Cleanup(func() { zedCloudBaseURL = previous })
	return cloud
}

func (c *fakeZedCloud) callsTo(path string) []fakeZedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	var matched []fakeZedCall
	for _, call := range c.calls {
		if call.path == path {
			matched = append(matched, call)
		}
	}
	return matched
}

func (c *fakeZedCloud) reject(token string) {
	c.mu.Lock()
	c.rejected[token] = true
	c.mu.Unlock()
}

func (c *fakeZedCloud) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	call := fakeZedCall{path: request.URL.Path, header: request.Header.Clone(), envelope: body}
	c.mu.Lock()
	c.calls = append(c.calls, call)
	c.mu.Unlock()

	switch request.URL.Path {
	case "/client/users/me":
		if request.Header.Get("Authorization") != zedTestUserID+" "+zedTestToken {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if c.profile != nil {
			writeTestJSON(writer, c.profile)
			return
		}
		writeTestJSON(writer, map[string]any{
			"user":                          map[string]any{"github_login": "octo", "name": "Octo"},
			"organizations":                 []map[string]any{{"id": zedTestOrgID, "name": "Octo", "is_personal": true}},
			"default_organization_id":       zedTestOrgID,
			"configuration_by_organization": map[string]any{zedTestOrgID: map[string]any{"is_zed_model_provider_enabled": true}},
			"plan":                          map[string]any{"plan_v3": "zed_pro"},
		})
	case "/client/llm_tokens":
		if request.Header.Get("Authorization") != zedTestUserID+" "+zedTestToken {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if gjson.GetBytes(body, "organization_id").String() != zedTestOrgID {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.issued++
		token := fmt.Sprintf("llm-%d", c.issued)
		c.mu.Unlock()
		writeTestJSON(writer, map[string]any{"token": token})
	default:
		token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		c.mu.Lock()
		rejected := c.rejected[token]
		c.mu.Unlock()
		if !strings.HasPrefix(token, "llm-") || rejected {
			writer.Header().Set(zedExpiredTokenHeader, "true")
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		c.serveLLM(writer, request, call)
	}
}

func (c *fakeZedCloud) serveLLM(writer http.ResponseWriter, request *http.Request, call fakeZedCall) {
	switch request.URL.Path {
	case "/models":
		if request.Header.Get(zedClientXAIHeader) != "true" {
			c.t.Errorf("/models did not advertise xAI support")
		}
		writeTestJSON(writer, map[string]any{"models": c.models, "default_model": "claude-sonnet-test", "recommended_models": []string{}})
	case "/count_tokens":
		writeTestJSON(writer, map[string]any{"tokens": 77})
	case "/completions":
		if c.completion != nil && c.completion(call, writer) {
			return
		}
		writer.Header().Set(zedServerStatusHeader, "true")
		writer.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(writer, defaultZedStream(gjson.GetBytes(call.envelope, "provider").String()))
	default:
		http.NotFound(writer, request)
	}
}

func ndjson(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

func defaultZedStream(provider string) string {
	switch provider {
	case "anthropic":
		return ndjson(
			`{"status":{"queued":{"position":1}}}`,
			`{"status":"started"}`,
			`{"event":{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":0}}}}`,
			`{"event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}}`,
			`{"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}}`,
			`{"event":{"type":"content_block_stop","index":0}}`,
			`{"event":{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}}`,
			`{"event":{"type":"message_stop"}}`,
			`{"status":"stream_ended"}`,
		)
	case "open_ai":
		return ndjson(
			`{"status":"started"}`,
			`{"event":{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}}`,
			`{"event":{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}}`,
			`{"event":{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}}`,
			`{"event":{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}}`,
			`{"event":{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":10,"output_tokens":2}}}}`,
			`{"status":"stream_ended"}`,
		)
	case "x_ai":
		return ndjson(
			`{"event":{"id":"c1","object":"chat.completion.chunk","model":"grok-test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}}`,
			`{"event":{"id":"c1","object":"chat.completion.chunk","model":"grok-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}}`,
			`{"status":"stream_ended"}`,
		)
	case "google":
		return ndjson(
			`{"event":{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1}}}`,
			`{"status":"stream_ended"}`,
		)
	}
	return ""
}

// startZedTestRuntime writes a credential and starts the real Zed runtime
// against the fake cloud.
func startZedTestRuntime(t *testing.T, cloud *fakeZedCloud, modelSpec string) *Runtime {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credential, _ := json.Marshal(map[string]any{
		"type": "zed", "user_id": zedTestUserID, "access_token": zedTestToken, "system_id": "sys-1", "login": "octo",
	})
	if err := os.WriteFile(filepath.Join(authDir, zedTestCredent), credential, 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := startZedOAuth(t.Context(), modelSpec, zedTestCredent)
	if err != nil {
		t.Fatalf("startZedOAuth() error: %v", err)
	}
	t.Cleanup(runtime.Stop)
	return runtime
}

func zedPost(t *testing.T, runtime *Runtime, path string, body any) (*http.Response, string) {
	t.Helper()
	var payload []byte
	switch typed := body.(type) {
	case []byte:
		payload = typed
	default:
		payload, _ = json.Marshal(typed)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, runtime.ClaudeBaseURL()+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("x-api-key", runtime.APIKey())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-version", "2023-06-01")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response, string(raw)
}

func zedClaudeCodeRequest(model string, stream bool) map[string]any {
	return map[string]any{
		"model": model, "max_tokens": 256, "stream": stream,
		"metadata":           map[string]any{"user_id": "device-123"},
		"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_tool_uses_20250919"}}},
		"system":             "You are helpful.",
		"messages":           []any{map[string]any{"role": "user", "content": "Say hello"}},
	}
}

func TestStartZedRuntimeVerifiesAccountAndPublishesOnlyServableModels(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")

	want := []string{"claude-sonnet-test", "gpt-test", "grok-test", "gemini-test"}
	if got := runtime.Models(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Models() = %v, want %v (data-retention, disabled and unknown-provider models hidden)", got, want)
	}
	if auths := runtime.ListAuths(); len(auths) != 1 || auths[0].Provider != ProviderZed || auths[0].Label != "octo" {
		t.Fatalf("ListAuths() = %+v", auths)
	}

	// Startup verifies the account, mints one LLM token, and fetches the catalog
	// with Zed's client identity.
	if len(cloud.callsTo("/client/users/me")) != 1 || len(cloud.callsTo("/client/llm_tokens")) != 1 {
		t.Fatalf("startup account calls: profile=%d tokens=%d", len(cloud.callsTo("/client/users/me")), len(cloud.callsTo("/client/llm_tokens")))
	}
	models := cloud.callsTo("/models")
	if len(models) != 1 || models[0].header.Get("Authorization") != "Bearer llm-1" ||
		models[0].header.Get(zedVersionHeader) != zedClientVersion || !strings.HasPrefix(models[0].header.Get("User-Agent"), "Zed/") {
		t.Fatalf("/models call = %+v", models)
	}

	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, runtime.Endpoint()+"/models", nil)
	request.Header.Set("x-api-key", runtime.APIKey())
	listed, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer listed.Body.Close()
	raw, _ := io.ReadAll(listed.Body)
	if ids := gjson.GetBytes(raw, "data.#.id").Array(); len(ids) != len(want) {
		t.Fatalf("/v1/models = %s", raw)
	}
}

func TestZedRuntimeRelaysAnthropicModelsInZedEnvelope(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")

	request := zedClaudeCodeRequest("claude-sonnet-test", true)
	request["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
	request["speed"] = "fast" // the model does not support fast mode
	response, body := zedPost(t, runtime, "/v1/messages", request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	for _, want := range []string{"event: message_start", `"text":"Hel"`, `"text":"lo"`, "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream is missing %q:\n%s", want, body)
		}
	}

	calls := cloud.callsTo("/completions")
	if len(calls) != 1 {
		t.Fatalf("completions calls = %d", len(calls))
	}
	call := calls[0]
	if call.header.Get("Authorization") != "Bearer llm-1" || call.header.Get(zedVersionHeader) != zedClientVersion ||
		call.header.Get(zedClientStatusHeader) != "true" || call.header.Get(zedClientStreamEndHeader) != "true" {
		t.Fatalf("completion headers = %v", call.header)
	}
	if gjson.GetBytes(call.envelope, "provider").String() != "anthropic" || gjson.GetBytes(call.envelope, "model").String() != "claude-sonnet-test" {
		t.Fatalf("envelope = %s", call.envelope)
	}
	providerRequest := call.providerRequest()
	for _, dropped := range []string{"stream", "metadata", "context_management", "speed"} {
		if providerRequest.Get(dropped).Exists() {
			t.Fatalf("provider_request kept %q: %s", dropped, providerRequest.Raw)
		}
	}
	if providerRequest.Get("messages.0.content").String() != "Say hello" || providerRequest.Get("thinking.budget_tokens").Int() != 1024 ||
		providerRequest.Get("max_tokens").Int() != 256 || providerRequest.Get("system").String() != "You are helpful." {
		t.Fatalf("provider_request = %s", providerRequest.Raw)
	}
}

func TestZedRuntimeFoldsStreamForNonStreamingClients(t *testing.T) {
	cloud := newFakeZedCloud(t)
	cloud.completion = func(call fakeZedCall, writer http.ResponseWriter) bool {
		writer.Header().Set(zedServerStatusHeader, "true")
		_, _ = io.WriteString(writer, ndjson(
			`{"status":"started"}`,
			`{"event":{"type":"message_start","message":{"id":"msg_9","type":"message","role":"assistant","model":"claude-sonnet-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":0}}}}`,
			`{"event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}}`,
			`{"event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}}`,
			`{"event":{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}}`,
			`{"event":{"type":"content_block_stop","index":0}}`,
			`{"event":{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}}`,
			`{"event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Reading"}}}`,
			`{"event":{"type":"content_block_stop","index":1}}`,
			`{"event":{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}}}`,
			`{"event":{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}}`,
			`{"event":{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"/tmp/a\"}"}}}`,
			`{"event":{"type":"content_block_stop","index":2}}`,
			`{"event":{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}}`,
			`{"event":{"type":"message_stop"}}`,
			`{"status":"stream_ended"}`,
		))
		return true
	}
	runtime := startZedTestRuntime(t, cloud, "")

	response, body := zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("claude-sonnet-test", false))
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("non-stream response = %d %s: %s", response.StatusCode, response.Header.Get("Content-Type"), body)
	}
	message := gjson.Parse(body)
	if message.Get("type").String() != "message" || message.Get("stop_reason").String() != "tool_use" ||
		message.Get("usage.input_tokens").Int() != 30 || message.Get("usage.output_tokens").Int() != 9 {
		t.Fatalf("folded message = %s", body)
	}
	content := message.Get("content").Array()
	if len(content) != 3 || content[0].Get("thinking").String() != "hmm" || content[0].Get("signature").String() != "sig" ||
		content[1].Get("text").String() != "Reading" || content[2].Get("input.path").String() != "/tmp/a" {
		t.Fatalf("folded content = %s", message.Get("content").Raw)
	}
	// Zed only streams, so the upstream request was a streaming one regardless.
	if len(cloud.callsTo("/completions")) != 1 {
		t.Fatalf("completions calls = %d", len(cloud.callsTo("/completions")))
	}
}

func TestZedRuntimeServesOpenAIModelsThroughResponses(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")

	response, body := zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("gpt-test", true))
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `"text":"Hello"`) || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("status=%d body:\n%s", response.StatusCode, body)
	}
	call := cloud.callsTo("/completions")[0]
	if gjson.GetBytes(call.envelope, "provider").String() != "open_ai" {
		t.Fatalf("envelope = %s", call.envelope)
	}
	providerRequest := call.providerRequest()
	if providerRequest.Get("model").String() != "gpt-test" || !providerRequest.Get("stream").Bool() || providerRequest.Get("store").Bool() {
		t.Fatalf("provider_request = %s", providerRequest.Raw)
	}
	if providerRequest.Get("client_metadata").Exists() {
		t.Fatalf("Codex-only client_metadata leaked to Zed: %s", providerRequest.Raw)
	}
	if providerRequest.Get("prompt_cache_key").String() == "" || !providerRequest.Get("input").IsArray() {
		t.Fatalf("provider_request lost Responses fields: %s", providerRequest.Raw)
	}
}

func TestZedRuntimeServesXAIModelsThroughChatCompletions(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")

	response, body := zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("grok-test", true))
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `"text":"Hello"`) || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("status=%d body:\n%s", response.StatusCode, body)
	}
	call := cloud.callsTo("/completions")[0]
	if gjson.GetBytes(call.envelope, "provider").String() != "x_ai" {
		t.Fatalf("envelope = %s", call.envelope)
	}
	providerRequest := call.providerRequest()
	if providerRequest.Get("model").String() != "grok-test" || !providerRequest.Get("stream").Bool() ||
		providerRequest.Get("max_completion_tokens").Int() != 256 || providerRequest.Get("max_tokens").Exists() {
		t.Fatalf("provider_request = %s", providerRequest.Raw)
	}

	// A non-streaming client is served from the same stream-only upstream.
	response, body = zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("grok-test", false))
	if response.StatusCode != http.StatusOK || gjson.Get(body, "content.0.text").String() != "Hello" {
		t.Fatalf("non-stream status=%d body: %s", response.StatusCode, body)
	}
}

func TestZedRuntimeServesGoogleModelsThroughGeminiConverter(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")

	request := zedClaudeCodeRequest("gemini-test", true)
	request["thinking"] = map[string]any{"type": "adaptive"}
	request["output_config"] = map[string]any{"effort": "xhigh"}
	response, body := zedPost(t, runtime, "/v1/messages", request)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `"text":"Hello"`) || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("status=%d body:\n%s", response.StatusCode, body)
	}
	call := cloud.callsTo("/completions")[0]
	if gjson.GetBytes(call.envelope, "provider").String() != "google" || gjson.GetBytes(call.envelope, "model").String() != "gemini-test" {
		t.Fatalf("envelope = %s", call.envelope)
	}
	providerRequest := call.providerRequest()
	if providerRequest.Get("model").String() != "models/gemini-test" ||
		providerRequest.Get("contents.0.parts.0.text").String() != "Say hello" ||
		providerRequest.Get("generationConfig.maxOutputTokens").Int() != 256 {
		t.Fatalf("provider_request = %s", providerRequest.Raw)
	}
	thinking := providerRequest.Get("generationConfig.thinkingConfig")
	if thinking.Get("thinkingLevel").String() != "HIGH" || !thinking.Get("includeThoughts").Bool() {
		t.Fatalf("thinkingConfig = %s, want public-API uppercase level with thoughts", thinking.Raw)
	}

	response, body = zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("gemini-test", false))
	if response.StatusCode != http.StatusOK || gjson.Get(body, "content.0.text").String() != "Hello" {
		t.Fatalf("non-stream status=%d body: %s", response.StatusCode, body)
	}
}

func TestZedRuntimeMintsFreshLLMTokenWhenServiceReportsExpiry(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")
	cloud.reject("llm-1") // the token cached during discovery expires

	response, body := zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("claude-sonnet-test", true))
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("status=%d body:\n%s", response.StatusCode, body)
	}
	calls := cloud.callsTo("/completions")
	if len(calls) != 2 || calls[0].header.Get("Authorization") != "Bearer llm-1" || calls[1].header.Get("Authorization") != "Bearer llm-2" {
		t.Fatalf("completions authorizations = %v", func() []string {
			var all []string
			for _, call := range calls {
				all = append(all, call.header.Get("Authorization"))
			}
			return all
		}())
	}
	if got := len(cloud.callsTo("/client/llm_tokens")); got != 2 {
		t.Fatalf("LLM tokens minted = %d, want 2 (discovery + one refresh)", got)
	}

	// The refreshed token is reused, not re-minted per request.
	zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("claude-sonnet-test", true))
	if got := len(cloud.callsTo("/client/llm_tokens")); got != 2 {
		t.Fatalf("LLM tokens minted after reuse = %d, want 2", got)
	}
}

func TestZedRuntimeSurfacesUpstreamStatusAndRetriesOnlyOnce(t *testing.T) {
	previous := upstreamFastRetryBackoff
	upstreamFastRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { upstreamFastRetryBackoff = previous })

	cloud := newFakeZedCloud(t)
	cloud.completion = func(call fakeZedCall, writer http.ResponseWriter) bool {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(writer, `{"code":"upstream_http_error","message":"Received an error from the Anthropic API: overloaded","upstream_status":529,"retry_after":12.2}`)
		return true
	}
	runtime := startZedTestRuntime(t, cloud, "")

	response, body := zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("claude-sonnet-test", true))
	if response.StatusCode != 529 {
		t.Fatalf("status = %d, want the upstream 529 Zed reported: %s", response.StatusCode, body)
	}
	if response.Header.Get("Retry-After") != "13" {
		t.Fatalf("Retry-After = %q, want 13", response.Header.Get("Retry-After"))
	}
	if gjson.Get(body, "error.type").String() != "overloaded_error" || !strings.Contains(gjson.Get(body, "error.message").String(), "overloaded") {
		t.Fatalf("error body = %s", body)
	}
	// One outer fast-retry budget: 3 attempts total. The gateway adds no retries
	// of its own, so nesting never multiplies them.
	if got := len(cloud.callsTo("/completions")); got != 3 {
		t.Fatalf("completions attempts = %d, want 3", got)
	}
}

func TestZedRuntimeExplainsPaymentRequiredWithoutRetrying(t *testing.T) {
	cloud := newFakeZedCloud(t)
	cloud.completion = func(call fakeZedCall, writer http.ResponseWriter) bool {
		writer.WriteHeader(http.StatusPaymentRequired)
		return true
	}
	runtime := startZedTestRuntime(t, cloud, "")

	response, body := zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("gpt-test", true))
	if response.StatusCode != http.StatusPaymentRequired || !strings.Contains(body, "upgrade your Zed plan") {
		t.Fatalf("status=%d body: %s", response.StatusCode, body)
	}
	if got := len(cloud.callsTo("/completions")); got != 1 {
		t.Fatalf("completions attempts = %d, want 1 (402 is not retryable)", got)
	}
}

func TestZedRuntimeReportsFailureInsideStream(t *testing.T) {
	cloud := newFakeZedCloud(t)
	cloud.completion = func(call fakeZedCall, writer http.ResponseWriter) bool {
		_, _ = io.WriteString(writer, ndjson(
			`{"status":"started"}`,
			`{"event":{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-test","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}}`,
			`{"status":{"failed":{"code":"upstream_http_429","message":"slow down","request_id":"3f2d6b0e-5a0e-4c0e-9c0e-6a0a0a0a0a0a","retry_after":2.5}}}`,
		))
		return true
	}
	runtime := startZedTestRuntime(t, cloud, "")

	_, body := zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("claude-sonnet-test", true))
	if !strings.Contains(body, "event: error") || !strings.Contains(body, `"rate_limit_error"`) || !strings.Contains(body, "slow down") {
		t.Fatalf("stream failure not reported as an Anthropic error event:\n%s", body)
	}
}

func TestZedRuntimeFlagsAnthropicStreamThatEndsEarly(t *testing.T) {
	cloud := newFakeZedCloud(t)
	cloud.completion = func(call fakeZedCall, writer http.ResponseWriter) bool {
		_, _ = io.WriteString(writer, ndjson(
			`{"event":{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-test","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}}`,
			`{"event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		))
		return true
	}
	runtime := startZedTestRuntime(t, cloud, "")

	_, body := zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("claude-sonnet-test", true))
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "ended before the response completed") {
		t.Fatalf("truncated stream was not reported:\n%s", body)
	}
}

func TestZedRuntimeCountsAnthropicTokensThroughZed(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")

	count := map[string]any{
		"model":    "claude-sonnet-test",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	response, body := zedPost(t, runtime, "/v1/messages/count_tokens", count)
	if response.StatusCode != http.StatusOK || gjson.Get(body, "input_tokens").Int() != 77 {
		t.Fatalf("status=%d body: %s", response.StatusCode, body)
	}
	call := cloud.callsTo("/count_tokens")[0]
	if gjson.GetBytes(call.envelope, "provider").String() != "anthropic" || call.providerRequest().Get("max_tokens").Int() != 1 {
		t.Fatalf("count envelope = %s", call.envelope)
	}
	if call.header.Get(zedClientStatusHeader) != "" {
		t.Fatalf("count request asked for status messages: %v", call.header)
	}
}

func TestZedRuntimeKeepsConfiguredContextAliasesAndRejectsUnknownModels(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "claude-sonnet-test[1m]")

	response, body := zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("claude-sonnet-test[1m]", true))
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("aliased model status=%d body:\n%s", response.StatusCode, body)
	}
	if got := gjson.GetBytes(cloud.callsTo("/completions")[0].envelope, "model").String(); got != "claude-sonnet-test" {
		t.Fatalf("upstream model = %q, want the alias stripped", got)
	}
	response, _ = zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("not-a-zed-model", true))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown model status = %d, want 400", response.StatusCode)
	}

	t.Setenv("HOME", t.TempDir())
	if _, err := startZedOAuthForSpec(t, "claude-sonnet-test,missing-model"); err == nil || !strings.Contains(err.Error(), "missing-model") {
		t.Fatalf("a configured model Zed does not offer must fail startup, got %v", err)
	}
}

// startZedOAuthForSpec starts the runtime under the current HOME and returns
// its startup error, for tests that expect startup to fail.
func startZedOAuthForSpec(t *testing.T, modelSpec string) (*Runtime, error) {
	t.Helper()
	home := os.Getenv("HOME")
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credential, _ := json.Marshal(map[string]any{"type": "zed", "user_id": zedTestUserID, "access_token": zedTestToken})
	if err := os.WriteFile(filepath.Join(authDir, zedTestCredent), credential, 0o600); err != nil {
		t.Fatal(err)
	}
	return startZedOAuth(t.Context(), modelSpec, zedTestCredent)
}

func TestStartZedRuntimeRejectsRevokedCredential(t *testing.T) {
	cloud := newFakeZedCloud(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credential, _ := json.Marshal(map[string]any{"type": "zed", "user_id": zedTestUserID, "access_token": "revoked", "organization_id": zedTestOrgID})
	if err := os.WriteFile(filepath.Join(authDir, zedTestCredent), credential, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := startZedOAuth(t.Context(), "", zedTestCredent)
	if err == nil || !strings.Contains(err.Error(), "ccl oauth zed") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a revoked credential must fail startup even with a stored organization, and say how to fix it, got %v", err)
	}
	if len(cloud.callsTo("/client/llm_tokens")) != 0 {
		t.Fatal("an LLM token was minted for a credential Zed rejected")
	}
}

func TestLoadZedCredentialRequiresBoundZedCredential(t *testing.T) {
	authDir := t.TempDir()
	if _, err := loadZedCredential(authDir, ""); err == nil {
		t.Fatal("an unbound credential must be an error")
	}
	if _, err := loadZedCredential(authDir, "missing.json"); err == nil || !strings.Contains(err.Error(), "ccl oauth zed") {
		t.Fatalf("missing credential error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "kimi.json"), []byte(`{"type":"kimi","access_token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadZedCredential(authDir, "kimi.json"); err == nil || !strings.Contains(err.Error(), "not Zed") {
		t.Fatalf("wrong-type credential error = %v", err)
	}
	// A numeric user id (hand-edited or older tooling) is accepted.
	if err := os.WriteFile(filepath.Join(authDir, "n.json"), []byte(`{"type":"zed","user_id":12,"access_token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credential, err := loadZedCredential(authDir, "n.json")
	if err != nil || credential.auth.userID != "12" {
		t.Fatalf("numeric user id: %+v, %v", credential, err)
	}
}

func TestFilterZedModelsHidesDisabledUnknownAndRetentionModels(t *testing.T) {
	catalog := []zedModel{
		{Provider: "anthropic", ID: "claude-a"},
		{Provider: "anthropic", ID: "CLAUDE-A"}, // duplicate by case
		{Provider: "anthropic", ID: "claude-fable-5-2"},
		{Provider: "open_ai", ID: "gpt-off", IsDisabled: true},
		{Provider: "future_ai", ID: "mystery"},
		{Provider: "google", ID: " gemini-a "},
		{Provider: "x_ai", ID: ""},
	}
	ids := func(models []zedModel) string {
		var out []string
		for _, model := range models {
			out = append(out, model.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(filterZedModels(catalog, false)); got != "claude-a,gemini-a" {
		t.Fatalf("default filter = %q", got)
	}
	if got := ids(filterZedModels(catalog, true)); got != "claude-a,claude-fable-5-2,gemini-a" {
		t.Fatalf("with data-retention consent = %q", got)
	}
}

func TestBuildZedRoutesBucketsByWireAndKeepsAliases(t *testing.T) {
	catalog := filterZedModels([]zedModel{
		{Provider: "anthropic", ID: "claude-a"}, {Provider: "open_ai", ID: "gpt-a"},
		{Provider: "x_ai", ID: "grok-a"}, {Provider: "google", ID: "gemini-a"},
	}, false)
	routes, err := buildZedRoutes("claude-a[1m],gpt-a", catalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes.anthropic) != 2 || routes.anthropic[0].Alias != "claude-a" || routes.anthropic[1].Alias != "claude-a[1m]" || routes.anthropic[1].Name != "claude-a" {
		t.Fatalf("anthropic routes = %+v", routes.anthropic)
	}
	if len(routes.responses) != 1 || len(routes.chat) != 1 || len(routes.google) != 1 {
		t.Fatalf("routes = %+v", routes)
	}
	if strings.Join(routes.models, ",") != "claude-a,gpt-a,grok-a,gemini-a" {
		t.Fatalf("published models = %v (configured aliases must not narrow the catalog)", routes.models)
	}
	if _, err := buildZedRoutes("nope,nope[1m]", catalog); err == nil || strings.Count(err.Error(), "nope") != 1 {
		t.Fatalf("missing model error = %v, want the model named once", err)
	}
}

func TestZedProfileSelectsOrganizationLikeZed(t *testing.T) {
	parse := func(raw string) *zedProfile {
		var profile zedProfile
		if err := json.Unmarshal([]byte(raw), &profile); err != nil {
			t.Fatal(err)
		}
		return &profile
	}
	cases := map[string]struct {
		raw  string
		want string
	}{
		"default wins":       {`{"default_organization_id":"org_d","organizations":[{"id":"org_p","is_personal":true}]}`, "org_d"},
		"personal next":      {`{"organizations":[{"id":"org_t"},{"id":"org_p","is_personal":true}]}`, "org_p"},
		"first as last case": {`{"organizations":[{"id":"org_t"},{"id":"org_u"}]}`, "org_t"},
		"none":               {`{"organizations":[]}`, ""},
	}
	for name, tc := range cases {
		if got := parse(tc.raw).organizationID(); got != tc.want {
			t.Fatalf("%s: organizationID = %q, want %q", name, got, tc.want)
		}
	}
	profile := parse(`{"configuration_by_organization":{"org_off":{"is_zed_model_provider_enabled":false}},"plan":{"plan_v3":"zed_student"}}`)
	if profile.modelProviderEnabled("org_off") || !profile.modelProviderEnabled("org_unknown") || profile.planName() != "zed_student" {
		t.Fatalf("profile = %+v", profile)
	}
}

func TestStartZedRuntimeRefusesOrganizationWithZedModelsDisabled(t *testing.T) {
	cloud := newFakeZedCloud(t)
	cloud.profile = map[string]any{
		"user":                          map[string]any{"github_login": "octo"},
		"default_organization_id":       zedTestOrgID,
		"configuration_by_organization": map[string]any{zedTestOrgID: map[string]any{"is_zed_model_provider_enabled": false}},
	}
	t.Setenv("HOME", t.TempDir())
	if _, err := startZedOAuthForSpec(t, ""); err == nil || !strings.Contains(err.Error(), "disabled by your organization") {
		t.Fatalf("startup error = %v", err)
	}
}

func TestZedWireRequestsAreShapedLikeZedsOwnClient(t *testing.T) {
	t.Run("anthropic keeps only Zed's fields and thinking/fast gates", func(t *testing.T) {
		body := []byte(`{"model":"m[1m]","stream":true,"metadata":{"user_id":"u"},"context_management":{"edits":[]},"service_tier":"auto",
			"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":2000},"speed":"fast","temperature":0.5,"tools":[{"name":"t","input_schema":{"type":"object"}}]}`)
		model := zedModel{ID: "m", MaxOutputTokens: 12345, SupportsThinking: false, SupportsFastMode: false}
		out, err := zedAnthropicProviderRequest(body, model, false)
		if err != nil {
			t.Fatal(err)
		}
		parsed := gjson.ParseBytes(out)
		for _, dropped := range []string{"stream", "metadata", "context_management", "service_tier", "thinking", "speed"} {
			if parsed.Get(dropped).Exists() {
				t.Fatalf("kept %q: %s", dropped, out)
			}
		}
		if parsed.Get("model").String() != "m" || parsed.Get("max_tokens").Int() != 12345 ||
			parsed.Get("temperature").Float() != 0.5 || parsed.Get("tools.0.name").String() != "t" {
			t.Fatalf("provider request = %s", out)
		}
		withFast, _ := zedAnthropicProviderRequest(body, zedModel{ID: "m", SupportsThinking: true, SupportsFastMode: true}, false)
		if !gjson.GetBytes(withFast, "thinking").Exists() || gjson.GetBytes(withFast, "speed").String() != "fast" {
			t.Fatalf("supported fields were dropped: %s", withFast)
		}
		counted, _ := zedAnthropicProviderRequest([]byte(`{"model":"m","messages":[]}`), model, true)
		if gjson.GetBytes(counted, "max_tokens").Int() != 1 {
			t.Fatalf("count request max_tokens = %s", counted)
		}
	})
	t.Run("responses drops fields the public API rejects", func(t *testing.T) {
		body := []byte(`{"model":"x","client_metadata":{"a":1},"stream":false,"service_tier":"priority","reasoning":{"effort":"high"},"include":["reasoning.encrypted_content"],"parallel_tool_calls":true}`)
		out, err := zedResponsesProviderRequest(body, zedModel{ID: "gpt"})
		if err != nil {
			t.Fatal(err)
		}
		parsed := gjson.ParseBytes(out)
		for _, dropped := range []string{"client_metadata", "service_tier", "reasoning", "include", "parallel_tool_calls"} {
			if parsed.Get(dropped).Exists() {
				t.Fatalf("kept %q: %s", dropped, out)
			}
		}
		if parsed.Get("model").String() != "gpt" || !parsed.Get("stream").Bool() {
			t.Fatalf("provider request = %s", out)
		}
	})
	t.Run("google thinking level is the public uppercase enum", func(t *testing.T) {
		for effort, want := range map[string]string{"minimal": "MINIMAL", "low": "LOW", "medium": "MEDIUM", "high": "HIGH", "xhigh": "HIGH", "max": "HIGH", "": "HIGH"} {
			if got := zedGeminiThinkingLevel(effort); got != want {
				t.Fatalf("level(%q) = %q, want %q", effort, got, want)
			}
		}
		out, err := zedGoogleProviderRequest([]byte(`{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingBudget":512}}}`), zedModel{ID: "g", SupportsThinking: false})
		if err != nil || gjson.GetBytes(out, "generationConfig.thinkingConfig").Exists() {
			t.Fatalf("thinking kept for a non-thinking model: %s, %v", out, err)
		}
	})
}

func TestParseZedStreamLine(t *testing.T) {
	if line := parseZedStreamLine([]byte(`{"event":{"type":"ping"}}`)); string(line.event) != `{"type":"ping"}` {
		t.Fatalf("wrapped event = %+v", line)
	}
	if line := parseZedStreamLine([]byte(`{"type":"ping"}`)); string(line.event) != `{"type":"ping"}` {
		t.Fatalf("bare event (server without status messages) = %+v", line)
	}
	for _, bare := range []string{`{"id":"c","choices":[],"status":"in-chunk"}`, `{"type":"x","event":{"nested":true}}`} {
		if line := parseZedStreamLine([]byte(bare)); line.event == nil || string(line.event) != bare {
			t.Fatalf("a bare event containing a wrapper-like key was mangled: %s -> %+v", bare, line)
		}
	}
	for _, status := range []string{`{"status":"started"}`, `{"status":"stream_ended"}`, `{"status":{"queued":{"position":3}}}`} {
		if line := parseZedStreamLine([]byte(status)); line.event != nil || line.failure != nil || line.status == "" {
			t.Fatalf("%s parsed as %+v", status, line)
		}
	}
	line := parseZedStreamLine([]byte(`{"status":{"failed":{"code":"upstream_http_503","message":"down","request_id":"r"}}}`))
	if line.failure == nil || line.failure.status != 503 || line.failure.message != "down" {
		t.Fatalf("failed status = %+v", line)
	}
	if line := parseZedStreamLine([]byte(`not json`)); line.event != nil || line.failure != nil {
		t.Fatalf("garbage line = %+v", line)
	}
}

func TestZedParseCloudFailureFollowsZedClientRules(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		header     http.Header
		body       string
		wantStatus int
		wantRetry  string
		wantInMsg  string
	}{
		{"upstream_status wins", 500, nil, `{"code":"upstream_http_error","message":"m","upstream_status":503}`, 503, "", "m"},
		{"status from code suffix", 500, nil, `{"code":"upstream_http_429","message":"limit","retry_after":30.5}`, 429, "31", "limit"},
		{"plain error keeps http status", 500, nil, `{"code":"internal","message":"boom"}`, 500, "", "boom"},
		{"non-json body", 502, nil, `Bad Gateway text`, 502, "", "Bad Gateway text"},
		{"retry-after header", 429, canonicalHeader("Retry-After", "9"), `{"code":"x","message":"y"}`, 429, "9", "y"},
		{"minimum version", 426, canonicalHeader(zedMinimumVersionHeader, "9.9.9"), `{"message":"old"}`, 426, "", "9.9.9"},
		{"empty 402", 402, nil, ``, 402, "", "upgrade your Zed plan"},
	}
	for _, tc := range cases {
		failure := zedParseCloudFailure(tc.status, tc.header, []byte(tc.body))
		if failure.status != tc.wantStatus || failure.retryAfter != tc.wantRetry || !strings.Contains(failure.message, tc.wantInMsg) {
			t.Fatalf("%s: %+v, want status=%d retry=%q msg~%q", tc.name, failure, tc.wantStatus, tc.wantRetry, tc.wantInMsg)
		}
	}
}

// canonicalHeader builds a header the way net/http hands one to a client.
func canonicalHeader(name, value string) http.Header {
	header := http.Header{}
	header.Set(name, value)
	return header
}

func TestZedTokenExpiryReadsJWTExpClaim(t *testing.T) {
	claims := base64URL(`{"exp":4102444800}`)
	if got := zedTokenExpiry("h." + claims + ".s"); got.Unix() != 4102444800 {
		t.Fatalf("exp = %v", got)
	}
	for _, token := range []string{"opaque", "a.b", "a." + base64URL(`{}`) + ".c", "a.!!!.c"} {
		if got := zedTokenExpiry(token); !got.IsZero() {
			t.Fatalf("zedTokenExpiry(%q) = %v, want zero", token, got)
		}
	}
}

func base64URL(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func TestFoldAnthropicStreamReportsStreamErrorsAndTruncation(t *testing.T) {
	if _, err := foldAnthropicStream([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n")); err == nil ||
		err.status() != 529 || err.message != "busy" {
		t.Fatalf("error event = %+v", err)
	}
	if _, err := foldAnthropicStream([]byte("data: [DONE]\n")); err == nil || err.status() != http.StatusBadGateway {
		t.Fatalf("empty stream = %+v", err)
	}
}
