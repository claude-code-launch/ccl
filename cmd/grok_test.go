package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func useFakeGrok(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "grok-bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "grok"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, name), data, 0o700); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(bin, "trace.jsonl")
	t.Setenv("PATH", bin)
	t.Setenv("CCL_TEST_FAKE_GROK", "1")
	t.Setenv("CCL_FAKE_TRACE", trace)
	return trace
}

func TestRunGrokAgentLaunchesLoopbackRuntimeAndForwardsArgs(t *testing.T) {
	saveGrokFixture(t, &provider.Config{
		ActiveProvider: "gw",
		BypassMode:     true,
		Providers: map[string]provider.Provider{
			"gw": {
				Name:        "gw",
				Type:        "openai",
				Endpoint:    "https://gw.invalid/v1",
				APIKey:      "upstream-secret",
				Model:       "claude-sonnet-4-6,claude-opus-4-6",
				EffortLevel: "high",
				Env:         map[string]string{"GROK_MODELS_LIST_URL": "https://override.invalid/models", "GROK_AUTH_PROVIDER_COMMAND": "invalid"},
			},
		},
	})
	tracePath := useFakeGrok(t)
	t.Setenv("XAI_API_KEY", "ambient-xai-key")
	t.Setenv("GROK_MODELS_BASE_URL", "https://ambient.invalid/v1")

	if err := runGrok([]string{"--agent", "fixture", "-p", "hello grok"}, ""); err != nil {
		t.Fatalf("runGrok: %v", err)
	}

	traces := readFakeTraces(t, tracePath)
	if len(traces) != 1 {
		t.Fatalf("traces = %+v", traces)
	}
	got := traces[0]
	wantArgs := []string{"--always-approve", "--agent", "fixture", "-p", "hello grok"}
	if !reflect.DeepEqual(got.Args, wantArgs) {
		t.Fatalf("args = %v, want %v", got.Args, wantArgs)
	}
	if !strings.HasPrefix(got.GrokBase, "http://127.0.0.1:") || !strings.HasSuffix(got.GrokBase, "/v1") {
		t.Fatalf("GROK_MODELS_BASE_URL = %q", got.GrokBase)
	}
	if !strings.HasPrefix(got.XAIKey, "ccl-") || got.XAIKey == "upstream-secret" || got.XAIKey == "ambient-xai-key" {
		t.Fatalf("XAI_API_KEY = %q, want isolated per-session key", got.XAIKey)
	}
	if got.GrokAuth == "" || got.AuthContents != "{}\n" {
		t.Fatal("GROK_AUTH_PATH was not set to isolate ~/.grok/auth.json")
	}
	if got.ListURL != "" || got.AuthProvider != "" {
		t.Fatal("provider environment bypassed the loopback")
	}
	if _, err := os.Stat(got.GrokAuth); !os.IsNotExist(err) {
		t.Fatal("launch auth file was not removed")
	}
	var overlay struct {
		Models   map[string]any  `json:"models"`
		Features map[string]bool `json:"features"`
	}
	if err := json.Unmarshal([]byte(got.GrokCfg), &overlay); err != nil {
		t.Fatalf("decode GROK_CONFIG %q: %v", got.GrokCfg, err)
	}
	if overlay.Models["default"] != "claude-sonnet-4-6" || overlay.Models["session_summary"] != "claude-sonnet-4-6" || overlay.Models["default_reasoning_effort"] != "high" {
		t.Fatalf("GROK_CONFIG models = %+v", overlay.Models)
	}
	wantAllowed := []any{"claude-sonnet-4-6", "claude-opus-4-6"}
	if !reflect.DeepEqual(overlay.Models["allowed_models"], wantAllowed) {
		t.Fatalf("GROK_CONFIG allowed_models = %#v, want %#v", overlay.Models["allowed_models"], wantAllowed)
	}
	if !overlay.Features["subagent_model_inheritance"] {
		t.Fatalf("GROK_CONFIG features = %+v, want subagent_model_inheritance=true", overlay.Features)
	}
}

func TestRunGrokAgentRejectsClaudeSubscriptionAccount(t *testing.T) {
	saveGrokFixture(t, &provider.Config{
		ActiveProvider: "work",
		Providers: map[string]provider.Provider{
			"work": {Name: "work", Type: "anthropic", Endpoint: "oauth://claude", OAuthProvider: "claude", OAuthAccountCredential: "0123456789abcdef0123456789abcdef"},
		},
	})
	_ = useFakeGrok(t)
	err := runGrok([]string{"-p", "hi"}, "")
	if err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("expected Claude account rejection for Grok Build, got: %v", err)
	}
}

func TestGrokBypassAliasesAndSubcommands(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{[]string{"-p", "hi"}, []string{"--always-approve", "-p", "hi"}},
		{[]string{"-p", "mcp"}, []string{"--always-approve", "-p", "mcp"}},
		{[]string{"-p", "-v"}, []string{"--always-approve", "-p", "-v"}},
		{[]string{"--single", "update"}, []string{"--always-approve", "--single", "update"}},
		{[]string{"--prompt-file", "update"}, []string{"--always-approve", "--prompt-file", "update"}},
		{[]string{"--prompt-json", "[]"}, []string{"--always-approve", "--prompt-json", "[]"}},
		{[]string{"-s", "update", "hello"}, []string{"--always-approve", "-s", "update", "hello"}},
		{[]string{"--worktree-ref", "main", "hello"}, []string{"--always-approve", "--worktree-ref", "main", "hello"}},
		{[]string{"--", "mcp"}, []string{"--always-approve", "--", "mcp"}},
	} {
		if got := applyGrokBypassMode(tc.in, true); !slices.Equal(got, tc.want) {
			t.Fatalf("applyGrokBypassMode(%v) = %v, want %v", tc.in, got, tc.want)
		}
		if isLocalGrokInvocation(tc.in) {
			t.Fatalf("isLocalGrokInvocation(%v) = true, want false", tc.in)
		}
	}
	for _, args := range [][]string{
		{"--always-approve", "-p", "hi"},
		{"--yolo", "-p", "hi"},
		{"--permission-mode", "plan"},
		{"--permission-mode=auto"},
		{"models"},
		{"--reasoning-effort", "high", "models"},
		{"--allow", "Bash(git:*)", "--deny", "Write", "--rules", "strict", "--system-prompt-override", "sys", "inspect", "--json"},
		{"doctor"},
		{"mcp", "list"},
		{"--debug", "mcp", "list"},
		{"--cwd", "/tmp", "inspect", "--json"},
		{"-v"},
		{"--help"},
		{"models", "--help"},
	} {
		if res := applyGrokBypassMode(args, true); !slices.Equal(res, args) {
			t.Fatalf("applyGrokBypassMode(%v) = %v", args, res)
		}
	}
	if !isLocalGrokInvocation([]string{"--reasoning-effort", "high", "mcp", "list"}) {
		t.Fatal("expected `--reasoning-effort high mcp list` to be classified as a local Grok subcommand")
	}
}

type fakeGrokTrace struct {
	Args         []string
	GrokBase     string
	XAIKey       string
	GrokCfg      string
	GrokAuth     string
	AuthContents string
	ListURL      string
	AuthProvider string
}

func fakeGrokProcess() int {
	trace := fakeGrokTrace{Args: os.Args[1:], GrokBase: os.Getenv("GROK_MODELS_BASE_URL"),
		XAIKey: os.Getenv("XAI_API_KEY"), GrokCfg: os.Getenv("GROK_CONFIG"), GrokAuth: os.Getenv("GROK_AUTH_PATH"),
		ListURL: os.Getenv("GROK_MODELS_LIST_URL"), AuthProvider: os.Getenv("GROK_AUTH_PROVIDER_COMMAND")}
	if trace.GrokAuth != "" {
		raw, err := os.ReadFile(trace.GrokAuth)
		if err != nil {
			return 1
		}
		trace.AuthContents = string(raw)
	}
	raw, err := json.Marshal(trace)
	if err != nil {
		return 1
	}
	if err := os.WriteFile(os.Getenv("CCL_FAKE_TRACE"), raw, 0o600); err != nil {
		return 1
	}
	return 0
}

func readFakeTraces(t *testing.T, path string) []fakeGrokTrace {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var trace fakeGrokTrace
	if err := json.Unmarshal(raw, &trace); err != nil {
		t.Fatal(err)
	}
	return []fakeGrokTrace{trace}
}

func saveGrokFixture(t *testing.T, cfg *provider.Config) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestLeadingProviderPreservesGrokArguments(t *testing.T) {
	args := []string{"--provider", "gateway", "--", "--agent", "review", "-p", "hello"}
	name, rest, handled, err := parseLaunchOptions(args)
	if err != nil || !handled || name != "gateway" || !slices.Equal(rest, args[3:]) {
		t.Fatalf("parseLaunchOptions: %q %v %v %v", name, rest, handled, err)
	}
	_, rest, handled, err = parseLaunchOptions(args[3:])
	if err != nil || handled || !slices.Equal(rest, args[3:]) {
		t.Fatalf("Grok --agent was consumed: %v %v %v", rest, handled, err)
	}
}

func TestLegacyDefaultAccountRequiresExplicitSwitch(t *testing.T) {
	saveGrokFixture(t, &provider.Config{ActiveProvider: provider.DefaultProvider, LegacyClaudeDefault: "old"})
	trace := useFakeGrok(t)
	t.Setenv("GROK_MODELS_BASE_URL", "https://native.invalid/v1")
	t.Setenv("GROK_AUTH_PATH", "")
	if err := runGrok(nil, ""); err == nil || !strings.Contains(err.Error(), "ccl use default") {
		t.Fatalf("retired default account: %v", err)
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("retired default launched Grok Build")
	}
	if err := runProviderUse(provider.DefaultProvider); err != nil {
		t.Fatal(err)
	}
	if err := runGrok([]string{"--version"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := readFakeTraces(t, trace)[0]; got.GrokBase != "https://native.invalid/v1" || got.GrokAuth != "" {
		t.Fatal("native default received a CCL session")
	}
}

func TestResolveRootGrokArgsStripsGrokEscapeAfterProvider(t *testing.T) {
	for _, tc := range []struct {
		in       []string
		wantName string
		wantArgs []string
		wantDisp bool
	}{
		{[]string{"grok", "doctor"}, "", []string{"doctor"}, true},
		{[]string{"--provider", "gw", "grok", "doctor"}, "gw", []string{"doctor"}, true},
		{[]string{"--provider=gw", "grok", "update"}, "gw", []string{"update"}, true},
		{[]string{"--provider", "gw", "--", "grok"}, "gw", []string{"grok"}, true},
		{[]string{"doctor"}, "", nil, false},
		{[]string{"--version"}, "", nil, false},
		{[]string{"-v"}, "", []string{"-v"}, true},
	} {
		name, rest, dispatch, err := resolveRootGrokArgs(tc.in)
		if err != nil || dispatch != tc.wantDisp || name != tc.wantName || !slices.Equal(rest, tc.wantArgs) {
			t.Fatalf("resolveRootGrokArgs(%v) = (%q, %v, %v, %v), want (%q, %v, %v, nil)",
				tc.in, name, rest, dispatch, err, tc.wantName, tc.wantArgs, tc.wantDisp)
		}
	}
}

func TestLocalGrokSubcommandsSkipProxyStartup(t *testing.T) {
	saveGrokFixture(t, &provider.Config{
		ActiveProvider: "gw",
		BypassMode:     true,
		Providers: map[string]provider.Provider{
			"gw": {
				Name:     "gw",
				Type:     "openai",
				Endpoint: "https://gw.invalid/v1",
				APIKey:   "upstream-secret",
				Model:    "gpt-5.4",
			},
		},
	})
	tracePath := useFakeGrok(t)
	t.Setenv("GROK_MODELS_BASE_URL", "https://ambient.invalid/v1")

	if err := runGrok([]string{"--debug", "mcp", "list"}, ""); err != nil {
		t.Fatalf("runGrok(mcp list): %v", err)
	}
	got := readFakeTraces(t, tracePath)[0]
	if !slices.Equal(got.Args, []string{"--debug", "mcp", "list"}) {
		t.Fatalf("args = %v, want [--debug mcp list]", got.Args)
	}
	if got.GrokAuth != "" || strings.HasPrefix(got.GrokBase, "http://127.0.0.1:") {
		t.Fatalf("local mcp list started loopback session: base=%q auth=%q", got.GrokBase, got.GrokAuth)
	}

	// Explicit invalid --provider must still fail even on a local subcommand.
	if err := runGrok([]string{"mcp", "list"}, "missing-provider"); err == nil {
		t.Fatal("expected explicit missing --provider to fail")
	}
}
