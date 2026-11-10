package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "ccl",
	Short: "Multi-provider launcher for Grok Build and Codex",
	Long: `ccl launches Grok Build with the active provider from ~/.ccl/config.yaml.

Common commands:
  ccl                         Start Grok Build with the active provider
  ccl --provider <name>       Launch once with another provider
  ccl use [name]              Switch providers; default uses Grok Build's own login
  ccl set [name]              Add or update a provider
  ccl ls                      List providers
  ccl oauth <backend>         Log in with a subscription account
  ccl codex -p <name>         Connect provider models to Codex
  ccl serve -p <name>         Use Grok Build in a local browser
  ccl doctor                  Check environment and provider connectivity
  ccl cloud login|push|pull   Manage encrypted configuration sync
  ccl log on|off              Configure per-session logs

Grok arguments pass through. Use ccl grok to bypass ccl command names
(for example, ccl grok doctor and ccl grok update).
`,
	// Execute prints every error once; cobra printing it as well duplicated it.
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Arguments are valid by now. A launch failure gets its own message.
		cmd.SilenceUsage = true
		return runGrok(args, "")
	},
}

func Execute() {
	if len(os.Args) > 1 {
		if err := retiredCommand(os.Args[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	configureLogging()
	// ccl codex start/stop recover under their own lifecycle lock.
	if len(os.Args) < 2 || os.Args[1] != "codex" {
		recoverStaleProxy()
	}

	if len(os.Args) > 1 {
		name, args, handled, err := parseLaunchOptions(os.Args[1:])
		first := os.Args[1]
		if handled || !isCclCommand(first) {
			if first == "grok" {
				args = args[1:]
			}
			if err == nil {
				err = runGrok(args, name)
			}
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					os.Exit(exitErr.ExitCode())
				}
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		}
	}

	rootCmd.FParseErrWhitelist.UnknownFlags = true

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var exitCoder interface{ ExitCode() int }
		if errors.As(err, &exitCoder) {
			os.Exit(exitCoder.ExitCode())
		}
		os.Exit(1)
	}
}

// configureLogging makes the persisted threshold available without opening a
// shared file. A Grok session or temporary provider runtime opens its own.
func configureLogging() {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	oauthproxy.ConfigureLogLevel(configuredLogLevel(cfg.LogLevel))
}

func isCclCommand(arg string) bool {
	switch arg {
	// --version asks about ccl. -v is forwarded to Grok.
	case "help", "completion", "-h", "--help", "--version":
		return true
	}

	for _, command := range rootCmd.Commands() {
		if command.Name() == arg {
			return true
		}
		for _, alias := range command.Aliases {
			if alias == arg {
				return true
			}
		}
	}

	return false
}

// Retired launcher commands must fail before falling through to a Grok prompt.
func retiredCommand(command string) error {
	switch command {
	case "proxy":
		return errors.New(locale.T("ccl proxy 已移除，请使用 ccl codex start/status/stop", "ccl proxy has been removed; use ccl codex start/status/stop"))
	case "claude", "acp", "sign", "statusline":
		return fmt.Errorf(locale.T("ccl %s 已退役。普通启动现使用 Grok Build", "ccl %s has been retired. Normal launches now use Grok Build"), command)
	default:
		return nil
	}
}

// Only a leading --provider selects a CCL provider. Grok flags such as
// --agent are passed through. -- separates CCL's option from Grok arguments.
func parseLaunchOptions(args []string) (name string, rest []string, handled bool, err error) {
	if len(args) == 0 {
		return "", args, false, nil
	}
	key, value, inline := strings.Cut(args[0], "=")
	if key != "--provider" {
		return "", args, false, nil
	}
	rest = args[1:]
	if !inline && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		value, rest = rest[0], rest[1:]
	}
	name = strings.TrimSpace(value)
	if name == "" {
		return "", nil, true, fmt.Errorf("--provider requires a name")
	}
	if len(rest) > 0 {
		key, _, _ := strings.Cut(rest[0], "=")
		switch key {
		case "--provider":
			return "", nil, true, fmt.Errorf("--provider may be specified only once")
		case "--":
			rest = rest[1:]
		}
	}
	return name, rest, true, nil
}

func runGrok(args []string, name string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}
	if name == "" {
		name = cfg.ActiveProvider
	}
	return runGrokAgent(cfg, name, args)
}

// resolveProvider returns the active configured provider.
func resolveProvider() (provider.Provider, error) {
	cfg, err := config.Load()
	if err != nil {
		return provider.Provider{}, fmt.Errorf("failed to load config: %w", err)
	}
	return resolveProviderFromConfig(cfg, cfg.ActiveProvider)
}

func resolveProviderFromConfig(cfg *provider.Config, name string) (provider.Provider, error) {
	if name == "" {
		return provider.Provider{}, errors.New(locale.T(
			"未选择 Provider。用 ccl set 添加、ccl use <name> 选择，或用 ccl use default 直接以 Grok Build 自身登录启动",
			"no provider selected. Add one with ccl set, choose one with ccl use <name>, or run Grok Build on its own login with ccl use default",
		))
	}
	p, ok := cfg.Providers[name]
	if !ok {
		return provider.Provider{}, fmt.Errorf("provider %q not found in configuration", name)
	}
	p.Name = name
	return p, nil
}
