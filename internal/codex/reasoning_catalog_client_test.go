package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// Check the app-server API used by the desktop picker, including independent
// mode without any OpenAI login. CLI effort overrides alone cannot verify it.
func TestRealCodexModelListRetainsPerModelEfforts(t *testing.T) {
	for _, mode := range []string{"shared", "independent"} {
		t.Run(mode, func(t *testing.T) {
			home, _ := isolatedCodexClient(t)
			independent := mode == "independent"
			if independent {
				t.Setenv("OPENAI_API_KEY", "")
				if err := os.Remove(filepath.Join(home, "auth.json")); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile("../modelsdev/modelsdev.json")
			if err != nil {
				t.Fatal(err)
			}
			cache, err := modelsdev.CachePath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(cache), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cache, data, 0600); err != nil {
				t.Fatal(err)
			}
			models := []string{"hy4-preview", "deepseek-v4.1-flash", "space-bunny"}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("model/list triggered inference")
				http.Error(w, "unexpected inference", 500)
			}))
			defer upstream.Close()
			runtime, err := oauthproxy.StartOpenAIChatAPI(t.Context(), upstream.URL, "fake-provider-key", strings.Join(models, ","))
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Stop()
			gateway, err := New(t.Context(), runtime, provider.Provider{Name: "chosen", Type: "openai", OAuthProvider: "workbuddycn"}, Options{Desktop: !independent, Independent: independent})
			if err != nil {
				t.Fatal(err)
			}
			defer gateway.Close()
			applyClientFixture(t, gateway, home)
			result := readClientModelList(t, home)
			want := map[string][]string{
				"hy4-preview":         {"high"},
				"deepseek-v4.1-flash": {"low", "high", "max"},
				"space-bunny":         {"low", "medium", "high", "xhigh", "max"},
			}
			for id, efforts := range want {
				alias := "ccl/chosen/" + id
				model := slices.IndexFunc(result, func(model clientReasoningModel) bool { return model.Model == alias })
				if model < 0 {
					t.Fatalf("app-server omitted %s", alias)
				}
				var actual []string
				for _, level := range result[model].Efforts {
					actual = append(actual, level.Effort)
				}
				if !slices.Equal(actual, efforts) {
					t.Errorf("%s: app-server efforts=%v, want %v", alias, actual, efforts)
				}
			}
		})
	}
}

type clientReasoningModel struct {
	Model   string `json:"model"`
	Efforts []struct {
		Effort string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
}

func readClientModelList(t *testing.T, home string) []clientReasoningModel {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Getenv("CCL_TEST_CODEX"), "-c", "features.code_mode_host=true", "app-server")
	command.Dir = home
	process.ConfigureGroup(command)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { process.KillTree(command); _ = command.Wait() }()
	encoder := json.NewEncoder(stdin)
	for _, request := range []map[string]any{
		{"id": 0, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "ccl-fixture", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}},
		{"method": "initialized"},
		{"id": 1, "method": "model/list", "params": map[string]any{"includeHidden": true, "cursor": nil, "limit": 100}},
	} {
		if err := encoder.Encode(request); err != nil {
			t.Fatal(err)
		}
	}
	response := make(chan json.RawMessage, 1)
	go func() {
		decoder := json.NewDecoder(stdout)
		for {
			var event struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			if decoder.Decode(&event) != nil {
				response <- nil
				return
			}
			if event.ID != nil && *event.ID == 1 {
				response <- event.Result
				return
			}
		}
	}()
	select {
	case raw := <-response:
		var result struct {
			Data []clientReasoningModel `json:"data"`
		}
		if err := json.Unmarshal(raw, &result); err != nil || len(result.Data) == 0 {
			t.Fatalf("model/list failed: %v result=%s", err, raw)
		}
		return result.Data
	case <-ctx.Done():
		t.Fatal("model/list timed out")
		return nil
	}
}
