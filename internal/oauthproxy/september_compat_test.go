package oauthproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAntigravityClaudeToolSchema(t *testing.T) {
	for _, model := range []string{"claude-sonnet-test", "gemini-test"} {
		raw := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"test"}],"tools":[{"name":"read_file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}`)
		result, err := convertAnthropicToGemini(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(result.geminiBody, "tools.0.functionDeclarations.0.parameters.properties.path.type").String(); got != "string" {
			t.Fatalf("missing schema: %s", result.geminiBody)
		}
		if gjson.GetBytes(result.geminiBody, "tools.0.functionDeclarations.0.parametersJsonSchema").Exists() {
			t.Fatal("public-API schema field leaked into Antigravity")
		}
	}
}

func TestChatStructureContainsNoPayload(t *testing.T) {
	body := []byte(`{"messages":[{"content":"secret-text"},{"content":[{"type":"image_url","image_url":{"url":"secret-url"}},{"type":"secret-type"}]}]}`)
	got := chatContentStructure(body)
	if strings.Contains(got, "secret") || !strings.Contains(got, `"part_image_url":1`) || !strings.Contains(got, `"part_unknown":1`) {
		t.Fatalf("unsafe/incorrect structure: %s", got)
	}
}

func TestAntigravityRuntimeToolSchemaWireContract(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		schema := gjson.GetBytes(raw, "request.tools.0.functionDeclarations.0.parameters")
		if !schema.IsObject() || schema.Get("properties.q.type").String() != "string" {
			http.Error(w, `{"error":{"message":"tools.0.custom.input_schema: Field required"}}`, http.StatusBadRequest)
			return
		}
		if gjson.GetBytes(raw, "request.tools.0.functionDeclarations.0.parametersJsonSchema").Exists() {
			t.Error("wrong schema field on wire")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"q\":\"test\"}}}]},\"finishReason\":\"STOP\"}]}}\n\n")
	}))
	defer upstream.Close()
	oldDaily, oldProd := antigravityBaseURLDaily, antigravityBaseURLProd
	antigravityBaseURLDaily, antigravityBaseURLProd = upstream.URL, upstream.URL
	t.Cleanup(func() { antigravityBaseURLDaily, antigravityBaseURLProd = oldDaily, oldProd })
	home := t.TempDir()
	t.Setenv("HOME", home)
	credential := writeGeminiCredential(t, home, "fake-gemini.json")
	for _, model := range []string{"claude-sonnet-test", "gemini-test"} {
		runtime, err := StartOAuth(context.Background(), ProviderGemini, model, filepath.Base(credential))
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer runtime.Stop()
			body := postClaudeMessageWithTools(t, context.Background(), runtime, model)
			if !strings.Contains(body, "tool_use") || !strings.Contains(body, "lookup") || !strings.Contains(body, "message_stop") {
				t.Fatalf("tool response did not roundtrip: %s", body)
			}
		}()
	}
}
