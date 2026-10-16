package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestAcpHelp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out, err := executeCommand(RootCmd(), "acp", "--help")
	if err != nil {
		t.Fatalf("acp --help: %v", err)
	}
	if !contains(out, "acp") || !contains(out, "ccl use --acp") {
		t.Fatalf("expected help to explain the shared provider selection, got:\n%s", out)
	}
	if contains(out, `"jsonrpc"`) {
		t.Fatalf("acp --help must not start JSON-RPC, got:\n%s", out)
	}
	if contains(out, "acp-permission") {
		t.Fatalf("acp --help must not list hidden acp-permission helper, got:\n%s", out)
	}
	if !contains(out, "--model") || !contains(out, "--effort") {
		t.Fatalf("acp --help must document --model and --effort, got:\n%s", out)
	}
	if contains(out, "provider [name]") {
		t.Fatalf("acp help must not expose a separate provider manager, got:\n%s", out)
	}
}

func TestAcpPermissionIsRetired(t *testing.T) {
	if isCclCommand("acp-permission") {
		t.Fatal("retired acp-permission must not be a cobra command")
	}
	for _, command := range RootCmd().Commands() {
		if command.Name() == "acp-permission" {
			t.Fatal("acp-permission appeared in cobra command list")
		}
	}
}

func TestACPDoesNotExposeSeparateProviderManager(t *testing.T) {
	for _, command := range acpCmd.Commands() {
		if command.Name() == "provider" {
			t.Fatal("ccl acp still exposes a separate provider manager")
		}
	}
}

func TestParseACPSessionChoices(t *testing.T) {
	choices, err := parseACPSessionChoices(" opus ", "MAX")
	if err != nil || choices != (acpSessionChoices{Model: "opus", Effort: "max"}) {
		t.Fatalf("choices = %+v, %v", choices, err)
	}
	if _, err := parseACPSessionChoices("", "extreme"); err == nil || !strings.Contains(err.Error(), "xhigh") {
		t.Fatalf("invalid effort error = %v", err)
	}
	if choices, err := parseACPSessionChoices("", ""); err != nil || choices != (acpSessionChoices{}) {
		t.Fatalf("no choices = %+v, %v", choices, err)
	}
}

// The choices ride on every generation's SDK options, whichever provider or
// the default selector a lease runs.
func TestACPLeaseOptionsCarryTheSessionChoices(t *testing.T) {
	manager := newACPLaunchManagerWith(func() (acpProviderSnapshot, error) {
		return acpProviderSnapshot{name: "a", provider: provider.Provider{Name: "a"}}, nil
	}, func(provider.Provider) (acpPreparedLaunch, error) {
		return &fakeACPPreparedLaunch{}, nil
	})
	defer manager.Close()
	manager.choices = acpSessionChoices{Model: "opus", Effort: "max"}
	lease, err := manager.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	options, err := lease.Options(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(options, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["model"]) != `"opus"` || string(got["effort"]) != `"max"` || got["settings"] == nil || got["env"] == nil {
		t.Fatalf("options = %s", options)
	}

	manager.choices = acpSessionChoices{}
	plain, err := lease.Options(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "model") || strings.Contains(string(plain), "effort") {
		t.Fatalf("options without choices = %s", plain)
	}
}
