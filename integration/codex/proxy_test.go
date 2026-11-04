package codex_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// The real Codex reads the exact config written by ccl codex start. The native
// login is fake, model requests can reach only the local provider fixture, and
// the command tool runs inside Codex itself rather than an emulated client.
func TestRealCodexUsesStandaloneProxyAndLocalToolLoop(t *testing.T) {
	binary, codex := os.Getenv("CCL_TEST_BINARY"), os.Getenv("CCL_TEST_CODEX")
	if binary == "" || codex == "" {
		t.Skip("set CCL_TEST_BINARY and CCL_TEST_CODEX")
	}
	binary, _ = filepath.Abs(binary)
	codex, _ = filepath.Abs(codex)
	for _, protocol := range []string{"anthropic", "openai"} {
		t.Run(protocol, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "user home \u6d4b\u8bd5")
			codexHome := filepath.Join(home, "Codex config")
			workspace := filepath.Join(home, "workspace")
			for _, directory := range []string{codexHome, workspace} {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for key, value := range map[string]string{"HOME": home, "USERPROFILE": home, "CODEX_HOME": codexHome, "CLAUDE_CONFIG_DIR": filepath.Join(home, "Claude"), "HTTP_PROXY": "http://127.0.0.1:9", "HTTPS_PROXY": "http://127.0.0.1:9", "ALL_PROXY": "http://127.0.0.1:9", "NO_PROXY": "127.0.0.1,localhost", "OPENAI_API_KEY": "fake-native-key", "CCL_LANG": "en", "CCL_PROXY_NO_MENUBAR": "1"} {
				t.Setenv(key, value)
			}
			configPath := filepath.Join(codexHome, "config.toml")
			if err := os.WriteFile(configPath, []byte("# native preferences\nmodel = 'native-fixture'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			auth := []byte(`{"OPENAI_API_KEY":"fake-native-key"}`)
			if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), auth, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(`{"models":[{"slug":"native-fixture","display_name":"Native fixture","description":"Local test","default_reasoning_level":null,"supported_reasoning_levels":[],"shell_type":"default","visibility":"list","supported_in_api":true,"priority":0,"truncation_policy":{"mode":"tokens","limit":10000},"context_window":32768,"input_modalities":["text"],"base_instructions":"You are a coding assistant.","support_verbosity":false,"experimental_supported_tools":[]}]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			var sawResult atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					http.Error(w, "bad request", 400)
					return
				}
				if body["model"] != "model-a" {
					t.Error("Codex alias reached provider")
					http.Error(w, "wrong model", 400)
					return
				}
				if protocol == "anthropic" {
					if r.Header.Get("X-Api-Key") != "fake-provider-key" || r.Header.Get("Authorization") != "" {
						t.Error("native credential reached provider")
					}
				} else {
					if r.Header.Get("Authorization") != "Bearer fake-provider-key" {
						t.Error("wrong Chat credentials")
					}
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("Chatgpt-Account-Id") != "" {
					t.Error("native identity reached provider")
				}
				var name string
				var names []string
				if tools, ok := body["tools"].([]any); ok {
					for _, entry := range tools {
						tool, _ := entry.(map[string]any)
						if function, ok := tool["function"].(map[string]any); ok {
							tool = function
						}
						candidate, _ := tool["name"].(string)
						names = append(names, candidate)
						if strings.Contains(candidate, "exec_command") {
							name = candidate
							break
						}
					}
				}
				first := calls.Add(1) == 1
				if first && name == "" {
					t.Errorf("Codex did not offer its command tool: %v", names)
					http.Error(w, "missing command tool", 400)
					return
				}
				if !first {
					raw, _ := json.Marshal(body["messages"])
					if strings.Contains(string(raw), "call_local_exec") && strings.Contains(string(raw), "codex-tool-fixture") && (strings.Contains(string(raw), `"type":"tool_result"`) || strings.Contains(string(raw), `"role":"tool"`)) {
						sawResult.Store(true)
					}
				}
				arguments := map[string]any{"cmd": "echo codex-tool-fixture", "max_output_tokens": 1000}
				if protocol == "anthropic" {
					content := []any{map[string]any{"type": "text", "text": "codex-fixture-done"}}
					stop := "end_turn"
					if first {
						content = []any{map[string]any{"type": "tool_use", "id": "call_local_exec", "name": name, "input": arguments}}
						stop = "tool_use"
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_local", "type": "message", "role": "assistant", "content": content, "stop_reason": stop, "usage": map[string]any{"input_tokens": 10, "output_tokens": 3}})
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					delta := map[string]any{"role": "assistant", "content": "codex-fixture-done"}
					finish := "stop"
					if first {
						raw, _ := json.Marshal(arguments)
						delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_local_exec", "type": "function", "function": map[string]any{"name": name, "arguments": string(raw)}}}}
						finish = "tool_calls"
					}
					raw, _ := json.Marshal(map[string]any{"id": "chat_local", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
					fmt.Fprintf(w, "data: %s\n\n", raw)
					raw, _ = json.Marshal(map[string]any{"id": "chat_local", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}})
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
				}
			}))
			t.Cleanup(upstream.Close)
			p := provider.Provider{Name: "chosen", Type: protocol, Endpoint: upstream.URL + "/v1", APIKey: "fake-provider-key", Model: "model-a", CustomModelID: "model-a", SonnetModel: "model-a"}
			if err := config.Save(&provider.Config{ActiveProvider: p.Name, Providers: map[string]provider.Provider{p.Name: p}}); err != nil {
				t.Fatal(err)
			}
			run := func(executable string, args ...string) (string, error) {
				command := process.Command(executable, args...)
				command.Dir = workspace
				process.ConfigureGroup(command)
				var output bytes.Buffer
				command.Stdout, command.Stderr = &output, &output
				if err := command.Start(); err != nil {
					return "", err
				}
				done := make(chan error, 1)
				go func() { done <- command.Wait() }()
				select {
				case err := <-done:
					return output.String(), err
				case <-time.After(45 * time.Second):
					process.KillTree(command)
					<-done
					return output.String(), fmt.Errorf("client timed out")
				}
			}
			if output, err := run(binary, "codex", "start"); err != nil {
				t.Fatalf("proxy start: %v\n%s", err, output)
			}
			t.Cleanup(func() {
				if output, err := run(binary, "codex", "stop"); err != nil {
					t.Errorf("proxy cleanup: %v %s", err, output)
				}
			})
			output, err := run(codex, "-m", "ccl/chosen/model-a", "-c", `web_search="disabled"`, "-c", `sandbox_mode="danger-full-access"`, "exec", "--json", "--skip-git-repo-check", "Use exec_command to echo codex-tool-fixture, then answer codex-fixture-done.")
			if err != nil || !strings.Contains(output, "codex-fixture-done") || calls.Load() < 2 || !sawResult.Load() {
				t.Fatalf("real Codex tool loop: %v calls=%d result=%v\n%s", err, calls.Load(), sawResult.Load(), output)
			}
			if output, err := run(binary, "codex", "stop"); err != nil {
				t.Fatalf("proxy stop: %v %s", err, output)
			}
			data, _ := os.ReadFile(configPath)
			if strings.Contains(string(data), "openai_base_url") || !strings.Contains(string(data), "# native preferences") {
				t.Fatalf("Codex config not restored: %s", data)
			}
			data, _ = os.ReadFile(filepath.Join(codexHome, "auth.json"))
			if !bytes.Equal(data, auth) {
				t.Fatal("native credentials were modified")
			}
		})
	}
}
