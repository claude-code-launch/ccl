package proxymanager

import (
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

	"github.com/claude-code-launch/ccl/internal/codex"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestStandaloneProxyAddsCodexModelsAndRestoresUserEdits(t *testing.T) {
	m, _ := isolatedManager(t)
	home := os.Getenv("CODEX_HOME")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	codexPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(codexPath, []byte("# user config\nmodel = 'native-fixture'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"models":[{"slug":"native-fixture","display_name":"Native","future_metadata":"preserved"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Authorization") != "Bearer fake-upstream" {
			t.Error("incorrect provider authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"codex-fixture"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	p := provider.Provider{Name: "chosen", Type: "openai", Endpoint: upstream.URL + "/v1", APIKey: "fake-upstream", Model: "model-a"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, p, "codex-fixture") }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("proxy did not exit")
		}
	})
	s := awaitRunning(t, m)
	if s.Codex == nil {
		t.Fatal("existing Codex profile was not connected")
	}
	data, err := os.ReadFile(s.Codex.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := codex.ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 2 || catalog.Models[1]["future_metadata"] != "preserved" {
		t.Fatal("native picker metadata lost")
	}
	request, _ := http.NewRequest(http.MethodPost, s.Codex.BaseURL+"/responses", strings.NewReader(`{"model":"ccl/chosen/model-a","input":"fixture"}`))
	request.Header.Set("Authorization", "Bearer fake-native-key")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(body), "codex-fixture") {
		t.Fatalf("Codex response: %d %s", response.StatusCode, body)
	}
	data, _ = os.ReadFile(codexPath)
	data = append(data, []byte("# later edit\ntheme = 'dark'\n")...)
	if err := os.WriteFile(codexPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(codexPath)
	if strings.Contains(string(data), "openai_base_url") || strings.Contains(string(data), "model_catalog_json") || !strings.Contains(string(data), "theme = 'dark'") || !strings.Contains(string(data), "model = 'native-fixture'") {
		t.Fatalf("Codex restore lost edits or retained overlay: %s", data)
	}
}

func TestStaleProxyRecoveryRestoresBothClients(t *testing.T) {
	m, claudePath := isolatedManager(t)
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("CODEX_HOME")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("# native\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value := "http://127.0.0.1:1/v1"
	overlay := &codex.Overlay{Path: path, CatalogPath: filepath.Join(m.Dir, "catalog.json"), Changes: []codex.ConfigChange{{Key: "openai_base_url", Applied: &value}}}
	if err := os.WriteFile(overlay.CatalogPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &State{ID: "dead", SettingsPath: claudePath, Codex: overlay}
	if err := applySettings(context.Background(), s, map[string]json.RawMessage{"model": json.RawMessage(`"proxy-model"`)}, m.statePath()); err != nil {
		t.Fatal(err)
	}
	if err := overlay.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restored, err := m.RecoverStale(context.Background()); err != nil || !restored {
		t.Fatalf("recovery: %v %v", restored, err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "openai_base_url") {
		t.Fatal("stale Codex routing remains")
	}
	fields, err := readSettings(claudePath)
	if err != nil || len(fields) != 0 {
		t.Fatalf("stale Claude overlay remains: %v %v", fields, err)
	}
}

func TestStandaloneProxyReloadInheritsUpdatedCodeModeAndVisionMetadata(t *testing.T) {
	m, _ := isolatedManager(t)
	m.CodexMode = "independent"
	home := os.Getenv("CODEX_HOME")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("# independent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"models":[{"slug":"native-old"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	p := provider.Provider{Name: "chosen", Type: "openai", Endpoint: upstream.URL + "/v1", APIKey: "fake-upstream", Model: "model-a", ModelsDevProvider: "fixture"}
	if err := config.Save(&provider.Config{ActiveProvider: p.Name, Providers: map[string]provider.Provider{p.Name: p}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, p, "codex-cu-reload") }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("proxy did not exit")
		}
	})
	s := awaitRunning(t, m)

	// Simulate Codex updating its native cache to code_mode_only and models.dev publishing vision support.
	if err := fsutil.WriteFileAtomic(filepath.Join(home, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-5.4","tool_mode":"code_mode_only","shell_type":"unified_exec","node_repl_auto_review_required":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cachePath, err := modelsdev.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFileAtomic(cachePath, []byte(`{"fixture":{"models":{"model-a":{"modalities":{"input":["text","image"],"output":["text"]},"limit":{"output":64000}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := reloadControl(context.Background(), s, p.Name, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.Codex.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := codex.ParseCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	entry := catalog.Models[0]
	modalitiesRaw, _ := json.Marshal(entry["input_modalities"])
	if entry["tool_mode"] != "code_mode_only" || entry["shell_type"] != "unified_exec" || entry["supports_image_detail_original"] != true || string(modalitiesRaw) != `["text","image"]` {
		t.Fatalf("live reload missed code_mode_only or vision metadata: %+v", entry)
	}
}
