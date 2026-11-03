package proxymanager

import (
	"context"
	"encoding/json"
)

// applySettings creates an old shared-proxy journal for migration tests only.
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
