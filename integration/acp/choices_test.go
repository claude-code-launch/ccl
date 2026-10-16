package acp_test

import (
	"bufio"
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

// `ccl acp --model/--effort` starts sessions on the chosen model and effort
// without the client selecting anything, through the real adapter.
func TestUpstreamACPSessionChoices(t *testing.T) {
	binary, adapter := os.Getenv("CCL_TEST_BINARY"), os.Getenv("CCL_TEST_AGENT_ACP")
	if binary == "" || adapter == "" {
		t.Skip("set CCL_TEST_BINARY and CCL_TEST_AGENT_ACP to run the real adapter")
	}
	home := filepath.Join(t.TempDir(), "ACP choices")
	project := filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "config"))
	t.Setenv("CCL_LANG", "en")
	t.Setenv("DISABLE_AUTOUPDATER", "1")
	requests := make(chan string, 5)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Model    string          `json:"model"`
			Thinking json.RawMessage `json:"thinking"`
		}
		_ = json.Unmarshal(body, &request)
		t.Logf("fixture %s model=%s thinking=%s", r.URL.Path, request.Model, request.Thinking)
		if len(body) > 0 && strings.Contains(r.URL.Path, "/v1/messages") {
			requests <- request.Model
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []struct{ name, data string }{
			{"message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`, request.Model)},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`},
			{"message_stop", `{"type":"message_stop"}`},
		} {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, event.data)
		}
	}))
	t.Cleanup(upstream.Close)
	a := provider.Provider{Name: "a", Type: "anthropic", Endpoint: upstream.URL, APIKey: "fake-key",
		Model: "fixture-a,fixture-other", CustomModelID: "fixture-a", SonnetModel: "fixture-a"}
	if err := config.Save(&provider.Config{ActiveProvider: "a", ACPProvider: "a", Providers: map[string]provider.Provider{"a": a}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, "acp", "--model", "fixture-other", "--effort", "max", "--adapter", adapter)
	cmd.Dir = project
	stderrPath := filepath.Join(home, "stderr.txt")
	stderr, _ := os.Create(stderrPath)
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
		case <-done:
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
	call := func(id int, method string, params any) map[string]json.RawMessage {
		t.Helper()
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(60 * time.Second)
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
			case <-deadline:
				data, _ := os.ReadFile(stderrPath)
				t.Fatalf("timeout on %s", method)
				_ = data
			}
		}
	}
	call(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	created := call(2, "session/new", map[string]any{"cwd": project, "mcpServers": []any{}})
	var options []struct {
		ID           string `json:"id"`
		CurrentValue string `json:"currentValue"`
	}
	if err := json.Unmarshal(created["configOptions"], &options); err != nil {
		t.Fatalf("configOptions: %s", created["configOptions"])
	}
	current := map[string]string{}
	for _, option := range options {
		current[option.ID] = option.CurrentValue
	}
	if current["model"] != "fixture-other" || current["effort"] != "max" {
		t.Fatalf("session options = %v, want model fixture-other and effort max", current)
	}
	var sessionID string
	_ = json.Unmarshal(created["sessionId"], &sessionID)
	if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{"sessionId": sessionID, "prompt": []map[string]string{{"type": "text", "text": "hi"}}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(60 * time.Second)
	for {
		select {
		case model := <-requests:
			if model != "fixture-other" {
				t.Fatalf("first turn hit %q, want fixture-other", model)
			}
			_ = stdin.Close()
			return
		case <-deadline:
			t.Fatal("no model request before timeout")
		}
	}
}
