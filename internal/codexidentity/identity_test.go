package codexidentity

import (
	"net/http"
	"testing"
)

func TestFormatUserAgentMatchesCodexCLIShape(t *testing.T) {
	got := formatUserAgent("Mac OS", "26.5.2", "arm64", "iTerm.app/3.6.10")
	want := "codex_cli_rs/0.147.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.10"
	if got != want {
		t.Fatalf("formatUserAgent() = %q, want %q", got, want)
	}
}

func TestApplyTurnHeadersUsesCurrentCodexNames(t *testing.T) {
	headers := make(http.Header)
	ApplyTurnHeaders(headers, "session-1", "thread-1", "window-1")
	for name, want := range map[string]string{
		"Originator":          Originator,
		"Version":             ClientVersion,
		"Session-Id":          "session-1",
		"Thread-Id":           "thread-1",
		"X-Client-Request-Id": "thread-1",
		"X-Codex-Window-Id":   "window-1",
	} {
		if got := headers.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if headers.Get("Session_id") != "" {
		t.Fatalf("legacy Session_id unexpectedly present: %v", headers)
	}
}
