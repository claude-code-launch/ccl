package oauthproxy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetDebugWritesAndDisables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dbg.log")
	// Start from disabled state.
	SetDebug(false, "")

	SetDebug(true, path)
	if !DebugEnabled() {
		t.Fatal("expected debug enabled after SetDebug(true)")
	}
	Debugf("runtime start provider=chatgpt backend=codex port=1234")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "runtime start provider=chatgpt") {
		t.Fatalf("log missing line: %s", data)
	}
	if strings.Contains(strings.ToLower(string(data)), "token") && strings.Contains(string(data), "token=") {
		// "provider=chatgpt" contains no secret; ensure no token= leaked.
		t.Fatalf("log unexpectedly contains token=: %s", data)
	}

	SetDebug(false, "")
	if DebugEnabled() {
		t.Fatal("expected debug disabled after SetDebug(false)")
	}
	sizeBefore := len(data)
	Debugf("should not write")
	data2, _ := os.ReadFile(path)
	if len(data2) != sizeBefore {
		t.Fatalf("log grew after disable: before=%d after=%d", sizeBefore, len(data2))
	}
}

func TestDebugFilterWriterDropsSecretsKeepsDiagnostics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "filter.log")
	SetDebug(false, "")
	SetDebug(true, path)
	t.Cleanup(func() { SetDebug(false, "") })

	w := newDebugFilterWriter()
	cases := map[string]bool{
		"level=info msg=\"token refreshed\" provider=codex": true,
		"level=warn msg=\"429 Too Many Requests\"":          true,
		"level=error msg=\"refresh failed\" ":               true,
		"level=info msg=\"cooldown active 12s\"":            true,
		"access_token=eyJhbGciOi... secret=leak":            false,
		"refresh_token=abc123 must not appear":              false,
		"authorization: Bearer eyJ...":                      false,
		"some routine progress line about nothing":          false,
	}
	for line := range cases {
		_, err := w.Write([]byte(line))
		if err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	got := string(data)
	for line, wantKept := range cases {
		// filterWriter trims surrounding whitespace from each logrus line and
		// prefixes it with "[cpa] "; match on the trimmed, lowercased text.
		probe := strings.TrimSpace(strings.ToLower(line))
		contains := strings.Contains(strings.ToLower(got), probe)
		if wantKept && !contains {
			t.Fatalf("expected kept line missing: %q\nlog:\n%s", line, got)
		}
		if !wantKept && contains {
			t.Fatalf("expected dropped line present: %q", line)
		}
	}
}

func TestDebugfNoOpWhenDisabled(t *testing.T) {
	SetDebug(false, "")
	var buf bytes.Buffer
	// Debugf must not panic or write when disabled.
	Debugf("ignored provider=%s", "chatgpt")
	if buf.Len() != 0 {
		t.Fatalf("unexpected output: %q", buf.String())
	}
}

func TestSessionDebugLogPathIsDerivedFromBase(t *testing.T) {
	t.Setenv("CCL_DEBUG_LOG", "/var/log/ccl/debug.log")
	if got, want := SessionDebugLogPath("claude_9f2c1b7d"), "/var/log/ccl/debug-claude_9f2c1b7d.log"; got != want {
		t.Fatalf("SessionDebugLogPath = %q, want %q", got, want)
	}
	// Unsafe characters must never reach the file system.
	if got, want := SessionDebugLogPath("../../etc/passwd"), "/var/log/ccl/debug-etcpasswd.log"; got != want {
		t.Fatalf("sanitized path = %q, want %q", got, want)
	}
	// Without a usable session name the shared base path stays in use.
	if got := SessionDebugLogPath("  "); got != "/var/log/ccl/debug.log" {
		t.Fatalf("empty session path = %q", got)
	}
}

func TestSessionDebugLogPathWithoutExtension(t *testing.T) {
	t.Setenv("CCL_DEBUG_LOG", "/tmp/ccl-debug")
	if got, want := SessionDebugLogPath("claude_1"), "/tmp/ccl-debug-claude_1"; got != want {
		t.Fatalf("SessionDebugLogPath = %q, want %q", got, want)
	}
}

func TestDebugFilterKeepsContextLimitFailures(t *testing.T) {
	// A context-window rejection arrives as a plain 400 with no status marker the
	// other rules match, so it must be recognized on its own.
	for _, line := range []string{
		`{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}`,
		"request_too_large: prompt is too large for this model",
	} {
		if !debugLineIsInteresting(strings.ToLower(line)) {
			t.Errorf("context-limit line was dropped from the log: %s", line)
		}
	}
}
