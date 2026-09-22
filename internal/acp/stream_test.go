package acp

import (
	"strings"
	"testing"
)

func TestJoinTextPromptIgnoresNonText(t *testing.T) {
	got := joinTextPrompt([]contentBlock{
		{Type: "image", Text: "nope"},
		{Type: "text", Text: "hello "},
		{Type: "", Text: "world"},
		{Type: "resource", Resource: &resourceBlock{Text: " src"}},
		{Type: "resource_link", URI: "file:///tmp/x.go"},
	})
	if got != "hello world src/tmp/x.go" {
		t.Fatalf("joinTextPrompt = %q", got)
	}
}

func TestMapStopReason(t *testing.T) {
	tests := []struct {
		subtype string
		want    string
	}{
		{"success", stopEndTurn},
		{"error_max_turns", stopMaxTurnRequests},
		{"error_max_output_tokens", stopMaxTokens},
		{"max_tokens", stopMaxTokens},
	}
	for _, tc := range tests {
		got := mapStopReason(claudeStreamEvent{Subtype: tc.subtype})
		if got != tc.want {
			t.Errorf("subtype %q: got %q want %q", tc.subtype, got, tc.want)
		}
	}
}

func TestClaudeArgsUsesStreamJSONWithoutSkipPermissions(t *testing.T) {
	args := ClaudeArgs()
	joined := strings.Join(args, " ")
	for _, want := range []string{"--print", "--output-format stream-json", "--input-format stream-json", "--strict-mcp-config"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ClaudeArgs missing %q: %v", want, args)
		}
	}
	for _, a := range args {
		if a == "--dangerously-skip-permissions" {
			t.Fatalf("production ACP must not skip permissions: %v", args)
		}
	}
	args[0] = "mutated"
	if ClaudeArgs()[0] == "mutated" {
		t.Fatal("ClaudeArgs should copy")
	}
}
