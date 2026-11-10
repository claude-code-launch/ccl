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
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n")
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
			raw := `{"model":"alias","max_output_tokens":37,"parallel_tool_calls":false,"tools":[{"type":"function","name":"lookup","parameters":` + requestSchema + `}],"tool_choice":"auto","text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":` + requestSchema + `}},"input":[{"type":"function_call","call_id":"call1","name":"lookup","arguments":` + strconvQuote(exactToolArguments) + `},{"type":"function_call_output","call_id":"call1","output":"ok"}]}`
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
			r.Header.Set("x-api-key", "local")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}
}

func strconvQuote(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func TestMessagesChatPreservesImageOrderAndURLs(t *testing.T) {
	raw := []byte(`{"model":"test","input":[{"role":"user","content":[{"type":"input_text","text":"before"},{"type":"input_image","image_url":"https://example.com/a.png"},{"type":"input_text","text":"after"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`)
	req, err := convertResponsesToChatCompletions(raw)
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
		parsedFormat, err := responsesStructuredFormat(json.RawMessage(`{"type":"json_schema","schema":` + schema + `}`))
		if err != nil {
			t.Fatal(err)
		}
		format := structuredOutputSchema(parsedFormat)
		if format == nil {
			t.Fatal("expected structured output schema")
		}
		if format["strict"] != false || string(format["schema"].(json.RawMessage)) != schema {
			t.Fatalf("optional schema changed: %v", format)
		}
	}
	for _, raw := range []string{`{"type":"other"}`, `{"type":"json_schema","schema":null}`} {
		if parsedFormat, err := responsesStructuredFormat(json.RawMessage(raw)); err == nil && structuredOutputSchema(parsedFormat) != nil {
			t.Fatal("invalid output format silently accepted")
		}
	}
}
