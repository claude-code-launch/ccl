package cmd

import (
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

// seedProviders writes a config into an isolated HOME so the test never reads
// or rewrites the developer's real ~/.ccl/config.yaml.
func seedProviders(t *testing.T, active string, names ...string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	cfg := &provider.Config{ActiveProvider: active, Providers: map[string]provider.Provider{}}
	for _, name := range names {
		cfg.Providers[name] = provider.Provider{Name: name, Type: "openai", Endpoint: "https://example.test/v1"}
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

func loadProviders(t *testing.T) *provider.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func TestProviderRemoveWithYesSkipsTheConfirmation(t *testing.T) {
	seedProviders(t, "keep", "keep", "drop")

	if err := runProviderRemove("drop", true, false); err != nil {
		t.Fatalf("runProviderRemove: %v", err)
	}
	cfg := loadProviders(t)
	if _, still := cfg.Providers["drop"]; still {
		t.Fatal("--yes did not delete the provider")
	}
	if _, gone := cfg.Providers["keep"]; !gone {
		t.Fatal("--yes deleted an unrelated provider")
	}
}

func TestProviderRemoveWithoutYesDeclinesOnUnansweredPrompt(t *testing.T) {
	// Under `go test` stdin is not a terminal, so the prompt reads EOF — the
	// same thing a scripted `ccl rm` sees. That must cancel, never delete.
	seedProviders(t, "drop", "drop")

	if err := runProviderRemove("drop", false, false); err != nil {
		t.Fatalf("runProviderRemove: %v", err)
	}
	if _, still := loadProviders(t).Providers["drop"]; !still {
		t.Fatal("an unanswered confirmation deleted the provider")
	}
}

func TestProviderCopyAndMoveOverwriteOnlyWithYes(t *testing.T) {
	t.Run("copy declines without --yes", func(t *testing.T) {
		seedProviders(t, "src", "src", "dst")
		if err := runProviderCopy("src", "dst", false); err != nil {
			t.Fatalf("runProviderCopy: %v", err)
		}
		if _, still := loadProviders(t).Providers["src"]; !still {
			t.Fatal("a declined copy removed the source")
		}
	})

	t.Run("move keeps the source when the overwrite is declined", func(t *testing.T) {
		seedProviders(t, "src", "src", "dst")
		if err := runProviderMove("src", "dst", false); err != nil {
			t.Fatalf("runProviderMove: %v", err)
		}
		cfg := loadProviders(t)
		if _, still := cfg.Providers["src"]; !still {
			t.Fatal("a declined rename still deleted the source")
		}
	})

	t.Run("move overwrites with --yes", func(t *testing.T) {
		seedProviders(t, "src", "src", "dst")
		if err := runProviderMove("src", "dst", true); err != nil {
			t.Fatalf("runProviderMove: %v", err)
		}
		cfg := loadProviders(t)
		if _, still := cfg.Providers["src"]; still {
			t.Fatal("--yes rename left the source behind")
		}
		if cfg.Providers["dst"].Name != "dst" {
			t.Fatalf("target provider name = %q, want dst", cfg.Providers["dst"].Name)
		}
	})
}

func TestProviderRemoveRepairsACPSelection(t *testing.T) {
	seedProviders(t, "keep", "keep", "drop")
	cfg := loadProviders(t)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	if err := runProviderRemove("drop", true, false); err != nil {
		t.Fatal(err)
	}
	cfg = loadProviders(t)
	if cfg.ActiveProvider != "keep" {
		t.Fatalf("active provider = %q", cfg.ActiveProvider)
	}
}

// TestProviderRemoveNeverSwitchesToAnotherProvider pins S8: deleting the
// active provider clears the selection instead of quietly moving the next
// session onto whichever provider sorts first.
func TestProviderRemoveNeverSwitchesToAnotherProvider(t *testing.T) {
	seedProviders(t, "drop", "aaa-billed", "drop")
	if err := runProviderRemove("drop", true, false); err != nil {
		t.Fatal(err)
	}
	if cfg := loadProviders(t); cfg.ActiveProvider != "" {
		t.Fatalf("active provider switched to %q", cfg.ActiveProvider)
	}
}

func TestProviderMoveRepairsACPSelection(t *testing.T) {
	seedProviders(t, "keep", "keep", "src")
	cfg := loadProviders(t)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	if err := runProviderMove("src", "dst", true); err != nil {
		t.Fatal(err)
	}
	cfg = loadProviders(t)
	if cfg.ActiveProvider != "keep" {
		t.Fatalf("active provider = %q", cfg.ActiveProvider)
	}
}

func TestDestructiveCommandsExposeYesOnBothSpellings(t *testing.T) {
	// Each constructor runs twice — once for the root shortcut, once under
	// `ccl provider` — so the flag var has to be per command, not shared. Two
	// commands built from the same constructor must not see each other's flag.
	for name, build := range map[string]func(string) *cobra.Command{
		"rm": newProviderRemoveCommand,
		"cp": newProviderCopyCommand,
		"mv": newProviderMoveCommand,
	} {
		root, sub := build(name), build(name)
		for _, c := range []*cobra.Command{root, sub} {
			if c.Flags().Lookup("yes") == nil {
				t.Fatalf("%s has no --yes flag", name)
			}
			if c.Flags().ShorthandLookup("y") == nil {
				t.Fatalf("%s has no -y shorthand", name)
			}
		}
		if err := root.Flags().Set("yes", "true"); err != nil {
			t.Fatalf("%s: set --yes: %v", name, err)
		}
		if sub.Flags().Lookup("yes").Value.String() != "false" {
			t.Fatalf("%s: the two commands share one --yes variable", name)
		}
	}
}

func TestPreviewCommandRootAndSubcommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &provider.Config{
		ActiveProvider: "alpha",
		Providers: map[string]provider.Provider{
			"alpha": {
				Name:     "alpha",
				Type:     "openai_responses",
				Endpoint: "https://alpha.test/v1",
				Model:    "gpt-5.4",
			},
			"beta": {
				Name:        "beta",
				Type:        "openai_responses",
				Endpoint:    "https://beta.test/v1",
				Model:       "claude-opus-4-6",
				EffortLevel: "high",
			},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	out, err := executeCommand(RootCmd(), "preview")
	if err != nil {
		t.Fatalf("ccl preview failed: %v", err)
	}
	if !strings.Contains(out, `"gpt-5.4"`) {
		t.Fatalf("ccl preview = %q, want gpt-5.4", out)
	}

	for _, args := range [][]string{
		{"preview", "beta"},
		{"provider", "preview", "beta"},
		{"preview", "-p", "beta"},
	} {
		out, err := executeCommand(RootCmd(), args...)
		if err != nil {
			t.Fatalf("ccl %v failed: %v", args, err)
		}
		if !strings.Contains(out, `"claude-opus-4-6"`) || !strings.Contains(out, `"high"`) {
			t.Fatalf("ccl %v = %q, want claude-opus-4-6 and high", args, out)
		}
	}

	if _, err := executeCommand(RootCmd(), "preview", "alpha", "-p", "beta"); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("conflicting args error = %v, want not both", err)
	}
	if f := previewCmd.Flags().Lookup("provider"); f != nil {
		_ = f.Value.Set("")
		f.Changed = false
	}
}

func TestProviderSetDefaultModelFlag(t *testing.T) {
	seedProviders(t, "alpha", "alpha", "beta")
	resetSetFlags := func() {
		for _, c := range []*cobra.Command{setCmd} {
			if c == nil {
				continue
			}
			for _, name := range []string{"default-model", "refresh-models"} {
				if f := c.Flags().Lookup(name); f != nil {
					if name == "refresh-models" {
						_ = f.Value.Set("false")
					} else {
						_ = f.Value.Set("")
					}
					f.Changed = false
				}
			}
		}
	}
	t.Cleanup(resetSetFlags)

	// 1. Active provider fallback: `ccl set -m model-a` updates alpha.
	if _, err := executeCommand(RootCmd(), "set", "-m", " model-a "); err != nil {
		t.Fatalf("ccl set -m: %v", err)
	}
	resetSetFlags()
	if got := loadProviders(t).Providers["alpha"].DefaultModel; got != "model-a" {
		t.Fatalf("alpha DefaultModel = %q, want model-a", got)
	}

	// 2. Explicit provider via `ccl provider set beta --default-model model-b[1m]`.
	if _, err := executeCommand(RootCmd(), "provider", "set", "beta", "--default-model", "model-b[1m]"); err != nil {
		t.Fatalf("ccl provider set beta --default-model: %v", err)
	}
	resetSetFlags()
	if got := loadProviders(t).Providers["beta"].DefaultModel; got != "model-b[1m]" {
		t.Fatalf("beta DefaultModel = %q, want model-b[1m]", got)
	}

	// 3. Clearing via empty string (`-m ""`).
	if _, err := executeCommand(RootCmd(), "set", "beta", "-m", ""); err != nil {
		t.Fatalf("ccl set beta -m '': %v", err)
	}
	resetSetFlags()
	if got := loadProviders(t).Providers["beta"].DefaultModel; got != "" {
		t.Fatalf("cleared beta DefaultModel = %q, want empty", got)
	}

	// 4. Rejects missing or default provider.
	if _, err := executeCommand(RootCmd(), "set", "missing", "-m", "m"); err == nil {
		t.Fatal("expected error for missing provider")
	}
	resetSetFlags()
	if _, err := executeCommand(RootCmd(), "set", "default", "-m", "m"); err == nil {
		t.Fatal("expected error for default provider")
	}
	resetSetFlags()
}
