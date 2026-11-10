package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// Exercise remote compaction with the real client's streaming decoder and
// persisted history. A JSON-only gateway test cannot verify the second turn.
func TestRealCodexContinuesAfterRemoteCompaction(t *testing.T) {
	executable := os.Getenv("CCL_TEST_CODEX")
	if executable == "" {
		t.Skip("set CCL_TEST_CODEX to the official Codex client")
	}
	executable, err := filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	for key, value := range map[string]string{"HOME": home, "USERPROFILE": home, "CODEX_HOME": home, "OPENAI_API_KEY": "fake-native-key", "HTTP_PROXY": "http://127.0.0.1:9", "HTTPS_PROXY": "http://127.0.0.1:9", "ALL_PROXY": "http://127.0.0.1:9", "NO_PROXY": "127.0.0.1,localhost"} {
		t.Setenv(key, value)
	}
	native, _ := json.Marshal(Catalog{Models: []Model{newModel("native-fixture", "native-fixture", nil, nil)}})
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), native, 0o600); err != nil {
		t.Fatal(err)
	}
	var turns, compactions atomic.Int32
	g := fixtureProviderGateway(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid JSON", 400)
			return
		}
		raw, _ := json.Marshal(body["messages"])
		reply := func(text string, tokens int) {
			writeChatFixture(w, r, map[string]any{"content": text}, "stop", tokens)
		}
		if strings.Contains(textValue(body["system"])+string(raw), "Summarize the conversation") {
			compactions.Add(1)
			if tools, _ := body["tools"].([]any); len(tools) != 0 {
				t.Error("compaction offered tools")
			}
			if !strings.Contains(string(raw), "keep-this-goal") {
				t.Errorf("compaction lost the goal: %s", raw)
			}
			reply("keep-this-goal in the next turn", 3)
			return
		}
		turn := turns.Add(1)
		if turn > 2 && (!strings.Contains(string(raw), "Conversation summary:") || !strings.Contains(string(raw), "keep-this-goal")) {
			t.Errorf("turn %d did not replay the summary: %s", turn, raw)
		}
		// Stay below the selected 500K budget's 85% trigger first, then cross
		// it. This catches both premature compaction from a lost window and a
		// client that ignores the configured threshold. No Codex override.
		tokens := 425001
		if turn == 1 {
			tokens = 400000
		}
		reply(fmt.Sprintf("turn-%d-done", turn), tokens)
	}, provider.Provider{Name: "chosen", Type: "openai", Model: "model-a", ContextPreset: provider.ContextPresetNameBalanced500K}, Options{Desktop: true})
	overlay, err := Plan(context.Background(), g, filepath.Join(home, "proxy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := overlay.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		command := process.Command(executable, args...)
		command.Dir = home
		process.ConfigureGroup(command)
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Codex: %v\n%s", err, output.String())
			}
		case <-time.After(45 * time.Second):
			process.KillTree(command)
			<-done
			t.Fatalf("Codex timed out\n%s", output.String())
		}
		return output.String()
	}
	output := run("-m", "ccl/chosen/model-a", "-c", `web_search="disabled"`, "exec", "--json", "--skip-git-repo-check", "Remember keep-this-goal.")
	if !strings.Contains(output, "turn-1-done") {
		t.Fatalf("first turn: %s", output)
	}
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
		t.Fatalf("missing session ID: %s", output)
	}
	for turn := 2; turn <= 4; turn++ {
		output = run("-m", "ccl/chosen/model-a", "-c", `web_search="disabled"`, "exec", "resume", "--json", "--skip-git-repo-check", session, "Continue keep-this-goal.")
		if !strings.Contains(output, fmt.Sprintf("turn-%d-done", turn)) {
			t.Fatalf("turn %d after compaction: %s", turn, output)
		}
		if got := compactions.Load(); got != int32(turn-2) {
			t.Fatalf("turn %d: compactions=%d, want %d", turn, got, turn-2)
		}
	}
	if turns.Load() != 4 || compactions.Load() != 2 {
		t.Fatalf("turns=%d compactions=%d; remote compaction was not exercised", turns.Load(), compactions.Load())
	}
}
