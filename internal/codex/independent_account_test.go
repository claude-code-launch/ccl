package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/process"
)

func TestRealCodexIndependentAccountReadIgnoresCachedChatGPTLogin(t *testing.T) {
	home, _ := isolatedCodexClient(t)
	t.Setenv("OPENAI_API_KEY", "")
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	claims := map[string]any{"email": "fixture@example.invalid", "exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "fixture-account", "chatgpt_plan_type": "plus", "chatgpt_user_id": "fixture-user", "user_id": "fixture-user"}}
	jwt := encode(map[string]any{"alg": "none"}) + "." + encode(claims) + ".fixture"
	auth, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{"id_token": jwt, "access_token": jwt, "refresh_token": "fake-refresh", "account_id": "fixture-account"}, "last_refresh": time.Now().UTC().Format(time.RFC3339)})
	if err := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0600); err != nil {
		t.Fatal(err)
	}
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("account/read triggered inference")
		http.Error(w, "unexpected", 500)
	}, "anthropic", Options{Independent: true})
	applyClientFixture(t, g, home)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Getenv("CCL_TEST_CODEX"), "app-server")
	command.Dir = home
	process.ConfigureGroup(command)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { process.KillTree(command); _ = command.Wait() }()
	encoder := json.NewEncoder(stdin)
	for _, request := range []map[string]any{
		{"id": 0, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "ccl-fixture", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}},
		{"method": "initialized"},
		{"id": 1, "method": "account/read", "params": map[string]any{"refreshToken": false}},
	} {
		if err := encoder.Encode(request); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan map[string]any, 1)
	go func() {
		decoder := json.NewDecoder(stdout)
		for {
			var event map[string]any
			if decoder.Decode(&event) != nil {
				done <- nil
				return
			}
			if event["id"] == float64(1) {
				done <- event
				return
			}
		}
	}()
	select {
	case event := <-done:
		result, _ := event["result"].(map[string]any)
		if result == nil || result["requiresOpenaiAuth"] != false || result["account"] != nil {
			t.Fatalf("cached ChatGPT identity still controls independent mode: %+v", event)
		}
	case <-ctx.Done():
		t.Fatal("independent account/read timed out")
	}
}
