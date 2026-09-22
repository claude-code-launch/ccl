package cmd

import (
	"testing"
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
	if contains(out, "provider [name]") {
		t.Fatalf("acp help must not expose a separate provider manager, got:\n%s", out)
	}
}

func TestAcpPermissionIsHiddenIntercept(t *testing.T) {
	if isCclCommand("acp-permission") {
		t.Fatal("acp-permission must not be a cobra command; cmd.Execute intercepts it like statusline")
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
