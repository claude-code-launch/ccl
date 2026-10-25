package webui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A conversation of the page is followed from where it stands, and what its
// own turns write to the transcript is not shown twice.
func TestMirrorSkipsThePagesOwnTurns(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	workspace := t.TempDir()
	real, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(config, "projects", nonAlphanumeric.ReplaceAllString(real, "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeID := "5d0c1f7e-2a43-4b8e-9d51-0e6f3a2b7c11"
	transcript := filepath.Join(dir, claudeID+".jsonl")
	write := func(lines ...string) {
		t.Helper()
		file, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		for _, line := range lines {
			if _, err := file.WriteString(line + "\n"); err != nil {
				t.Fatal(err)
			}
		}
	}
	prompt := func(text string) string {
		return `{"type":"user","message":{"role":"user","content":"` + text + `"},"timestamp":"2026-10-22T09:00:00Z"}`
	}
	write(prompt("from the page"))

	server, err := New(Config{Workspace: workspace, StoreRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	session, err := server.store.Create(Meta{Title: "t", Workspace: workspace, ClaudeSessionID: claudeID})
	if err != nil {
		t.Fatal(err)
	}
	prompts := func() []string {
		t.Helper()
		events, err := session.Events(0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, event := range events {
			if event.Type == EventUser {
				out = append(out, string(event.Payload))
			}
		}
		return out
	}

	// History the page already shows is not replayed.
	server.syncMirror(session)
	if got := prompts(); len(got) != 0 {
		t.Fatalf("first look replayed %v", got)
	}
	write(prompt("from a terminal"))
	server.syncMirror(session)
	if got := prompts(); len(got) != 1 || !strings.Contains(got[0], "from a terminal") {
		t.Fatalf("terminal prompt = %v", got)
	}

	// A turn of the page writes to the transcript too; its events come from
	// its own stream.
	server.pauseMirror(session)
	write(prompt("the page's next turn"))
	server.syncMirror(session)
	write(prompt("the terminal again"))
	server.syncMirror(session)
	got := prompts()
	if len(got) != 2 || !strings.Contains(got[1], "the terminal again") {
		t.Fatalf("prompts after the page's turn = %v", got)
	}
	if meta := session.Meta(); meta.Turns != 2 {
		t.Fatalf("turns = %d", meta.Turns)
	}
}
