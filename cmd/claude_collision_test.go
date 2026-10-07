package cmd

import (
	"sort"
	"testing"
)

// allowedClaudeCollisions are ccl's own diagnostics and updater. Claude Code's
// versions stay reachable as `ccl claude doctor` / `ccl claude update`.
var allowedClaudeCollisions = map[string]bool{"doctor": true, "update": true}

func TestRootCommandsDoNotShadowClaudeSubcommands(t *testing.T) {
	claude := make(map[string]bool, len(claudeSubcommands))
	for _, name := range claudeSubcommands {
		claude[name] = true
	}
	var collisions []string
	for _, command := range rootCmd.Commands() {
		for _, name := range append([]string{command.Name()}, command.Aliases...) {
			if claude[name] && !allowedClaudeCollisions[name] {
				collisions = append(collisions, name)
			}
		}
	}
	sort.Strings(collisions)
	if len(collisions) > 0 {
		t.Fatalf("root commands shadow Claude Code subcommands: %v", collisions)
	}
	// `ccl claude <sub>` is the documented way to reach Claude Code's own.
	if isCclCommand("claude") {
		t.Fatal("`claude` must stay a pass-through prefix, not a ccl command")
	}
}
