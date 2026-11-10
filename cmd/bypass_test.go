package cmd

import (
	"bytes"
	"slices"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestRunBypassToggleAndShow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&provider.Config{Providers: map[string]provider.Provider{}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runBypass(&out, []string{"on"}); err != nil {
		t.Fatalf("runBypass(on): %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.BypassMode {
		t.Fatal("BypassMode is false after on")
	}
	if !bytes.Contains(out.Bytes(), []byte(grokAlwaysApproveFlag)) {
		t.Fatalf("on output lacks warning: %q", out.String())
	}

	out.Reset()
	if err := runBypass(&out, nil); err != nil {
		t.Fatalf("runBypass(show): %v", err)
	}
	if out.String() != "Bypass = on\n" {
		t.Fatalf("show output = %q", out.String())
	}

	out.Reset()
	if err := runBypass(&out, []string{"off"}); err != nil {
		t.Fatalf("runBypass(off): %v", err)
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BypassMode {
		t.Fatal("BypassMode is true after off")
	}
}

func TestRunBypassRejectsInvalidValue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := runBypass(&bytes.Buffer{}, []string{"maybe"}); err == nil {
		t.Fatal("expected invalid value error")
	}
}

func TestApplyGrokBypassMode(t *testing.T) {
	input := []string{"--model", "sonnet"}
	got := applyGrokBypassMode(input, true)
	want := []string{grokAlwaysApproveFlag, "--model", "sonnet"}
	if !slices.Equal(got, want) {
		t.Fatalf("applyGrokBypassMode = %v, want %v", got, want)
	}
	if !slices.Equal(input, []string{"--model", "sonnet"}) {
		t.Fatalf("input mutated: %v", input)
	}

	got = applyGrokBypassMode([]string{grokAlwaysApproveFlag, "--model", "opus"}, true)
	if got[0] != grokAlwaysApproveFlag || len(got) != 3 {
		t.Fatalf("duplicate flag injected: %v", got)
	}

	got = applyGrokBypassMode(input, false)
	if !slices.Equal(got, input) {
		t.Fatalf("disabled bypass changed args: %v", got)
	}
}

// TestApplyGrokBypassModeStaysOutOfTheWay pins S15: an explicit permission mode for
// this run, and a Grok Build subcommand, are passed through untouched.
func TestApplyGrokBypassModeStaysOutOfTheWay(t *testing.T) {
	for _, args := range [][]string{
		{"--permission-mode", "plan"},
		{"--permission-mode=acceptEdits", "-p", "hi"},
		{"mcp", "list"},
		{"doctor"},
		{"update"},
	} {
		if got := applyGrokBypassMode(args, true); !slices.Equal(got, args) {
			t.Fatalf("applyGrokBypassMode(%v) = %v", args, got)
		}
	}
	// A session that merely mentions a subcommand name later still gets it.
	if got := applyGrokBypassMode([]string{"-p", "mcp"}, true); got[0] != grokAlwaysApproveFlag {
		t.Fatalf("a prompt mentioning mcp lost bypass: %v", got)
	}
}
