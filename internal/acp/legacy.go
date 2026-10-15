package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Read the old CCL id mapping only for session/load compatibility. New
// sessions use the adapter's own ids and transcript persistence directly.
func legacySession(dir, id string) (string, string) {
	if dir == "" || id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return "", ""
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil || len(data) > 64<<10 {
		return "", ""
	}
	var entry struct {
		SessionID       string `json:"sessionId"`
		ClaudeSessionID string `json:"claudeSessionId"`
		CWD             string `json:"cwd"`
	}
	if json.Unmarshal(data, &entry) != nil || entry.SessionID != id || !filepath.IsAbs(entry.CWD) {
		return "", ""
	}
	return entry.ClaudeSessionID, entry.CWD
}
