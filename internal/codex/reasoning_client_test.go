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
	"github.com/claude-code-launch/ccl/internal/protocol"
)

// The real client must accept the reasoning catalog, honor the selected effort,
// and carry it across both protocol conversions into the local Chat provider.
func TestRealCodexReasoningOptionsReachChatProvider(t *testing.T) {
	for _, mode := range []string{"shared", "independent"} {
		t.Run(mode, func(t *testing.T) {
			for _, effort := range []string{"high", "xhigh", "max"} {
				t.Run(effort, func(t *testing.T) {
					testRealCodexReasoningEffort(t, effort, mode == "independent")
				})
			}
		})
	}
}

func testRealCodexReasoningEffort(t *testing.T, effort string, independent bool) {
	t.Helper()
	executable := os.Getenv("CCL_TEST_CODEX")
	if executable == "" {
		t.Skip("set CCL_TEST_CODEX to the pinned official Codex client")
	}
	executable, err := filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	for key, value := range map[string]string{"HOME": home, "USERPROFILE": home, "CODEX_HOME": home, "OPENAI_API_KEY": "fake-native-key", "HTTP_PROXY": "http://127.0.0.1:9", "HTTPS_PROXY": "http://127.0.0.1:9", "ALL_PROXY": "http://127.0.0.1:9", "NO_PROXY": "127.0.0.1,localhost"} {
		t.Setenv(key, value)
	}
	if independent {
		t.Setenv("OPENAI_API_KEY", "")
	}
	native, _ := json.Marshal(Catalog{Models: []Model{newModel("native-fixture", "native-fixture", nil, nil)}})
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), native, 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid JSON", 400)
			return
		}
		if body["reasoning_effort"] != effort || body["model"] != "model-a" {
			t.Errorf("selected effort lost: effort=%v model=%v", body["reasoning_effort"], body["model"])
		}
		if r.Header.Get("Authorization") != "Bearer fake-provider-key" || r.Header.Get("Chatgpt-Account-Id") != "" {
			t.Error("native identity crossed the provider boundary")
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"chat_fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"reasoning-fixture-done\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat_fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}, "openai", Options{Desktop: !independent, Independent: independent})
	// Space-Bunny advertises five tiers and defaults to max. The max case
	// leaves the CLI override unset to verify that the catalog default works.
	info := protocol.ModelInfo{ID: "model-a", DisplayName: "Reasoner", ReasoningLevels: protocol.GPTReasoningLevels()[:5], DefaultReasoningLevel: "max"}
	g.models = []Model{newModel("ccl/chosen/model-a", "model-a", map[string]protocol.ModelInfo{"model-a": info}, nil)}
	overlay, err := Plan(context.Background(), g, filepath.Join(home, "proxy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := overlay.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	args := []string{"-m", "ccl/chosen/model-a", "-c", `web_search="disabled"`}
	if effort != "max" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", effort))
	}
	args = append(args, "exec", "--json", "--skip-git-repo-check", "Answer reasoning-fixture-done.")
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
		if err != nil || calls.Load() == 0 || !strings.Contains(output.String(), "reasoning-fixture-done") {
			t.Fatalf("official client reasoning request: %v calls=%d\n%s", err, calls.Load(), output.String())
		}
	case <-time.After(45 * time.Second):
		process.KillTree(command)
		<-done
		t.Fatalf("official client timed out\n%s", output.String())
	}
}
