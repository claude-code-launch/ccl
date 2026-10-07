package acp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestToolTitlePicksTheMostUsefulLabel covers the titles the client shows for
// the tools an agent uses most, including the truncation of a long command.
func TestToolTitlePicksTheMostUsefulLabel(t *testing.T) {
	long := strings.Repeat("x", 120)
	for name, testCase := range map[string]struct {
		tool  string
		input string
		want  string
	}{
		"edit uses the file base name": {
			tool: "Edit", input: `{"file_path":"/tmp/deep/nested/notes.md"}`,
			want: "Edit notes.md",
		},
		"write falls back to the path key": {
			tool: "Write", input: `{"path":"a/b/out.txt"}`,
			want: "Write out.txt",
		},
		"read uses the file base name": {
			tool: "Read", input: `{"file_path":"main.go"}`,
			want: "Read main.go",
		},
		// Only the three file tools get a decorated title; a suffixed name is
		// not recognized.
		"unknown suffix keeps the name": {
			tool: "read_file", input: `{"file_path":"main.go"}`,
			want: "read_file",
		},
		"bash uses the command": {
			tool: "Bash", input: `{"command":"go test ./..."}`,
			want: "go test ./...",
		},
		"bash truncates a long command": {
			tool: "Bash", input: `{"command":"` + long + `"}`,
			want: strings.Repeat("x", 80) + "...",
		},
		"unrecognized tool keeps its name": {
			tool: "WebFetch", input: `{"url":"https://example.com"}`,
			want: "WebFetch",
		},
		"mcp tool keeps the qualified name": {
			tool: "mcp__xcode__BuildProject", input: `{}`,
			want: "mcp__xcode__BuildProject",
		},
		"edit without a path keeps the name": {
			tool: "Edit", input: `{}`, want: "Edit",
		},
		"unnamed tool": {tool: "", input: `{}`, want: "tool"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := toolTitle(testCase.tool, json.RawMessage(testCase.input)); got != testCase.want {
				t.Fatalf("toolTitle(%q) = %q, want %q", testCase.tool, got, testCase.want)
			}
		})
	}

	// A non-JSON input must not panic or invent a title.
	if got := toolTitle("Bash", json.RawMessage("not json")); got != "Bash" {
		t.Fatalf("toolTitle() with malformed input = %q", got)
	}
}

func TestToolLocationsResolveTheReportedPath(t *testing.T) {
	cwd := "/work"
	for name, testCase := range map[string]struct {
		input string
		want  string
	}{
		"relative path":        {input: `{"file_path":"a/b.txt"}`, want: "/work/a/b.txt"},
		"absolute path":        {input: `{"file_path":"/etc/hosts"}`, want: "/etc/hosts"},
		"traversal is cleaned": {input: `{"file_path":"../secret"}`, want: "/secret"},
		"notebook path":        {input: `{"notebook_path":"nb.ipynb"}`, want: "/work/nb.ipynb"},
		"no path":              {input: `{"command":"ls"}`, want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			locations := toolLocations(cwd, "Read", json.RawMessage(testCase.input))
			if testCase.want == "" {
				if locations != nil {
					t.Fatalf("locations = %#v", locations)
				}
				return
			}
			if len(locations) != 1 || locations[0].Path != testCase.want {
				t.Fatalf("locations = %#v, want %q", locations, testCase.want)
			}
		})
	}
}

// TestToolDiffsWriteReportsThePreviousContents pins that a Write diff carries
// the old file when it exists, and no old side when it does not (or is too
// large to read).
func TestToolDiffsWriteReportsThePreviousContents(t *testing.T) {
	cwd := t.TempDir()
	newFile := json.RawMessage(`{"file_path":"fresh.txt","contents":"hello"}`)
	diffs := toolDiffs(cwd, "Write", newFile)
	if len(diffs) != 1 || diffs[0].OldText != nil || diffs[0].NewText != "hello" {
		t.Fatalf("new file diff = %#v", diffs)
	}
	if diffs[0].Path != filepath.Join(cwd, "fresh.txt") {
		t.Fatalf("diff path = %q", diffs[0].Path)
	}

	// "content" is accepted as an alias for "contents".
	aliased := toolDiffs(cwd, "Write", json.RawMessage(`{"file_path":"x.txt","content":"alias"}`))
	if len(aliased) != 1 || aliased[0].NewText != "alias" {
		t.Fatalf("aliased diff = %#v", aliased)
	}
	if toolDiffs(cwd, "Write", json.RawMessage(`{}`)) != nil {
		t.Fatal("a Write without a path produced a diff")
	}

	// An oversized existing file is not read into the diff.
	huge := filepath.Join(cwd, "huge.bin")
	if err := os.WriteFile(huge, bytes.Repeat([]byte("x"), (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	diffs = toolDiffs(cwd, "Write", json.RawMessage(`{"file_path":"huge.bin","contents":""}`))
	if len(diffs) != 1 || diffs[0].OldText != nil {
		t.Fatalf("oversized write diff = %#v", diffs)
	}

	// Unrelated tools produce no diff.
	if toolDiffs(cwd, "Bash", json.RawMessage(`{"command":"ls"}`)) != nil {
		t.Fatal("Bash produced a diff")
	}
	if toolDiffs(cwd, "Edit", json.RawMessage(`{"old_string":"a"}`)) != nil {
		t.Fatal("an Edit without a path produced a diff")
	}
}

func TestAbsPathHandlesEveryShape(t *testing.T) {
	for name, testCase := range map[string]struct{ cwd, path, want string }{
		"empty":                {"", "", ""},
		"relative with cwd":    {"/work", "a.txt", "/work/a.txt"},
		"relative without cwd": {"", "a.txt", "a.txt"},
		"absolute":             {"/work", "/etc/hosts", "/etc/hosts"},
		"cleaned":              {"/work", "a/../b.txt", "/work/b.txt"},
		"absolute cleaned":     {"/work", "/a/./b", "/a/b"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := absPath(testCase.cwd, testCase.path); got != testCase.want {
				t.Fatalf("absPath(%q, %q) = %q, want %q", testCase.cwd, testCase.path, got, testCase.want)
			}
		})
	}
}

// TestToolResultTextUnwrapsEveryShape covers the three tool result encodings
// Claude Code produces: a bare string, content blocks, and something else.
func TestToolResultTextUnwrapsEveryShape(t *testing.T) {
	for name, testCase := range map[string]struct {
		raw  string
		want string
	}{
		"empty":       {raw: "", want: ""},
		"bare string": {raw: `"done"`, want: "done"},
		"content blocks are concatenated": {
			raw:  `[{"type":"text","text":"one"},{"type":"text","text":"two"}]`,
			want: "onetwo",
		},
		"non-text blocks contribute nothing": {
			raw: `[{"type":"image"}]`, want: "",
		},
		"an object is returned verbatim": {
			raw: `{"exit":1}`, want: `{"exit":1}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := toolResultText(json.RawMessage(testCase.raw)); got != testCase.want {
				t.Fatalf("toolResultText(%q) = %q, want %q", testCase.raw, got, testCase.want)
			}
		})
	}
}

func TestRawAsAnyPreservesUndecodablePayloads(t *testing.T) {
	if rawAsAny(nil) != nil {
		t.Fatal("an empty payload was not nil")
	}
	if got := rawAsAny(json.RawMessage(`{"a":1}`)); got == nil {
		t.Fatal("an object decoded to nil")
	}
	// Malformed JSON is passed through untouched rather than dropped, so the
	// client still sees what the model sent.
	if got, ok := rawAsAny(json.RawMessage("{not json")).(json.RawMessage); !ok ||
		string(got) != "{not json" {
		t.Fatalf("malformed payload = %#v", got)
	}
}

// TestPromptToClaudeContentConvertsEveryBlockKind pins the mapping from ACP
// prompt blocks onto Anthropic content blocks.
func TestPromptToClaudeContentConvertsEveryBlockKind(t *testing.T) {
	cwd := t.TempDir()
	imagePath := filepath.Join(cwd, "shot.gif")
	if err := os.WriteFile(imagePath, []byte("GIF89a"), 0o600); err != nil {
		t.Fatal(err)
	}
	var errw bytes.Buffer
	got := promptToClaudeContent(cwd, []contentBlock{
		{Type: "text", Text: "hello"},
		{Type: "", Text: "unnamed"},
		{Type: "text", Text: ""},
		{Type: "resource", Resource: &resourceBlock{Text: "attached notes"}},
		{Type: "resource", Resource: nil},
		{Type: "resource", Resource: &resourceBlock{Text: ""}},
		{Type: "resource_link", URI: "file:///tmp/report.pdf"},
		{Type: "resource_link", Name: "no-uri-name"},
		{Type: "resource_link"},
		{Type: "image", URI: "file://" + imagePath},
		{Type: "image", Data: "inline", MimeType: "image/webp"},
		{Type: "image", Data: "inline-default"},
		{Type: "image", URI: "https://example.com/remote.png"},
		{Type: "unknown", Text: "dropped"},
	}, &errw)

	if len(got) != 8 {
		t.Fatalf("converted %d blocks: %#v", len(got), got)
	}
	if got[0].Text != "hello" || got[1].Text != "unnamed" {
		t.Fatalf("text blocks = %#v", got[:2])
	}
	if got[2].Text != "attached notes" {
		t.Fatalf("resource block = %#v", got[2])
	}
	if got[3].Text != "/tmp/report.pdf" || got[4].Text != "no-uri-name" {
		t.Fatalf("resource_link blocks = %#v", got[3:5])
	}
	// A file URI is read and base64 encoded, with the MIME type from the suffix.
	if got[5].Type != "image" || got[5].Source == nil ||
		got[5].Source.MediaType != "image/gif" ||
		got[5].Source.Data != base64.StdEncoding.EncodeToString([]byte("GIF89a")) {
		t.Fatalf("file image = %#v", got[5])
	}
	if got[6].Source.MediaType != "image/webp" || got[6].Source.Data != "inline" {
		t.Fatalf("inline image = %#v", got[6])
	}
	if got[7].Source.MediaType != "image/png" {
		t.Fatalf("default MIME = %#v", got[7].Source)
	}
	if !strings.Contains(errw.String(), "unsupported uri") {
		t.Fatalf("dropped image was not reported: %q", errw.String())
	}
}

// TestResolveImageReportsEveryDrop pins that a missing or unreachable image is
// reported and skipped instead of aborting the prompt.
func TestResolveImageReportsEveryDrop(t *testing.T) {
	var errw bytes.Buffer
	if _, _, ok := resolveImage("", contentBlock{URI: "https://example.com/x.png"}, &errw); ok {
		t.Fatal("a remote URI was accepted")
	}
	if !strings.Contains(errw.String(), "unsupported uri") {
		t.Fatalf("log = %q", errw.String())
	}

	errw.Reset()
	if _, _, ok := resolveImage("", contentBlock{URI: "file:///does/not/exist.png"}, &errw); ok {
		t.Fatal("a missing file was accepted")
	}
	if !strings.Contains(errw.String(), "dropping image") {
		t.Fatalf("log = %q", errw.String())
	}

	// A nil writer must not panic.
	if _, _, ok := resolveImage("", contentBlock{URI: "https://example.com/x.png"}, nil); ok {
		t.Fatal("a remote URI was accepted with no log writer")
	}
}

func TestImagePathResolution(t *testing.T) {
	for name, testCase := range map[string]struct{ cwd, uri, want string }{
		"empty":             {uri: "", want: ""},
		"file uri":          {uri: "file:///tmp/a.png", want: "/tmp/a.png"},
		"absolute":          {uri: "/tmp/b.png", want: "/tmp/b.png"},
		"relative with cwd": {cwd: "/work", uri: "c.png", want: "/work/c.png"},
		"relative no cwd":   {uri: "c.png", want: ""},
		"remote scheme":     {cwd: "/work", uri: "https://example.com/d.png", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := imagePath(testCase.cwd, testCase.uri); got != testCase.want {
				t.Fatalf("imagePath(%q, %q) = %q, want %q", testCase.cwd, testCase.uri, got, testCase.want)
			}
		})
	}
}

func TestMimeFromPathUsesTheSuffix(t *testing.T) {
	for path, want := range map[string]string{
		"a.jpg": "image/jpeg", "a.JPEG": "image/jpeg", "a.gif": "image/gif",
		"a.webp": "image/webp", "a.png": "image/png", "a.txt": "image/png",
		"noext": "image/png",
	} {
		if got := mimeFromPath(path); got != want {
			t.Fatalf("mimeFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestClaudeTranscriptPath mirrors Claude Code's per-project transcript layout
// and refuses to guess when it has no session or no home.
func TestClaudeTranscriptPath(t *testing.T) {
	if got := claudeTranscriptPath("/work", ""); got != "" {
		t.Fatalf("empty session path = %q", got)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := claudeTranscriptPath("/work/my-project", "session-1")
	want := filepath.Join(home, ".claude", "projects", "-work-my-project", "session-1.jsonl")
	if got != want {
		t.Fatalf("claudeTranscriptPath() = %q, want %q", got, want)
	}
}

// TestReplayTranscriptEmitsToolCallsAndUserText covers the transcript replay a
// session/load performs: assistant text, tool calls, tool results, and the
// replayed user turns that a live stream would not resend.
func TestReplayTranscriptEmitsToolCallsAndUserText(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "session.jsonl")
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working"},{"type":"tool_use","id":"tu-1","name":"Edit","input":{"file_path":"a.txt","old_string":"x","new_string":"y"}},{"type":"tool_use","name":"Edit"}]}}`,
		`{"type":"user","message":{"content":[{"type":"text","text":"do it"},{"type":"tool_result","tool_use_id":"tu-1","content":[{"type":"text","text":"ok"}]},{"type":"tool_result","content":"ignored"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu-2","is_error":true,"content":"bad"}]}}`,
		`{"type":"system"}`,
		``,
		`not json`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	var emitted []any
	if err := replayTranscript(path, cwd, func(v any) { emitted = append(emitted, v) }); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, event := range emitted {
		switch typed := event.(type) {
		case sessionUpdate:
			kinds = append(kinds, typed.SessionUpdate+":"+typed.Content.Text)
		case toolCallUpdate:
			kinds = append(kinds, typed.SessionUpdate+":"+typed.ToolCallID+":"+typed.Status)
		default:
			kinds = append(kinds, "unknown")
		}
	}
	want := []string{
		"agent_message_chunk:working",
		"tool_call:tu-1:in_progress",
		"user_message_chunk:do it",
		"tool_call_update:tu-1:completed",
		"tool_call_update:tu-2:failed",
	}
	if strings.Join(kinds, "|") != strings.Join(want, "|") {
		t.Fatalf("emitted\n %v\nwant\n %v", kinds, want)
	}

	if err := replayTranscript(filepath.Join(cwd, "missing.jsonl"), cwd, nil); err == nil {
		t.Fatal("a missing transcript was accepted")
	}
	if err := replayTranscript(path, cwd, nil); err != nil {
		t.Fatalf("a nil emit function broke the replay: %v", err)
	}
}

func TestReplayTranscriptRefusesAnOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), (32<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replayTranscript(path, "", nil); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized transcript error = %v", err)
	}
}

// TestJoinTextPromptFlattensBlocks pins the plain-text form used when a prompt
// is forwarded to surfaces that cannot carry blocks.
func TestJoinTextPromptFlattensBlocks(t *testing.T) {
	got := joinTextPrompt([]contentBlock{
		{Type: "text", Text: "a"},
		{Type: "", Text: "b"},
		{Type: "resource", Resource: &resourceBlock{Text: "c"}},
		{Type: "resource_link", URI: "file:///tmp/d"},
		{Type: "resource_link", Name: "e"},
		{Type: "resource_link"},
		{Type: "image", Data: "ignored"},
	})
	if got != "abc/tmp/de" {
		t.Fatalf("joinTextPrompt() = %q", got)
	}
}
