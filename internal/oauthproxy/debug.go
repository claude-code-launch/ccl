package oauthproxy

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// defaultLogName is the base name for ccl's per-session runtime logs. The full
// base path can be overridden with CCL_LOG_FILE (CCL_DEBUG_LOG remains a
// compatibility fallback for existing scripts).
const defaultLogName = "ccl-debug.log"

// LogLevel is ccl's persisted representation of the standard slog levels.
// "off" disables file logging entirely.
type LogLevel string

const (
	LogLevelOff   LogLevel = "off"
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

var (
	logStateMu sync.RWMutex
	logLevel   = LogLevelOff
	logPath    string
	logFile    *os.File
	logger     = slog.New(slog.NewTextHandler(io.Discard, nil))
)

// sensitiveMarkers identifies log lines that likely carry credentials. The
// whole line is dropped from third-party logger output to avoid leaking refresh
// tokens, access tokens, API keys, or Authorization headers into ccl's log.
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

// interestingMarkers identifies logrus output worth preserving. CLIProxyAPI is
// noisy, so routine progress remains suppressed; retained lines keep an
// explicit logrus WARN/ERROR severity when one is present.
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
	"context_length",
	"context length",
	"context window",
	"context_too_large",
	"request_too_large",
	"too large",
	"invalid_request",
}

// ParseLogLevel accepts ccl's standard logging levels.
func ParseLogLevel(raw string) (LogLevel, bool) {
	switch LogLevel(strings.ToLower(strings.TrimSpace(raw))) {
	case LogLevelOff:
		return LogLevelOff, true
	case LogLevelDebug:
		return LogLevelDebug, true
	case LogLevelInfo:
		return LogLevelInfo, true
	case LogLevelWarn, "warning":
		return LogLevelWarn, true
	case LogLevelError:
		return LogLevelError, true
	default:
		return LogLevelOff, false
	}
}

func (level LogLevel) slogLevel() slog.Level {
	switch level {
	case LogLevelDebug:
		return slog.LevelDebug
	case LogLevelWarn:
		return slog.LevelWarn
	case LogLevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ConfigureLogLevel records the logging threshold without creating a shared
// file. A Claude session or temporary provider runtime opens its own sink when
// it starts.
func ConfigureLogLevel(level LogLevel) {
	logStateMu.Lock()
	defer logStateMu.Unlock()
	closeLogSinkLocked()
	logLevel = level
}

func closeLogSinkLocked() {
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	logPath = ""
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
}

// SetLogLevel opens ccl's current per-session log sink. A level of "off"
// disables logging; all other levels use Go's standard slog text handler.
// File-system failures are returned instead of silently disabling diagnostics.
func SetLogLevel(level LogLevel, path string) error {
	logStateMu.Lock()
	defer logStateMu.Unlock()

	if level == LogLevelOff {
		closeLogSinkLocked()
		logLevel = LogLevelOff
		return nil
	}

	if strings.TrimSpace(path) == "" {
		path = ResolveLogTemplatePath()
	}
	if logFile != nil && logPath == path && logLevel == level {
		return nil
	}
	closeLogSinkLocked()
	logLevel = level
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create log directory %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", path, err)
	}
	logPath = path
	logFile = f
	logger = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: level.slogLevel()}))
	return nil
}

// CloseLog closes the active session file while preserving the configured
// threshold for the next session.
func CloseLog() {
	logStateMu.Lock()
	defer logStateMu.Unlock()
	closeLogSinkLocked()
}

// LogDir is ~/.ccl/logs, where ccl keeps its diagnostics.
func LogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".ccl", "logs"), nil
}

// ResolveLogTemplatePath returns the filename template used to derive each
// suffixed session log. The template itself is never opened by ccl.
func ResolveLogTemplatePath() string {
	if v := strings.TrimSpace(os.Getenv("CCL_LOG_FILE")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("CCL_DEBUG_LOG")); v != "" {
		return v
	}
	dir, err := LogDir()
	if err != nil {
		return filepath.Join(os.TempDir(), defaultLogName)
	}
	return filepath.Join(dir, defaultLogName)
}

// LogConfigured reports whether a session should open a log file.
func LogConfigured() bool {
	logStateMu.RLock()
	defer logStateMu.RUnlock()
	return logLevel != LogLevelOff
}

// LogEnabled reports whether ccl's current session file is active.
func LogEnabled() bool {
	logStateMu.RLock()
	defer logStateMu.RUnlock()
	return logLevel != LogLevelOff && logFile != nil
}

// CurrentLogLevel reports the active logging threshold.
func CurrentLogLevel() LogLevel {
	logStateMu.RLock()
	defer logStateMu.RUnlock()
	return logLevel
}

// LogFilePath reports the active session log path, or an empty string when off.
func LogFilePath() string {
	logStateMu.RLock()
	defer logStateMu.RUnlock()
	return logPath
}

// SessionLogPath derives one log file per temporary Claude session from the
// configured base path. Keeping the session name in the filename lets all
// logger levels write together without interleaving unrelated Claude sessions.
func SessionLogPath(session string) string {
	base := ResolveLogTemplatePath()
	session = sanitizeLogSessionName(session)
	if session == "" {
		return base
	}
	extension := filepath.Ext(base)
	return strings.TrimSuffix(base, extension) + "-" + session + extension
}

// EnsureSessionLog opens a uniquely named file for a temporary runtime when a
// caller has not already opened the surrounding Claude session's file. owned
// tells the runtime whether it must close the sink during teardown.
func EnsureSessionLog(prefix string) (path string, owned bool, err error) {
	if !LogConfigured() || LogEnabled() {
		return LogFilePath(), false, nil
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", false, fmt.Errorf("generate log session name: %w", err)
	}
	session := fmt.Sprintf("%s_%x", sanitizeLogSessionName(prefix), random)
	path = SessionLogPath(session)
	if err := SetLogLevel(CurrentLogLevel(), path); err != nil {
		return "", false, err
	}
	return path, true, nil
}

func sanitizeLogSessionName(session string) string {
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

// LogDebugEnabled reports whether DEBUG entries are collected. HTTP payloads
// are deliberately DEBUG only because they can contain full prompts, tools, and
// user-provided secrets.
func LogDebugEnabled() bool {
	return CurrentLogLevel() == LogLevelDebug
}

func logf(level slog.Level, format string, args ...any) {
	logMessage(level, fmt.Sprintf(format, args...))
}

func logMessage(level slog.Level, message string) {
	logStateMu.RLock()
	l := logger
	logStateMu.RUnlock()
	ctx := context.Background()
	if !l.Enabled(ctx, level) {
		return
	}
	l.Log(ctx, level, message)
}

// LogInfof writes a normal runtime event. Existing ccl diagnostics use this
// level so `ccl log on` is useful without exposing request payloads.
func LogInfof(format string, args ...any) { logf(slog.LevelInfo, format, args...) }

// LogDebugf writes sensitive or high-volume detail visible only with
// `ccl log --level debug`.
func LogDebugf(format string, args ...any) { logf(slog.LevelDebug, format, args...) }

// LogWarnf and LogErrorf are available for callers that can classify an event.
func LogWarnf(format string, args ...any)  { logf(slog.LevelWarn, format, args...) }
func LogErrorf(format string, args ...any) { logf(slog.LevelError, format, args...) }

// LogUpstreamStatusf classifies HTTP status records consistently. Successful
// per-request records are DEBUG; client failures are WARN; server failures are
// ERROR.
func LogUpstreamStatusf(status int, format string, args ...any) {
	switch {
	case status >= 500:
		LogErrorf(format, args...)
	case status >= 400:
		LogWarnf(format, args...)
	default:
		LogDebugf(format, args...)
	}
}

// DebugHTTPBody writes an explicitly debug-level HTTP payload. Callers must
// never pass headers because they can contain credentials.
func DebugHTTPBody(label string, body []byte) {
	if !LogDebugEnabled() {
		return
	}
	LogDebugf("http payload begin label=%q bytes=%d", label, len(body))
	LogDebugf("http payload data label=%q data=%s", label, body)
	LogDebugf("http payload end label=%q", label)
}

// debugPrefixWriter funnels a component's low-volume output into ccl's slog
// file. ReverseProxy requires a standard-library *log.Logger, so this adapter
// is intentionally kept at the boundary.
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
	LogErrorf("[%s] %s", w.prefix, line)
	return len(p), nil
}

func newComponentLogger(prefix string) *stdlog.Logger {
	return stdlog.New(debugPrefixWriter{prefix: prefix}, "", 0)
}

func debugLineIsInteresting(lower string) bool {
	for _, marker := range interestingMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// debugFilterWriter screens noisy CLIProxyAPI logrus output before it reaches
// ccl's slog sink and maps explicit logrus severities onto slog.
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
		switch {
		case strings.Contains(lower, "level=error"), strings.Contains(lower, "level=fatal"), strings.Contains(lower, "level=panic"):
			LogErrorf("[cpa] %s", line)
		case strings.Contains(lower, "level=warn"), strings.Contains(lower, "level=warning"):
			LogWarnf("[cpa] %s", line)
		default:
			LogInfof("[cpa] %s", line)
		}
	}
	return len(p), nil
}

func newDebugFilterWriter() io.Writer { return debugFilterWriter{} }
