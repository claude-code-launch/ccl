package codexidentity

import (
	"net/http"
	"runtime"
	"strings"
	"testing"
)

// TestTerminalTokenPrefersTheTerminalProgram covers the precedence the wire
// identity follows: TERM_PROGRAM wins, then the iTerm/Terminal session markers,
// then a bare TERM, then unknown.
func TestTerminalTokenPrefersTheTerminalProgram(t *testing.T) {
	for _, name := range []string{
		"TERM_PROGRAM", "TERM_PROGRAM_VERSION", "ITERM_SESSION_ID",
		"ITERM_PROFILE", "TERM_SESSION_ID", "TERM",
	} {
		t.Setenv(name, "")
	}

	for name, testCase := range map[string]struct {
		env  map[string]string
		want string
	}{
		"program and version": {
			env:  map[string]string{"TERM_PROGRAM": "iTerm.app", "TERM_PROGRAM_VERSION": "3.6.10"},
			want: "iTerm.app/3.6.10",
		},
		"program alone": {
			env:  map[string]string{"TERM_PROGRAM": "Apple_Terminal"},
			want: "Apple_Terminal",
		},
		"blank version falls back to the program": {
			env:  map[string]string{"TERM_PROGRAM": "iTerm.app", "TERM_PROGRAM_VERSION": "  "},
			want: "iTerm.app",
		},
		// TERM_PROGRAM outranks the iTerm session markers.
		"program outranks session markers": {
			env:  map[string]string{"TERM_PROGRAM": "WezTerm", "ITERM_SESSION_ID": "w0t0p0"},
			want: "WezTerm",
		},
		"iterm session id": {
			env:  map[string]string{"ITERM_SESSION_ID": "w0t0p0:1"},
			want: "iTerm.app",
		},
		"iterm profile": {
			env:  map[string]string{"ITERM_PROFILE": "Default"},
			want: "iTerm.app",
		},
		"apple terminal": {
			env:  map[string]string{"TERM_SESSION_ID": "abc"},
			want: "Apple_Terminal",
		},
		"bare term": {
			env:  map[string]string{"TERM": "xterm-256color"},
			want: "xterm-256color",
		},
		"nothing set": {
			env:  map[string]string{},
			want: "unknown",
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, name := range []string{
				"TERM_PROGRAM", "TERM_PROGRAM_VERSION", "ITERM_SESSION_ID",
				"ITERM_PROFILE", "TERM_SESSION_ID", "TERM",
			} {
				t.Setenv(name, "")
			}
			for key, value := range testCase.env {
				t.Setenv(key, value)
			}
			if got := terminalToken(); got != testCase.want {
				t.Fatalf("terminalToken() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestSanitizeTokenKeepsTheHeaderSafe pins that anything outside printable
// ASCII — and the parentheses the User-Agent format reserves — becomes "_", so
// a hostile terminal name cannot forge the header's structure.
func TestSanitizeTokenKeepsTheHeaderSafe(t *testing.T) {
	for name, testCase := range map[string]struct {
		input string
		want  string
	}{
		"printable ascii is kept": {input: "iTerm.app/3.6.10", want: "iTerm.app/3.6.10"},
		"spaces become underscores": {
			input: "some terminal", want: "some_terminal",
		},
		"parentheses are escaped": {
			input: "evil; Mac OS 1)", want: "evil;_Mac_OS_1_",
		},
		"control characters are escaped": {
			input: "x\x00\x1b[31m", want: "x__[31m",
		},
		"non-ascii is escaped": {input: "终端", want: "__"},
		"empty stays empty":    {input: "", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sanitizeToken(testCase.input); got != testCase.want {
				t.Fatalf("sanitizeToken(%q) = %q, want %q", testCase.input, got, testCase.want)
			}
		})
	}
}

// TestArchitectureUsesTheCodexSpelling pins the amd64 translation and that
// other architectures pass through untouched.
func TestArchitectureUsesTheCodexSpelling(t *testing.T) {
	got := architecture()
	if runtime.GOARCH == "amd64" && got != "x86_64" {
		t.Fatalf("architecture() = %q, want x86_64 on amd64", got)
	}
	if runtime.GOARCH != "amd64" && got != runtime.GOARCH {
		t.Fatalf("architecture() = %q, want %q", got, runtime.GOARCH)
	}
}

// TestUserAgentCarriesTheOwnedIdentity pins the shape every Codex request is
// stamped with, including that the value is computed once and reused.
func TestUserAgentCarriesTheOwnedIdentity(t *testing.T) {
	agent := UserAgent()
	if !strings.HasPrefix(agent, Originator+"/"+ClientVersion+" (") {
		t.Fatalf("UserAgent() = %q", agent)
	}
	// The platform segment is parenthesized and the terminal token follows it.
	segments := strings.SplitN(agent, ") ", 2)
	if len(segments) != 2 || !strings.Contains(segments[0], "; ") {
		t.Fatalf("UserAgent() = %q", agent)
	}
	if terminal := segments[1]; terminal == "" || strings.ContainsAny(terminal, " ()") {
		t.Fatalf("UserAgent() terminal segment = %q", terminal)
	}
	if again := UserAgent(); again != agent {
		t.Fatalf("UserAgent() changed between calls: %q then %q", agent, again)
	}
}

func TestApplyClientHeadersStampsTheCatalogIdentity(t *testing.T) {
	headers := make(http.Header)
	ApplyClientHeaders(headers)
	if got := headers.Get("User-Agent"); got != UserAgent() {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := headers.Get("Originator"); got != Originator {
		t.Fatalf("Originator = %q", got)
	}
	if got := headers.Get("Version"); got != ClientVersion {
		t.Fatalf("Version = %q", got)
	}
}

// TestApplyTurnHeadersOmitsAnEmptyWindow pins that the window header is only
// sent when the caller has one, matching the Codex client.
func TestApplyTurnHeadersOmitsAnEmptyWindow(t *testing.T) {
	headers := make(http.Header)
	ApplyTurnHeaders(headers, "session-2", "thread-2", "")
	if _, present := headers["X-Codex-Window-Id"]; present {
		t.Fatalf("empty window id was sent: %v", headers)
	}
	if got := headers.Get("Session-Id"); got != "session-2" {
		t.Fatalf("Session-Id = %q", got)
	}
}
