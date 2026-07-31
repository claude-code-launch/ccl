package cmd

import "testing"

func TestVersionFlagIsHandledByCclNotForwarded(t *testing.T) {
	// Anything isCclCommand rejects is handed to Claude Code, which starts a
	// real session. `ccl --version` asks about ccl, so it must not go there.
	if !isCclCommand("--version") {
		t.Fatal("--version is forwarded to Claude Code instead of printing ccl's version")
	}

	// Whitelisting alone is not enough: cobra only handles --version when
	// Version is set, and FParseErrWhitelist.UnknownFlags would otherwise drop
	// the flag silently and fall through to the root RunE, i.e. a session.
	if rootCmd.Version == "" {
		t.Fatal("rootCmd.Version is unset, so cobra treats --version as an unknown flag")
	}
	if flag := rootCmd.Flags().Lookup("version"); flag == nil {
		// cobra registers it lazily from Version; force the init that Execute does.
		rootCmd.InitDefaultVersionFlag()
		if rootCmd.Flags().Lookup("version") == nil {
			t.Fatal("cobra did not register a --version flag")
		}
	}
}

func TestShortVerboseFlagStaysWithClaudeCode(t *testing.T) {
	// -v is Claude Code's to interpret. Claiming it here would silently change
	// what `ccl -v` does.
	if isCclCommand("-v") {
		t.Fatal("-v was claimed by ccl; it should reach Claude Code")
	}
}

func TestRegisteredCommandsAreNotForwarded(t *testing.T) {
	// A command that ccl owns but isCclCommand misses would launch a billed
	// session on a plain typo-free invocation.
	for _, name := range []string{"doctor", "set", "ls", "use", "map", "models", "env", "lang", "version", "provider"} {
		if !isCclCommand(name) {
			t.Fatalf("%q is a ccl command but would be forwarded to Claude Code", name)
		}
	}
}
