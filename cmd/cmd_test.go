package cmd

import (
	"bytes"
	"context"
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

	out, err := executeCommand(RootCmd(), "ls")
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

func TestRootHelpUsesNewCommandNames(t *testing.T) {
	out, err := executeCommand(RootCmd(), "--help")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{"  oauth", "  bypass", "  debug", "  ls", "  cp", "  mv", "  rm", "  preview", "  provider", "  login", "  push", "  pull", "  tag", "  status"} {
		if !contains(out, want) {
			t.Fatalf("expected root help to contain %q, got:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"\n  conf", "\n  list", "\n  run", "\n  settings"} {
		if contains(out, unwanted) {
			t.Fatalf("root help should not contain old command %q, got:\n%s", unwanted, out)
		}
	}
}

func TestProviderHelpUsesShortSubcommands(t *testing.T) {
	out, err := executeCommand(RootCmd(), "provider", "--help")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{"ccl provider [command]", "  ls", "  cp", "  mv", "  rm", "  preview", "  set", "  use", "  map", "  models", "  env"} {
		if !contains(out, want) {
			t.Fatalf("expected provider help to contain %q, got:\n%s", want, out)
		}
	}
	if contains(out, "\n  list") {
		t.Fatalf("provider help should prefer ls over list, got:\n%s", out)
	}
}

func TestProviderSubcommandAliasesExist(t *testing.T) {
	for _, args := range [][]string{
		{"provider", "map", "--help"},
		{"provider", "models", "--help"},
		{"provider", "env", "--help"},
		{"doctor", "--help"},
		{"version", "--help"},
		{"update", "--help"},
	} {
		if _, err := executeCommand(RootCmd(), args...); err != nil {
			t.Fatalf("expected %v to be registered, got error: %v", args, err)
		}
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

func TestProviderAuthLabelAcceptsDisplayProtocolLabels(t *testing.T) {
	testCases := []struct {
		name string
		p    provider.Provider
		want string
	}{
		{
			name: "openai chat display label",
			p:    provider.Provider{Type: "openai(chat)"},
			want: "bearer",
		},
		{
			name: "openai agent display label",
			p:    provider.Provider{Type: "openai(agent)"},
			want: "bearer",
		},
		{
			name: "anthropic bearer",
			p:    provider.Provider{Type: "anthropic", AnthropicAuth: "bearer"},
			want: "bearer",
		},
		{
			name: "anthropic x-api-key",
			p:    provider.Provider{Type: "anthropic"},
			want: "x-api-key",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerAuthLabel(tc.p); got != tc.want {
				t.Fatalf("providerAuthLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEndpointPathIsEmpty(t *testing.T) {
	if !endpointPathIsEmpty("https://token.sensenova.cn") {
		t.Fatalf("expected bare endpoint path to be empty")
	}
	if endpointPathIsEmpty("https://token.sensenova.cn/v1") {
		t.Fatalf("expected /v1 endpoint path to be non-empty")
	}
}

func TestReviewPageShowsBearerForOpenAIChatDisplayLabel(t *testing.T) {
	p := provider.Provider{
		Name:     "display-label",
		Type:     "openai(chat)",
		Endpoint: "https://example.com/v1",
	}
	m := NewAdvancedConfigModel(&p)
	m.page = 4
	view := m.View().Content

	if !contains(view, "Auth") {
		t.Fatalf("expected review page to include Auth row")
	}
	if !contains(view, "bearer") {
		t.Fatalf("expected review page to show bearer auth, got: %s", view)
	}
	if contains(view, "unknown") {
		t.Fatalf("review page should not show unknown auth, got: %s", view)
	}
}

func TestCredentialPageMasksAPIKey(t *testing.T) {
	p := provider.Provider{
		Name:     "masked-key",
		Type:     "anthropic",
		Endpoint: "https://open.bigmodel.cn/api/anthropic",
		APIKey:   "super-secret-api-key-1234567890",
	}
	m := NewAdvancedConfigModel(&p)
	view := m.View().Content

	for _, want := range []string{"Endpoint URL", "API Key", "Protocol: Anthropic"} {
		if !contains(view, want) {
			t.Fatalf("expected credential page to contain %q, got: %s", want, view)
		}
	}
	if contains(view, p.APIKey) {
		t.Fatalf("credential page should mask API key, got: %s", view)
	}
}

func TestPrintProvidersUsesCompactTableByDefault(t *testing.T) {
	cfg := &provider.Config{
		ActiveProvider: "beta",
		Providers: map[string]provider.Provider{
			"beta": {
				Name:        "beta",
				Type:        "openai",
				Endpoint:    "https://example.com/v1",
				Model:       "pool-a,pool-b,pool-c,pool-d",
				OpusModel:   "manual-opus",
				SonnetModel: "sonnet",
			},
		},
	}

	buf := new(bytes.Buffer)
	if err := printProviders(buf, cfg, false, "empty", "Registered providers:"); err != nil {
		t.Fatalf("printProviders failed: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"Registered providers:", "NAME", "TYPE", "AUTH", "MODELS", "SLOTS", "beta", "openai(chat)", "bearer", "4", "2/5"} {
		if !contains(out, want) {
			t.Fatalf("expected compact output to contain %q, got:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"manual-opus", "sonnet", "O:manual-opus", "S:sonnet"} {
		if contains(out, unwanted) {
			t.Fatalf("compact output should not contain %q, got:\n%s", unwanted, out)
		}
	}
	if contains(out, "pool-a,pool-b") {
		t.Fatalf("compact output should not include full model pool, got:\n%s", out)
	}
}

func TestPrintProvidersAllShowsFullModelPool(t *testing.T) {
	cfg := &provider.Config{
		ActiveProvider: "beta",
		Providers: map[string]provider.Provider{
			"beta": {
				Name:     "beta",
				Type:     "openai",
				Endpoint: "https://example.com/v1",
				Model:    "pool-a,pool-b,pool-c,pool-d",
			},
		},
	}

	buf := new(bytes.Buffer)
	if err := printProviders(buf, cfg, true, "empty", "Registered providers:"); err != nil {
		t.Fatalf("printProviders failed: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"* beta", "Endpoint : https://example.com/v1", "Models   : 4", "Pool     : pool-a,pool-b,pool-c,pool-d"} {
		if !contains(out, want) {
			t.Fatalf("expected detailed output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestPrintProvidersHidesSingleAccountProvidersInAuthGroups(t *testing.T) {
	cfg := &provider.Config{
		Providers: map[string]provider.Provider{
			"gg": {
				Name:      "gg",
				Type:      "openai",
				AuthGroup: "gg",
			},
			"grok-a": {
				Name:                   "grok-a",
				Type:                   "openai",
				OAuthAccountCredential: "xai-a.json",
			},
			"grok-b": {
				Name:                   "grok-b",
				Type:                   "openai",
				OAuthAccountCredential: "xai-b.json",
			},
			"grok-c": {
				Name:                   "grok-c",
				Type:                   "openai",
				OAuthAccountCredential: "xai-c.json",
			},
		},
		AuthGroups: map[string]provider.AuthGroup{
			"gg": {OAuthProvider: "grok", Credentials: []string{"xai-a.json", "XAI-B.JSON"}},
		},
	}

	for _, showAll := range []bool{false, true} {
		buf := new(bytes.Buffer)
		if err := printProviders(buf, cfg, showAll, "empty", "Registered providers:"); err != nil {
			t.Fatalf("printProviders(showAll=%t) failed: %v", showAll, err)
		}
		out := buf.String()
		for _, hidden := range []string{"grok-a", "grok-b"} {
			if contains(out, hidden) {
				t.Fatalf("group member %q should be hidden (showAll=%t):\n%s", hidden, showAll, out)
			}
		}
		for _, visible := range []string{"gg", "grok-c"} {
			if !contains(out, visible) {
				t.Fatalf("provider %q should remain visible (showAll=%t):\n%s", visible, showAll, out)
			}
		}
	}
}

func TestPrintProvidersDistinguishesGroupFromNormalByConfiguration(t *testing.T) {
	cfg := &provider.Config{
		Providers: map[string]provider.Provider{
			"ordinary": {
				Name: "ordinary",
				Type: "openai",
			},
			"shared-grok": {
				Name:      "shared-grok",
				Type:      "openai",
				AuthGroup: "grok-team",
			},
		},
	}

	buf := new(bytes.Buffer)
	if err := printProviders(buf, cfg, false, "empty", "Registered providers:"); err != nil {
		t.Fatalf("printProviders failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"KIND", "ordinary", "normal", "shared-grok", "group"} {
		if !contains(out, want) {
			t.Fatalf("expected provider output to contain %q, got:\n%s", want, out)
		}
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

func TestMapAutoRefreshesModelPoolWithoutTransferringOneMToUnknownModels(t *testing.T) {
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

	if err := runMapAuto(context.Background(), []string{"mock"}); err != nil {
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
	if p.OpusModel != "model-a" {
		t.Fatalf("expected unknown replacement model to lose 1M marker, got %q", p.OpusModel)
	}
	if p.SonnetModel != "model-b" || p.HaikuModel != "model-c" || p.CustomModelID != "model-d" {
		t.Fatalf("unexpected slot mapping: %+v", p)
	}
	if _, ok := p.Env[autoCompactWindowEnv]; ok {
		t.Fatalf("expected stale 1M compact window removed for unknown models, got %+v", p.Env)
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

func TestMapAutoClearsStaleContextPreset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := newMockGatewayServer(t, []string{"model-a", "model-b", "model-c", "model-d"}, false)

	cfg := &provider.Config{
		ActiveProvider: "mock",
		Providers: map[string]provider.Provider{
			"mock": {
				Name:      "mock",
				Type:      "openai",
				Endpoint:  server.URL + "/v1",
				APIKey:    "test-key",
				OpusModel: "old-opus[1m]",
				Env: map[string]string{
					maxContextTokensEnv:  maxContext1M,
					autoCompactWindowEnv: compactWindow1M,
				},
			},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := runMapAuto(context.Background(), []string{"mock"}); err != nil {
		t.Fatal(err)
	}
	updated, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := updated.Providers["mock"]
	// ccl declares no context size any more, so a preset written by an older
	// version is cleared and Claude Code sizes the session per slot.
	for _, key := range []string{maxContextTokensEnv, autoCompactWindowEnv, autoCompactPctEnv} {
		if value, ok := p.Env[key]; ok {
			t.Fatalf("stale %s survived map auto: %q", key, value)
		}
	}
}

func TestCloudAndOAuthCommandTrees(t *testing.T) {
	for _, args := range [][]string{
		{"cloud", "--help"},
		{"cloud", "login", "--help"},
		{"cloud", "remote", "ls", "--help"},
		{"cloud", "status", "--help"},
		{"cloud", "key", "export", "--help"},
		{"cloud", "device", "--help"},
		{"oauth", "sync", "--help"},
		{"oauth", "group", "--help"},
		{"oauth", "import", "--help"},
		// root compatibility aliases
		{"login", "--help"},
		{"push", "--help"},
		{"pull", "--help"},
		{"status", "--help"},
		{"key", "--help"},
		{"device", "--help"},
		{"sync", "--help"},
		{"logout", "--help"},
		{"tag", "--help"},
	} {
		out, err := executeCommand(RootCmd(), args...)
		if err != nil {
			t.Fatalf("expected %v to be registered, got error: %v\n%s", args, err, out)
		}
	}
}

func TestCloudLoginHelpDescribesPlatformsAndKeyModes(t *testing.T) {
	out, err := executeCommand(RootCmd(), "cloud", "login", "--help")
	if err != nil {
		t.Fatalf("cloud login --help: %v", err)
	}
	for _, want := range []string{
		"icloud",
		"google-drive",
		"Built-in Desktop OAuth",
		"no OAuth JSON required",
		"drive.appdata",
		"appDataFolder",
		"ccl cloud key export",
		"CCL_SYNC_PASSPHRASE",
		"--passphrase",
		"Examples:",
		"ccl cloud login google-drive personal",
	} {
		if !contains(out, want) {
			t.Fatalf("expected cloud login help to contain %q, got:\n%s", want, out)
		}
	}
	// Root compatibility alias should share the same command help.
	rootOut, err := executeCommand(RootCmd(), "login", "--help")
	if err != nil {
		t.Fatalf("login --help: %v", err)
	}
	if !contains(rootOut, "google-drive") || !contains(rootOut, "Built-in Desktop OAuth") {
		t.Fatalf("root login --help should describe google-drive login, got:\n%s", rootOut)
	}
}

func TestDoctorHelpDescribesProviderHealth(t *testing.T) {
	out, err := executeCommand(RootCmd(), "doctor", "--help")
	if err != nil {
		t.Fatalf("doctor --help: %v", err)
	}
	for _, want := range []string{
		"Show environment prerequisites",
		"Model pool (provider.Model) is an optional candidate list",
		"empty pool with filled slots is normal",
		"ccl cloud status",
	} {
		if !contains(out, want) {
			t.Fatalf("expected doctor help to contain %q, got:\n%s", want, out)
		}
	}
	// provider status/doctor entry points were removed.
	providerHelp, err := executeCommand(RootCmd(), "provider", "--help")
	if err != nil {
		t.Fatalf("provider --help: %v", err)
	}
	for _, unwanted := range []string{"\n  status", "\n  doctor"} {
		if contains(providerHelp, unwanted) {
			t.Fatalf("provider help should not list %q, got:\n%s", unwanted, providerHelp)
		}
	}
}

func TestDoctorConfiguredSlotCount(t *testing.T) {
	p := provider.Provider{OpusModel: "a", SonnetModel: "b"}
	if got := doctorConfiguredSlotCount(p); got != 2 {
		t.Fatalf("slot count = %d, want 2", got)
	}
	if got := doctorConfiguredSlotCount(provider.Provider{}); got != 0 {
		t.Fatalf("empty slot count = %d", got)
	}
}

func TestAccumulateAuthMetadataCounts(t *testing.T) {
	var counts authMetadataCounts
	accumulateAuthMetadata(&counts, map[string]any{
		"unavailable":      true,
		"status":           "error",
		"status_message":   "quota exhausted",
		"quota":            map[string]any{"exceeded": true},
		"next_retry_after": "2026-07-28T12:00:00Z",
	}, false, "", "", false, false)
	if counts != (authMetadataCounts{Unavailable: 1, Status: 1, StatusMessage: 1, Quota: 1, NextRetryAfter: 1}) {
		t.Fatalf("metadata counts = %+v", counts)
	}

	var runtimeOnly authMetadataCounts
	accumulateAuthMetadata(&runtimeOnly, nil, true, "error", "quota exhausted", true, true)
	if runtimeOnly != (authMetadataCounts{Unavailable: 1, Status: 1, StatusMessage: 1, Quota: 1, NextRetryAfter: 1}) {
		t.Fatalf("runtime fallback counts = %+v", runtimeOnly)
	}
}
