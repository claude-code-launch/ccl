package cmd

import (
	"bytes"
	"os"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestRunLogToggleAndShow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&provider.Config{Providers: map[string]provider.Provider{}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runLog(&out, []string{"on"}, ""); err != nil {
		t.Fatalf("runLog(on): %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("LogLevel = %q after on, want info", cfg.LogLevel)
	}
	if !bytes.Contains(out.Bytes(), []byte("Configured log level = info")) {
		t.Fatalf("on output missing status: %q", out.String())
	}
	if _, err := os.Stat(oauthproxy.ResolveLogTemplatePath()); !os.IsNotExist(err) {
		t.Fatalf("ccl log on created a shared log file: %v", err)
	}

	out.Reset()
	if err := runLog(&out, nil, ""); err != nil {
		t.Fatalf("runLog(show): %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Filename template: ")) || !bytes.Contains(out.Bytes(), []byte("Per session: ")) {
		t.Fatalf("show output = %q", out.String())
	}

	out.Reset()
	if err := runLog(&out, []string{"off"}, ""); err != nil {
		t.Fatalf("runLog(off): %v", err)
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "off" {
		t.Fatalf("LogLevel = %q after off, want off", cfg.LogLevel)
	}
}

func TestRunLogDefaultsToOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var out bytes.Buffer
	if err := runLog(&out, nil, ""); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Configured log level = off")) {
		t.Fatalf("default status = %q", out.String())
	}
}

func TestRunLogDebug(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&provider.Config{Providers: map[string]provider.Provider{}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runLog(&out, nil, "debug"); err != nil {
		t.Fatalf("runLog(--level debug): %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q after debug, want debug", cfg.LogLevel)
	}
	if !bytes.Contains(out.Bytes(), []byte("Configured log level = debug")) {
		t.Fatalf("debug output missing status: %q", out.String())
	}
}

func TestRunLogRejectsInvalidValue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := runLog(&bytes.Buffer{}, []string{"maybe"}, ""); err == nil {
		t.Fatal("expected invalid value error")
	}
	if err := runLog(&bytes.Buffer{}, []string{"on"}, "debug"); err == nil {
		t.Fatal("expected conflict between toggle and level")
	}
}
