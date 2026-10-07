package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ccl hands Claude Code its session settings with --settings, which outranks
// the user's own settings files. For preferences — output style, reply
// language, status line, the /model lineup — that would silently undo what the
// user chose in Claude Code (/output-style, /config, their own statusLine), so
// ccl only supplies a preference when no settings file the user controls
// defines it.

// claudeConfigDir is where Claude Code keeps user settings: CLAUDE_CONFIG_DIR,
// or ~/.claude.
func claudeConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// userDefinedSettings returns the top-level keys set in the user's Claude Code
// settings and in the project's (.claude/settings.json and
// settings.local.json under cwd). Unreadable files count as empty.
func userDefinedSettings(cwd string) map[string]bool {
	keys := make(map[string]bool)
	var paths []string
	if dir := claudeConfigDir(); dir != "" {
		paths = append(paths, filepath.Join(dir, "settings.json"))
	}
	if cwd != "" {
		paths = append(paths,
			filepath.Join(cwd, ".claude", "settings.json"),
			filepath.Join(cwd, ".claude", "settings.local.json"))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || len(data) > 4<<20 {
			continue
		}
		var parsed map[string]json.RawMessage
		if json.Unmarshal(data, &parsed) != nil {
			continue
		}
		for key, value := range parsed {
			if string(value) != "null" {
				keys[key] = true
			}
		}
	}
	return keys
}

// userSettingsCWD is var so tests can pin the project directory.
var userSettingsCWD = func() string {
	cwd, _ := os.Getwd()
	return cwd
}
