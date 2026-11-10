package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/protocol"
)

func isolatedCodexClient(t *testing.T) (string, func(...string) string) {
	t.Helper()
	executable := os.Getenv("CCL_TEST_CODEX")
	if executable == "" {
		t.Skip("set CCL_TEST_CODEX to the official Codex client")
	}
	executable, err := filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	for key, value := range map[string]string{"HOME": home, "USERPROFILE": home, "CODEX_HOME": home, "OPENAI_API_KEY": "fake-native-key", "HTTP_PROXY": "http://127.0.0.1:9", "HTTPS_PROXY": "http://127.0.0.1:9", "ALL_PROXY": "http://127.0.0.1:9", "NO_PROXY": "127.0.0.1,localhost"} {
		t.Setenv(key, value)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"OPENAI_API_KEY":"fake-native-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	native, _ := json.Marshal(Catalog{Models: []Model{newModel("native-fixture", "native-fixture", nil, nil)}})
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), native, 0o600); err != nil {
		t.Fatal(err)
	}
	return home, func(args ...string) string {
		t.Helper()
		command := process.Command(executable, args...)
		command.Dir = home
		process.ConfigureGroup(command)
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Codex: %v\n%s", err, output.String())
			}
		case <-time.After(30 * time.Second):
			process.KillTree(command)
			<-done
			t.Fatalf("Codex timed out\n%s", output.String())
		}
		return output.String()
	}
}

func applyClientFixture(t *testing.T, g *Gateway, home string) {
	t.Helper()
	overlay, err := Plan(context.Background(), g, filepath.Join(home, "proxy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := overlay.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRealCodexRetriesProviderCapacity(t *testing.T) {
	home, run := isolatedCodexClient(t)
	var calls atomic.Int32
	var rejectedAt atomic.Int64
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			rejectedAt.Store(time.Now().UnixNano())
			w.Header().Set("Retry-After", "2")
			writeJSON(w, 429, map[string]any{"type": "error", "error": map[string]any{"type": "rate_limit_error", "message": "Qoder model is temporarily resource limited (retry after 2s)"}})
			return
		}
		if elapsed := time.Since(time.Unix(0, rejectedAt.Load())); elapsed < 1900*time.Millisecond {
			t.Errorf("client retried before Retry-After: %s", elapsed)
		}
		writeChatText(w, r, "capacity-recovered")
	}, "openai", Options{Desktop: true})
	applyClientFixture(t, g, home)
	output := run("-m", "ccl/chosen/model-a", "-c", `web_search="disabled"`, "exec", "--json", "--skip-git-repo-check", "Reply to the fixture.")
	if calls.Load() != 2 || !strings.Contains(output, "capacity-recovered") {
		t.Fatalf("capacity rejection did not recover through the real client: calls=%d\n%s", calls.Load(), output)
	}
}

func TestRealCodexReadsScreenshotThroughConvertedProvider(t *testing.T) {
	home, run := isolatedCodexClient(t)
	path := filepath.Join(home, "screen.png")
	var screenshot bytes.Buffer
	if err := png.Encode(&screenshot, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, screenshot.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var sawImage atomic.Bool
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if calls.Add(1) == 1 {
			writeChatTool(w, r, "screen_call", findTool(t, body, "view_image"), map[string]any{"path": path})
			return
		}
		raw := textValue(body["messages"])
		sawImage.Store(strings.Contains(raw, `"type":"image_url"`) && strings.Contains(raw, "data:image/png;base64,"))
		writeChatText(w, r, "image-roundtrip-done")
	}, "openai", Options{Desktop: true})
	editFixtureModels(g, func([]Model) []Model {
		return []Model{newModel("ccl/chosen/model-a", "model-a", map[string]protocol.ModelInfo{"model-a": {InputModalities: []string{"text", "image"}}}, nil)}
	})
	applyClientFixture(t, g, home)
	output := run("-m", "ccl/chosen/model-a", "-c", `web_search="disabled"`, "exec", "--json", "--skip-git-repo-check", "Read the fixture screenshot with view_image.")
	if calls.Load() != 2 || !sawImage.Load() || !strings.Contains(output, "image-roundtrip-done") {
		t.Fatalf("screenshot lost: calls=%d image=%v\n%s", calls.Load(), sawImage.Load(), output)
	}
}

func TestRealCodexResumesAcrossNativeAndConvertedModels(t *testing.T) {
	for _, mode := range []string{"shared-provider", "independent-provider"} {
		t.Run(mode, func(t *testing.T) {
			home, run := isolatedCodexClient(t)
			var convertedCalls, nativeCalls atomic.Int32
			native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nativeCalls.Add(1)
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				for _, raw := range body["input"].([]any) {
					item := raw.(map[string]any)
					if item["type"] == "reasoning" && item["encrypted_content"] == nil {
						writeError(w, 404, "invalid_request_error", "unpersisted reasoning item not found", "")
						return
					}
				}
				if !strings.Contains(textValue(body["input"]), "keep-this-goal") {
					t.Error("native model lost the goal")
				}
				reasoning := map[string]any{"id": "rs_native_fixture", "type": "reasoning", "summary": []any{}, "encrypted_content": "opaque-native-fixture"}
				writeNativeFixture(w, []any{reasoning, migrationNoticeItem("native-continued")}, 3)
			}))
			t.Cleanup(native.Close)
			g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if convertedCalls.Add(1) == 1 {
					writeChatFixture(w, r, map[string]any{"reasoning_content": "fixture thinking", "content": "converted-first"}, "stop", 3)
					return
				}
				raw := textValue(body["messages"])
				if !strings.Contains(raw, "native-continued") || !strings.Contains(raw, "keep-this-goal") || strings.Contains(raw, "opaque-native-fixture") {
					t.Error("converted model did not resume native history correctly")
				}
				writeChatText(w, r, "converted-continued")
			}, "openai", Options{Desktop: true, OpenAIURL: native.URL, ChatGPTURL: native.URL})
			applyClientFixture(t, g, home)
			runTurn := func(args ...string) string {
				if mode == "independent-provider" {
					base, _ := json.Marshal(g.BaseURL())
					args = append([]string{"-c", `model_provider="ccl"`, "-c", `model_providers.ccl.name="CCL"`, "-c", "model_providers.ccl.base_url=" + string(base), "-c", `model_providers.ccl.wire_api="responses"`, "-c", "model_providers.ccl.requires_openai_auth=false", "-c", `model_providers.ccl.experimental_bearer_token="fake-native-key"`}, args...)
				}
				return run(args...)
			}
			output := runTurn("-m", "ccl/chosen/model-a", "-c", `web_search="disabled"`, "exec", "--json", "--skip-git-repo-check", "Remember keep-this-goal.")
			var session string
			for _, line := range strings.Split(output, "\n") {
				var event struct {
					Type string `json:"type"`
					ID   string `json:"thread_id"`
				}
				if json.Unmarshal([]byte(line), &event) == nil && event.Type == "thread.started" {
					session = event.ID
				}
			}
			if session == "" || !strings.Contains(output, "converted-first") {
				t.Fatalf("first turn failed: %s", output)
			}
			output = runTurn("-m", "native-fixture", "-c", `web_search="disabled"`, "exec", "resume", "--json", "--skip-git-repo-check", session, "Continue.")
			assertCodexThreadID(t, output, session)
			if !strings.Contains(output, "native-continued") {
				t.Fatalf("native turn failed: %s", output)
			}
			output = runTurn("-m", "ccl/chosen/model-a", "-c", `web_search="disabled"`, "exec", "resume", "--json", "--skip-git-repo-check", session, "Continue again.")
			assertCodexThreadID(t, output, session)
			if !strings.Contains(output, "converted-continued") || convertedCalls.Load() != 2 || nativeCalls.Load() != 1 {
				t.Fatalf("model switching failed: converted=%d native=%d\n%s", convertedCalls.Load(), nativeCalls.Load(), output)
			}
		})
	}
}

func assertCodexThreadID(t *testing.T, output, expected string) {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		var event struct {
			Type string `json:"type"`
			ID   string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "thread.started" {
			if event.ID != expected {
				t.Fatalf("provider switch changed thread ID: %s -> %s", expected, event.ID)
			}
			return
		}
	}
	t.Fatal("resume did not report a thread ID")
}
