package oauthproxy

import (
	"fmt"
	"io"
	stdlog "log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// defaultDebugLogName is the base file name for ccl diagnostics inside LogDir.
// The full path can be overridden with the CCL_DEBUG_LOG environment variable.
const defaultDebugLogName = "ccl-debug.log"

var (
	debugStateMu sync.RWMutex
	debugEnabled bool
	debugLogPath string
	debugFile    *os.File
)

// sensitiveMarkers identifies log lines that likely carry credentials. The
// whole line is dropped to avoid leaking refresh tokens, access tokens, API
// keys, or Authorization headers into the debug log.
var sensitiveMarkers = []string{
	"refresh_token",
	"refresh token",
	"access_token",
	"access token",
	"authorization:",
	"authorization =",
	"api_key",
	"apikey",
	"api-key",
	"bearer ",
	"\"token\":",
	"token=",
}

// interestingMarkers identifies log lines worth keeping: upstream errors, rate
// limiting, OAuth refresh/cooldown, and stream failures. Everything else from
// the noisy CLIProxyAPI logrus stream is dropped.
var interestingMarkers = []string{
	"refresh",
	"cooldown",
	"unauthorized",
	"401",
	"429",
	"500",
	"502",
	"503",
	"504",
	"rate limit",
	"ratelimit",
	"quota",
	"stream",
	"expired",
	"token refreshed",
	"refreshing token",
	"tryrefresh",
	"error",
	"fail",
	// Context-limit rejections. Upstream reports them as a plain 400, which none
	// of the status markers above match, so they would otherwise be dropped from
	// the log even though they are one of the most common session failures.
	"context_length",
	"context length",
	"context window",
	"context_too_large",
	"request_too_large",
	"too large",
	"invalid_request",
}

// SetDebug enables or disables runtime diagnostics at the package level. When
// enabling, the given path (or the resolved default/override) is opened in
// append mode; when disabling, any open file is closed so the log stops
// growing and prior content is preserved.
func SetDebug(enabled bool, path string) {
	debugStateMu.Lock()
	defer debugStateMu.Unlock()

	if !enabled {
		if debugFile != nil {
			_ = debugFile.Close()
			debugFile = nil
		}
		debugEnabled = false
		debugLogPath = ""
		return
	}

	if strings.TrimSpace(path) == "" {
		path = ResolveDebugLogPath()
	}
	if debugFile != nil && debugLogPath == path {
		debugEnabled = true
		return
	}
	if debugFile != nil {
		_ = debugFile.Close()
		debugFile = nil
	}
	// The log directory is ccl's own and may not exist yet; 0o700 keeps
	// diagnostics readable only by the user, unlike a shared temp directory.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			debugEnabled = false
			debugLogPath = ""
			return
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		// Fall back to disabled state rather than crashing the launcher.
		debugEnabled = false
		debugLogPath = ""
		return
	}
	debugEnabled = true
	debugLogPath = path
	debugFile = f
}

// LogDir is ~/.ccl/logs, where ccl keeps its diagnostics.
//
// Logs belong with the rest of ccl's state rather than in the system temp
// directory: /tmp is world-readable, cleared on a schedule the user does not
// control, and shared with every other tool on the machine.
func LogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".ccl", "logs"), nil
}

// ResolveDebugLogPath returns the configured debug log destination, honoring the
// CCL_DEBUG_LOG override before ~/.ccl/logs/ccl-debug.log.
func ResolveDebugLogPath() string {
	if v := strings.TrimSpace(os.Getenv("CCL_DEBUG_LOG")); v != "" {
		return v
	}
	dir, err := LogDir()
	if err != nil {
		// Without a home directory there is nowhere better than the temp dir.
		return filepath.Join(os.TempDir(), defaultDebugLogName)
	}
	return filepath.Join(dir, defaultDebugLogName)
}

// SessionDebugLogPath returns a per-session log path derived from the configured
// destination by appending the session name to its base name.
//
// One shared file interleaves every session, which makes a single run impossible
// to read back: the runtime, the upstream errors and the launcher lines of
// different sessions end up mixed together. Sessions are named after the settings
// file ccl generates for them, so the log sits next to it.
func SessionDebugLogPath(session string) string {
	base := ResolveDebugLogPath()
	session = sanitizeDebugSessionName(session)
	if session == "" {
		return base
	}
	extension := filepath.Ext(base)
	return strings.TrimSuffix(base, extension) + "-" + session + extension
}

// sanitizeDebugSessionName keeps only characters that are safe in a file name.
func sanitizeDebugSessionName(session string) string {
	var builder strings.Builder
	for _, char := range strings.TrimSpace(session) {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '_', char == '-', char == '.':
			builder.WriteRune(char)
		}
	}
	return strings.Trim(builder.String(), ".-_")
}

// DebugEnabled reports whether diagnostics are currently active.
func DebugEnabled() bool {
	debugStateMu.RLock()
	defer debugStateMu.RUnlock()
	return debugEnabled
}

// DebugLogPath returns the active debug log path (empty when disabled).
func DebugLogPath() string {
	debugStateMu.RLock()
	defer debugStateMu.RUnlock()
	return debugLogPath
}

// Debugf writes a single timestamped line to the debug log when enabled. It is
// a no-op when disabled; callers pass only non-sensitive fields.
func Debugf(format string, args ...any) {
	debugStateMu.RLock()
	enabled := debugEnabled
	f := debugFile
	debugStateMu.RUnlock()
	if !enabled || f == nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%s ", time.Now().Format(time.RFC3339Nano))
	_, _ = fmt.Fprintf(f, format, args...)
	_, _ = fmt.Fprintln(f)
}

// debugPrefixWriter funnels a component's log output into the ccl debug log,
// tagged with the component name and screened for credentials.
//
// Unlike debugFilterWriter it keeps every line: it is used for components whose
// output is already low volume and always diagnostic, such as the reverse-proxy
// error log, which was previously discarded entirely.
type debugPrefixWriter struct{ prefix string }

func (w debugPrefixWriter) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	if line == "" {
		return len(p), nil
	}
	lower := strings.ToLower(line)
	for _, marker := range sensitiveMarkers {
		if strings.Contains(lower, marker) {
			return len(p), nil
		}
	}
	Debugf("[%s] %s", w.prefix, line)
	return len(p), nil
}

// newComponentLogger returns a logger whose lines land in the ccl debug log. It
// is a no-op sink while debugging is disabled.
func newComponentLogger(prefix string) *stdlog.Logger {
	return stdlog.New(debugPrefixWriter{prefix: prefix}, "", 0)
}

// debugLineIsInteresting reports whether an already-lowercased CLIProxyAPI log
// line carries a diagnostic worth keeping.
func debugLineIsInteresting(lower string) bool {
	for _, marker := range interestingMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// debugFilterWriter is an io.Writer that screens CLIProxyAPI logrus output
// before it reaches the debug file: drop lines carrying secrets, keep only
// lines that look like useful diagnostics, write the survivors with a prefix
// so they are distinguishable from ccl's own Debugf lines.
type debugFilterWriter struct{}

func (debugFilterWriter) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	if line == "" {
		return len(p), nil
	}
	lower := strings.ToLower(line)
	for _, marker := range sensitiveMarkers {
		if strings.Contains(lower, marker) {
			return len(p), nil
		}
	}
	if debugLineIsInteresting(lower) {
		Debugf("[cpa] %s", line)
	}
	return len(p), nil
}

// newDebugFilterWriter returns a debug-screening writer for logrus output.
func newDebugFilterWriter() io.Writer { return debugFilterWriter{} }
