package oauthproxy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetLogLevelWritesAndDisables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ccl.log")
	SetLogLevel(LogLevelOff, "")

	SetLogLevel(LogLevelInfo, path)
	if !LogEnabled() || CurrentLogLevel() != LogLevelInfo {
		t.Fatalf("log state = enabled:%t level:%s", LogEnabled(), CurrentLogLevel())
	}
	LogInfof("runtime start provider=chatgpt backend=codex port=1234")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "level=INFO") || !strings.Contains(string(data), "runtime start provider=chatgpt") {
		t.Fatalf("unexpected slog entry: %s", data)
	}

	SetLogLevel(LogLevelOff, "")
	if LogEnabled() {
		t.Fatal("expected log disabled after off")
	}
	sizeBefore := len(data)
	LogInfof("should not write")
	data2, _ := os.ReadFile(path)
	if len(data2) != sizeBefore {
		t.Fatalf("log grew after disable: before=%d after=%d", sizeBefore, len(data2))
	}
}

func TestConfigureLogLevelDoesNotCreateSharedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_LOG_FILE", "")
	ConfigureLogLevel(LogLevelInfo)
	t.Cleanup(func() { _ = SetLogLevel(LogLevelOff, "") })
	if !LogConfigured() || LogEnabled() {
		t.Fatalf("configured=%t enabled=%t", LogConfigured(), LogEnabled())
	}
	if _, err := os.Stat(ResolveLogTemplatePath()); !os.IsNotExist(err) {
		t.Fatalf("configuration created shared file: %v", err)
	}
}

func TestEnsureSessionLogCreatesOnlySuffixedRuntimeFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_LOG_FILE", "")
	ConfigureLogLevel(LogLevelInfo)
	t.Cleanup(func() { _ = SetLogLevel(LogLevelOff, "") })
	path, owned, err := EnsureSessionLog("runtime")
	if err != nil {
		t.Fatal(err)
	}
	if !owned || !LogEnabled() || !strings.Contains(filepath.Base(path), "ccl-debug-runtime_") {
		t.Fatalf("path=%q owned=%t enabled=%t", path, owned, LogEnabled())
	}
	if _, err := os.Stat(ResolveLogTemplatePath()); !os.IsNotExist(err) {
		t.Fatalf("session logging created the unsuffixed template: %v", err)
	}
	CloseLog()
	if LogEnabled() || !LogConfigured() {
		t.Fatalf("after close: enabled=%t configured=%t", LogEnabled(), LogConfigured())
	}
}

func TestSetLogLevelReportsFileFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetLogLevel(LogLevelInfo, filepath.Join(blocker, "session.log")); err == nil {
		t.Fatal("expected log path error")
	}
	if LogEnabled() {
		t.Fatal("failed sink should not be active")
	}
}

func TestExplicitLogSeverity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "severity.log")
	if err := SetLogLevel(LogLevelDebug, path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetLogLevel(LogLevelOff, "") })
	LogInfof("failed wording stays explicitly info")
	LogUpstreamStatusf(403, "status=%d", 403)
	LogUpstreamStatusf(503, "status=%d", 503)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"level=INFO msg=\"failed wording stays explicitly info\"", "level=WARN msg=\"status=403\"", "level=ERROR msg=\"status=503\""} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in log:\n%s", want, text)
		}
	}
}

func TestDebugFilterWriterDropsSecretsKeepsDiagnostics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "filter.log")
	SetLogLevel(LogLevelOff, "")
	SetLogLevel(LogLevelInfo, path)
	t.Cleanup(func() { SetLogLevel(LogLevelOff, "") })

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
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	got := strings.ToLower(string(data))
	for line, wantKept := range cases {
		probe := strings.ToLower(line)
		if wantKept {
			switch {
			case strings.Contains(probe, "token refreshed"):
				probe = "token refreshed"
			case strings.Contains(probe, "429 too many requests"):
				probe = "429 too many requests"
			case strings.Contains(probe, "refresh failed"):
				probe = "refresh failed"
			case strings.Contains(probe, "cooldown active"):
				probe = "cooldown active"
			}
		}
		contains := strings.Contains(got, probe)
		if wantKept && !contains {
			t.Fatalf("expected kept line missing: %q\nlog:\n%s", line, got)
		}
		if !wantKept && contains {
			t.Fatalf("expected dropped line present: %q", line)
		}
	}
}

func TestLogInfofNoOpWhenDisabled(t *testing.T) {
	SetLogLevel(LogLevelOff, "")
	var buf bytes.Buffer
	LogInfof("ignored provider=%s", "chatgpt")
	if buf.Len() != 0 {
		t.Fatalf("unexpected output: %q", buf.String())
	}
}

func TestDebugHTTPBodyRequiresDebugLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.log")
	SetLogLevel(LogLevelOff, "")
	t.Cleanup(func() { SetLogLevel(LogLevelOff, "") })

	SetLogLevel(LogLevelInfo, path)
	DebugHTTPBody("request", []byte(`{"input":"not logged"}`))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(data), "not logged") {
		t.Fatalf("payload was logged at info: %s", data)
	}

	SetLogLevel(LogLevelDebug, path)
	DebugHTTPBody("request", []byte(`{"input":"logged"}`))
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read debug log: %v", err)
	}
	if !strings.Contains(string(data), "level=DEBUG") || !strings.Contains(string(data), "logged") {
		t.Fatalf("payload missing at debug: %s", data)
	}
}

func TestDebugFilterKeepsContextLimitFailures(t *testing.T) {
	for _, line := range []string{
		`{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}`,
		"request_too_large: prompt is too large for this model",
	} {
		if !debugLineIsInteresting(strings.ToLower(line)) {
			t.Errorf("context-limit line was dropped from the log: %s", line)
		}
	}
}

func TestResolveLogTemplatePathDefaultsAndOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CCL_LOG_FILE", "")
	t.Setenv("CCL_DEBUG_LOG", "")

	want := filepath.Join(home, ".ccl", "logs", "ccl-debug.log")
	if got := ResolveLogTemplatePath(); got != want {
		t.Fatalf("ResolveLogTemplatePath = %q, want %q", got, want)
	}
	t.Setenv("CCL_LOG_FILE", "/var/log/elsewhere.log")
	if got := ResolveLogTemplatePath(); got != "/var/log/elsewhere.log" {
		t.Fatalf("CCL_LOG_FILE override ignored: %q", got)
	}
}

func TestSessionLogPathIsDerivedFromBase(t *testing.T) {
	t.Setenv("CCL_LOG_FILE", "/var/log/ccl/debug.log")
	if got, want := SessionLogPath("claude_9f2c1b7d"), "/var/log/ccl/debug-claude_9f2c1b7d.log"; got != want {
		t.Fatalf("SessionLogPath = %q, want %q", got, want)
	}
	if got, want := SessionLogPath("../../etc/passwd"), "/var/log/ccl/debug-etcpasswd.log"; got != want {
		t.Fatalf("sanitized session path = %q, want %q", got, want)
	}
}

func TestSetLogLevelCreatesTheLogDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CCL_LOG_FILE", "")
	t.Cleanup(func() { SetLogLevel(LogLevelOff, "") })

	path := ResolveLogTemplatePath()
	SetLogLevel(LogLevelInfo, path)
	if !LogEnabled() {
		t.Fatal("log did not enable; the log directory was probably not created")
	}
	LogInfof("hello")
	SetLogLevel(LogLevelOff, "")

	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("log directory was not created: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("log directory mode = %o, want 700", mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !bytes.Contains(data, []byte("hello")) {
		t.Fatalf("log content = %q", data)
	}
}

func TestParseLogLevel(t *testing.T) {
	for raw, want := range map[string]LogLevel{
		"off": LogLevelOff, "debug": LogLevelDebug, "info": LogLevelInfo,
		"warn": LogLevelWarn, "warning": LogLevelWarn, "error": LogLevelError,
	} {
		got, ok := ParseLogLevel(raw)
		if !ok || got != want {
			t.Errorf("ParseLogLevel(%q) = %q, %t; want %q, true", raw, got, ok, want)
		}
	}
}
