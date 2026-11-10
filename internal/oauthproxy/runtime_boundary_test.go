package oauthproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRuntimePreservesEndpointQueryAndEscaping(t *testing.T) {
	for _, dialect := range []string{"chat", "responses"} {
		t.Run(dialect, func(t *testing.T) {
			var got string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.RequestURI()
				w.Header().Set("Content-Type", "application/json")
				switch dialect {
				case "chat":
					io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
				case "responses":
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
				}
			}))
			defer up.Close()
			endpoint := up.URL + "/tenant%2Fname/v1?tenant=demo%2F"
			var rt *Runtime
			var err error
			want := "/tenant%2Fname/v1/"
			switch dialect {
			case "chat":
				rt, err = StartOpenAIChatAPI(context.Background(), endpoint, "fake", "m")
				want += "chat/completions?tenant=demo%2F"
			case "responses":
				rt, err = StartOpenAIResponsesAPI(context.Background(), endpoint, "fake", "m")
				want += "responses?tenant=demo%2F"
			}
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Stop()
			req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses", strings.NewReader(`{"model":"m","input":[{"role":"user","content":"hi"}],"max_output_tokens":1}`))
			req.Header.Set("x-api-key", rt.APIKey())
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			if got != want {
				t.Fatalf("upstream URI=%q, want %q", got, want)
			}
		})
	}
}

func TestNativeModelSuffixResponses(t *testing.T) {
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()
	rt, err := StartMixedProtocolAPIKeyRuntime(context.Background(), up.URL, "fake", "claude-test[1m]", map[string]string{"claude-test": "openai"})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses", strings.NewReader(`{"model":"claude-test[1m]","input":[{"role":"user","content":"hi"}],"max_output_tokens":1}`))
	req.Header.Set("x-api-key", rt.APIKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || gjson.GetBytes(got, "model").String() != "claude-test" {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
}

func TestChatToolImagesFollowAllToolReplies(t *testing.T) {
	raw := []byte(`{"model":"vision","tools":[{"type":"function","name":"screenshot","parameters":{"type":"object"}}],"input":[{"type":"function_call","call_id":"a","name":"screenshot","arguments":"{}"},{"type":"function_call","call_id":"b","name":"screenshot","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":[{"type":"input_text","text":"first"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]},{"type":"function_call_output","call_id":"b","output":[{"type":"input_image","image_url":"https://example.com/b.png"}]},{"role":"user","content":[{"type":"input_text","text":"compare these"}]}]}`)
	c, err := convertResponsesToChatCompletions(raw)
	if err != nil {
		t.Fatal(err)
	}
	root := gjson.ParseBytes(c.body)
	msgs := root.Get("messages").Array()
	if len(msgs) != 4 || msgs[1].Get("role").String() != "tool" || msgs[2].Get("role").String() != "tool" || msgs[3].Get("role").String() != "user" {
		t.Fatalf("wrong ordering: %s", c.body)
	}
	parts := msgs[3].Get("content").Array()
	if len(parts) != 5 || !strings.Contains(parts[0].Get("text").String(), "a:") || !strings.Contains(parts[2].Get("text").String(), "b:") || parts[1].Get("image_url.url").String() != "data:image/png;base64,aGVsbG8=" || parts[3].Get("image_url.url").String() != "https://example.com/b.png" || parts[4].Get("text").String() != "compare these" {
		t.Fatalf("lost images/context: %s", c.body)
	}
	if !strings.Contains(msgs[1].Get("content").String(), "first") {
		t.Fatal("tool text lost")
	}
}

func TestZeroFreshInputUsageIsAuthoritative(t *testing.T) {
	for _, dialect := range []string{"chat", "responses", "gemini"} {
		t.Run(dialect, func(t *testing.T) {
			a := newResponseAssembler(&adapterRequest{inputTokens: 777}, nil)
			switch dialect {
			case "chat":
				chatApplyUsage(a, map[string]any{"prompt_tokens": float64(100), "prompt_tokens_details": map[string]any{"cached_tokens": float64(100)}})
			case "responses":
				s := &codexResponsesStreamState{assembler: a}
				if err := s.processTerminal("response.completed", map[string]any{"usage": map[string]any{"input_tokens": float64(100), "input_tokens_details": map[string]any{"cached_tokens": float64(100)}}}); err != nil {
					t.Fatal(err)
				}
			case "gemini":
				n := 0
				if err := processGeminiChunk([]byte(`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":100}}`), a, &n); err != nil {
					t.Fatal(err)
				}
			}
			inTokens, _ := a.tokenTotals()
			usage := a.usage()
			details, _ := usage["input_tokens_details"].(map[string]any)
			if inTokens != 0 || a.cacheReadTokens != 100 || usage["input_tokens"] != 100 || details["cached_tokens"] != 100 {
				t.Fatalf("inTokens=%d cacheRead=%d usage=%+v", inTokens, a.cacheReadTokens, usage)
			}
		})
	}
	a := newResponseAssembler(&adapterRequest{inputTokens: 777}, nil)
	chatApplyUsage(a, map[string]any{})
	if a.usage()["input_tokens"] != 777 {
		t.Fatal("missing usage must retain estimate")
	}
	chatApplyUsage(a, map[string]any{"prompt_tokens": float64(0), "completion_tokens": float64(0)})
	encoded, _ := json.Marshal(a.usage())
	if a.usage()["input_tokens"] != 0 {
		t.Fatalf("explicit zero lost: %s", encoded)
	}
}
