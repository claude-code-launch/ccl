package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/claude-code-launch/ccl/internal/cloudsync"
	"github.com/spf13/cobra"
)

func newCloudLoginCommand() *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "login [platform] [alias]",
		Short: "Connect an encrypted cloud synchronization provider",
		Long: `Connect ccl to a cloud sync provider, create or open an encrypted profile,
and register a local remote alias.

Platforms (default: google-drive):

  icloud         Uses the signed-in macOS iCloud Drive account (macOS only)
  google-drive   Opens a browser OAuth flow
                 Built-in Desktop OAuth client + PKCE; no OAuth JSON required
                 Tokens refresh automatically; only drive.appdata is requested
                 --no-browser prints the URL to open on any device and reads
                 the redirected 127.0.0.1 address pasted back (default over
                 SSH, in Cloud Shell, and on Linux without a display)
                 Encrypted bundle lives in the app-private appDataFolder

Alias (optional second argument):

  - omitted + no existing remote of that platform → alias = platform name
  - omitted + exactly one existing remote → reuses that alias
  - omitted + multiple remotes → error; pass an explicit alias
  - provided → create/reuse that named remote (e.g. personal, work)

Examples:

  ccl cloud login icloud
  ccl cloud login google-drive
  ccl cloud login google-drive personal
  ccl cloud login google-drive work

Encryption key:

  A random 256-bit key, kept at ~/.ccl/cloud/profiles/<profile-id>/key and in
  the cloud profile itself, so access to the cloud account protects the data:
  a new device only needs ccl cloud login with the same account. Anyone who
  controls that account (or a ccl token for it) can read the synced
  provider credentials.

Related:

  ccl cloud status / push / pull
  ccl cloud remote ls
  ccl cloud logout [alias]
`,
		Example: `  ccl cloud login                     # same as: ccl cloud login google-drive
  ccl cloud login google-drive personal
  ccl cloud login icloud
  ccl cloud login google-drive --no-browser # approve in a browser on another machine`,
		Args: cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Login failures are runtime errors; print them without the usage.
			cmd.SilenceUsage = true
			if len(args) == 0 {
				args = []string{"google-drive"}
			}
			platform := strings.ToLower(strings.TrimSpace(args[0]))
			switch platform {
			case "icloud":
			case "google-drive":
			default:
				return fmt.Errorf("unsupported cloud provider %q; use icloud or google-drive", args[0])
			}
			alias := ""
			if len(args) == 2 {
				alias = args[1]
			} else {
				var aliasErr error
				alias, aliasErr = defaultCloudLoginAlias(platform)
				if aliasErr != nil {
					return aliasErr
				}
			}
			var (
				result cloudsync.LoginResult
				err    error
			)
			switch platform {
			case "icloud":
				result, err = cloudsync.LoginICloudNamed(alias)
			case "google-drive":
				ctx := cmd.Context()
				paste := headlessHost()
				if cmd.Flags().Changed("no-browser") {
					paste = noBrowser
				}
				if paste {
					ctx = cloudsync.WithPastedRedirect(ctx, cmd.InOrStdin())
				}
				result, err = cloudsync.LoginGoogleDriveNamed(ctx, alias, cmd.ErrOrStderr())
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s sync.\n", platform)
			fmt.Fprintf(cmd.OutOrStdout(), "Remote: %s\n", result.Alias)
			fmt.Fprintf(cmd.OutOrStdout(), "Encrypted storage: %s\n", result.RemoteDir)
			fmt.Fprintf(cmd.OutOrStdout(), "Device: %s\n", result.DeviceID)
			fmt.Fprintf(cmd.OutOrStdout(), "Unlock: %s\n", cloudsync.KeyModeDescription(result.KeyMode))
			if !result.Existing {
				fmt.Fprintln(cmd.OutOrStdout(), "A new encrypted sync profile was created.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false,
		"Google Drive: print the URL instead of opening a browser and paste back the redirected address (default on SSH, Cloud Shell and Linux without a display)")
	return cmd
}

// headlessHost reports a session that cannot open a local browser, where a
// browser redirect to 127.0.0.1 could never reach this host.
func headlessHost() bool {
	if os.Getenv("CLOUD_SHELL") == "true" || os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return true
	}
	return runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}

func defaultCloudLoginAlias(providerName string) (string, error) {
	remotes, err := cloudsync.ListRemotes()
	if err != nil {
		return "", err
	}
	var matches []string
	for _, remote := range remotes {
		if remote.Provider == providerName {
			matches = append(matches, remote.Alias)
		}
	}
	switch len(matches) {
	case 0:
		return providerName, nil
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf(
			"more than one %s remote exists; specify an alias, for example `ccl cloud login %s %s`",
			providerName, providerName, matches[0],
		)
	}
}

func newCloudPushCommand() *cobra.Command {
	var force bool
	var target string
	var all bool
	var bestEffort bool
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Push an encrypted ccl snapshot to cloud storage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if all && strings.TrimSpace(target) != "" {
				return fmt.Errorf("--all and --to cannot be used together")
			}
			cmd.SilenceUsage = true
			outcomes, pushErr := cloudsync.PushRemotes(target, all, force, bestEffort)
			for _, outcome := range outcomes {
				if outcome.Err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "%s: failed: %v\n", outcome.Alias, outcome.Err)
					continue
				}
				if !outcome.Result.Uploaded {
					fmt.Fprintf(
						cmd.OutOrStdout(),
						"%s: tag %q already has the same configuration; nothing to push.\n",
						outcome.Alias, outcome.Result.Tag,
					)
					continue
				}
				fmt.Fprintf(
					cmd.OutOrStdout(), "%s: pushed encrypted snapshot %s with tag %q.\n",
					outcome.Alias, shortSyncID(outcome.Result.ID), outcome.Result.Tag,
				)
			}
			return pushErr
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite a conflicting remote latest/tag")
	cmd.Flags().StringVar(&target, "to", "", "Push to a specific cloud remote alias")
	cmd.Flags().BoolVar(&all, "all", false, "Push to all mirror-enabled cloud remotes")
	cmd.Flags().BoolVar(&bestEffort, "best-effort", false, "Continue with available non-conflicting remotes")
	return cmd
}

func newCloudPullCommand() *cobra.Command {
	var tag string
	var force bool
	var source string
	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Pull and decrypt a ccl snapshot from cloud storage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			manager, err := cloudsync.LoadRemote(source)
			if err != nil {
				return err
			}
			result, err := manager.Pull(tag, force)
			if err != nil {
				return err
			}
			if !result.Downloaded {
				fmt.Fprintf(cmd.OutOrStdout(), "Local configuration already matches cloud tag %q.\n", result.Tag)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pulled encrypted snapshot %s from tag %q.\n", shortSyncID(result.ID), result.Tag)
			if result.BackupPath != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Previous local configuration was encrypted at %s\n", result.BackupPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "latest", "Remote tag to pull")
	cmd.Flags().BoolVar(&force, "force", false, "Replace unsynchronized local changes (an encrypted backup is kept)")
	cmd.Flags().StringVar(&source, "from", "", "Pull from a specific cloud remote alias")
	return cmd
}

func newCloudTagCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "tag [name]",
		Short: "Tag the current local configuration for the next push",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			manager, err := cloudsync.Load()
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			result, err := manager.Tag(name)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Tagged local configuration as %q (%s).\n", result.Tag, shortSyncID(result.Hash))
			return nil
		},
	}
}

func newCloudStatusCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "status [remote]",
		Short: "Show encrypted cloud synchronization status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(args) != 0 {
				return fmt.Errorf("--all and a remote alias cannot be used together")
			}
			var aliases []string
			if all {
				remotes, err := cloudsync.ListRemotes()
				if err != nil {
					return err
				}
				for _, remote := range remotes {
					aliases = append(aliases, remote.Alias)
				}
			} else if len(args) == 1 {
				aliases = []string{args[0]}
			} else {
				aliases = []string{""}
			}
			out := bufio.NewWriter(cmd.OutOrStdout())
			defer out.Flush()
			var failures int
			for index, alias := range aliases {
				manager, err := cloudsync.LoadRemote(alias)
				if err != nil {
					failures++
					fmt.Fprintf(out, "Cloud sync %s\n  State    : unavailable\n  Error    : %v\n", alias, err)
					continue
				}
				status, err := manager.Status()
				if err != nil {
					failures++
					fmt.Fprintf(out, "Cloud sync %s\n  State    : unavailable\n  Error    : %v\n", alias, err)
					continue
				}
				if index > 0 {
					fmt.Fprintln(out)
				}
				writeCloudStatus(out, status)
			}
			if failures > 0 {
				return fmt.Errorf("%d cloud remote(s) could not be inspected", failures)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Show every configured cloud remote")
	return cmd
}

func writeCloudStatus(out io.Writer, status cloudsync.Status) {
	role := "mirror"
	if status.Primary {
		role = "primary"
	}
	fmt.Fprintf(out, "Cloud sync %s\n", status.Alias)
	fmt.Fprintf(out, "  Provider : %s\n", status.Provider)
	fmt.Fprintf(out, "  Role     : %s\n", role)
	fmt.Fprintf(out, "  Mirror   : %t\n", status.Mirror)
	fmt.Fprintf(out, "  Unlock   : %s\n", cloudsync.KeyModeDescription(status.KeyMode))
	fmt.Fprintf(out, "  State    : %s\n", status.State)
	fmt.Fprintf(out, "  Device   : %s\n", status.DeviceID)
	fmt.Fprintf(out, "  Storage  : %s\n", status.RemoteDir)
	fmt.Fprintf(out, "  Local    : %s\n", shortSyncID(status.LocalHash))
	if status.PendingTag != "" {
		fmt.Fprintf(out, "  Tag      : %s (pending)\n", status.PendingTag)
	}
	if status.RemoteID != "" {
		fmt.Fprintf(out, "  Remote   : %s (%s)\n", shortSyncID(status.RemoteID), status.RemoteTag)
		fmt.Fprintf(out, "  Uploaded : %s by %s\n", status.RemoteCreated.Format("2006-01-02 15:04:05Z"), status.RemoteDeviceID)
	} else {
		fmt.Fprintf(out, "  Remote   : (empty)\n")
	}
	if !status.LastSyncAt.IsZero() {
		fmt.Fprintf(out, "  Last sync: %s (%s)\n", status.LastSyncAt.Format("2006-01-02 15:04:05Z"), status.LastOperation)
	}
}

func shortSyncID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 12 {
		return value[:12]
	}
	if value == "" {
		return "-"
	}
	return value
}

// newCloudCommand is the cloud-sync command tree.
func newCloudCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud",
		Short: "Manage encrypted cloud synchronization",
		Long: `Encrypted multi-remote cloud synchronization.

Common commands:

  ccl cloud login
  ccl cloud push
  ccl cloud pull
  ccl cloud status
  ccl cloud remote ls
`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(
		newCloudLoginCommand(),
		newCloudLogoutCommand(),
		newCloudPushCommand(),
		newCloudPullCommand(),
		newCloudTagCommand(),
		newCloudStatusCommand(),
		newCloudRemoteCommand(),
	)
	return cmd
}

func newCloudRemoteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Manage cloud synchronization remotes",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(
		newCloudRemoteListCommand(),
		newCloudRemoteUseCommand(),
		newCloudRemoteRenameCommand(),
		newCloudRemoteSetCommand(),
	)
	return cmd
}

func newCloudRemoteListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List configured cloud remotes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			remotes, err := cloudsync.ListRemotes()
			if err != nil {
				return err
			}
			if len(remotes) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No cloud remotes configured.")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Cloud remotes")
			fmt.Fprintln(cmd.OutOrStdout(), "  NAME             PROVIDER       ROLE      MIRROR  LOGIN")
			for _, remote := range remotes {
				marker := " "
				role := "mirror"
				if remote.Primary {
					marker = "*"
					role = "primary"
				}
				login := "signed out"
				if remote.SignedIn {
					login = "ready"
				}
				fmt.Fprintf(
					cmd.OutOrStdout(), "%s %-16s %-14s %-9s %-7t %s\n",
					marker, remote.Alias, remote.Provider, role, remote.Mirror, login,
				)
			}
			return nil
		},
	}
}

func newCloudRemoteUseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "use <alias>",
		Short: "Select the primary cloud remote",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cloudsync.SetPrimaryRemote(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Primary cloud remote: %s\n", args[0])
			return nil
		},
	}
}

func newCloudRemoteRenameCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old-alias> <new-alias>",
		Short: "Rename a cloud remote",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cloudsync.RenameRemote(args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Renamed cloud remote %s to %s.\n", args[0], args[1])
			return nil
		},
	}
}

func newCloudRemoteSetCommand() *cobra.Command {
	var mirror bool
	var noMirror bool
	cmd := &cobra.Command{
		Use:   "set <alias>",
		Short: "Change cloud remote settings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if mirror == noMirror {
				return fmt.Errorf("specify exactly one of --mirror or --no-mirror")
			}
			if err := cloudsync.SetRemoteMirror(args[0], mirror); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cloud remote %s mirror: %t\n", args[0], mirror)
			return nil
		},
	}
	cmd.Flags().BoolVar(&mirror, "mirror", false, "Include this remote in `ccl cloud push --all`")
	cmd.Flags().BoolVar(&noMirror, "no-mirror", false, "Exclude this remote from `ccl cloud push --all`")
	return cmd
}

func newCloudLogoutCommand() *cobra.Command {
	var all bool
	var revoke bool
	var deleteRemote bool
	var forceLocal bool
	var yes bool
	cmd := &cobra.Command{
		Use:   "logout [alias]",
		Short: "Remove a local cloud connection",
		Long: `Remove a cloud connection from this device.

By default, logout only deletes the selected remote's local token, cache, and
sync cursor. It does not delete encrypted cloud data or the profile key.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(args) != 0 {
				return fmt.Errorf("--all and a remote alias cannot be used together")
			}
			if all && deleteRemote {
				return fmt.Errorf("--delete-remote requires one exact alias and cannot be used with --all")
			}
			var aliases []string
			if all {
				remotes, err := cloudsync.ListRemotes()
				if err != nil {
					return err
				}
				for _, remote := range remotes {
					aliases = append(aliases, remote.Alias)
				}
			} else if len(args) == 1 {
				aliases = []string{args[0]}
			} else {
				alias, err := cloudsync.PrimaryRemoteAlias()
				if err != nil {
					return err
				}
				aliases = []string{alias}
			}
			if deleteRemote {
				if err := confirmCloudRemoteDeletion(
					cmd.InOrStdin(), cmd.ErrOrStderr(), aliases[0], yes,
				); err != nil {
					return err
				}
			}
			for _, alias := range aliases {
				result, err := cloudsync.LogoutRemote(cmd.Context(), alias, cloudsync.LogoutOptions{
					Revoke: revoke, DeleteRemote: deleteRemote, ForceLocal: forceLocal,
				})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Logged out cloud remote %s locally.\n", result.Alias)
				if result.TokenRevoked {
					fmt.Fprintln(cmd.OutOrStdout(), "Google authorization was revoked.")
				}
				if result.RemoteDeleted {
					fmt.Fprintln(cmd.OutOrStdout(), "Remote encrypted CCL data was permanently deleted.")
				}
				if result.NewPrimary != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "New primary cloud remote: %s\n", result.NewPrimary)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Log out all local cloud remotes")
	cmd.Flags().BoolVar(&revoke, "revoke", false, "Revoke the provider authorization before local logout")
	cmd.Flags().BoolVar(&deleteRemote, "delete-remote", false, "Permanently delete encrypted CCL data from the selected remote")
	cmd.Flags().BoolVar(&forceLocal, "force-local", false, "Remove the local connection even if authorization revocation fails")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm permanent remote data deletion")
	return cmd
}

func confirmCloudRemoteDeletion(in io.Reader, errOut io.Writer, alias string, yes bool) error {
	if yes {
		return nil
	}
	file, ok := in.(*os.File)
	if !ok || !term.IsTerminal(file.Fd()) {
		return fmt.Errorf("--delete-remote requires --yes in a non-interactive session")
	}
	fmt.Fprintf(errOut, "Type the cloud remote alias %q to permanently delete its encrypted CCL data: ", alias)
	reader := bufio.NewReader(file)
	value, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(value) != alias {
		return fmt.Errorf("remote deletion was not confirmed")
	}
	return nil
}

func init() {
	rootCmd.AddCommand(newCloudCommand())
}
