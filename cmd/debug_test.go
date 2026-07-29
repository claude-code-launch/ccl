package cmd

import (
	"bytes"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestRunDebugToggleAndShow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&provider.Config{Providers: map[string]provider.Provider{}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runDebug(&out, []string{"on"}); err != nil {
		t.Fatalf("runDebug(on): %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DebugMode {
		t.Fatal("DebugMode is false after on")
	}
	if !bytes.Contains(out.Bytes(), []byte("Debug = on")) {
		t.Fatalf("on output missing status: %q", out.String())
	}

	out.Reset()
	if err := runDebug(&out, nil); err != nil {
		t.Fatalf("runDebug(show): %v", err)
	}
	if out.String() != "Debug = on\nLog: /tmp/ccl-debug.log\n" && !bytes.Contains(out.Bytes(), []byte("Log: ")) {
		t.Fatalf("show output = %q", out.String())
	}

	out.Reset()
	if err := runDebug(&out, []string{"off"}); err != nil {
		t.Fatalf("runDebug(off): %v", err)
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DebugMode {
		t.Fatal("DebugMode is true after off")
	}
}

func TestRunDebugRejectsInvalidValue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := runDebug(&bytes.Buffer{}, []string{"maybe"}); err == nil {
		t.Fatal("expected invalid value error")
	}
}
