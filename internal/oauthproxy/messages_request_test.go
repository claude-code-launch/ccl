package oauthproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const exactToolArguments = `{"record_id":9007199254740993,"nested":[0.123456789012345678901,1e1000]}`
const requestSchema = `{"type":"object","properties":{"record_id":{"type":"integer","const":9007199254740993}},"required":["record_id"],"additionalProperties":false}`

func TestMessagesWirePreservesArgumentsSchemaAndLimits(t *testing.T) {
	for _, backend := range []string{"chat", "responses", "codex", "xai"} {
		t.Run(backend, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, _ := io.ReadAll(r.Body)
				argumentsPath, schemaPath, formatPath, limitPath := "input.0.arguments", "tools.0.parameters", "text.format", "max_output_tokens"
				if backend == "chat" {
					argumentsPath, schemaPath, formatPath, limitPath = "messages.0.tool_calls.0.function.arguments", "tools.0.function.parameters", "response_format.json_schema", "max_tokens"
				}
				if got := gjson.GetBytes(body, argumentsPath).String(); got != exactToolArguments {
					t.Errorf("arguments changed: %s", got)
				}
				for _, path := range []string{schemaPath, formatPath + ".schema"} {
					if got := gjson.GetBytes(body, path+".properties.record_id.const").Raw; got != "9007199254740993" {
						t.Errorf("schema numeric value changed at %s: %s", path, got)
					}
				}
				if !gjson.GetBytes(body, formatPath+".strict").Bool() || gjson.GetBytes(body, formatPath+".name").String() != "answer" {
					t.Errorf("format missing: %s", body)
				}
				if got := gjson.GetBytes(body, "parallel_tool_calls"); !got.Exists() || got.Bool() {
					t.Error("disable parallel tool calls was lost")
				}
				if gjson.GetBytes(body, "model").String() != "upstream-model" {
					t.Error("route alias was not applied")
				}
				if backend == "codex" {
					if gjson.GetBytes(body, limitPath).Exists() {
						t.Error("unsupported Codex token limit forwarded")
					}
				} else if gjson.GetBytes(body, limitPath).Int() != 37 {
					t.Error("token limit lost")
				}
				w.Header().Set("Content-Type", "application/json")
				if backend == "chat" {
					_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
				} else {
					_, _ = io.WriteString(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
				}
			}))
			defer upstream.Close()
			routes := []runtimeModelRoute{{Alias: "alias", Name: "upstream-model"}}
			var handler http.Handler
			if backend == "chat" {
				handler = newChatCompletionsService("local", upstream.URL, routes, "key", NewUsageTracker()).handler()
			} else {
				var authorizer codexResponsesAuthorizer = &codexStaticAuthorizer{token: "key"}
				if backend == "codex" {
					path := filepath.Join(t.TempDir(), "credential.json")
					if err := os.WriteFile(path, []byte(`{"type":"codex","access_token":"test-only"}`), 0600); err != nil {
						t.Fatal(err)
					}
					authorizer = &codexOAuthAuthorizer{path: path}
				}
				handler = newResponsesService("local", upstream.URL, routes, authorizer, NewUsageTracker(), backend == "xai").handler()
			}
			raw := `{"model":"alias","max_tokens":37,"tools":[{"name":"lookup","input_schema":` + requestSchema + `}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true},"output_config":{"format":{"type":"json_schema","name":"answer","schema":` + requestSchema + `}},"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"lookup","input":` + exactToolArguments + `}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"ok"}]}]}`
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(raw))
			r.Header.Set("x-api-key", "local")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}
}

func TestMessagesChatPreservesImageOrderAndURLs(t *testing.T) {
	raw := []byte(`{"model":"test","messages":[{"role":"user","content":[{"type":"text","text":"before"},{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},{"type":"text","text":"after"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}`)
	req, err := convertAnthropicToChatCompletions(raw)
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(req.body, "messages.0.content").Array()
	if len(parts) != 4 {
		t.Fatalf("parts lost: %s", req.body)
	}
	for i, kind := range []string{"text", "image_url", "text", "image_url"} {
		if parts[i].Get("type").String() != kind {
			t.Fatalf("content order changed: %s", req.body)
		}
	}
	if parts[1].Get("image_url.url").String() != "https://example.com/a.png" || parts[3].Get("image_url.url").String() != "data:image/png;base64,AA==" {
		t.Fatalf("image source changed: %s", req.body)
	}
}

func TestMessagesStructuredOutputOptionalProperties(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object","properties":{"x":{"type":"string"}},"additionalProperties":false}`,
		`{"type":"object","properties":{"x":{"type":"object","properties":{"optional":{"type":"integer"}},"additionalProperties":false}},"required":["x"],"additionalProperties":false}`,
	} {
		output := &anthropicOutput{Format: json.RawMessage(`{"type":"json_schema","schema":` + schema + `}`)}
		format, err := messagesStructuredOutput(output)
		if err != nil {
			t.Fatal(err)
		}
		if format["strict"] != false || string(format["schema"].(json.RawMessage)) != schema {
			t.Fatalf("optional schema changed: %v", format)
		}
	}
	for _, raw := range []string{`{"type":"other"}`, `{"type":"json_schema","schema":null}`} {
		if _, err := messagesStructuredOutput(&anthropicOutput{Format: json.RawMessage(raw)}); err == nil {
			t.Fatal("invalid output format silently accepted")
		}
	}
}
