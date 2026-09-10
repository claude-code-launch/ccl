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
	for _, dialect := range []string{"chat", "responses", "anthropic"} {
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
				default:
					io.WriteString(w, `{"type":"message","content":[]}`)
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
			default:
				rt, err = StartMixedProtocolAPIKeyRuntime(context.Background(), endpoint, "fake", "m", map[string]string{"m": "anthropic"})
				want += "messages?tenant=demo%2F&beta=true"
			}
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Stop()
			req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/messages", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`))
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

func TestNativeModelSuffixMessagesAndCountTokens(t *testing.T) {
	for _, path := range []string{"/messages", "/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			var got []byte
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ = io.ReadAll(r.Body)
				io.WriteString(w, `{"input_tokens":1}`)
			}))
			defer up.Close()
			rt, err := StartMixedProtocolAPIKeyRuntime(context.Background(), up.URL, "fake", "claude-test[1m]", map[string]string{"claude-test": "anthropic"})
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Stop()
			req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+path, strings.NewReader(`{"model":"claude-test[1m]","messages":[{"role":"user","content":"hi"}],"metadata":{"large":9007199254740993},"max_tokens":1}`))
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
			if gjson.GetBytes(got, "metadata.large").Raw != "9007199254740993" {
				t.Fatalf("metadata changed: %s", got)
			}
		})
	}
}

func TestChatToolImagesFollowAllToolReplies(t *testing.T) {
	raw := []byte(`{"model":"vision","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"screenshot","input":{}},{"type":"tool_use","id":"b","name":"screenshot","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"first"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]},{"type":"tool_result","tool_use_id":"b","content":[{"type":"image","source":{"type":"url","url":"https://example.com/b.png"}}]},{"type":"text","text":"compare these"}]}]}`)
	c, err := convertAnthropicToChatCompletions(raw)
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
	for _, dialect := range []string{"chat", "responses", "gemini", "commandcode"} {
		t.Run(dialect, func(t *testing.T) {
			a := newAnthropicResponseAssembler(&anthropicAdapterRequest{inputTokens: 777}, nil)
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
			case "commandcode":
				commandcodeApplyUsage(a, gjson.Parse(`{"inputTokens":100,"cachedInputTokens":100}`))
			}
			if got := a.usage(); got["input_tokens"] != 0 || got["cache_read_input_tokens"] != 100 {
				t.Fatalf("usage=%v", got)
			}
		})
	}
	a := newAnthropicResponseAssembler(&anthropicAdapterRequest{inputTokens: 777}, nil)
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
