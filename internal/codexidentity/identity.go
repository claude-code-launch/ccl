// Package codexidentity defines the Codex wire identity owned by CCL.
//
// Keep these values independent from CLIProxyAPI: a CPA dependency update must
// never silently change how CCL identifies its Codex Responses requests.
package codexidentity

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

const (
	// ClientVersion is the Codex protocol baseline implemented by CCL. Update it
	// deliberately when validating CCL against a newer Codex client protocol.
	ClientVersion = "0.147.0"
	Originator    = "codex_cli_rs"
)

var (
	userAgentOnce  sync.Once
	userAgentValue string
)

// UserAgent mirrors the public Codex CLI wire format without depending on an
// installed Codex binary or CPA's release-specific constants.
func UserAgent() string {
	userAgentOnce.Do(func() {
		platform, version := platformIdentity()
		userAgentValue = formatUserAgent(platform, version, architecture(), terminalToken())
	})
	return userAgentValue
}

// ApplyClientHeaders adds the identity shared by Codex model-catalog and
// Responses requests.
func ApplyClientHeaders(headers http.Header) {
	headers.Set("User-Agent", UserAgent())
	headers.Set("Originator", Originator)
	headers.Set("Version", ClientVersion)
}

// ApplyTurnHeaders adds the request-scoped identity used by Codex Responses.
func ApplyTurnHeaders(headers http.Header, sessionID, threadID, windowID string) {
	ApplyClientHeaders(headers)
	headers.Set("Session-Id", sessionID)
	headers.Set("Thread-Id", threadID)
	headers.Set("X-Client-Request-Id", threadID)
	if windowID != "" {
		headers.Set("X-Codex-Window-Id", windowID)
	}
}

func formatUserAgent(platform, version, architecture, terminal string) string {
	return fmt.Sprintf("%s/%s (%s %s; %s) %s", Originator, ClientVersion,
		platform, version, architecture, terminal)
}

func platformIdentity() (string, string) {
	platform := runtime.GOOS
	version := "unknown"
	switch runtime.GOOS {
	case "darwin":
		platform = "Mac OS"
		if output, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
			version = strings.TrimSpace(string(output))
		}
	case "linux":
		platform = "Linux"
		if output, err := exec.Command("uname", "-r").Output(); err == nil {
			version = strings.TrimSpace(string(output))
		}
	case "windows":
		platform = "Windows"
	}
	if version == "" {
		version = "unknown"
	}
	return platform, version
}

func architecture() string {
	if runtime.GOARCH == "amd64" {
		return "x86_64"
	}
	return runtime.GOARCH
}

func terminalToken() string {
	if program := strings.TrimSpace(os.Getenv("TERM_PROGRAM")); program != "" {
		if version := strings.TrimSpace(os.Getenv("TERM_PROGRAM_VERSION")); version != "" {
			return sanitizeToken(program + "/" + version)
		}
		return sanitizeToken(program)
	}
	if os.Getenv("ITERM_SESSION_ID") != "" || os.Getenv("ITERM_PROFILE") != "" {
		return "iTerm.app"
	}
	if os.Getenv("TERM_SESSION_ID") != "" {
		return "Apple_Terminal"
	}
	if terminal := strings.TrimSpace(os.Getenv("TERM")); terminal != "" {
		return sanitizeToken(terminal)
	}
	return "unknown"
}

func sanitizeToken(value string) string {
	return strings.Map(func(char rune) rune {
		if char >= '!' && char <= '~' && char != '(' && char != ')' {
			return char
		}
		return '_'
	}, value)
}
