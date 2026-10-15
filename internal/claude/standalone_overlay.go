package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// This is the read-only view of proxymanager's recovery journal. It stays here
// to avoid a dependency cycle: proxymanager uses the launcher to prepare models.
type standaloneChange struct {
	Env     bool            `json:"env"`
	Key     string          `json:"key"`
	Before  json.RawMessage `json:"before,omitempty"`
	Applied json.RawMessage `json:"applied"`
}

func rawSettings(path string) map[string]json.RawMessage {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 4<<20 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return nil
	}
	return fields
}

func sameSetting(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
}

// standaloneChanges returns only fields still equal to the proxy's value.
// Fields changed by the user since startup remain authoritative.
func standaloneChanges() []standaloneChange {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, ".ccl", "proxy", "state.json"))
	if err != nil || len(data) > 4<<20 {
		return nil
	}
	var journal struct {
		SettingsPath string             `json:"settings_path"`
		Changes      []standaloneChange `json:"changes"`
	}
	if json.Unmarshal(data, &journal) != nil {
		return nil
	}
	path, err := filepath.Abs(UserSettingsPath())
	if err != nil || filepath.Clean(journal.SettingsPath) != path {
		return nil
	}
	fields := rawSettings(path)
	if fields == nil {
		return nil
	}
	var env map[string]json.RawMessage
	_ = json.Unmarshal(fields["env"], &env)
	var result []standaloneChange
	for _, change := range journal.Changes {
		current := fields[change.Key]
		if change.Env {
			current = env[change.Key]
		}
		if sameSetting(current, change.Applied) {
			result = append(result, change)
		}
	}
	return result
}

func (s settingsJSON) MarshalJSON() ([]byte, error) {
	type plain settingsJSON
	data, err := json.Marshal(plain(s))
	if err != nil || len(s.proxyDefaults) == 0 {
		return data, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for key, value := range s.proxyDefaults {
		if _, exists := fields[key]; !exists {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

// standaloneAbsentValues pins the pre-proxy view of a key the daemon installed
// while it was absent from the user's own settings. Claude Code drops the whole
// --settings file when one of these keys is null (verified with 2.1.296), which
// would silently fall back to the standalone overlay, so each gets a valid
// value that behaves like the key being absent. Keys whose schema tolerates
// null (effortLevel, ultracode, modelPicker, ...) keep it.
var standaloneAbsentValues = map[string]json.RawMessage{
	"model":          json.RawMessage(`""`),
	"modelOverrides": json.RawMessage(`{}`),
	"fastMode":       json.RawMessage(`false`),
	"outputStyle":    json.RawMessage(`"default"`),
	"language":       json.RawMessage(`""`),
	"statusLine":     json.RawMessage(`{"type":"command","command":""}`),
}

func standaloneOverlayDefault(key string, before json.RawMessage) json.RawMessage {
	if len(before) > 0 && string(before) != "null" {
		return before
	}
	if value, ok := standaloneAbsentValues[key]; ok {
		return value
	}
	return json.RawMessage(`null`)
}

func applyStandaloneDefaults(settings *settingsJSON, userKeys map[string]bool, useProxy bool) {
	changes := standaloneChanges()
	if len(changes) == 0 {
		return
	}
	directAPIKey := !useProxy && settings.Env["ANTHROPIC_API_KEY"] != ""
	settings.proxyDefaults = make(map[string]json.RawMessage)
	for _, change := range changes {
		if change.Env {
			if _, set := settings.Env[change.Key]; !set {
				before := os.Getenv(change.Key)
				_ = json.Unmarshal(change.Before, &before)
				settings.Env[change.Key] = before
			}
			continue
		}
		switch change.Key {
		case "outputStyle", "language", "statusLine", "modelPicker":
			if userKeys[change.Key] {
				continue
			}
		}
		settings.proxyDefaults[change.Key] = standaloneOverlayDefault(change.Key, change.Before)
	}
	// An unused credential from the standalone user settings must never be
	// merged into a different ccl provider session.
	if useProxy || !directAPIKey {
		settings.Env["ANTHROPIC_API_KEY"] = ""
	} else {
		settings.Env["ANTHROPIC_AUTH_TOKEN"] = ""
	}
}

// PlainSettingsWithoutProxy lets ccl provider off use the user's pre-proxy
// settings without stopping the independently running proxy for other clients.
// No file is created when there is no active overlay.
func PlainSettingsWithoutProxy() (string, func(), error) {
	changes := standaloneChanges()
	if len(changes) == 0 {
		return "", func() {}, nil
	}
	fields := make(map[string]json.RawMessage)
	env := make(map[string]json.RawMessage)
	projectKeys := make(map[string]bool)
	projectEnvKeys := make(map[string]bool)
	if cwd := userSettingsCWD(); cwd != "" {
		for _, name := range []string{"settings.json", "settings.local.json"} {
			for key, value := range rawSettings(filepath.Join(cwd, ".claude", name)) {
				if string(value) != "null" {
					projectKeys[key] = true
				}
				if key == "env" {
					var values map[string]json.RawMessage
					_ = json.Unmarshal(value, &values)
					for envKey := range values {
						projectEnvKeys[envKey] = true
					}
				}
			}
		}
	}
	for _, change := range changes {
		value := change.Before
		if change.Env {
			if projectEnvKeys[change.Key] {
				continue
			}
			if len(value) == 0 {
				value, _ = json.Marshal(os.Getenv(change.Key))
			}
			env[change.Key] = value
		} else if !projectKeys[change.Key] {
			fields[change.Key] = standaloneOverlayDefault(change.Key, value)
		}
	}
	var err error
	fields["env"], err = json.Marshal(env)
	if err != nil {
		return "", nil, err
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return "", nil, err
	}
	path, err := writeSettingsBytes(data, newSessionName())
	if err != nil {
		return "", nil, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}
