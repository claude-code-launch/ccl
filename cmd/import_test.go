package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
)

func TestRunImportCommandCodeCreatesProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	original := oauthImport
	oauthImport = func(_ context.Context, target string) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "commandcode", Path: "commandcode.json"}, nil
	}
	t.Cleanup(func() { oauthImport = original })

	var out bytes.Buffer
	if err := runImport(context.Background(), &out, []string{"commandcode"}); err != nil {
		t.Fatalf("runImport() error: %v", err)
	}
	if !strings.Contains(out.String(), `Imported commandcode credential as provider "commandcode"`) {
		t.Fatalf("output = %q", out.String())
	}
	if !strings.Contains(out.String(), "Protocol: commandcode (fixed for this backend)") {
		t.Fatalf("output = %q", out.String())
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveProvider != "commandcode" {
		t.Fatalf("active provider = %q", cfg.ActiveProvider)
	}
	p, ok := cfg.Providers["commandcode"]
	if !ok {
		t.Fatalf("provider not created: %+v", cfg.Providers)
	}
	if p.Type != "commandcode" || p.Endpoint != "oauth://commandcode" || p.OAuthProvider != "commandcode" {
		t.Fatalf("Command Code provider = %+v", p)
	}
	if p.OAuthAccountCredential != "commandcode.json" {
		t.Fatalf("credential binding = %q", p.OAuthAccountCredential)
	}
}

func TestRunImportAliasBecomesProviderName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	original := oauthImport
	oauthImport = func(_ context.Context, target string) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "commandcode", Path: "commandcode.json"}, nil
	}
	t.Cleanup(func() { oauthImport = original })

	if err := runImport(context.Background(), &bytes.Buffer{}, []string{"commandcode", "work"}); err != nil {
		t.Fatalf("runImport() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Providers["work"]
	if !ok || cfg.ActiveProvider != "work" {
		t.Fatalf("alias provider = %+v, active = %q", cfg.Providers, cfg.ActiveProvider)
	}
	if p.OAuthAccountCredential != "commandcode.json" {
		t.Fatalf("credential binding = %q", p.OAuthAccountCredential)
	}
}

func TestRunImportRejectsUnsupportedSourceAndReservedAlias(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// The real ImportCredential dispatch rejects non-commandcode sources
	// before touching HOME or the network.
	if err := runImport(context.Background(), &bytes.Buffer{}, []string{"gpt"}); err == nil {
		t.Fatal("runImport(gpt) should fail")
	}
	// A reserved alias fails before any import work runs.
	if err := runImport(context.Background(), &bytes.Buffer{}, []string{"commandcode", "gpt"}); err == nil {
		t.Fatal("runImport(commandcode gpt) should fail on the reserved alias")
	}
}

func TestRunAuthCommandCodeRidesTheLoginPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	loginCalls := 0
	original := oauthLogin
	oauthLogin = func(_ context.Context, target string, opts oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		loginCalls++
		if target != oauthproxy.ProviderCommandCode {
			t.Fatalf("oauthLogin target = %q, want commandcode", target)
		}
		if opts.Stdin == nil {
			t.Fatal("oauthLogin for commandcode must receive a stdin reader for the paste path")
		}
		return oauthproxy.LoginResult{Provider: target, Backend: "commandcode", Path: "commandcode.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = original })

	var out bytes.Buffer
	if err := runAuth(context.Background(), &out, strings.NewReader(""), []string{"commandcode"}, authOptions{}); err != nil {
		t.Fatalf("runAuth(commandcode) error: %v", err)
	}
	if loginCalls != 1 {
		t.Fatalf("OAuth login ran %d times, want 1", loginCalls)
	}
	if !strings.Contains(out.String(), `Authenticated commandcode as provider "commandcode"`) {
		t.Fatalf("output = %q", out.String())
	}
}
