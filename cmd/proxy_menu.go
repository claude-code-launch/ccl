package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/proxymanager"
	"github.com/spf13/cobra"
)

// Menu data contains names only. The UI must never receive provider keys,
// endpoints, account credentials, or the proxy's control token.
func proxyMenuConfig(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	current := cfg.ActiveProvider
	m, err := proxymanager.Default()
	if err != nil {
		return err
	}
	if state, err := m.Read(); err != nil {
		return err
	} else if state != nil {
		current = state.Provider
	}
	names := make([]string, 0, len(cfg.Providers))
	for name, p := range cfg.Providers {
		if provider.IsDefaultProvider(name) || provider.ValidateRuntime(p) != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
		Current   string   `json:"current"`
		Providers []string `json:"providers"`
	}{current, names})
}

// A failed menu switch restores the previous running provider. Updating the
// selected configuration happens only after the replacement reports readiness.
func proxyMenuSwitch(cmd *cobra.Command, _ []string) (switchErr error) {
	defer func() {
		if switchErr != nil {
			proxymanager.ShowMenuError(switchErr.Error())
		}
	}()
	name, _ := cmd.Flags().GetString("provider")
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("select a provider")
	}
	m, err := proxymanager.Default()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	unlock, err := fsutil.AcquireFileLock(cmd.Context(), filepath.Join(m.Dir, "menu.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	target, ok := cfg.Providers[name]
	if !ok || provider.IsDefaultProvider(name) || provider.ValidateRuntime(target) != nil {
		return fmt.Errorf("select a provider supported by the standalone proxy")
	}
	previous, err := m.Read()
	if err != nil {
		return err
	}
	var rollback *provider.Provider
	if previous != nil && m.Running(cmd.Context(), previous) {
		if p, exists := cfg.Providers[previous.Provider]; exists {
			p.Name = previous.Provider
			rollback = &p
		}
	}
	if err := runProxyStart(cmd, nil); err != nil {
		if rollback != nil {
			executable, execErr := os.Executable()
			if execErr != nil {
				return errors.Join(err, execErr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if previous.Codex != nil {
				endpoint, parseErr := url.Parse(previous.Codex.BaseURL)
				if parseErr != nil {
					return errors.Join(err, parseErr)
				}
				m.CodexListenAddress = endpoint.Host
			}
			m.CodexMode = previous.CodexMode
			_, restoreErr := m.Start(ctx, executable, *rollback)
			return errors.Join(err, restoreErr)
		}
		return err
	}
	if err := config.Update(func(current *provider.Config) error {
		state, err := m.Read()
		if err != nil {
			return err
		}
		if state == nil || state.Provider != name {
			return fmt.Errorf("proxy changed before its menu selection could be saved")
		}
		if _, exists := current.Providers[name]; !exists {
			return fmt.Errorf("selected provider was removed while switching")
		}
		// The app controls when its picker reloads. Leave it running; the user
		// can restart it if the list remains stale.
		current.ActiveProvider = name
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func addProxyMenuCommands(command *cobra.Command) {
	command.AddCommand(&cobra.Command{Use: "menu-configs", Hidden: true, Args: cobra.NoArgs, RunE: proxyMenuConfig},
		&cobra.Command{Use: "menu-switch", Hidden: true, Args: cobra.NoArgs, RunE: proxyMenuSwitch})
}
