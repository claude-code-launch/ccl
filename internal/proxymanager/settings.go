package proxymanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/claude-code-launch/ccl/internal/fsutil"
)

// Changes keep only fields owned by this proxy. Restore compares the current
// value with Applied so user edits made while the proxy runs are retained.
type Change struct {
	Env     bool            `json:"env,omitempty"`
	Key     string          `json:"key"`
	Before  json.RawMessage `json:"before,omitempty"`
	Applied json.RawMessage `json:"applied"`
}

func readSettings(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]json.RawMessage), nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, fmt.Errorf("Claude settings exceed 4 MiB")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("invalid Claude settings: %w", err)
	}
	if fields == nil {
		return nil, fmt.Errorf("Claude settings must be a JSON object")
	}
	return fields, nil
}

func settingsEnv(fields map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	env := make(map[string]json.RawMessage)
	if raw := fields["env"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &env); err != nil || env == nil {
			return nil, fmt.Errorf("Claude settings env must be an object")
		}
	}
	return env, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

func withSettings(ctx context.Context, path string, edit func(map[string]json.RawMessage) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	release, err := fsutil.AcquireFileLock(ctx, path+".ccl-lock")
	if err != nil {
		return err
	}
	defer release()
	fields, err := readSettings(path)
	if err != nil {
		return err
	}
	if err := edit(fields); err != nil {
		return err
	}
	return writeJSON(path, fields)
}

func applySettings(ctx context.Context, s *State, settings map[string]json.RawMessage, statePath string) error {
	return withSettings(ctx, s.SettingsPath, func(fields map[string]json.RawMessage) error {
		env, err := settingsEnv(fields)
		if err != nil {
			return err
		}
		proxyEnv, err := settingsEnv(settings)
		if err != nil {
			return err
		}
		s.EnvBefore = append(json.RawMessage(nil), fields["env"]...)
		if len(s.EnvBefore) > 0 && string(s.EnvBefore) != "null" {
			s.EnvBefore = json.RawMessage(`{}`)
		}
		for key, value := range settings {
			if key == "env" {
				continue
			}
			s.Changes = append(s.Changes, Change{Key: key, Before: fields[key], Applied: value})
			fields[key] = value
		}
		for key, value := range proxyEnv {
			s.Changes = append(s.Changes, Change{Env: true, Key: key, Before: env[key], Applied: value})
			env[key] = value
		}
		fields["env"], err = json.Marshal(env)
		if err != nil {
			return err
		}
		// Persist the recovery journal before touching Claude settings. If the
		// process crashes on either side of the settings write, stop/start can
		// restore just the values that were actually installed.
		return writeJSON(statePath, s)
	})
}

func restoreSettings(ctx context.Context, s *State) error {
	return withSettings(ctx, s.SettingsPath, func(fields map[string]json.RawMessage) error {
		env, err := settingsEnv(fields)
		if err != nil {
			return err
		}
		for _, change := range s.Changes {
			target := fields
			if change.Env {
				target = env
			}
			if !equalJSON(target[change.Key], change.Applied) {
				continue
			}
			if len(change.Before) == 0 {
				delete(target, change.Key)
			} else {
				target[change.Key] = change.Before
			}
		}
		if len(env) == 0 && (len(s.EnvBefore) == 0 || string(s.EnvBefore) == "null") {
			if len(s.EnvBefore) == 0 {
				delete(fields, "env")
			} else {
				fields["env"] = s.EnvBefore
			}
		} else {
			fields["env"], err = json.Marshal(env)
		}
		return err
	})
}

func equalJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
}
