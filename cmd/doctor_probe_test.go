package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	if err := runDoctor(context.Background(), false); err != nil {
		t.Fatalf("runDoctor() = %v", err)
	}
	if got := inference.Load(); got != 0 {
		t.Fatalf("doctor without --probe sent %d inference request(s)", got)
	}

	if err := runDoctor(context.Background(), true); err != nil {
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
