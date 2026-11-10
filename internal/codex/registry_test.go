package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func registryBinding(t *testing.T, name, model, key string) RuntimeBinding {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Authorization") != "Bearer "+key || r.Header.Get("Cookie") != "" || r.Header.Get("Chatgpt-Account-Id") != "" {
			t.Error("provider credential boundary crossed")
		}
		raw, _ := io.ReadAll(r.Body)
		var request map[string]any
		_ = json.Unmarshal(raw, &request)
		if request["model"] != model {
			t.Errorf("wrong route: %s", raw)
		}
		writeChatText(w, r, name+":"+model)
	}))
	t.Cleanup(upstream.Close)
	runtime, err := oauthproxy.StartOpenAIChatAPI(context.Background(), upstream.URL+"/v1", key, model)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)
	return RuntimeBinding{Provider: provider.Provider{Name: name, Type: "openai", Model: model}, Runtime: runtime, Close: runtime.Stop}
}

func TestRegistrySwapDrainsRequestsAndKeepsAccountsSeparate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	entered, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatDelta(w, "old generation completed")
		_ = http.NewResponseController(w).Flush()
		once.Do(func() { close(entered) })
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		finishChatFixture(w)
	}, "openai", Options{Desktop: true})
	var retired atomic.Int32
	g.OwnRuntime(func() { retired.Add(1) })
	initialBase, initialToken := g.BaseURL(), g.Token()
	received := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, g.BaseURL()+"/responses", strings.NewReader(`{"model":"ccl/chosen/model-a","input":"hold old generation","stream":true}`))
		req.Header.Set("Authorization", "Bearer native-key")
		response, _ := http.DefaultClient.Do(req)
		received <- response
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not enter old runtime")
	}
	next := registryBinding(t, "chosen", "model-b", "account-one-key")
	other := registryBinding(t, "other", "model-b", "account-two-key")
	path := filepath.Join(t.TempDir(), "catalog.json")
	raw, _ := json.Marshal(Catalog{Models: append(g.Models(), Model{"slug": "native-model", "future_field": "retain"})})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	overlay := &Overlay{CatalogPath: path}
	if err := g.ReplaceProviders([]RuntimeBinding{next, other}, overlay.ReplaceCatalog); err != nil {
		t.Fatal(err)
	}
	if retired.Load() != 0 || g.BaseURL() != initialBase || g.Token() != initialToken {
		t.Fatal("swap interrupted an active lease or changed the endpoint")
	}
	for _, name := range []string{"chosen", "other"} {
		response := responseObject(t, postResponses(t, g, map[string]any{"model": "ccl/" + name + "/model-b", "input": "new generation"}))
		raw, _ := json.Marshal(response)
		if !strings.Contains(string(raw), name+":model-b") {
			t.Fatalf("wrong provider: %s", raw)
		}
	}
	if response := postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "input": "removed"}); response.StatusCode != 404 {
		t.Fatal("removed route still available")
	}
	close(finish)
	select {
	case response := <-received:
		if response == nil {
			t.Fatal("in-flight request disconnected")
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if !strings.Contains(string(raw), "old generation completed") {
			t.Fatalf("old response lost: %s", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old request did not finish")
	}
	deadline := time.Now().Add(time.Second)
	for retired.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if retired.Load() != 1 {
		t.Fatal("retired runtime did not close after its last lease")
	}
	saved, _ := os.ReadFile(path)
	if strings.Contains(string(saved), "ccl/chosen/model-a") || !strings.Contains(string(saved), "retain") || !strings.Contains(string(saved), "ccl/other/model-b") {
		t.Fatalf("catalog replacement = %s", saved)
	}
	failed := registryBinding(t, "chosen", "model-c", "unused-key")
	if err := g.ReplaceProviders([]RuntimeBinding{failed}, func([]Model) error { return errors.New("disk unavailable") }); err == nil {
		t.Fatal("failed persistence accepted")
	}
	if g.Models()[0]["slug"] != "ccl/chosen/model-b" {
		t.Fatal("failed update replaced the last registry")
	}
}

func TestRemoteCatalogMergedETagAndAuthentication(t *testing.T) {
	g := fixtureGateway(t, func(http.ResponseWriter, *http.Request) {}, "openai", Options{Desktop: true})
	g.inheritClientToolMode(Catalog{Models: []Model{{"slug": "native-only", "unknown_native_field": "keep"}}})
	get := func(tag string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, g.BaseURL()+"/models?client_version=0.166.0", nil)
		req.Header.Set("If-None-Match", tag)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { response.Body.Close() })
		return response
	}
	first := get("")
	raw, _ := io.ReadAll(first.Body)
	if first.StatusCode != 200 || !strings.Contains(string(raw), "native-only") || !strings.Contains(string(raw), "ccl/chosen/model-a") || first.Header.Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("remote catalog = %d %s", first.StatusCode, raw)
	}
	if get(first.Header.Get("ETag")).StatusCode != 304 {
		t.Fatal("unchanged catalog did not revalidate")
	}
	if err := g.RefreshMetadata(provider.Provider{Name: "chosen", ContextPreset: provider.ContextPresetNameBalanced800K}, func([]Model) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if response := get(first.Header.Get("ETag")); response.StatusCode != 200 || response.Header.Get("ETag") == first.Header.Get("ETag") {
		t.Fatal("catalog revision did not advance")
	}
	independent := fixtureGateway(t, func(http.ResponseWriter, *http.Request) {}, "openai", Options{Independent: true, CatalogMode: CatalogRemote})
	response, err := http.Get(independent.BaseURL() + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("custom catalog accepted an unauthenticated client")
	}
}
