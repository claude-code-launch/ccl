package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestProxyCommandDoesNotFallThroughToClaude(t *testing.T) {
	if !isCclCommand("proxy") {
		t.Fatal("proxy would launch Claude Code")
	}
	cmd := newProxyCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"--provider", "start", "stop", "status"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("proxy help missing %s", text)
		}
	}
}

func TestProxyRejectsAmbiguousProviderSelection(t *testing.T) {
	cmd := newProxyCommand()
	cmd.SetArgs([]string{"start", "provider1", "-p", "provider2"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("error=%v", err)
	}
}
