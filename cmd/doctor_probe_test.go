package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// TestDoctorOnlyProbesModelsOnRequest pins S5: a plain doctor run sends no
// per-model inference request (those are billed) and never rewrites the
// config; --probe tests every pooled model but still writes nothing.
func TestDoctorOnlyProbesModelsOnRequest(t *testing.T) {
	var inference atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"b"},{"id":"a"}]}`)
			return
		}
		inference.Add(1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	// The pool deliberately lists an unavailable-looking order; doctor used to
	// reorder and save it.
	if err := config.Save(&provider.Config{ActiveProvider: "gw", Providers: map[string]provider.Provider{
		"gw": {Name: "gw", Type: "openai", Endpoint: server.URL + "/v1", APIKey: "k",
			Model: "a,b", OpusModel: "a", SonnetModel: "b", HaikuModel: "b"},
	}}); err != nil {
		t.Fatal(err)
	}
	// Let config.Load apply its one-time migrations first, so the comparison
	// below isolates what doctor itself writes.
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".ccl", "config.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := runDoctor(context.Background(), false, providerTarget{}); err != nil {
		t.Fatalf("runDoctor() = %v", err)
	}
	if got := inference.Load(); got != 0 {
		t.Fatalf("doctor without --probe sent %d inference request(s)", got)
	}

	if err := runDoctor(context.Background(), true, providerTarget{}); err != nil {
		t.Fatalf("runDoctor(--probe) = %v", err)
	}
	if got := inference.Load(); got != 2 {
		t.Fatalf("doctor --probe sent %d inference request(s), want one per pooled model", got)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("doctor rewrote the config:\n--- before\n%s\n--- after\n%s", before, after)
	}
}

// TestMapAutoOnlyProbesOnRequest: map auto recommends from the catalog without
// billed requests; --probe tests each model and drops the ones that fail.
func TestMapAutoOnlyProbesOnRequest(t *testing.T) {
	var inference atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gw-pro"},{"id":"gw-flash"},{"id":"gw-broken-pro-max"}]}`)
			return
		}
		inference.Add(1)
		if strings.Contains(string(body), "gw-broken") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"no such model"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&provider.Config{ActiveProvider: "gw", Providers: map[string]provider.Provider{
		"gw": {Name: "gw", Type: "openai", Endpoint: server.URL + "/v1", APIKey: "k"},
	}}); err != nil {
		t.Fatal(err)
	}

	if err := runMapAuto(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if got := inference.Load(); got != 0 {
		t.Fatalf("map auto sent %d inference request(s) without --probe", got)
	}
	cfg, _ := config.Load()
	if got := cfg.Providers["gw"].OpusModel; got != "gw-broken-pro-max" {
		t.Fatalf("unprobed opus = %q, want the catalog's strongest name", got)
	}

	if err := runMapAuto(context.Background(), nil, true); err != nil {
		t.Fatal(err)
	}
	if got := inference.Load(); got != 3 {
		t.Fatalf("map auto --probe sent %d request(s), want one per model", got)
	}
	cfg, _ = config.Load()
	p := cfg.Providers["gw"]
	for _, slot := range []string{p.OpusModel, p.SonnetModel, p.HaikuModel, p.FableModel, p.CustomModelID} {
		if strings.Contains(slot, "gw-broken") {
			t.Fatalf("--probe mapped a model that failed: %+v", p)
		}
	}
}
