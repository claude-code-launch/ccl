package claudeaccount_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/claudeaccount"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// The real adapter starts a native subscription session from an isolated fake
// profile. A cross-account change must fail before any prompt is dispatched.
func TestRealACPClaudeAccountBoundary(t *testing.T) {
	binary, adapter := os.Getenv("CCL_TEST_BINARY"), os.Getenv("CCL_TEST_AGENT_ACP")
	if binary == "" || adapter == "" {
		t.Skip("set CCL_TEST_BINARY and CCL_TEST_AGENT_ACP")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "original"))
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, "http://127.0.0.1:9")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")
	t.Setenv("DISABLE_AUTOUPDATER", "1")
	store, _ := claudeaccount.Open()
	providers := map[string]provider.Provider{}
	for _, name := range []string{"work", "personal"} {
		candidate, err := store.PrepareNew(name)
		if err != nil {
			t.Fatal(err)
		}
		credential := map[string]any{"claudeAiOauth": map[string]any{
			"accessToken": "fake-" + name, "refreshToken": "fake-refresh", "expiresAt": int64(4102444800000), "scopes": []string{"user:inference"}, "subscriptionType": "max",
		}}
		profile := map[string]any{"hasCompletedOnboarding": true, "oauthAccount": map[string]string{"accountUuid": name, "emailAddress": name + "@test.invalid", "organizationUuid": "fake-org"}}
		for file, value := range map[string]any{".credentials.json": credential, ".claude.json": profile} {
			data, _ := json.Marshal(value)
			if err := os.WriteFile(filepath.Join(candidate.Path, file), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		providers[name] = provider.Provider{Name: name, Type: "anthropic", Endpoint: "oauth://claude", OAuthProvider: "claude", OAuthAccountCredential: candidate.ID()}
	}
	if err := config.Save(&provider.Config{ActiveProvider: "default", DefaultClaudeProvider: "work", Providers: providers}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, "acp", "--adapter", adapter)
	cmd.Dir = home
	stderrPath := filepath.Join(home, "stderr.txt")
	stderr, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stderr.Close() })
	cmd.Stderr = stderr
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = in.Close()
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
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 65536), 16<<20)
		for scanner.Scan() {
			var frame map[string]json.RawMessage
			if json.Unmarshal(scanner.Bytes(), &frame) == nil {
				frames <- frame
			}
		}
	}()
	encoder := json.NewEncoder(in)
	call := func(id int, method string, params any) map[string]json.RawMessage {
		t.Helper()
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(20 * time.Second)
		for {
			select {
			case frame, open := <-frames:
				if !open {
					logs, _ := os.ReadFile(stderrPath)
					t.Fatalf("adapter EOF: %s", logs)
				}
				var got int
				if json.Unmarshal(frame["id"], &got) == nil && got == id {
					return frame
				}
			case <-deadline:
				logs, _ := os.ReadFile(stderrPath)
				t.Fatalf("timeout on %s: %s", method, logs)
			}
		}
	}
	if frame := call(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); len(frame["error"]) > 0 {
		t.Fatalf("initialize: %s", frame["error"])
	}
	created := call(2, "session/new", map[string]any{"cwd": home, "mcpServers": []any{}})
	if len(created["error"]) > 0 {
		t.Fatalf("new session: %s", created["error"])
	}
	var session struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(created["result"], &session)
	if session.SessionID == "" {
		t.Fatal("missing native session id")
	}
	conflictingProject := filepath.Join(home, "other-project")
	if err := os.MkdirAll(filepath.Join(conflictingProject, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conflictingProject, ".claude", "settings.json"), []byte(`{"apiKeyHelper":"echo fake-helper"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	refusedProject := call(20, "session/new", map[string]any{"cwd": conflictingProject, "mcpServers": []any{}})
	if !strings.Contains(string(refusedProject["error"]), "conflicts with apiKeyHelper") {
		t.Fatalf("conflicting ACP project was accepted: %s", refusedProject)
	}
	if err := config.Update(func(c *provider.Config) error { c.DefaultClaudeProvider = "personal"; return nil }); err != nil {
		t.Fatal(err)
	}
	refused := call(3, "session/prompt", map[string]any{"sessionId": session.SessionID, "prompt": []map[string]string{{"type": "text", "text": "must not reach inference"}}})
	if !bytes.Contains(refused["error"], []byte("restart ccl acp")) {
		t.Fatalf("cross-account prompt was not refused: %s", refused)
	}
	call(4, "session/close", map[string]string{"sessionId": session.SessionID})
}
