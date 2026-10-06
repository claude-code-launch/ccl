package oauthproxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestZedRouterListsItsCatalogAndRefusesUnknownKeys covers the discovery
// endpoints Claude Code hits before a prompt: the model list is the account's
// catalog, and every route demands the runtime key.
func TestZedRouterListsItsCatalogAndRefusesUnknownKeys(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, runtime.ClaudeBaseURL()+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/models status = %d, want 401", response.StatusCode)
	}

	request.Header.Set("x-api-key", runtime.APIKey())
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if response.StatusCode != http.StatusOK || gjson.Get(body, "object").String() != "list" {
		t.Fatalf("status=%d body: %s", response.StatusCode, body)
	}
	ids := gjson.Get(body, "data.#.id").Array()
	if len(ids) != len(runtime.Models()) {
		t.Fatalf("listed %d models, runtime reports %d", len(ids), len(runtime.Models()))
	}
	for _, id := range ids {
		if strings.Contains(id.String(), "claude-disabled") || strings.Contains(id.String(), "mistral-future") {
			t.Fatalf("/v1/models published an unservable model: %s", body)
		}
	}
}

// TestZedRouterCountsTokensOnEveryWire checks that count_tokens works whichever
// data plane the model belongs to: Zed answers for Anthropic models, and the
// other planes estimate locally because Zed cannot count for them.
func TestZedRouterCountsTokensOnEveryWire(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")

	messages := []any{map[string]any{"role": "user", "content": "hi"}}
	for _, tc := range []struct {
		model     string
		countedBy string // "zed" for the models Zed counts itself
	}{
		{model: "claude-sonnet-test", countedBy: "zed"},
		{model: "gpt-test"},
		{model: "grok-test"},
		{model: "gemini-test"},
	} {
		response, body := zedPost(t, runtime, "/v1/messages/count_tokens",
			map[string]any{"model": tc.model, "messages": messages})
		if response.StatusCode != http.StatusOK || gjson.Get(body, "input_tokens").Int() <= 0 {
			t.Fatalf("%s: status=%d body: %s", tc.model, response.StatusCode, body)
		}
		want := int64(77) // estimateApproxTokensBytes is never exactly 77 for this body
		if tc.countedBy == "zed" {
			if gjson.Get(body, "input_tokens").Int() != want {
				t.Fatalf("%s: input_tokens = %s, want Zed's %d", tc.model, body, want)
			}
			if got := len(cloud.callsTo("/count_tokens")); got != 1 {
				t.Fatalf("%s: Zed count calls = %d, want 1", tc.model, got)
			}
		} else if gjson.Get(body, "input_tokens").Int() == want {
			t.Fatalf("%s: reported Zed's %d, want a local estimate", tc.model, want)
		}
	}

	response, body := zedPost(t, runtime, "/v1/messages/count_tokens", map[string]any{"model": "not-a-zed-model"})
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(gjson.Get(body, "error.message").String(), "not routed") {
		t.Fatalf("unknown model status=%d body: %s", response.StatusCode, body)
	}
}

// TestZedRouterRejectsMalformedInboundBodies covers the router's own guards,
// which run before any service sees the request.
func TestZedRouterRejectsMalformedInboundBodies(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")

	for _, key := range []string{"", "wrong-key"} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			runtime.ClaudeBaseURL()+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-test"}`))
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			request.Header.Set("x-api-key", key)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized || gjson.GetBytes(raw, "error.type").String() != "authentication_error" {
			t.Fatalf("key %q: status=%d body: %s", key, response.StatusCode, raw)
		}
	}
	if got := len(cloud.callsTo("/completions")); got != 0 {
		t.Fatalf("unauthenticated request reached Zed: %d calls", got)
	}
}
