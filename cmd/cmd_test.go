package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

func executeCommand(root *cobra.Command, args ...string) (string, error) {
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)

	// 执行命令
	executed, err := root.ExecuteC()
	if executed != nil {
		if helpFlag := executed.Flags().Lookup("help"); helpFlag != nil {
			_ = helpFlag.Value.Set("false")
			helpFlag.Changed = false
		}
	}
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

	for _, want := range []string{"  codex", "  oauth", "  bypass", "  log", "  ls", "  use", "  preview", "  set", "  provider", "  cloud"} {
		if !contains(out, want) {
			t.Fatalf("expected root help to contain %q, got:\n%s", want, out)
		}
	}
	// Old root shortcuts are gone entirely.
	for _, unwanted := range []string{"\n  conf", "\n  list", "\n  run", "\n  settings", "acp-permission",
		"\n  cp ", "\n  mv ", "\n  rm ", "\n  login", "\n  push", "\n  pull", "\n  tag ", "\n  status",
		"\n  import", "\n  auth", "\n  debug", "\n  map"} {
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

	for _, want := range []string{"ccl provider [command]", "  ls", "  cp", "  mv", "  rm", "  preview", "  set", "  use", "  models"} {
		if !contains(out, want) {
			t.Fatalf("expected provider help to contain %q, got:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"\n  list", "\n  map", "\n  env", "\n  effort"} {
		if contains(out, unwanted) {
			t.Fatalf("provider help should not contain removed command %q, got:\n%s", unwanted, out)
		}
	}
}

func TestProviderSubcommandAliasesExist(t *testing.T) {
	for _, args := range [][]string{
		{"provider", "models", "--help"},
		{"provider", "preview", "--help"},
		{"preview", "--help"},
		{"doctor", "--help"},
		{"version", "--help"},
		{"update", "--help"},
	} {
		if _, err := executeCommand(RootCmd(), args...); err != nil {
			t.Fatalf("expected %v to be registered, got error: %v", args, err)
		}
	}
	for _, c := range providerCmd.Commands() {
		switch c.Name() {
		case "env", "effort", "map":
			t.Fatalf("removed provider subcommand %q is still registered", c.Name())
		}
	}
}

// TestResolveProviderIgnoresShellCredentials pins S11: with no provider
// selected, keys exported in the shell for other tools must not become an
// implicit, billed Grok Build provider.
func TestResolveProviderIgnoresShellCredentials(t *testing.T) {
	saveGrokFixture(t, &provider.Config{})
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://token.sensenova.cn/v1")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-other-tool")
	t.Setenv("OPENAI_API_KEY", "sk-openai-other-tool")

	p, err := resolveProvider()
	if err == nil {
		t.Fatalf("resolveProvider() built a provider from the shell: %+v", p)
	}
	if !strings.Contains(err.Error(), "ccl use default") {
		t.Fatalf("error does not offer the subscription path: %v", err)
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
	enterDetectedReview(m, "model-a", "model-b")
	view := renderView(t, m)

	if !contains(view, locale.T("鉴权", "Auth")) {
		t.Fatalf("expected review page to include Auth row")
	}
	if !contains(view, "bearer") {
		t.Fatalf("expected review page to show bearer auth, got: %s", view)
	}
	if contains(view, "unknown") {
		t.Fatalf("review page should not show unknown auth, got: %s", view)
	}
}

func TestCredentialPageShowsAPIKeyPlaintext(t *testing.T) {
	p := provider.Provider{
		Name:     "plaintext-key",
		Type:     "anthropic",
		Endpoint: "https://open.bigmodel.cn/api/anthropic",
		APIKey:   "super-secret-api-key-1234567890",
	}
	m := NewAdvancedConfigModel(&p)
	view := renderView(t, m)

	for _, want := range []string{locale.T("端点 URL", "Endpoint URL"), "API Key", "‹ Retired ›"} {
		if !contains(view, want) {
			t.Fatalf("expected credential page to contain %q, got: %s", want, view)
		}
	}
	// The API key is now edited as plaintext in a multi-line textarea, so the
	// page shows the key verbatim instead of masking it.
	if !contains(view, p.APIKey) {
		t.Fatalf("credential page should show the API key in plaintext, got: %s", view)
	}
}

func TestPrintProvidersUsesCompactTableByDefault(t *testing.T) {
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
	if err := printProviders(buf, cfg, false, "empty", "Registered providers:"); err != nil {
		t.Fatalf("printProviders failed: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"Registered providers:", "NAME", "USED BY", "TYPE/AUTH", "MODELS", "beta", "Grok Build", "openai-chat/bearer", "4"} {
		if !contains(out, want) {
			t.Fatalf("expected compact output to contain %q, got:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"manual-opus", "sonnet", "O:manual-opus", "S:sonnet", "default (200K/1M)"} {
		if contains(out, unwanted) {
			t.Fatalf("compact output should not contain %q, got:\n%s", unwanted, out)
		}
	}
	for _, unwanted := range []string{"CONTEXT", "SLOTS", "EFFORT"} {
		if contains(out, unwanted) {
			t.Fatalf("compact output should not contain the %s column, got:\n%s", unwanted, out)
		}
	}
	if contains(out, "pool-a,pool-b") {
		t.Fatalf("compact output should not include full model pool, got:\n%s", out)
	}
}

func TestPrintProvidersShowsGrokSelection(t *testing.T) {
	cfg := &provider.Config{
		ActiveProvider: "normal-config",
		Providers: map[string]provider.Provider{
			"normal-config": {Name: "normal-config", Type: "anthropic"},
			"acp-config":    {Name: "acp-config", Type: "openai"},
			"unused":        {Name: "unused", Type: "anthropic"},
		},
	}

	buf := new(bytes.Buffer)
	if err := printProviders(buf, cfg, false, "empty", "Registered providers:"); err != nil {
		t.Fatalf("printProviders failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"normal-config  Grok Build", "acp-config     -", "unused         -"} {
		if !contains(out, want) {
			t.Fatalf("provider selection %q missing from:\n%s", want, out)
		}
	}
}

func TestProviderTypeAuthSummaryUsesCompactOAuthBackend(t *testing.T) {
	tests := []struct {
		name string
		p    provider.Provider
		want string
	}{
		{name: "autoclaw oauth", p: provider.Provider{Type: "autoclaw", OAuthProvider: "autoclaw"}, want: "autoclaw/oauth"},
		{name: "anthropic key", p: provider.Provider{Type: "anthropic"}, want: "anthropic/retired/unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := providerTypeAuthSummary(tt.p); got != tt.want {
				t.Fatalf("providerTypeAuthSummary() = %q, want %q", got, tt.want)
			}
		})
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

	for _, want := range []string{"beta", "Used By  : Grok Build", "Endpoint : https://example.com/v1", "Default  : auto (pool-a)", "Models   : 4", "Pool IDs : pool-a,pool-b,pool-c,pool-d"} {
		if !contains(out, want) {
			t.Fatalf("expected detailed output to contain %q, got:\n%s", want, out)
		}
	}
	if contains(out, "Slot IDs") {
		t.Fatalf("detailed output should not contain Slot IDs, got:\n%s", out)
	}
}

func TestCloudAndOAuthCommandTrees(t *testing.T) {
	for _, args := range [][]string{
		{"cloud", "--help"},
		{"cloud", "login", "--help"},
		{"cloud", "remote", "ls", "--help"},
		{"cloud", "status", "--help"},
		{"oauth", "--help"},
	} {
		out, err := executeCommand(RootCmd(), args...)
		if err != nil {
			t.Fatalf("expected %v to be registered, got error: %v\n%s", args, err, out)
		}
	}
	// The former/removed root commands are not registered at root.
	for _, name := range []string{"cp", "mv", "env", "effort", "models", "login", "push", "pull", "status", "key", "logout", "tag", "device"} {
		if isCclCommand(name) {
			t.Fatalf("root shortcut %q is still registered", name)
		}
	}
}

func TestObsoleteOAuthManagementCommandsAreRemoved(t *testing.T) {
	for _, command := range authCmd.Commands() {
		switch command.Name() {
		case "group", "import", "sync":
			t.Fatalf("obsolete oauth %s command is still registered", command.Name())
		}
	}
	for _, command := range rootCmd.Commands() {
		if command.Name() == "sync" {
			t.Fatal("obsolete root sync compatibility command is still registered")
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
		"only needs ccl cloud login",
		"Examples:",
		"ccl cloud login google-drive personal",
		"default: google-drive",
		"login [platform] [alias]",
	} {
		if !contains(out, want) {
			t.Fatalf("expected cloud login help to contain %q, got:\n%s", want, out)
		}
	}
}

func TestDoctorHelpDescribesProviderHealth(t *testing.T) {
	out, err := executeCommand(RootCmd(), "doctor", "--help")
	if err != nil {
		t.Fatalf("doctor --help: %v", err)
	}
	for _, want := range []string{
		"Show environment prerequisites",
		"Model pool (provider.Model) lists the models exposed by the provider runtime",
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
