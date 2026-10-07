package oauthproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompactionHeaderMarksTheRequest(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hello there"}]}`)
	plain, err := convertAnthropicToCodexResponsesWithHint(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if plain.compaction {
		t.Fatal("an ordinary request was treated as compaction")
	}
	hinted, err := convertAnthropicToCodexResponsesWithHint(raw, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !hinted.compaction || hinted.compactionReason != "header_auto" {
		t.Fatalf("header hint ignored: compaction=%t reason=%q", hinted.compaction, hinted.compactionReason)
	}
}

func TestPassthroughStripsHintHeaders(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	defer upstream.Close()
	service := newAnthropicPassthroughService("local", upstream.URL, []string{"m"}, &chatStaticAuthorizer{token: "up"}, NewUsageTracker())
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("x-api-key", "local")
	for _, header := range claudeCodeHintHeaders {
		request.Header.Set(header, "x")
	}
	request.Header.Set("X-Claude-Code-Session-Id", "s-1")
	service.handler().ServeHTTP(httptest.NewRecorder(), request)
	if got == nil {
		t.Fatal("upstream was not called")
	}
	for _, header := range claudeCodeHintHeaders {
		if got.Get(header) != "" {
			t.Errorf("%s reached the upstream", header)
		}
	}
	if got.Get("X-Claude-Code-Session-Id") != "s-1" {
		t.Fatal("the session ID (which OpenCode requires) was stripped too")
	}
}

func TestFastModeHeaderRequestsPriority(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	for name, testCase := range map[string]struct {
		hints codexConvertHints
		body  string
		want  string
	}{
		"off":           {codexConvertHints{}, "", ""},
		"session fast":  {codexConvertHints{fast: true}, "", "priority"},
		"explicit wins": {codexConvertHints{fast: true}, `"service_tier":"flex",`, "flex"},
	} {
		body := raw
		if testCase.body != "" {
			body = []byte(strings.Replace(string(raw), "{", "{"+testCase.body, 1))
		}
		converted, err := convertAnthropicToCodexResponsesWith(body, testCase.hints)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if strings.Contains(string(converted.body), `"service_tier":"priority"`) {
			got = "priority"
		} else if strings.Contains(string(converted.body), `"service_tier":"flex"`) {
			got = "flex"
		}
		if got != testCase.want {
			t.Fatalf("%s: service_tier = %q in %s", name, got, converted.body)
		}
	}
}
