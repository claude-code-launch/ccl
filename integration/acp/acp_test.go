package acp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// This runs the real upstream adapter and its bundled Claude SDK. Every model
// request goes to the local fixture; no account or installed Claude is used.
func TestUpstreamACPConfiguration(t *testing.T) {
	binary, adapter := os.Getenv("CCL_TEST_BINARY"), os.Getenv("CCL_TEST_AGENT_ACP")
	if binary == "" || adapter == "" {
		t.Skip("set CCL_TEST_BINARY and CCL_TEST_AGENT_ACP to run the real adapter")
	}
	home := filepath.Join(t.TempDir(), "ACP user \u6d4b\u8bd5")
	project := filepath.Join(home, "project with spaces")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "Claude settings \u914d\u7f6e"))
	t.Setenv("CCL_LANG", "en")
	t.Setenv("DISABLE_AUTOUPDATER", "1")
	t.Setenv("ANTHROPIC_MODEL", "stale-shell-model")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "stale-shell-token")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	requests := make(chan string, 20)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			_, _ = io.WriteString(w, `{"input_tokens":2}`)
			return
		}
		if r.Method != http.MethodPost {
			_, _ = io.WriteString(w, `{"data":[],"has_more":false}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &request)
		t.Logf("local fixture %s model=%s", r.URL.Path, request.Model)
		requests <- request.Model
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(r.URL.Path, "chat/completions") {
			if r.Header.Get("Authorization") != "Bearer fake-b-key" {
				http.Error(w, "wrong provider B credential", 401)
				return
			}
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"chat-fixture\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"provider-b-ok\"},\"finish_reason\":null}]}\n\n", request.Model)
			_, _ = io.WriteString(w, "data: {\"id\":\"chat-fixture\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
			return
		}
		if r.Header.Get("X-Api-Key") != "fake-a-key" {
			http.Error(w, "wrong provider A credential", 401)
			return
		}
		for _, event := range []struct{ name, data string }{
			{"message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg-fixture","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"usage":{"input_tokens":2,"output_tokens":0}}}`, request.Model)},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"provider-a-ok"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`},
			{"message_stop", `{"type":"message_stop"}`},
		} {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, event.data)
		}
	}))
	t.Cleanup(upstream.Close)
	a := provider.Provider{Name: "a", Type: "anthropic", Endpoint: upstream.URL, APIKey: "fake-a-key",
		Model: "fixture-a,fixture-other", CustomModelID: "fixture-a", SonnetModel: "fixture-a"}
	b := provider.Provider{Name: "b", Type: "openai", Endpoint: upstream.URL + "/chat/v1", APIKey: "fake-b-key",
		Model: "fixture-b,fixture-third", CustomModelID: "fixture-b", SonnetModel: "fixture-b"}
	if err := config.Save(&provider.Config{ActiveProvider: "a", ACPProvider: "a", Providers: map[string]provider.Provider{"a": a, "b": b}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, "acp", "--adapter", adapter)
	cmd.Dir = project
	stderrPath := filepath.Join(home, "adapter-stderr.txt")
	stderr, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stderr.Close() })
	cmd.Stderr = stderr
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case err := <-done:
			if err != nil && !t.Failed() {
				t.Errorf("adapter exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("adapter did not exit on EOF")
		}
	})
	frames := make(chan map[string]json.RawMessage, 200)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 65536), 16<<20)
		for scanner.Scan() {
			var frame map[string]json.RawMessage
			if json.Unmarshal(scanner.Bytes(), &frame) == nil {
				frames <- frame
			}
		}
	}()
	enc := json.NewEncoder(stdin)
	var notifications []byte
	call := func(id int, method string, params any) map[string]json.RawMessage {
		t.Helper()
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(30 * time.Second)
		for {
			select {
			case frame, open := <-frames:
				if !open {
					data, _ := os.ReadFile(stderrPath)
					t.Fatalf("adapter EOF on %s: %s", method, data)
				}
				if string(frame["id"]) == fmt.Sprint(id) {
					if len(frame["error"]) > 0 {
						data, _ := os.ReadFile(stderrPath)
						t.Fatalf("%s: %s\n%s", method, frame["error"], data)
					}
					var result map[string]json.RawMessage
					_ = json.Unmarshal(frame["result"], &result)
					return result
				}
				notifications = append(notifications, frame["params"]...)
			case <-deadline:
				data, _ := os.ReadFile(stderrPath)
				t.Fatalf("timeout on %s: %s\nnotifications=%s", method, data, notifications)
			}
		}
	}
	initialized := call(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{"terminal": true}})
	if !bytes.Contains(initialized["agentInfo"], []byte("claude-agent-acp")) || !bytes.Contains(initialized["agentCapabilities"], []byte(`"sse":true`)) {
		t.Fatalf("CCL did not expose the real adapter capabilities: %s", initialized)
	}
	created := call(2, "session/new", map[string]any{"cwd": project, "mcpServers": []any{}, "_meta": map[string]any{"client-marker": "retained"}})
	var sessionID string
	_ = json.Unmarshal(created["sessionId"], &sessionID)
	if sessionID == "" || !bytes.Contains(created["configOptions"], []byte("fixture-other")) {
		t.Fatalf("upstream session or unmapped model missing: %s", created)
	}
	call(3, "session/set_config_option", map[string]any{"sessionId": sessionID, "configId": "model", "value": "fixture-other"})
	prompt := func(id int) {
		call(id, "session/prompt", map[string]any{"sessionId": sessionID, "prompt": []map[string]string{{"type": "text", "text": "local fixture check"}}})
	}
	prompt(4)
	if model := <-requests; model != "fixture-other" {
		t.Fatalf("ACP model selection reached %q", model)
	}
	if err := config.Update(func(cfg *provider.Config) error { cfg.ACPProvider = "b"; return nil }); err != nil {
		t.Fatal(err)
	}
	prompt(5)
	if model := <-requests; model != "fixture-b" {
		t.Fatalf("provider switch reached %q, want fixture-b", model)
	}
	if !bytes.Contains(notifications, []byte("provider-a-ok")) || !bytes.Contains(notifications, []byte("provider-b-ok")) {
		t.Fatalf("upstream streaming updates missing: %s", notifications)
	}
	call(6, "session/close", map[string]any{"sessionId": sessionID})
	log, _ := os.ReadFile(stderrPath)
	if bytes.Contains(log, []byte("fake-a-key")) || bytes.Contains(log, []byte("fake-b-key")) {
		t.Fatal("upstream diagnostics exposed provider credentials")
	}
	t.Log("real adapter capabilities, model selection, streaming, config refresh and close passed")
}
