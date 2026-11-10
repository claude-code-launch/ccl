package groklaunch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestRealGrokUsesCCLResponses(t *testing.T) {
	binary := os.Getenv("CCL_TEST_GROK")
	if binary == "" {
		t.Skip("set CCL_TEST_GROK to the Grok Build binary")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	for key, value := range map[string]string{
		"HOME": home, "USERPROFILE": home, "GROK_HOME": filepath.Join(home, ".grok"),
		"XDG_CONFIG_HOME": filepath.Join(home, "config"), "XDG_DATA_HOME": filepath.Join(home, "data"), "XDG_CACHE_HOME": filepath.Join(home, "cache"),
		"HTTP_PROXY": "http://127.0.0.1:9", "HTTPS_PROXY": "http://127.0.0.1:9", "ALL_PROXY": "http://127.0.0.1:9", "NO_PROXY": "127.0.0.1,localhost",
	} {
		t.Setenv(key, value)
		t.Setenv(strings.ToLower(key), value)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fake-upstream-key" ||
			json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "ccl-fixture" {
			t.Error("Grok request did not reach the configured provider")
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"local-grok-fixture\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"local-grok-fixture"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(upstream.Close)

	launch, err := Prepare(t.Context(), binary, provider.Provider{
		Name: "fixture", Type: "openai", Endpoint: upstream.URL + "/v1", APIKey: "fake-upstream-key", Model: "ccl-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(launch.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, launch.Path, "--disable-web-search", "--single", "Reply with fixture output.")
	cmd.Env = launch.Env
	cmd.Dir = home
	out, err := cmd.CombinedOutput()
	if err != nil || calls.Load() == 0 || !strings.Contains(string(out), "local-grok-fixture") {
		t.Fatalf("Grok fixture failed: calls=%d err=%v\n%s", calls.Load(), err, out)
	}
}
