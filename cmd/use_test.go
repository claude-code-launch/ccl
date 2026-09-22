package cmd

import (
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// TestUseWithoutArgsAndNoProviders verifies bare `ccl use` fails with a
// guidance error before any TUI starts when nothing is configured.
func TestUseWithoutArgsAndNoProviders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	out, err := executeCommand(RootCmd(), "use")
	if err == nil {
		t.Fatalf("expected error, got nil (output: %q)", out)
	}
	// Both localized variants of the guidance message mention 'ccl set', so
	// the assertion stays locale-independent.
	if want := "'ccl set'"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), want)
	}
}

// TestUseWithExplicitNameSwitchesActiveProvider keeps the non-interactive
// `ccl use <name>` path working, including config persistence.
func TestUseWithExplicitNameSwitchesActiveProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["alpha"] = provider.Provider{Name: "alpha"}
	cfg.Providers["beta"] = provider.Provider{Name: "beta"}
	cfg.ActiveProvider = "alpha"
	cfg.ACPProvider = "alpha"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	out, err := executeCommand(RootCmd(), "use", "beta")
	if err != nil {
		t.Fatalf("unexpected error: %v (output: %q)", err, out)
	}

	// The success notice goes to the process stdout (fmt.Printf), not the
	// cobra output buffer, so assert on the persisted config instead.
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ActiveProvider != "beta" {
		t.Fatalf("persisted active provider = %q, want beta", reloaded.ActiveProvider)
	}
	if reloaded.ACPProvider != "alpha" {
		t.Fatalf("persisted ACP provider = %q, want alpha", reloaded.ACPProvider)
	}
}

func TestUseACPSelectsSharedProviderConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := &provider.Config{
		ActiveProvider: "alpha",
		ACPProvider:    "alpha",
		Providers: map[string]provider.Provider{
			"alpha": {Name: "alpha", Type: "anthropic", Endpoint: "https://alpha.example"},
			"cc":    {Name: "cc", Type: "openai", Endpoint: "https://cc.example/v1"},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	out, err := executeCommand(RootCmd(), "use", "--acp", "cc")
	if err != nil {
		t.Fatalf("ccl use --acp cc: %v (output: %q)", err, out)
	}

	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveProvider != "alpha" || got.ACPProvider != "cc" {
		t.Fatalf("provider selections = normal:%q ACP:%q", got.ActiveProvider, got.ACPProvider)
	}
	if len(got.Providers) != 2 || got.Providers[got.ACPProvider].Endpoint != "https://cc.example/v1" {
		t.Fatalf("ACP did not reference the shared cc config: %+v", got)
	}
}

func TestUseRejectsNonstandardSingleDashACP(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out, err := executeCommand(RootCmd(), "use", "-acp", "cc")
	if err == nil {
		t.Fatalf("expected -acp to be rejected, output: %q", out)
	}
	if !strings.Contains(err.Error(), "unknown flag: -acp") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUseHelpDocumentsACPSelection(t *testing.T) {
	out, err := executeCommand(RootCmd(), "use", "--help")
	if err != nil {
		t.Fatalf("ccl use --help: %v", err)
	}
	for _, want := range []string{"use [--acp] [provider]", "ccl use --acp cc", "--acp"} {
		if !strings.Contains(out, want) {
			t.Fatalf("use help missing %q:\n%s", want, out)
		}
	}
}

// TestUseUnknownProviderStillErrors preserves the explicit-not-found path.
func TestUseUnknownProviderStillErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	out, err := executeCommand(RootCmd(), "use", "missing")
	if err == nil {
		t.Fatalf("expected error, got nil (output: %q)", out)
	}
	for _, want := range []string{`"missing"`, "'ccl set'"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
}
