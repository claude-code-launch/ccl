package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestToolKind(t *testing.T) {
	tests := []struct {
		name string
		kind string
	}{
		{"Read", "read"},
		{"Edit", "edit"},
		{"Write", "edit"},
		{"NotebookEdit", "edit"},
		{"Bash", "execute"},
		{"Grep", "search"},
		{"Glob", "search"},
		{"WebFetch", "fetch"},
		{"TodoWrite", "think"},
		{"mcp__xcode__Read", "read"},
		{"Unknown", "other"},
	}
	for _, tc := range tests {
		if got := toolKind(tc.name); got != tc.kind {
			t.Errorf("toolKind(%q) = %q want %q", tc.name, got, tc.kind)
		}
	}
}

func TestToolDiffsEditUsesAbsolutePath(t *testing.T) {
	cwd := t.TempDir()
	diffs := toolDiffs(cwd, "Edit", json.RawMessage(`{"file_path":"a.txt","old_string":"old","new_string":"new"}`))
	if len(diffs) != 1 || diffs[0].Type != "diff" {
		t.Fatalf("diffs = %#v", diffs)
	}
	want := filepath.Join(cwd, "a.txt")
	if diffs[0].Path != want || diffs[0].OldText == nil || *diffs[0].OldText != "old" || diffs[0].NewText != "new" {
		t.Fatalf("diff = %#v want path %s", diffs[0], want)
	}
}

func TestToolDiffsWriteReadsExistingFile(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "a.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	diffs := toolDiffs(cwd, "Write", json.RawMessage(`{"file_path":"a.txt","contents":"new"}`))
	if len(diffs) != 1 || diffs[0].OldText == nil || *diffs[0].OldText != "old" || diffs[0].NewText != "new" {
		t.Fatalf("write diff = %#v", diffs)
	}
}

func TestPromptToClaudeContentImage(t *testing.T) {
	got := promptToClaudeContent("", []contentBlock{{
		Type:     "image",
		MimeType: "image/png",
		Data:     "abcd",
	}}, nil)
	if len(got) != 1 || got[0].Type != "image" || got[0].Source == nil {
		t.Fatalf("content = %#v", got)
	}
	if got[0].Source.Type != "base64" || got[0].Source.MediaType != "image/png" || got[0].Source.Data != "abcd" {
		t.Fatalf("source = %#v", got[0].Source)
	}
}
