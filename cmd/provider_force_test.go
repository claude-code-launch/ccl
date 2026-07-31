package cmd

import (
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

	if err := runProviderRemove("drop", true); err != nil {
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

	if err := runProviderRemove("drop", false); err != nil {
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

// seedActiveProviderEnv writes one provider with Env into an isolated HOME.
func seedActiveProviderEnv(t *testing.T, env map[string]string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	cfg := &provider.Config{
		ActiveProvider: "gw",
		Providers: map[string]provider.Provider{
			"gw": {Name: "gw", Type: "openai", Endpoint: "https://example.test/v1", Env: env},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

func TestEnvRemoveHonorsYesAndDeclinesWithout(t *testing.T) {
	t.Run("--yes deletes", func(t *testing.T) {
		seedActiveProviderEnv(t, map[string]string{"KEEP": "1", "DROP": "2"})
		if err := runEnvRemove("DROP", true); err != nil {
			t.Fatalf("runEnvRemove: %v", err)
		}
		env := loadProviders(t).Providers["gw"].Env
		if _, still := env["DROP"]; still {
			t.Fatal("--yes did not delete the key")
		}
		if _, gone := env["KEEP"]; !gone {
			t.Fatal("--yes deleted an unrelated key")
		}
	})

	t.Run("an unanswered prompt keeps the key", func(t *testing.T) {
		// Under `go test` stdin is not a terminal, the same as a scripted run.
		seedActiveProviderEnv(t, map[string]string{"DROP": "2"})
		if err := runEnvRemove("DROP", false); err != nil {
			t.Fatalf("runEnvRemove: %v", err)
		}
		if _, still := loadProviders(t).Providers["gw"].Env["DROP"]; !still {
			t.Fatal("an unanswered confirmation deleted the key")
		}
	})
}

func TestEnvMoveOverwritesOnlyWithYes(t *testing.T) {
	t.Run("declined overwrite leaves both keys", func(t *testing.T) {
		seedActiveProviderEnv(t, map[string]string{"OLD": "a", "NEW": "b"})
		if err := runEnvMove("OLD", "NEW", false); err != nil {
			t.Fatalf("runEnvMove: %v", err)
		}
		env := loadProviders(t).Providers["gw"].Env
		if env["OLD"] != "a" || env["NEW"] != "b" {
			t.Fatalf("a declined overwrite changed the env: %v", env)
		}
	})

	t.Run("--yes overwrites", func(t *testing.T) {
		seedActiveProviderEnv(t, map[string]string{"OLD": "a", "NEW": "b"})
		if err := runEnvMove("OLD", "NEW", true); err != nil {
			t.Fatalf("runEnvMove: %v", err)
		}
		env := loadProviders(t).Providers["gw"].Env
		if _, still := env["OLD"]; still {
			t.Fatal("--yes rename left the old key behind")
		}
		if env["NEW"] != "a" {
			t.Fatalf("NEW = %q, want the renamed value %q", env["NEW"], "a")
		}
	})
}

func TestEnvSubcommandsExposeYesPerCommand(t *testing.T) {
	// newEnvCommand is built twice (ccl env, ccl provider env), so each build
	// must get its own flag variable.
	rootEnv, providerEnv := newEnvCommand("env"), newEnvCommand("env")
	for _, sub := range []string{"rm", "mv"} {
		var pair []*cobra.Command
		for _, parent := range []*cobra.Command{rootEnv, providerEnv} {
			var found *cobra.Command
			for _, c := range parent.Commands() {
				if c.Name() == sub {
					found = c
				}
			}
			if found == nil {
				t.Fatalf("env has no %q subcommand", sub)
			}
			if found.Flags().ShorthandLookup("y") == nil {
				t.Fatalf("env %s has no -y shorthand", sub)
			}
			pair = append(pair, found)
		}
		if err := pair[0].Flags().Set("yes", "true"); err != nil {
			t.Fatalf("env %s: set --yes: %v", sub, err)
		}
		if pair[1].Flags().Lookup("yes").Value.String() != "false" {
			t.Fatalf("env %s: the two builds share one --yes variable", sub)
		}
	}
}
