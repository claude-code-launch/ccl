package oauthproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
)

func TestLoginQoderUsesDirectBrowserOAuth(t *testing.T) {
	var loginQuery, pollQuery string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/deviceToken/poll":
			pollQuery = request.URL.RawQuery
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"token": "qoder-access", "refresh_token": "qoder-refresh", "user_id": "user-42", "expires_in": 3600,
			})
		case "/api/v1/userinfo":
			if request.Header.Get("Authorization") != "Bearer qoder-access" {
				t.Fatalf("userinfo authorization = %q", request.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"email": "qoder@example.com", "name": "Qoder User"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	originalLogin, originalOpenAPI := qoderLoginBaseURL, qoderOpenAPIBaseURL
	originalInterval, originalOpener := qoderPollInterval, qoderBrowserOpener
	qoderLoginBaseURL = server.URL + "/device/selectAccounts"
	qoderOpenAPIBaseURL = server.URL
	qoderPollInterval = time.Millisecond
	qoderBrowserOpener = func(target string) error {
		parsed := strings.SplitN(target, "?", 2)
		if len(parsed) == 2 {
			loginQuery = parsed[1]
		}
		return nil
	}
	t.Cleanup(func() {
		qoderLoginBaseURL, qoderOpenAPIBaseURL = originalLogin, originalOpenAPI
		qoderPollInterval, qoderBrowserOpener = originalInterval, originalOpener
	})

	authDir := t.TempDir()
	result, err := loginQoder(context.Background(), authDir, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != ProviderQoder || result.Backend != ProviderQoder {
		t.Fatalf("result = %+v", result)
	}
	if filepath.Base(result.Path) != "qoder-qoder@example.com.json" {
		t.Fatalf("credential path = %q", result.Path)
	}
	loginValues, _ := url.ParseQuery(loginQuery)
	pollValues, _ := url.ParseQuery(pollQuery)
	if loginValues.Get("nonce") == "" || loginValues.Get("nonce") != pollValues.Get("nonce") {
		t.Fatalf("nonce mismatch: login=%q poll=%q", loginValues.Get("nonce"), pollValues.Get("nonce"))
	}
	verifier := pollValues.Get("verifier")
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); loginValues.Get("challenge") != want {
		t.Fatalf("PKCE challenge = %q, want %q", loginValues.Get("challenge"), want)
	}
	if loginValues.Get("machine_id") == "" || loginValues.Get("challenge_method") != "S256" {
		t.Fatalf("login query = %v", loginValues)
	}

	raw, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["type"] != ProviderQoder || stored["access_token"] != "qoder-access" || stored["refresh_token"] != "qoder-refresh" || stored["user_id"] != "user-42" {
		t.Fatalf("stored credential = %+v", stored)
	}
	if info, err := os.Stat(result.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential permissions = %v, %v", info, err)
	}
}

func TestQoderEncodeBodyMatchesProtocol(t *testing.T) {
	for _, input := range [][]byte{nil, []byte("a"), []byte("hello"), []byte(`{"key":"value"}`), {0x00, 0xff, 0x80, 0x7f}} {
		encoded := qoderEncodeBody(input)
		decoded, err := decodeQoderBody(encoded)
		if err != nil {
			t.Fatalf("decode %q: %v", encoded, err)
		}
		if !bytes.Equal(decoded, input) {
			t.Fatalf("round trip = %x, want %x", decoded, input)
		}
		if strings.Contains(encoded, "=") {
			t.Fatalf("encoded body contains standard padding: %q", encoded)
		}
	}
}

func TestQoderEnvelopeRecognizesNestedQueueResponse(t *testing.T) {
	queue, _ := json.Marshal(map[string]any{
		"isQueued": true, "modelKey": "qmodel_38max", "queueCount": 5266,
		"queueType": "slow", "retryAfterSeconds": 30, "serviceAvailable": true, "waitTime": 15305,
	})
	inner, _ := json.Marshal(map[string]any{"code": "10605", "message": string(queue)})
	outer, _ := json.Marshal(map[string]any{"code": "403", "message": string(inner)})
	envelope, _ := json.Marshal(map[string]any{"statusCodeValue": 403, "body": string(outer)})

	_, err := qoderEnvelopeBody(envelope)
	var streamErr *qoderStreamError
	if !errors.As(err, &streamErr) || streamErr.queue == nil {
		t.Fatalf("queue error = %#v, %v", streamErr, err)
	}
	if streamErr.queue.ModelKey != "qmodel_38max" || streamErr.queue.QueueCount != 5266 || streamErr.queue.RetryAfterSeconds != 30 || !streamErr.queue.ServiceAvailable {
		t.Fatalf("queue info = %#v", streamErr.queue)
	}
	if qoderAnthropicErrorType(err) != "rate_limit_error" || !strings.Contains(err.Error(), "5266 requests ahead") {
		t.Fatalf("translated queue error = %q (%s)", err, qoderAnthropicErrorType(err))
	}
}

func TestQoderAuthHeadersUseCOSYProtocolWithoutPlaintextToken(t *testing.T) {
	body := []byte("encoded-body")
	headers, err := qoderBuildAuthHeaders(body, "https://api3.qoder.sh/algo/api/v2/model/list?Encode=1", qoderSigningCredential{
		userID: "user-1", accessToken: "secret-access", name: "User", email: "user@example.com", machineID: "machine-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"Authorization", "Cosy-Key", "Cosy-User", "Cosy-Date", "Cosy-Bodyhash", "Cosy-Bodylength", "Cosy-Sigpath", "Cosy-Machineid"} {
		if headers.Get(required) == "" {
			t.Fatalf("missing %s: %+v", required, headers)
		}
	}
	if headers.Get("Cosy-Sigpath") != "/api/v2/model/list" || headers.Get("Cosy-Bodylength") != fmt.Sprintf("%d", len(body)) {
		t.Fatalf("signature headers = %+v", headers)
	}
	for name, values := range headers {
		if strings.Contains(strings.Join(values, ""), "secret-access") {
			t.Fatalf("header %s contains plaintext token", name)
		}
	}
}

func TestQoderRuntimeRefreshesCredentialDiscoversModelsAndTranslatesMessages(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir, err := ensureAuthDir()
	if err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(authDir, "qoder-test.json")
	credential := map[string]any{
		"type": ProviderQoder, "access_token": "expired", "refresh_token": "refresh", "user_id": "user-1",
		"machine_id": "machine-1", "email": "qoder@example.com", "expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
	}
	raw, _ := json.Marshal(credential)
	if err := os.WriteFile(credentialPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var refreshCalls atomic.Int32
	requestBody := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/algo/api/v3/user/refresh_token":
			refreshCalls.Add(1)
			_ = json.NewEncoder(writer).Encode(map[string]any{"token": "fresh-access", "refresh_token": "fresh-refresh", "expires_in": 3600})
		case "/algo/api/v2/model/list":
			if request.URL.Query().Get("Encode") != "1" || !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer COSY.") {
				t.Fatalf("model discovery request headers/query invalid")
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"chat": []any{
				map[string]any{
					"key": "qmodel", "enable": true, "display_name": "Qwen Plus", "max_input_tokens": 180000,
					"max_output_tokens": 32768, "price_factor": 0.5, "is_reasoning": true, "is_vl": true,
					"is_new": true, "promotion": map[string]any{"active": false}, "source": "system",
					"context_config": map[string]any{"200k": map[string]any{"token_count": 200000}, "1m": map[string]any{"token_count": 1000000}},
				},
			}})
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			encoded, _ := io.ReadAll(request.Body)
			decoded, err := decodeQoderBody(string(encoded))
			if err != nil {
				t.Fatalf("decode upstream request: %v", err)
			}
			var body map[string]any
			if err := json.Unmarshal(decoded, &body); err != nil {
				t.Fatalf("decode upstream JSON: %v", err)
			}
			requestBody <- body
			writer.Header().Set("Content-Type", "text/event-stream")
			writeQoderTestSSE(writer, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": "think"}}}})
			writeQoderTestSSE(writer, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "answer"}}}})
			writeQoderTestSSE(writer, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": "call-1", "function": map[string]any{"name": "shell", "arguments": `{"command":"pwd"}`}},
			}}}}})
			writeQoderTestSSE(writer, map[string]any{
				"choices": []any{map[string]any{"finish_reason": "tool_calls", "delta": map[string]any{}}},
				"usage":   map[string]any{"prompt_tokens": 42, "completion_tokens": 7, "prompt_tokens_details": map[string]any{"cached_tokens": 5, "cache_write_tokens": 2}},
			})
			_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	originalAPI, originalCenter := qoderAPIBaseURL, qoderCenterBaseURL
	qoderAPIBaseURL, qoderCenterBaseURL = server.URL, server.URL
	t.Cleanup(func() { qoderAPIBaseURL, qoderCenterBaseURL = originalAPI, originalCenter })

	runtime, err := StartOAuth(context.Background(), ProviderQoder, "qmodel[1m]", filepath.Base(credentialPath))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls.Load())
	}
	if models := runtime.Models(); len(models) != 1 || models[0] != "qmodel" {
		t.Fatalf("runtime models = %v", models)
	}
	modelsRequest, _ := http.NewRequest(http.MethodGet, runtime.Endpoint()+"/models", nil)
	modelsRequest.Header.Set("x-api-key", runtime.APIKey())
	modelsResponse, err := http.DefaultClient.Do(modelsRequest)
	if err != nil {
		t.Fatal(err)
	}
	var modelCatalog struct {
		Data []struct {
			ID                 string   `json:"id"`
			DisplayName        string   `json:"display_name"`
			RateMultiplier     *float64 `json:"rate_multiplier"`
			IsNew              bool     `json:"is_new"`
			PromotionAvailable bool     `json:"promotion_available"`
		} `json:"data"`
	}
	if err := json.NewDecoder(modelsResponse.Body).Decode(&modelCatalog); err != nil {
		_ = modelsResponse.Body.Close()
		t.Fatal(err)
	}
	_ = modelsResponse.Body.Close()
	if len(modelCatalog.Data) != 1 || modelCatalog.Data[0].ID != "qmodel" || modelCatalog.Data[0].DisplayName != "Qwen Plus" {
		t.Fatalf("local model catalog = %#v", modelCatalog.Data)
	}
	if names := runtime.ModelDisplayNames(); names["qmodel"] != "Qwen Plus" {
		t.Fatalf("runtime model display names = %#v", names)
	}
	if modelCatalog.Data[0].RateMultiplier == nil || *modelCatalog.Data[0].RateMultiplier != 0.5 || !modelCatalog.Data[0].IsNew || !modelCatalog.Data[0].PromotionAvailable {
		t.Fatalf("local model metadata = %#v", modelCatalog.Data[0])
	}

	payload := map[string]any{
		"model": "qmodel[1m]", "max_tokens": 100, "stream": false,
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 1000},
		"system":   "follow the system",
		"tools":    []any{map[string]any{"name": "shell", "description": "run command", "input_schema": map[string]any{"type": "object"}}},
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	payloadRaw, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/messages", bytes.NewReader(payloadRaw))
	req.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseRaw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("messages status=%d body=%s", response.StatusCode, responseRaw)
	}
	var output struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type      string         `json:"type"`
			Thinking  string         `json:"thinking"`
			Signature string         `json:"signature"`
			Text      string         `json:"text"`
			ID        string         `json:"id"`
			Name      string         `json:"name"`
			Input     map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(responseRaw, &output); err != nil {
		t.Fatal(err)
	}
	if output.StopReason != "tool_use" || len(output.Content) != 3 {
		t.Fatalf("translated response = %+v", output)
	}
	if output.Content[0].Type != "thinking" || output.Content[0].Thinking != "think" || output.Content[0].Signature != "qoder" {
		t.Fatalf("thinking block = %+v", output.Content[0])
	}
	if output.Content[1].Type != "text" || output.Content[1].Text != "answer" {
		t.Fatalf("text block = %+v", output.Content[1])
	}
	if output.Content[2].Type != "tool_use" || output.Content[2].Name != "shell" || output.Content[2].Input["command"] != "pwd" {
		t.Fatalf("tool block = %+v", output.Content[2])
	}

	upstreamBody := <-requestBody
	if upstreamBody["session_type"] != "qodercli" {
		t.Fatalf("session_type = %v", upstreamBody["session_type"])
	}
	modelConfig := upstreamBody["model_config"].(map[string]any)
	if modelConfig["key"] != "qmodel" {
		t.Fatalf("model_config = %+v", modelConfig)
	}
	messages := upstreamBody["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["content"] != "hello" {
		t.Fatalf("upstream messages = %+v", messages)
	}
	usage, ok := runtime.Usage().Snapshot()
	if !ok || len(usage) != 1 || usage[0].Model != "Qwen Plus" || usage[0].InputTokens != 35 || usage[0].OutputTokens != 7 || usage[0].CacheReadTokens != 5 || usage[0].CacheWriteTokens != 2 {
		t.Fatalf("usage = %+v", usage)
	}

	storedRaw, _ := os.ReadFile(credentialPath)
	if !bytes.Contains(storedRaw, []byte("fresh-access")) || !bytes.Contains(storedRaw, []byte("fresh-refresh")) {
		t.Fatalf("refreshed credential not persisted: %s", storedRaw)
	}
}

func TestQoderTransformMessagesAcceptsGatewaySystemRole(t *testing.T) {
	messages := []anthropicMessage{
		{
			Role: "system",
			Content: json.RawMessage(`[
				{"type":"text","text":"first instruction"},
				{"type":"text","text":"second instruction","cache_control":{"type":"ephemeral"}}
			]`),
		},
		{Role: "user", Content: json.RawMessage(`"hello"`)},
	}

	transformed, lastUser, err := qoderTransformMessages(messages)
	if err != nil {
		t.Fatal(err)
	}
	if lastUser != "hello" {
		t.Fatalf("last user = %q", lastUser)
	}
	if len(transformed) != 2 {
		t.Fatalf("transformed messages = %+v", transformed)
	}
	system := transformed[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "first instruction\nsecond instruction" {
		t.Fatalf("system message = %+v", system)
	}
	user := transformed[1].(map[string]any)
	if user["role"] != "user" || user["content"] != "hello" {
		t.Fatalf("user message = %+v", user)
	}
}

func TestQoderSelectModelAcceptsDisplayAlias(t *testing.T) {
	catalog := []qoderModel{{ID: "dfmodel", Name: "DeepSeek-V4-Flash"}}
	selected, ok := qoderSelectModel("DeepSeek-V4-Flash[1m]", catalog)
	if !ok || selected.ID != "dfmodel" {
		t.Fatalf("selected = %+v, ok=%t", selected, ok)
	}
}

func writeQoderTestSSE(writer io.Writer, body any) {
	inner, _ := json.Marshal(body)
	envelope, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "statusCode": "OK", "body": string(inner)})
	_, _ = fmt.Fprintf(writer, "data:%s\n\n", envelope)
}

func decodeQoderBody(encoded string) ([]byte, error) {
	standardized := make([]byte, len(encoded))
	for index, character := range []byte(encoded) {
		if character == '$' {
			standardized[index] = '='
			continue
		}
		customIndex := strings.IndexByte(qoderCustomBase64, character)
		if customIndex < 0 {
			return nil, fmt.Errorf("unknown custom character %q", character)
		}
		standardized[index] = qoderStandardBase64[customIndex]
	}
	n := len(standardized)
	a := n / 3
	restored := append([]byte{}, standardized[n-a:]...)
	restored = append(restored, standardized[a:n-a]...)
	restored = append(restored, standardized[:a]...)
	return base64.StdEncoding.DecodeString(string(restored))
}
