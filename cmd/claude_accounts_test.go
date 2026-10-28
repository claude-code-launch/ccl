package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/claudeaccount"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

type fakeClaudeTrace struct {
	Args     []string `json:"args"`
	Profile  string   `json:"profile"`
	APIKey   string   `json:"apiKey"`
	Token    string   `json:"token"`
	Base     string   `json:"base"`
	Settings string   `json:"settings"`
}

// Invoked only by TestMain in the isolated fake executable copied below.
func fakeClaudeProcess() int {
	trace := fakeClaudeTrace{Args: os.Args[1:], Profile: os.Getenv("CLAUDE_CONFIG_DIR"),
		APIKey: os.Getenv("ANTHROPIC_API_KEY"), Token: os.Getenv("ANTHROPIC_AUTH_TOKEN"), Base: os.Getenv("ANTHROPIC_BASE_URL")}
	for i, arg := range trace.Args {
		if arg == "--settings" && i+1 < len(trace.Args) {
			data, _ := os.ReadFile(trace.Args[i+1])
			trace.Settings = string(data)
		}
	}
	file, err := os.OpenFile(os.Getenv("CCL_FAKE_TRACE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 1
	}
	_ = json.NewEncoder(file).Encode(trace)
	_ = file.Close()
	if len(trace.Args) >= 2 && trace.Args[0] == "auth" && trace.Args[1] == "login" {
		if os.Getenv("CCL_FAKE_LOGIN_FAIL") == "1" {
			return 1
		}
		_ = os.WriteFile(filepath.Join(trace.Profile, ".credentials.json"), []byte("fake-credential"), 0o600)
	}
	if len(trace.Args) >= 2 && trace.Args[0] == "auth" && trace.Args[1] == "status" && os.Getenv("CCL_FAKE_STATUS_EMAIL") != "" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"loggedIn": true, "authMethod": "claude.ai", "email": os.Getenv("CCL_FAKE_STATUS_EMAIL"),
		})
	}
	return 0
}

func useFakeClaude(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "Claude bin \u6d4b\u8bd5")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(filepath.Join(bin, name), os.O_CREATE|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(bin, "trace.jsonl")
	t.Setenv("PATH", bin)
	t.Setenv("CCL_TEST_FAKE_CLAUDE", "1")
	t.Setenv("CCL_FAKE_TRACE", trace)
	return trace
}

func readFakeTraces(t *testing.T, path string) []fakeClaudeTrace {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var traces []fakeClaudeTrace
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var trace fakeClaudeTrace
		if err := json.Unmarshal([]byte(line), &trace); err != nil {
			t.Fatal(err)
		}
		traces = append(traces, trace)
	}
	return traces
}

func TestLeadingLaunchOptionsPreserveClaudeArguments(t *testing.T) {
	tests := []struct {
		args     []string
		options  launchOptions
		forward  []string
		handled  bool
		hasError bool
	}{
		{[]string{"--provider", "a", "-p", "hello"}, launchOptions{provider: "a"}, []string{"-p", "hello"}, true, false},
		{[]string{"--provider=a", "--", "--provider", "upstream"}, launchOptions{provider: "a"}, []string{"--provider", "upstream"}, true, false},
		{[]string{"--account", "work", "resume"}, launchOptions{account: "work"}, []string{"resume"}, true, false},
		{[]string{"--account=work", "--provider", "default"}, launchOptions{provider: "default", account: "work"}, []string{}, true, false},
		{[]string{"-p", "hello", "--provider", "upstream"}, launchOptions{}, []string{"-p", "hello", "--provider", "upstream"}, false, false},
		{[]string{"--provider"}, launchOptions{}, nil, true, true},
		{[]string{"--provider", "a", "--provider", "b"}, launchOptions{provider: "a"}, nil, true, true},
		{[]string{"--provider", "a", "--account", "work"}, launchOptions{provider: "a", account: "work"}, nil, true, true},
	}
	for _, test := range tests {
		t.Run(fmt.Sprint(test.args), func(t *testing.T) {
			options, forward, handled, err := parseLaunchOptions(test.args)
			if handled != test.handled || (err != nil) != test.hasError || options != test.options || !reflect.DeepEqual(forward, test.forward) {
				t.Fatalf("got %+v %q %v %v", options, forward, handled, err)
			}
		})
	}
}

func TestClaudeAccountLoginSelectionAndOneShotProvider(t *testing.T) {
	saveToggleFixture(t, &provider.Config{ActiveProvider: "a", Providers: map[string]provider.Provider{
		"a": {Name: "a", Type: "anthropic", Endpoint: "https://a.invalid", APIKey: "fake-a", Model: "model-a"},
		"b": {Name: "b", Type: "anthropic", Endpoint: "https://b.invalid", APIKey: "fake-b", Model: "model-b"},
	}})
	tracePath := useFakeClaude(t)
	t.Setenv("ANTHROPIC_API_KEY", "stale-fake-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "stale-fake-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://stale.invalid")
	var output bytes.Buffer
	for _, name := range []string{"work", "personal"} {
		command := newAuthCommand()
		command.SetOut(&output)
		command.SetArgs([]string{"claude", name})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	traces := readFakeTraces(t, tracePath)
	if len(traces) != 2 || traces[0].Profile == traces[1].Profile {
		t.Fatalf("logins did not use separate profiles: %+v", traces)
	}
	for _, trace := range traces {
		if !reflect.DeepEqual(trace.Args, []string{"auth", "login", "--claudeai"}) || trace.APIKey != "" || trace.Token != "" || trace.Base != "" {
			t.Fatalf("unsafe login environment: %+v", trace)
		}
	}
	if cfg := loadToggleConfig(t); cfg.ActiveProvider != provider.DefaultProvider {
		t.Fatalf("login did not select Claude default: %q", cfg.ActiveProvider)
	}
	if err := runClaudeWithOptions([]string{"auth", "status", "--json"}, launchOptions{account: "work"}); err != nil {
		t.Fatal(err)
	}
	if err := runClaude([]string{"auth", "status", "--json"}); err != nil {
		t.Fatal(err)
	}
	traces = readFakeTraces(t, tracePath)
	if traces[2].Profile != traces[0].Profile || traces[3].Profile != traces[1].Profile {
		t.Fatalf("one-shot or selected account used wrong profile: %+v", traces)
	}
	if err := config.Update(func(cfg *provider.Config) error { cfg.ActiveProvider = "a"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := runClaudeWithOptions([]string{"-p", "fixture prompt"}, launchOptions{provider: "b"}); err != nil {
		t.Fatal(err)
	}
	traces = readFakeTraces(t, tracePath)
	last := traces[len(traces)-1]
	if !strings.Contains(last.Settings, "fake-b") || !strings.Contains(last.Settings, "https://b.invalid") || !reflect.DeepEqual(last.Args[len(last.Args)-2:], []string{"-p", "fixture prompt"}) {
		t.Fatalf("one-shot provider or Claude arguments not applied: %+v", last)
	}
	if cfg := loadToggleConfig(t); cfg.ActiveProvider != "a" {
		t.Fatalf("one-shot launch changed active provider: %q", cfg.ActiveProvider)
	}
	if err := runClaudeWithOptions([]string{"auth", "status"}, launchOptions{account: "work"}); err != nil {
		t.Fatal(err)
	}
	if cfg := loadToggleConfig(t); cfg.ActiveProvider != "a" {
		t.Fatalf("one-shot account changed active provider: %q", cfg.ActiveProvider)
	}
	store, _ := claudeaccount.Open()
	selected, _, err := store.Resolve("")
	if err != nil || selected != "personal" {
		t.Fatalf("one-shot launch changed account selection: %q %v", selected, err)
	}
	if err := selectClaudeAccount(context.Background(), &output, "work"); err != nil {
		t.Fatal(err)
	}
	if err := runClaude([]string{"auth", "status"}); err != nil {
		t.Fatal(err)
	}
	traces = readFakeTraces(t, tracePath)
	if traces[len(traces)-1].Profile != traces[0].Profile {
		t.Fatal("persistent account selection did not take effect")
	}
}

func TestFailedClaudeAccountLoginLeavesSelectionUntouched(t *testing.T) {
	saveToggleFixture(t, &provider.Config{ActiveProvider: "a", Providers: map[string]provider.Provider{"a": {Name: "a"}}})
	useFakeClaude(t)
	t.Setenv("CCL_FAKE_LOGIN_FAIL", "1")
	if err := loginClaudeAccount(context.Background(), io.Discard, "work"); err == nil {
		t.Fatal("failed login succeeded")
	}
	if got := loadToggleConfig(t).ActiveProvider; got != "a" {
		t.Fatalf("failed login changed provider to %q", got)
	}
	store, _ := claudeaccount.Open()
	names, _, err := store.List()
	if err != nil || len(names) != 0 {
		t.Fatalf("failed login was registered: %v %v", names, err)
	}
}

func TestUnnamedClaudeLoginUsesReportedEmail(t *testing.T) {
	saveToggleFixture(t, &provider.Config{ActiveProvider: provider.DefaultProvider})
	trace := useFakeClaude(t)
	t.Setenv("CCL_FAKE_STATUS_EMAIL", "user@example.invalid")
	var output bytes.Buffer
	command := newAuthCommand()
	command.SetOut(&output)
	command.SetArgs([]string{"claude"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	store, _ := claudeaccount.Open()
	name, _, err := store.Resolve("")
	if err != nil || name != "user@example.invalid" {
		t.Fatalf("unnamed account selection = %q: %v", name, err)
	}
	traces := readFakeTraces(t, trace)
	if len(traces) != 2 || !reflect.DeepEqual(traces[1].Args, []string{"auth", "status", "--json"}) || traces[0].Profile != traces[1].Profile {
		t.Fatalf("status lookup did not use the new login's profile: %+v", traces)
	}
}
