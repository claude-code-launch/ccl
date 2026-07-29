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
