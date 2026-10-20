package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/claude-code-launch/ccl/internal/fsutil"
)

// Claude Code signs git commits and pull requests with a Co-Authored-By
// trailer by default. ccl hides that signature unless the user asks for it
// (`ccl sign on`), because a third-party provider's sessions should not look
// like Claude's. `--settings` outranks ~/.claude/settings.json, so the switch
// travels in ccl's own settings file; `ccl sign --default` additionally edits
// the user's settings so a plain `claude` follows.
var signOn atomic.Bool

// ConfigureSignOn records the persisted switch. It must be called once a
// command has loaded its config, before any Claude Code launch.
func ConfigureSignOn(enabled bool) { signOn.Store(enabled) }

// SigningEnabled reports whether ccl leaves Claude Code's attribution alone.
func SigningEnabled() bool { return signOn.Load() }

// attributionOff hides both signatures. The empty texts are Claude Code's
// documented way to suppress them; sessionUrl drops the session link trailer.
func attributionOff() *attributionConfig {
	return &attributionConfig{SessionURL: false}
}

var attributionOffJSON = json.RawMessage(`{"commit":"","pr":"","sessionUrl":false}`)

// GlobalSigning describes what ~/.claude/settings.json says about attribution.
type GlobalSigning int

const (
	// GlobalSigningDefault means no attribution is configured, so Claude Code
	// signs.
	GlobalSigningDefault GlobalSigning = iota
	// GlobalSigningDisabled means the user's settings hide the signature.
	GlobalSigningDisabled
	// GlobalSigningCustom means the user configured their own attribution text.
	GlobalSigningCustom
)

// ReadGlobalSigning inspects the user's own Claude Code settings.
func ReadGlobalSigning() GlobalSigning {
	fields := rawSettings(UserSettingsPath())
	if fields == nil {
		return GlobalSigningDefault
	}
	return globalSigningOf(fields["attribution"], fields["includeCoAuthoredBy"])
}

func globalSigningOf(attribution, includeCoAuthoredBy json.RawMessage) GlobalSigning {
	if attribution == nil && includeCoAuthoredBy == nil {
		return GlobalSigningDefault
	}
	if isFalse(includeCoAuthoredBy) || isFalse(attribution) {
		return GlobalSigningDisabled
	}
	var object struct {
		Commit *string `json:"commit"`
		PR     *string `json:"pr"`
	}
	if json.Unmarshal(attribution, &object) == nil && object.Commit != nil && *object.Commit == "" {
		return GlobalSigningDisabled
	}
	if string(attribution) != "null" || string(includeCoAuthoredBy) != "null" {
		return GlobalSigningCustom
	}
	return GlobalSigningDefault
}

func isFalse(raw json.RawMessage) bool {
	return string(raw) == "false"
}

// WriteGlobalSigning merges Claude Code's attribution setting into the user's
// settings file, keeping every other key.
//
// Turning signing off writes ccl's empty attribution. Turning it back on only
// removes the value ccl wrote: a custom attribution text the user chose is
// left in place, and changed reports false.
func WriteGlobalSigning(enabled bool) (changed bool, err error) {
	path := UserSettingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := fsutil.AcquireFileLock(ctx, path+".ccl-lock")
	if err != nil {
		return false, err
	}
	defer release()

	fields, err := readUserSettings(path)
	if err != nil {
		return false, err
	}
	if !enabled {
		if sameSetting(fields["attribution"], attributionOffJSON) {
			return false, nil
		}
		fields["attribution"] = attributionOffJSON
		return true, writeUserSettings(path, fields)
	}
	// Enabled: undo exactly what ccl wrote, never a text the user chose.
	attribution := fields["attribution"]
	if sameSetting(attribution, attributionOffJSON) {
		delete(fields, "attribution")
		return true, writeUserSettings(path, fields)
	}
	if isFalse(attribution) {
		delete(fields, "attribution")
		return true, writeUserSettings(path, fields)
	}
	if isFalse(fields["includeCoAuthoredBy"]) {
		delete(fields, "includeCoAuthoredBy")
		return true, writeUserSettings(path, fields)
	}
	return false, nil
}

func readUserSettings(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return make(map[string]json.RawMessage), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object: %w", path, err)
	}
	if fields == nil {
		return nil, fmt.Errorf("%s is not a JSON object", path)
	}
	return fields, nil
}

func writeUserSettings(path string, fields map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
