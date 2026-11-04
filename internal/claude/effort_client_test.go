package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestRealClaudeModelMenusUsePerModelEffortCapabilities(t *testing.T) {
	executable, sdk := os.Getenv("CCL_TEST_CLAUDE"), os.Getenv("CCL_TEST_CLAUDE_SDK")
	if executable == "" || sdk == "" {
		t.Skip("set CCL_TEST_CLAUDE and CCL_TEST_CLAUDE_SDK for the isolated client check")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for the official client SDK")
	}
	seedClaudeEffortMetadata(t)
	var inference atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			inference.Add(1)
			http.Error(w, "inference is forbidden in the effort menu check", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
	}))
	defer server.Close()
	c := providerContext{
		provider: provider.Provider{
			Type: "anthropic", Endpoint: server.URL, APIKey: "fake-local-only", StatuslineDisabled: true,
			ModelsDevProvider: "fixture", Model: "acme/standard[1m],acme/deep,acme/plain,unknown-model",
			OpusModel: "acme/standard[1m]", HaikuModel: "acme/plain", CustomModelID: "acme/deep",
		},
		baseURL: server.URL, modelNames: map[string]string{"acme/deep": "Deep Model"},
	}
	settings := c.settingsForCWD(t.TempDir())
	raw, _ := json.Marshal(settings)
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settingsPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "../../integration/modelpicker/effort.mjs", sdk, settingsPath)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_") || key == "CLAUDECODE" {
			continue
		}
		command.Env = append(command.Env, value)
	}
	command.Env = append(command.Env, "CLAUDE_CONFIG_DIR="+os.Getenv("CLAUDE_CONFIG_DIR"),
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1",
		"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9", "ALL_PROXY=http://127.0.0.1:9", "NO_PROXY=127.0.0.1,localhost")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("client menu initialization: %v\n%s", err, output)
	}
	var models []struct {
		Value                 string   `json:"value"`
		SupportsEffort        bool     `json:"supportsEffort"`
		SupportedEffortLevels []string `json:"supportedEffortLevels"`
	}
	if err := json.Unmarshal(output, &models); err != nil {
		t.Fatalf("decode client menu: %v\n%s", err, output)
	}
	for model, want := range map[string][]string{
		"opus":       {"low", "medium", "high"},
		"Deep Model": {"low", "medium", "high", "xhigh", "max"},
		"haiku":      nil,
	} {
		found := false
		for _, row := range models {
			if row.Value == model {
				found = true
				if row.SupportsEffort != (len(want) > 0) || !slices.Equal(row.SupportedEffortLevels, want) {
					t.Fatalf("model %s: effort=%t levels=%v, want %v", model, row.SupportsEffort, row.SupportedEffortLevels, want)
				}
			}
		}
		if !found {
			t.Fatalf("model %s missing from real client menu", model)
		}
	}
	if inference.Load() != 0 {
		t.Fatal("menu initialization attempted inference")
	}
}
