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
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CCL_PROXY_NO_MENUBAR", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	locale.SetLanguage("en")
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(os.Getenv("CODEX_HOME"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "models_cache.json"), []byte(`{"models":[{"slug":"native-fixture"}]}`), 0o600); err != nil {
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

func TestCodexProxyRequiresCodexProfile(t *testing.T) {
	m, claudePath := isolatedManager(t)
	if err := os.RemoveAll(os.Getenv("CODEX_HOME")); err != nil {
		t.Fatal(err)
	}
	_, err := m.Start(context.Background(), "must-not-execute", provider.Provider{Name: "chosen"})
	if err == nil || !strings.Contains(err.Error(), "Codex profile not found") {
		t.Fatalf("missing profile: %v", err)
	}
	if state, err := m.Read(); err != nil || state != nil {
		t.Fatal("missing Codex profile left a proxy journal")
	}
	if _, err := os.Stat(claudePath); !os.IsNotExist(err) {
		t.Fatal("missing Codex profile created Claude settings")
	}
}

func TestStandaloneCodexProxyLeavesClaudeSettingsAlone(t *testing.T) {
	for _, auth := range []string{"", "bearer"} {
		for _, initial := range []string{"", "{broken Claude settings", `{"model":"original","env":{"KEEP":"original"}}`} {
			t.Run(auth+"/"+initial, func(t *testing.T) {
				m, path := isolatedManager(t)
				if initial != "" {
					if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					key := r.Header.Get("X-Api-Key")
					if auth == "bearer" {
						key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					}
					if key != "fake-upstream-key" || (auth == "bearer" && r.Header.Get("X-Api-Key") != "") || (auth == "" && r.Header.Get("Authorization") != "") {
						t.Error("incorrect provider credentials")
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"msg_fixture","type":"message","role":"assistant","content":[{"type":"text","text":"codex-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				}))
				defer upstream.Close()
				p := provider.Provider{Name: "chosen", Type: "anthropic", Endpoint: upstream.URL + "/v1", APIKey: "fake-upstream-key", Model: "model-a", AnthropicAuth: auth}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- m.Run(ctx, p, "codex-only-fixture") }()
				t.Cleanup(func() {
					cancel()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("proxy did not exit")
					}
				})
				state := awaitRunning(t, m)
				if state.Codex == nil || state.BaseURL != state.Codex.BaseURL || state.SettingsPath != "" || len(state.Changes) != 0 || len(state.EnvBefore) != 0 {
					t.Fatal("new daemon retained a Claude settings overlay")
				}
				data, err := os.ReadFile(path)
				if initial == "" {
					if !os.IsNotExist(err) {
						t.Fatal("proxy created Claude settings")
					}
				} else if err != nil || string(data) != initial {
					t.Fatal("proxy touched Claude settings")
				}
				response, err := http.Get(state.ControlURL + "/health")
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != 401 {
					t.Fatal("control endpoint accepted missing credentials")
				}
				request, _ := http.NewRequest(http.MethodPost, state.BaseURL+"/responses", strings.NewReader(`{"model":"ccl/chosen/model-a","input":"fixture"}`))
				request.Header.Set("Authorization", "Bearer fake-native-key")
				response, err = http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				data, _ = io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != 200 || !bytes.Contains(data, []byte("codex-ok")) {
					t.Fatalf("Codex inference failed: %d %s", response.StatusCode, data)
				}
				edited := []byte("{later user edit, still not valid JSON")
				if err := os.WriteFile(path, edited, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := m.Stop(context.Background()); err != nil {
					t.Fatal(err)
				}
				data, err = os.ReadFile(path)
				if err != nil || !bytes.Equal(data, edited) {
					t.Fatal("stop changed later Claude edits")
				}
				if _, err := os.Stat(m.statePath()); !os.IsNotExist(err) {
					t.Fatal("state retained after stop")
				}
			})
		}
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
	edited, err := readSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	edited["model"] = json.RawMessage(`"user-edited-model"`)
	if err := writeJSON(path, edited); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	fields, err := readSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || string(fields["language"]) != `"English"` || string(fields["model"]) != `"user-edited-model"` {
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

func TestLegacyRecoveryRejectsMalformedSettingsWithoutOverwriting(t *testing.T) {
	m, path := isolatedManager(t)
	original := []byte("{broken JSON after an old proxy changed the file")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	state := &State{ID: "legacy", SettingsPath: path, Changes: []Change{{Key: "model", Applied: json.RawMessage(`"old-proxy"`)}}}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(m.statePath(), state); err != nil {
		t.Fatal(err)
	}
	if restored, err := m.RecoverStale(context.Background()); err == nil || restored {
		t.Fatal("unsafe legacy recovery succeeded")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, original) {
		t.Fatal("malformed legacy settings overwritten")
	}
	if _, err := os.Stat(m.statePath()); err != nil {
		t.Fatal("legacy recovery journal discarded")
	}
}
