package oauthproxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestZedRuntimeServesHealthAndRefusesNonGETModels covers the small routes a
// client probes before it sends a prompt.
func TestZedRuntimeServesHealthAndRefusesNonGETModels(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, runtime.ClaudeBaseURL()+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"status":"ok"`) {
		t.Fatalf("healthz status=%d body=%s", response.StatusCode, raw)
	}

	// The model list is a GET; a POST to it is refused before anything else.
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, runtime.ClaudeBaseURL()+"/v1/models",
		strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/models status = %d, want 405: %s", response.StatusCode, raw)
	}
}

// TestZedRuntimeRefusesModelListsWithoutTheRuntimeKey pins that the catalog is
// as private as the prompts.
func TestZedRuntimeRefusesModelListsWithoutTheRuntimeKey(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")

	for _, header := range []string{"", "Bearer wrong"} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, runtime.ClaudeBaseURL()+"/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("authorization %q: status = %d, want 401", header, response.StatusCode)
		}
	}
	// The same endpoints without the /v1 prefix are served identically.
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, runtime.ClaudeBaseURL()+"/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/models status = %d, want 200", response.StatusCode)
	}
}

func TestBuildZedRoutesRefusesAnEmptyCatalog(t *testing.T) {
	if _, err := buildZedRoutes("", nil); err == nil || !strings.Contains(err.Error(), "no usable model routes") {
		t.Fatalf("empty catalog error = %v", err)
	}
}

// TestZedRouterRefusesUnknownModelsOnBothMessagePaths covers the router's own
// dispatch guard, which must answer in the Anthropic dialect on both paths.
func TestZedRouterRefusesUnknownModelsOnBothMessagePaths(t *testing.T) {
	cloud := newFakeZedCloud(t)
	runtime := startZedTestRuntime(t, cloud, "")

	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		response, body := zedPost(t, runtime, path, map[string]any{"model": "no-such-model"})
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(body, "not routed") {
			t.Fatalf("%s: status=%d body: %s", path, response.StatusCode, body)
		}
	}
	if got := len(cloud.callsTo("/completions")) + len(cloud.callsTo("/count_tokens")); got != 0 {
		t.Fatalf("an unrouted model reached Zed: %d calls", got)
	}
}
