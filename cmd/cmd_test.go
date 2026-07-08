package cmd

import (
	"bytes"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

func executeCommand(root *cobra.Command, args ...string) (string, error) {
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)

	// 执行命令
	_, err := root.ExecuteC()
	return buf.String(), err
}

// 简单字符串包含函数
func contains(s, substr string) bool {
	return bytes.Contains([]byte(s), []byte(substr))
}

func TestCmd_Doctor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	out, err := executeCommand(RootCmd(), "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "No providers added yet"; !contains(out, want) {
		t.Errorf("expected output to contain %q, got %q", want, out)
	}
}
func TestCmd_Set(t *testing.T) {
	cmd := SetCMD()
	if cmd.Use != "set [name]" {
		t.Fatalf("unexpected set command use: %q", cmd.Use)
	}
}

func TestResolveProviderUsesAnthropicAuthTokenEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://token.sensenova.cn/v1")
	t.Setenv("ANTHROPIC_MODEL", "sensenova-u1-fast")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	p, err := resolveProvider()
	if err != nil {
		t.Fatalf("resolveProvider failed: %v", err)
	}
	if p.Type != "anthropic" {
		t.Fatalf("expected anthropic provider, got %q", p.Type)
	}
	if p.APIKey != "bearer-token" {
		t.Fatalf("expected auth token as API key, got %q", p.APIKey)
	}
	if p.AnthropicAuth != "bearer" {
		t.Fatalf("expected bearer auth, got %q", p.AnthropicAuth)
	}
	if p.Model != "sensenova-u1-fast" {
		t.Fatalf("expected ANTHROPIC_MODEL fallback, got %q", p.Model)
	}
}

func TestMapAutoAssignsAvailableModelsInOrder(t *testing.T) {
	p := testProviderWithOldMappings()
	assigned := applySequentialSlotMapping(sequentialSlotPointers(&p), []string{
		"model-a",
		"model-b",
		"model-c",
		"model-d",
		"model-e",
	})

	if assigned != 4 {
		t.Fatalf("expected 4 assigned slots, got %d", assigned)
	}
	if p.OpusModel != "model-a" || p.SonnetModel != "model-b" || p.HaikuModel != "model-c" || p.CustomModelID != "model-d" {
		t.Fatalf("models were not assigned sequentially: %+v", p)
	}
}

func TestMapAutoClearsUnassignedTrailingSlots(t *testing.T) {
	p := testProviderWithOldMappings()
	assigned := applySequentialSlotMapping(sequentialSlotPointers(&p), []string{"model-a", "model-b"})

	if assigned != 2 {
		t.Fatalf("expected 2 assigned slots, got %d", assigned)
	}
	if p.OpusModel != "model-a" || p.SonnetModel != "model-b" {
		t.Fatalf("first slots not assigned sequentially: %+v", p)
	}
	if p.HaikuModel != "" || p.CustomModelID != "" {
		t.Fatalf("unassigned trailing slots should be cleared: %+v", p)
	}
}

func TestMapAutoRefreshesModelPoolAndPreservesOneMSlots(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := newMockGatewayServer(t, []string{"model-a", "model-b", "model-c", "model-d"}, false)

	cfg := &provider.Config{
		ActiveProvider: "mock",
		Providers: map[string]provider.Provider{
			"mock": {
				Name:        "mock",
				Type:        "openai",
				Endpoint:    server.URL + "/v1",
				APIKey:      "test-key",
				OpusModel:   "old-opus[1m]",
				SonnetModel: "old-sonnet",
				Env: map[string]string{
					autoCompactWindowEnv: "1000000",
				},
			},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("failed to save config: %v", err)
	}

	if err := runMapAuto([]string{"mock"}); err != nil {
		t.Fatalf("runMapAuto failed: %v", err)
	}

	updated, err := config.Load()
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	p := updated.Providers["mock"]

	if p.Model != "model-a,model-b,model-c,model-d" {
		t.Fatalf("expected refreshed model pool, got %q", p.Model)
	}
	if p.OpusModel != "model-a[1m]" {
		t.Fatalf("expected opus 1M preference to be preserved, got %q", p.OpusModel)
	}
	if p.SonnetModel != "model-b" || p.HaikuModel != "model-c" || p.CustomModelID != "model-d" {
		t.Fatalf("unexpected slot mapping: %+v", p)
	}
	if p.Env[autoCompactWindowEnv] != "1000000" {
		t.Fatalf("expected auto compact window env to remain enabled, got %+v", p.Env)
	}
}

func testProviderWithOldMappings() provider.Provider {
	return provider.Provider{
		OpusModel:     "old-opus",
		SonnetModel:   "old-sonnet",
		HaikuModel:    "old-haiku",
		CustomModelID: "old-custom",
	}
}
