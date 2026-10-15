package proxy_test

import (
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
	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/proxymanager"
)

// CCL_TEST_BINARY selects the native binary built by CI. With no binary this
// integration test skips; it never builds or starts a real Claude executable.
func TestProxyProcessLifecycle(t *testing.T) {
	source := os.Getenv("CCL_TEST_BINARY")
	if source == "" {
		t.Skip("set CCL_TEST_BINARY to a built native ccl binary")
	}
	source, err := filepath.Abs(source)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "test home \u6d4b\u8bd5")
	binDir := filepath.Join(home, "bin with spaces")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binDir, filepath.Base(source))
	if err := os.WriteFile(binary, data, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "Claude settings \u914d\u7f6e"))
	t.Setenv("CCL_LANG", "en")
	settingsPath := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected method", 405)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		if body["model"] != "model-a" {
			http.Error(w, "wrong model", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat/v1/chat/completions":
			if r.Header.Get("Authorization") != "Bearer fake-chat-key" {
				http.Error(w, "wrong chat auth", 401)
				return
			}
			_, _ = io.WriteString(w, `{"id":"chat-fake","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"chat-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
		case "/native/v1/messages":
			if r.Header.Get("Authorization") != "Bearer fake-native-key" || r.Header.Get("X-Api-Key") != "" {
				http.Error(w, "wrong native auth", 401)
				return
			}
			_, _ = io.WriteString(w, `{"id":"message-fake","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"native-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
		default:
			http.Error(w, "unexpected route", 404)
		}
	}))
	t.Cleanup(upstream.Close)
	chat := provider.Provider{Name: "chat-fixture", Type: "openai", Endpoint: upstream.URL + "/chat/v1", APIKey: "fake-chat-key", Model: "model-a,model-b", SonnetModel: "model-a", CustomModelID: "model-a"}
	native := provider.Provider{Name: "native-fixture", Type: "anthropic", Endpoint: upstream.URL + "/native/v1", APIKey: "fake-native-key", Model: chat.Model, CustomModelID: "model-a", AnthropicAuth: "bearer"}
	if err := config.Save(&provider.Config{ActiveProvider: chat.Name, Providers: map[string]provider.Provider{chat.Name: chat, native.Name: native}}); err != nil {
		t.Fatal(err)
	}
	writeObject(t, settingsPath, map[string]any{"model": "original-model", "env": map[string]string{"ANTHROPIC_BASE_URL": "https://original.invalid", "KEEP": "original"}})
	manager := &proxymanager.Manager{Dir: filepath.Join(home, ".ccl", "proxy")}
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, args...)
		output, err := command.CombinedOutput()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return string(output), err
	}
	cli := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("ccl %v: %v\n%s", args, err, out)
		}
		return out
	}
	t.Cleanup(func() {
		if out, err := run("proxy", "stop"); err != nil {
			t.Errorf("cleanup stop: %v\n%s", err, out)
		}
	})
	state := func() *proxymanager.State {
		t.Helper()
		s, err := manager.Read()
		if err != nil || s == nil {
			t.Fatalf("read running proxy: %v", err)
		}
		return s
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	request := func(url, token string) (int, []byte) {
		t.Helper()
		r, err := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(`{"model":"model-a","max_tokens":8,"messages":[{"role":"user","content":"test"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}
	inference := func(want string) {
		t.Helper()
		fields := readObject(t, settingsPath)
		var env map[string]string
		if err := json.Unmarshal(fields["env"], &env); err != nil {
			t.Fatal(err)
		}
		status, body := request(env["ANTHROPIC_BASE_URL"], env["ANTHROPIC_AUTH_TOKEN"])
		if status != 200 || !bytes.Contains(body, []byte(`"text":"`+want+`"`)) {
			t.Fatalf("inference status=%d body=%s", status, body)
		}
	}

	// The launching CLI exits before inference: this exercises actual detached
	// child processes, Windows creation flags and paths with spaces/Unicode.
	cli("proxy", "-p", chat.Name)
	first := state()
	if first.Provider != chat.Name || first.PID <= 0 || !manager.Running(context.Background(), first) {
		t.Fatal("background proxy not running")
	}
	inference("chat-ok")
	if status, _ := request(first.BaseURL, ""); status != 401 {
		t.Fatalf("unauthenticated inference status=%d", status)
	}
	fields := readObject(t, settingsPath)
	var env map[string]string
	_ = json.Unmarshal(fields["env"], &env)
	status := cli("proxy", "status")
	for _, secret := range []string{chat.APIKey, native.APIKey, env["ANTHROPIC_AUTH_TOKEN"], first.ControlToken} {
		if strings.Contains(status, secret) {
			t.Fatal("status disclosed a credential")
		}
	}
	cli("proxy", "start", chat.Name)
	if state().ID != first.ID {
		t.Fatal("identical start did not reuse proxy")
	}
	concurrent := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			out, err := run("proxy", "-p", chat.Name)
			if err != nil {
				err = fmt.Errorf("%w: %s", err, out)
			}
			concurrent <- err
		}()
	}
	for i := 0; i < 3; i++ {
		if err := <-concurrent; err != nil {
			t.Fatal(err)
		}
	}
	if state().ID != first.ID {
		t.Fatal("concurrent starts created another proxy")
	}
	t.Log("background start, authentication, status and concurrent starts passed")

	if err := config.Update(func(cfg *provider.Config) error {
		p := cfg.Providers[chat.Name]
		p.SonnetModel = "model-b"
		cfg.Providers[chat.Name] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cli("proxy", "start", "-p", chat.Name)
	if state().ID == first.ID {
		t.Fatal("updated provider did not reload")
	}
	fields = readObject(t, settingsPath)
	_ = json.Unmarshal(fields["env"], &env)
	if env["ANTHROPIC_DEFAULT_SONNET_MODEL"] != "model-b" {
		t.Fatal("updated mapping not applied")
	}
	cli("use", native.Name)
	cli("proxy")
	if state().Provider != native.Name {
		t.Fatal("active provider not used for switching")
	}
	inference("native-ok")
	fields = readObject(t, settingsPath)
	_ = json.Unmarshal(fields["env"], &env)
	env["USER_ADDED"] = "keep"
	fields["env"], _ = json.Marshal(env)
	fields["permissions"] = json.RawMessage(`{"allow":["Read"]}`)
	writeObject(t, settingsPath, fields)
	cli("proxy", "stop")
	assertRestored(t, settingsPath)
	if s, err := manager.Read(); err != nil || s != nil {
		t.Fatalf("state retained after stop: %v", err)
	}
	cli("proxy", "stop")
	t.Log("provider reload, switching, direct settings access and restoration passed")

	cli("proxy", "-p", chat.Name)
	crashed := state()
	process, err := os.FindProcess(crashed.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		_ = process.Release()
		t.Fatal(err)
	}
	_ = process.Release()
	deadline := time.Now().Add(5 * time.Second)
	for manager.Running(context.Background(), crashed) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if out := cli("proxy", "status"); !strings.Contains(out, "not responding") {
		t.Fatalf("crash not detected: %s", out)
	}
	// Any other ccl command undoes the dead proxy's overlay on its way in.
	if out := cli("ls"); !strings.Contains(out, "previous Claude settings restored") {
		t.Fatalf("stale overlay not recovered: %s", out)
	}
	assertRestored(t, settingsPath)
	if s, err := manager.Read(); err != nil || s != nil {
		t.Fatalf("journal retained after recovery: %v", err)
	}
	cli("proxy", "stop")
	t.Log("forced process termination and automatic recovery passed")

	broken := []byte("{broken settings")
	if err := os.WriteFile(settingsPath, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run("proxy", "-p", chat.Name); err == nil {
		t.Fatalf("malformed settings accepted: %s", out)
	}
	if actual, err := os.ReadFile(settingsPath); err != nil || !bytes.Equal(actual, broken) {
		t.Fatal("malformed settings overwritten")
	}
	if s, err := manager.Read(); err != nil || s != nil {
		t.Fatalf("failed start left state: %v", err)
	}
	t.Log("startup failure retains malformed settings and leaves no running proxy")
}

func readObject(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func writeObject(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFileAtomic(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertRestored(t *testing.T, path string) {
	t.Helper()
	fields := readObject(t, path)
	var model string
	if err := json.Unmarshal(fields["model"], &model); err != nil || model != "original-model" {
		t.Fatal("original model not restored")
	}
	var env map[string]string
	if err := json.Unmarshal(fields["env"], &env); err != nil {
		t.Fatal(err)
	}
	if env["ANTHROPIC_BASE_URL"] != "https://original.invalid" || env["KEEP"] != "original" || env["USER_ADDED"] != "keep" {
		t.Fatal("original settings or user edits lost")
	}
	if _, exists := env["ANTHROPIC_AUTH_TOKEN"]; exists {
		t.Fatal("local credential not removed")
	}
	if !bytes.Contains(fields["permissions"], []byte(`"Read"`)) {
		t.Fatal("user permissions edit lost")
	}
}
