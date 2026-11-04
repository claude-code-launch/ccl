package codex_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/proxymanager"
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

// Provider switches keep the old loopback URL, but a chat retains its model.
// Exercise that exact boundary with the official client and fake credentials:
// the old model must not silently cross into the newly selected account.
func TestRealCodexRejectsGeminiRouteAfterWorkBuddySwitch(t *testing.T) {
	binary, executable := os.Getenv("CCL_TEST_BINARY"), os.Getenv("CCL_TEST_CODEX")
	if binary == "" || executable == "" {
		t.Skip("set CCL_TEST_BINARY and CCL_TEST_CODEX")
	}
	binary, _ = filepath.Abs(binary)
	executable, _ = filepath.Abs(executable)
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex-home")
	for key, value := range map[string]string{"HOME": home, "USERPROFILE": home, "CODEX_HOME": codexHome, "CLAUDE_CONFIG_DIR": filepath.Join(home, "claude-home"), "HTTP_PROXY": "http://127.0.0.1:9", "HTTPS_PROXY": "http://127.0.0.1:9", "ALL_PROXY": "http://127.0.0.1:9", "NO_PROXY": "127.0.0.1,localhost", "OPENAI_API_KEY": "fake-native-key", "CCL_LANG": "en", "CCL_PROXY_NO_MENUBAR": "1"} {
		t.Setenv(key, value)
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"OPENAI_API_KEY":"fake-native-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(`{"models":[{"slug":"native-fixture","input_modalities":["text"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var geminiCalls, workBuddyCalls atomic.Int32
	gemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		geminiCalls.Add(1)
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "model-a" || r.Header.Get("X-Api-Key") != "fake-gemini-key" {
			http.Error(w, "wrong Gemini fixture request", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_gemini", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "text", "text": "gemini-sandbox-done"}}, "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
	}))
	t.Cleanup(gemini.Close)
	geminiProvider := provider.Provider{Name: "gemini-fixture", Type: "anthropic", Endpoint: gemini.URL + "/v1", APIKey: "fake-gemini-key", Model: "model-a", CustomModelID: "model-a"}
	if err := config.Save(&provider.Config{ActiveProvider: geminiProvider.Name, Providers: map[string]provider.Provider{geminiProvider.Name: geminiProvider}}); err != nil {
		t.Fatal(err)
	}
	run := func(executable string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, args...)
		command.Dir = home
		process.ConfigureGroup(command)
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		err := command.Run()
		return output.String(), err
	}
	if output, err := run(binary, "codex", "start", "--independent", geminiProvider.Name); err != nil {
		t.Fatalf("start Gemini fixture: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		if output, err := run(binary, "codex", "stop"); err != nil {
			t.Errorf("stop fixture: %v\n%s", err, output)
		}
	})
	output, err := run(executable, "-m", "ccl/gemini-fixture/model-a", "-c", `web_search="disabled"`, "exec", "--json", "--skip-git-repo-check", "Reply to the local fixture.")
	if err != nil || !strings.Contains(output, "gemini-sandbox-done") || geminiCalls.Load() != 1 {
		t.Fatalf("Gemini fixture failed: %v calls=%d\n%s", err, geminiCalls.Load(), output)
	}
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
	if session == "" {
		t.Fatalf("Gemini fixture did not create a chat: %s", output)
	}
	manager := &proxymanager.Manager{Dir: filepath.Join(home, ".ccl", "proxy")}
	before, err := manager.Read()
	if err != nil || before == nil || before.Codex == nil {
		t.Fatalf("missing Gemini state: %v", err)
	}

	// Replace the daemon without another model request. Its WorkBuddy runtime
	// is a local protocol fixture, not the user's subscription or account.
	workBuddy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workBuddyCalls.Add(1)
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "model-b" || r.Header.Get("Authorization") != "Bearer fake-workbuddy-key" {
			http.Error(w, "wrong WorkBuddy fixture request", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		raw, _ := json.Marshal(map[string]any{"id": "chat_workbuddy", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "workbuddy-sandbox-done"}, "finish_reason": nil}}})
		fmt.Fprintf(w, "data: %s\n\n", raw)
		raw, _ = json.Marshal(map[string]any{"id": "chat_workbuddy", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
	}))
	t.Cleanup(workBuddy.Close)
	workBuddyProvider := provider.Provider{Name: "workbuddy-fixture", Type: "openai", Endpoint: workBuddy.URL + "/v1", APIKey: "fake-workbuddy-key", Model: "model-b", CustomModelID: "model-b"}
	if err := config.Update(func(cfg *provider.Config) error {
		cfg.ActiveProvider = workBuddyProvider.Name
		cfg.Providers[workBuddyProvider.Name] = workBuddyProvider
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if output, err := run(binary, "codex", "start", "--independent", workBuddyProvider.Name); err != nil {
		t.Fatalf("start WorkBuddy fixture: %v\n%s", err, output)
	}
	after, err := manager.Read()
	if err != nil || after == nil || after.Codex == nil {
		t.Fatalf("missing WorkBuddy state: %v", err)
	}
	if before.Codex.BaseURL != after.Codex.BaseURL {
		t.Fatalf("provider switch moved the cached endpoint: %s -> %s", before.Codex.BaseURL, after.Codex.BaseURL)
	}
	// The desktop resumes with its persisted model. Passing it explicitly also
	// prevents the CLI from replacing it with the new daemon's default model.
	output, err = run(executable, "-m", "ccl/gemini-fixture/model-a", "exec", "resume", "--json", "--skip-git-repo-check", session, "Continue on the old model.")
	if err == nil || !strings.Contains(output, "select a model from the active CCL provider") || workBuddyCalls.Load() != 0 || geminiCalls.Load() != 1 {
		t.Fatalf("stale Gemini chat after provider switch: %v workbuddy=%d gemini=%d\n%s", err, workBuddyCalls.Load(), geminiCalls.Load(), output)
	}
	output, err = run(executable, "-m", "ccl/workbuddy-fixture/model-b", "-c", `web_search="disabled"`, "exec", "--json", "--skip-git-repo-check", "Reply to the WorkBuddy fixture.")
	if err != nil || !strings.Contains(output, "workbuddy-sandbox-done") || workBuddyCalls.Load() != 1 {
		t.Fatalf("WorkBuddy fixture failed: %v calls=%d\n%s", err, workBuddyCalls.Load(), output)
	}
	if output, err := run(binary, "codex", "start", "--independent", geminiProvider.Name); err != nil {
		t.Fatalf("restore Gemini fixture: %v\n%s", err, output)
	}
	output, err = run(executable, "exec", "resume", "--json", "--skip-git-repo-check", session, "Continue after restoring Gemini.")
	if err != nil || !strings.Contains(output, "gemini-sandbox-done") || geminiCalls.Load() != 2 {
		t.Fatalf("restored Gemini chat failed: %v calls=%d\n%s", err, geminiCalls.Load(), output)
	}
}
