package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRealCodexSwitchesToIndependentAfterNativeCompaction(t *testing.T) {
	home, run := isolatedCodexClient(t)
	// Keep a source catalog as desktop profiles do. The client's background
	// /models cache refresh must not replace our native fixture's metadata.
	nativeCatalogPath := filepath.Join(home, "native-source.json")
	entry := newModel("native-fixture", "native-fixture", nil, nil)
	entry["context_window"], entry["max_context_window"], entry["auto_compact_token_limit"] = 500000, 500000, 425000
	catalogJSON, _ := json.Marshal(Catalog{Models: []Model{entry}})
	if err := os.WriteFile(nativeCatalogPath, catalogJSON, 0600); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(nativeCatalogPath)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model_catalog_json = "+string(config)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var compactions, nativeTurns atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.HasSuffix(r.URL.Path, "/compact") {
			compactions.Add(1)
			writeJSON(w, 200, map[string]any{"id": "cmp_native_fixture", "object": "response.compaction", "output": []any{map[string]any{"type": "compaction", "id": "cmp_native_item", "encrypted_content": "opaque-native-compaction-fixture"}}, "usage": responseUsage()})
			return
		}
		items, _ := inputItems(mustMarshalFixture(body["input"]))
		_, triggered := hoistCompactionTrigger(items)
		if triggered {
			compactions.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			stream := newResponseStream(w, "native-fixture", true, nil)
			_ = stream.start()
			item := map[string]any{"type": "compaction", "id": "cmp_native_item", "encrypted_content": "opaque-native-compaction-fixture"}
			_ = stream.emit("response.output_item.added", map[string]any{"output_index": 0, "item": item})
			_ = stream.emit("response.output_item.done", map[string]any{"output_index": 0, "item": item})
			response := stream.response("completed")
			response["output"] = []any{item}
			_ = stream.emit("response.completed", map[string]any{"response": response})
			return
		}
		turn := nativeTurns.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		stream := newResponseStream(w, "native-fixture", true, nil)
		stream.usage = map[string]any{"input_tokens": 425001, "output_tokens": 2, "total_tokens": 425003, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}
		_ = stream.start()
		_ = stream.addBlock(0, map[string]any{"type": "text", "text": fmt.Sprintf("native-turn-%d", turn)})
		_ = stream.closeBlock(0)
		stream.finish()
	}))
	defer native.Close()
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("converted provider called before switching")
		http.Error(w, "unexpected", 500)
	}, "anthropic", Options{Desktop: true, OpenAIURL: native.URL, ChatGPTURL: native.URL})
	shared, err := Plan(context.Background(), g, home+"/shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := shared.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	output := run("-m", "native-fixture", "-c", `model_auto_compact_token_limit=425000`, "-c", `web_search="disabled"`, "exec", "--json", "--skip-git-repo-check", "Remember keep-this-goal and pending-fix.")
	var session string
	for _, line := range strings.Split(output, "\n") {
		var event struct {
			Type string `json:"type"`
			ID   string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "thread.started" {
			session = event.ID
		}
	}
	if session == "" {
		t.Fatalf("missing chat ID: %s", output)
	}
	output = run("-m", "native-fixture", "-c", `model_auto_compact_token_limit=425000`, "-c", `web_search="disabled"`, "exec", "resume", "--json", "--skip-git-repo-check", session, "Continue.")
	if compactions.Load() == 0 {
		t.Fatalf("native compaction was not exercised: %s", output)
	}
	if err := shared.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	converted := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		text := textValue(body["messages"])
		if !strings.Contains(text, "keep-this-goal") || !strings.Contains(text, "pending-fix") || strings.Contains(text, "opaque-native-compaction-fixture") {
			t.Error("native compacted context was not recovered")
		}
		if strings.Contains(text, "CONTEXT CHECKPOINT COMPACTION") {
			writeMessage(w, []map[string]any{{"type": "text", "text": "keep-this-goal pending-fix migration handoff summary"}})
			return
		}
		writeMessage(w, []map[string]any{{"type": "text", "text": "independent-resumed-after-native-compaction"}})
	}, "anthropic", Options{Independent: true})
	applyClientFixture(t, converted, home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"OPENAI_API_KEY":`+strconv.Quote(converted.Token())+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", converted.Token())
	output = run("-c", `model_provider="openai"`, "-c", "openai_base_url="+strconv.Quote(converted.BaseURL()), "-m", "ccl/chosen/model-a", "-c", `web_search="disabled"`, "exec", "resume", "--json", "--skip-git-repo-check", session, "Continue using the independent provider.")
	assertCodexThreadID(t, output, session)
	if !strings.Contains(output, "independent-resumed-after-native-compaction") {
		t.Fatalf("resume failed: %s", output)
	}
	if !strings.Contains(output, "older details may be missing") {
		t.Fatal("client did not display the local history migration limitation")
	}
}

func mustMarshalFixture(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }
