package proxymanager

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func isolatedManager(t *testing.T) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	locale.SetLanguage("en")
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return &Manager{Dir: filepath.Join(home, ".ccl", "proxy")}, path
}

func awaitRunning(t *testing.T, m *Manager) *State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s, err := m.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.Running(context.Background(), s) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("proxy did not become ready")
	return nil
}

func TestStandaloneProxyRoutesAndRestoresUserEdits(t *testing.T) {
	for _, auth := range []string{"", "bearer"} {
		t.Run("anthropic-"+auth, func(t *testing.T) {
			m, path := isolatedManager(t)
			original := map[string]any{"env": map[string]string{"ANTHROPIC_BASE_URL": "https://original.invalid", "ANTHROPIC_AUTH_TOKEN": "original-token", "KEEP": "old"}, "model": "original-model", "permissions": map[string]any{"allow": []string{"Read"}}}
			if err := writeJSON(path, original); err != nil {
				t.Fatal(err)
			}
			requests := make(chan string, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := r.Header.Get("X-Api-Key")
				if auth == "bearer" {
					key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				}
				if key != "upstream-fake-key" {
					http.Error(w, "wrong upstream key", 401)
					return
				}
				if (auth == "bearer" && r.Header.Get("X-Api-Key") != "") || (auth == "" && r.Header.Get("Authorization") != "") {
					http.Error(w, "leaked local auth", 400)
					return
				}
				body, _ := io.ReadAll(r.Body)
				requests <- r.URL.Path + " " + string(body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_test","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
			}))
			defer upstream.Close()
			p := provider.Provider{Name: "provider1", Type: "anthropic", Endpoint: upstream.URL + "/v1", APIKey: "upstream-fake-key", Model: "model-a", CustomModelID: "model-a", AnthropicAuth: auth}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- m.Run(ctx, p, path, "test-instance"); close(done) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
				}
			})
			s := awaitRunning(t, m)
			settings, err := readSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			env, err := settingsEnv(settings)
			if err != nil {
				t.Fatal(err)
			}
			var token string
			if err := json.Unmarshal(env["ANTHROPIC_AUTH_TOKEN"], &token); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			if bytes.Contains(data, []byte(p.APIKey)) {
				t.Fatal("upstream key leaked into Claude settings")
			}
			// The management socket and inference socket both require local auth.
			response, err := http.Get(s.ControlURL + "/health")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 401 {
				t.Fatalf("unauthenticated control status=%d", response.StatusCode)
			}
			request, _ := http.NewRequest(http.MethodPost, s.BaseURL+"/v1/messages", strings.NewReader(`{"model":"model-a","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`))
			request.Header.Set("Authorization", "Bearer "+token)
			response, err = http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 || !bytes.Contains(body, []byte(`"text":"ok"`)) {
				t.Fatalf("status=%d body=%s", response.StatusCode, body)
			}
			select {
			case got := <-requests:
				if !strings.HasPrefix(got, "/v1/messages ") {
					t.Fatalf("route=%s", got)
				}
			case <-time.After(time.Second):
				t.Fatal("no upstream request")
			}
			// Simulate unrelated settings edits and an explicit model override.
			settings["permissions"] = json.RawMessage(`{"allow":["Read","Write"]}`)
			settings["model"] = json.RawMessage(`"user-edited-model"`)
			env["KEEP"] = json.RawMessage(`"edited"`)
			env["NEW"] = json.RawMessage(`"new"`)
			settings["env"], _ = json.Marshal(env)
			if err := writeJSON(path, settings); err != nil {
				t.Fatal(err)
			}
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			if err := m.Stop(stopCtx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			settings, _ = readSettings(path)
			env, _ = settingsEnv(settings)
			if string(env["ANTHROPIC_BASE_URL"]) != `"https://original.invalid"` || string(env["ANTHROPIC_AUTH_TOKEN"]) != `"original-token"` {
				t.Fatal("previous transport settings were not restored")
			}
			if string(settings["model"]) != `"user-edited-model"` || string(env["KEEP"]) != `"edited"` || string(env["NEW"]) != `"new"` {
				t.Fatal("user edits were lost")
			}
			if _, err := os.Stat(m.statePath()); !os.IsNotExist(err) {
				t.Fatalf("state not removed: %v", err)
			}
		})
	}
}

func TestDeadProxyRecoveryRestoresJournal(t *testing.T) {
	m, path := isolatedManager(t)
	if err := writeJSON(path, map[string]any{"language": "English"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &State{ID: "dead", SettingsPath: path, ControlURL: "http://127.0.0.1:1", ControlToken: "fake"}
	settings := map[string]json.RawMessage{"env": json.RawMessage(`{"ANTHROPIC_BASE_URL":"http://127.0.0.1:12345"}`), "model": json.RawMessage(`"proxy-model"`)}
	if err := applySettings(context.Background(), s, settings, m.statePath()); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	fields, err := readSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || string(fields["language"]) != `"English"` {
		t.Fatalf("restored fields=%v", fields)
	}
}

func TestRecoverStaleRestoresOnlyADeadProxy(t *testing.T) {
	m, path := isolatedManager(t)
	if restored, err := m.RecoverStale(context.Background()); restored || err != nil {
		t.Fatalf("no journal: restored=%v err=%v", restored, err)
	}
	if _, err := os.Stat(m.Dir); !os.IsNotExist(err) {
		t.Fatal("recovery without a journal created the proxy directory")
	}
	if err := writeJSON(path, map[string]any{"language": "English"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &State{ID: "dead", SettingsPath: path, ControlURL: "http://127.0.0.1:1", ControlToken: "fake"}
	settings := map[string]json.RawMessage{"env": json.RawMessage(`{"ANTHROPIC_BASE_URL":"http://127.0.0.1:12345"}`), "model": json.RawMessage(`"proxy-model"`)}
	if err := applySettings(context.Background(), s, settings, m.statePath()); err != nil {
		t.Fatal(err)
	}

	// A live proxy holds the daemon lock; a start or stop holds the command
	// lock. Either way the overlay is not ours to undo.
	for _, lock := range []string{"daemon.lock", "command.lock"} {
		release, err := fsutil.AcquireFileLock(context.Background(), filepath.Join(m.Dir, lock))
		if err != nil {
			t.Fatal(err)
		}
		restored, err := m.RecoverStale(context.Background())
		release()
		if restored || err != nil {
			t.Fatalf("%s held: restored=%v err=%v", lock, restored, err)
		}
		if fields, _ := readSettings(path); string(fields["model"]) != `"proxy-model"` {
			t.Fatalf("%s held: overlay removed: %v", lock, fields)
		}
	}

	restored, err := m.RecoverStale(context.Background())
	if !restored || err != nil {
		t.Fatalf("dead proxy: restored=%v err=%v", restored, err)
	}
	fields, err := readSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || string(fields["language"]) != `"English"` {
		t.Fatalf("restored fields=%v", fields)
	}
	if _, err := os.Stat(m.statePath()); !os.IsNotExist(err) {
		t.Fatalf("journal not removed: %v", err)
	}
}

func TestProxyRejectsMalformedSettingsWithoutOverwriting(t *testing.T) {
	m, path := isolatedManager(t)
	original := []byte("{broken json")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	err := applySettings(context.Background(), &State{ID: "invalid", SettingsPath: path}, map[string]json.RawMessage{"env": json.RawMessage(`{}`)}, m.statePath())
	if err == nil {
		t.Fatal("malformed settings accepted")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, original) {
		t.Fatal("malformed settings overwritten")
	}
	if _, err := os.Stat(m.statePath()); !os.IsNotExist(err) {
		t.Fatal("journal created without valid settings")
	}
}
