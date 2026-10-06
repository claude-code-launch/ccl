package oauthproxy

import (
	"runtime"
	"strings"
	"testing"
)

// TestZedUserAgentNamesTheClientLikeZed does not assert this machine's own
// OS/arch only: the mapping for the other platforms must stay correct, because
// Zed's cloud reads the string to decide compatibility.
func TestZedUserAgentNamesTheClientLikeZed(t *testing.T) {
	agent := zedUserAgent()
	if !strings.HasPrefix(agent, "Zed/"+zedClientVersion+" (") || !strings.HasSuffix(agent, ")") {
		t.Fatalf("zedUserAgent() = %q", agent)
	}
	if strings.Contains(agent, "darwin") || strings.Contains(agent, "amd64") {
		t.Fatalf("Go platform names leaked into %q", agent)
	}
	osName, arch := "unknown", "unknown"
	if inside := strings.TrimSuffix(strings.TrimPrefix(agent, "Zed/"+zedClientVersion+" ("), ")"); inside != "" {
		parts := strings.SplitN(inside, "; ", 2)
		if len(parts) == 2 {
			osName, arch = parts[0], parts[1]
		}
	}
	wantOS := runtime.GOOS
	if wantOS == "darwin" {
		wantOS = "macos"
	}
	wantArch := runtime.GOARCH
	switch wantArch {
	case "amd64":
		wantArch = "x86_64"
	case "arm64":
		wantArch = "aarch64"
	}
	if osName != wantOS || arch != wantArch {
		t.Fatalf("zedUserAgent() = %q, want (%s; %s)", agent, wantOS, wantArch)
	}
}

// TestZedFirstNonEmptyTrimsAndSkipsBlanks pins the helper used to pick a
// display login out of possibly-empty profile fields.
func TestZedFirstNonEmptyTrimsAndSkipsBlanks(t *testing.T) {
	if got := zedFirstNonEmpty("", "  ", "\t", "octo"); got != "octo" {
		t.Fatalf("zedFirstNonEmpty = %q", got)
	}
	if got := zedFirstNonEmpty("  p  ", "q"); got != "p" {
		t.Fatalf("zedFirstNonEmpty = %q, want the first trimmed value", got)
	}
	if got := zedFirstNonEmpty("", " "); got != "" {
		t.Fatalf("zedFirstNonEmpty = %q, want empty", got)
	}
}
