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
	var passphraseFile string
	var passphraseMode bool
	cmd := &cobra.Command{
		Use:   "login <platform> [alias]",
		Short: "Connect an encrypted cloud synchronization provider",
		Long: `Connect ccl to a cloud sync provider, create or open an end-to-end encrypted
profile, and register a local remote alias.

Platforms:

  icloud         Uses the signed-in macOS iCloud Drive account (macOS only)
  google-drive   Opens a browser OAuth flow (aliases: google, drive)
                 Built-in Desktop OAuth client + PKCE; no OAuth JSON required
                 Tokens refresh automatically; only drive.appdata is requested
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
  ccl login google-drive              # root compatibility alias

Encryption key (never uploaded):

  macOS default     random 256-bit key at
                    ~/.ccl/cloud/profiles/<profile-id>/key
  Linux / Windows   passphrase-derived key (prompt, or non-interactive below)
  --passphrase      force passphrase mode on macOS
  --passphrase-file read passphrase from a local file
  CCL_SYNC_PASSPHRASE
                    non-interactive passphrase from the environment

After a successful local-key login, run:

  ccl cloud key export

and store the recovery key offline. New devices without a local key should use
ccl cloud key import or ccl cloud device request after authorizing the same
cloud account.

Related:

  ccl cloud status / push / pull
  ccl cloud remote ls
  ccl cloud logout [alias]
`,
		Example: `  ccl cloud login google-drive
  ccl cloud login google-drive personal
  ccl cloud login icloud
  ccl cloud login google-drive --passphrase
  ccl cloud login google-drive --passphrase-file ~/.ccl-passphrase`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform := strings.ToLower(strings.TrimSpace(args[0]))
			switch platform {
			case "icloud":
			case "google-drive", "google", "drive":
				platform = "google-drive"
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
				result     cloudsync.LoginResult
				err        error
				passphrase string
			)
			hasProfile, profileErr := cloudsync.HasActiveProfile()
			if profileErr != nil {
				return profileErr
			}
			usePassphrase := !hasProfile &&
				(passphraseMode || strings.TrimSpace(passphraseFile) != "" || runtime.GOOS != "darwin")
			if usePassphrase {
				passphrase, err = readSyncPassphrase(cmd.InOrStdin(), cmd.ErrOrStderr(), passphraseFile)
			}
			if err == nil {
				switch platform {
				case "icloud":
					result, err = cloudsync.LoginICloudNamed(alias, usePassphrase, passphrase)
				case "google-drive":
					result, err = cloudsync.LoginGoogleDriveNamed(
						cmd.Context(), alias, usePassphrase, passphrase, cmd.ErrOrStderr(),
					)
				}
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
				fmt.Fprintln(cmd.OutOrStdout(), "A new end-to-end encrypted sync profile was created.")
			}
			if result.Migrated {
				fmt.Fprintln(cmd.OutOrStdout(), "The existing encryption key was migrated to the selected local key mode.")
			}
			if result.KeyMode == "local" {
				fmt.Fprintln(cmd.OutOrStdout(), "Run `ccl cloud key export` now and keep the recovery key offline.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&passphraseMode, "passphrase", false,
		"Force passphrase-derived key mode (default on Linux/Windows; optional on macOS)")
	cmd.Flags().StringVar(&passphraseFile, "passphrase-file", "",
		"Read passphrase from a local file (also: CCL_SYNC_PASSPHRASE)")
	return cmd
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

func readSyncPassphrase(in io.Reader, errOut io.Writer, passphraseFile string) (string, error) {
	if path := strings.TrimSpace(passphraseFile); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read passphrase file: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	if value := os.Getenv("CCL_SYNC_PASSPHRASE"); value != "" {
		return value, nil
	}
	file, ok := in.(*os.File)
	if !ok || !term.IsTerminal(file.Fd()) {
		return "", fmt.Errorf("a terminal is required for the passphrase prompt; use --passphrase-file or CCL_SYNC_PASSPHRASE")
	}
	fmt.Fprint(errOut, "Cloud sync passphrase: ")
	first, err := term.ReadPassword(file.Fd())
	fmt.Fprintln(errOut)
	if err != nil {
		return "", err
	}
	fmt.Fprint(errOut, "Confirm passphrase: ")
	second, err := term.ReadPassword(file.Fd())
	fmt.Fprintln(errOut)
	if err != nil {
		return "", err
	}
	if string(first) != string(second) {
		return "", fmt.Errorf("passphrases do not match")
	}
	return string(first), nil
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

func newCloudKeyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Export or import the cloud synchronization recovery key",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newCloudKeyExportCommand(), newCloudKeyImportCommand())
	return cmd
}

func newCloudKeyExportCommand() *cobra.Command {
	var outputPath string
	var force bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export an offline recovery key for the current sync profile",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := cloudsync.ExportRecoveryKey()
			if err != nil {
				return err
			}
			if path := strings.TrimSpace(outputPath); path != "" {
				flags := os.O_WRONLY | os.O_CREATE
				if force {
					flags |= os.O_TRUNC
				} else {
					flags |= os.O_EXCL
				}
				file, openErr := os.OpenFile(path, flags, 0o600)
				if openErr != nil {
					return fmt.Errorf("create recovery key file: %w", openErr)
				}
				if chmodErr := file.Chmod(0o600); chmodErr != nil {
					_ = file.Close()
					return fmt.Errorf("secure recovery key file: %w", chmodErr)
				}
				if _, writeErr := fmt.Fprintln(file, result.RecoveryKey); writeErr != nil {
					_ = file.Close()
					return fmt.Errorf("write recovery key file: %w", writeErr)
				}
				if closeErr := file.Close(); closeErr != nil {
					return fmt.Errorf("close recovery key file: %w", closeErr)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Recovery key exported to %s\n", path)
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.RecoveryKey)
			fmt.Fprintln(cmd.ErrOrStderr(), "Keep this recovery key offline. Anyone who has it can decrypt your ccl sync data.")
			return nil
		},
	}
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "Write the recovery key to a new 0600 file")
	cmd.Flags().BoolVar(&force, "force", false, "Replace an existing output file")
	return cmd
}

func newCloudKeyImportCommand() *cobra.Command {
	var inputPath string
	var providerName string
	cmd := &cobra.Command{
		Use:   "import [recovery-key]",
		Short: "Restore sync access on a new device",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := readRecoveryKey(cmd.InOrStdin(), cmd.ErrOrStderr(), args, inputPath)
			if err != nil {
				return err
			}
			result, err := cloudsync.ImportRecoveryKeyForProvider(value, providerName)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Recovery key imported and verified.")
			fmt.Fprintf(cmd.OutOrStdout(), "Device: %s\n", result.DeviceID)
			fmt.Fprintf(cmd.OutOrStdout(), "Unlock: %s\n", cloudsync.KeyModeDescription(result.KeyMode))
			return nil
		},
	}
	cmd.Flags().StringVarP(&inputPath, "file", "f", "", "Read the recovery key from a file")
	cmd.Flags().StringVar(&providerName, "provider", "", "Cloud provider to restore (icloud or google-drive; default: auto)")
	return cmd
}

func readRecoveryKey(in io.Reader, errOut io.Writer, args []string, inputPath string) (string, error) {
	if len(args) == 1 {
		return strings.TrimSpace(args[0]), nil
	}
	if path := strings.TrimSpace(inputPath); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read recovery key file: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	if value := strings.TrimSpace(os.Getenv("CCL_SYNC_RECOVERY_KEY")); value != "" {
		return value, nil
	}
	if file, ok := in.(*os.File); ok && term.IsTerminal(file.Fd()) {
		fmt.Fprint(errOut, "Recovery key: ")
		value, err := term.ReadPassword(file.Fd())
		fmt.Fprintln(errOut)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(value)), nil
	}
	data, err := io.ReadAll(io.LimitReader(in, 4097))
	if err != nil {
		return "", fmt.Errorf("read recovery key: %w", err)
	}
	if len(data) > 4096 {
		return "", fmt.Errorf("recovery key input is too large")
	}
	if value := strings.TrimSpace(string(data)); value != "" {
		return value, nil
	}
	return "", fmt.Errorf("a recovery key is required as an argument, --file, CCL_SYNC_RECOVERY_KEY, or stdin")
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

// newCloudCommand is the canonical cloud-sync command tree.
// Root-level login/push/pull/... remain registered as compatibility aliases.
func newCloudCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud",
		Short: "Manage encrypted cloud synchronization",
		Long: `Encrypted multi-remote cloud synchronization.

Preferred forms:

  ccl cloud login icloud
  ccl cloud push
  ccl cloud pull
  ccl cloud status
  ccl cloud key export
  ccl cloud device request --via personal
  ccl cloud remote ls

Root-level login/push/pull/status/key/device/logout/tag remain available as
compatibility aliases for the same subcommands.
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
		newCloudKeyCommand(),
		newCloudDeviceCommand(),
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

func newCloudDeviceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "device",
		Short: "Pair a new device with an encrypted cloud profile",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(
		newCloudDeviceRequestCommand(),
		newCloudDeviceListCommand(),
		newCloudDeviceApproveCommand(),
		newCloudDeviceDenyCommand(),
		newCloudDeviceCompleteCommand(),
		newCloudDevicePendingCommand(),
	)
	return cmd
}

func newCloudDeviceRequestCommand() *cobra.Command {
	var via string
	var name string
	cmd := &cobra.Command{
		Use:   "request",
		Short: "Request approval from an authorized device",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(via) == "" {
				return fmt.Errorf("--via requires the alias used during cloud login")
			}
			result, err := cloudsync.StartPairing(cmd.Context(), via, name)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pairing approval code: %s\n", result.Code)
			fmt.Fprintf(cmd.OutOrStdout(), "Remote: %s\n", result.Alias)
			fmt.Fprintf(
				cmd.OutOrStdout(), "Expires: %s\n",
				result.ExpiresAt.Format("2006-01-02 15:04:05Z"),
			)
			fmt.Fprintln(
				cmd.OutOrStdout(),
				"On an authorized device, run `ccl cloud device approve "+result.Code+"`.",
			)
			fmt.Fprintln(
				cmd.OutOrStdout(),
				"After approval, run `ccl cloud device complete "+result.Code+"` on this device.",
			)
			return nil
		},
	}
	cmd.Flags().StringVar(&via, "via", "", "Cloud remote alias used to exchange pairing envelopes")
	cmd.Flags().StringVar(&name, "name", "", "Name shown on the approving device")
	return cmd
}

func newCloudDeviceListCommand() *cobra.Command {
	var via string
	var all bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List pending pairing requests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if all && strings.TrimSpace(via) != "" {
				return fmt.Errorf("--all and --via cannot be used together")
			}
			requests, err := cloudsync.ListPairingRequests(cmd.Context(), via, all)
			if err != nil {
				return err
			}
			if len(requests) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No pending device pairing requests.")
				return nil
			}
			for _, request := range requests {
				fmt.Fprintf(
					cmd.OutOrStdout(),
					"- %s  %s  via %s  expires %s\n",
					request.Code, request.DeviceName, request.Alias,
					request.ExpiresAt.Format("2006-01-02 15:04:05Z"),
				)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&via, "via", "", "Inspect one cloud remote alias")
	cmd.Flags().BoolVar(&all, "all", false, "Inspect every configured cloud remote")
	return cmd
}

func newCloudDeviceApproveCommand() *cobra.Command {
	var via string
	cmd := &cobra.Command{
		Use:   "approve <code>",
		Short: "Approve a pairing request using the code shown on the new device",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := cloudsync.ApprovePairing(cmd.Context(), args[0], via)
			if err != nil {
				return err
			}
			fmt.Fprintf(
				cmd.OutOrStdout(), "Approved %s for device %s (%s).\n",
				result.Code, result.DeviceName, shortSyncID(result.DeviceID),
			)
			return nil
		},
	}
	cmd.Flags().StringVar(&via, "via", "", "Approve through one cloud remote alias")
	return cmd
}

func newCloudDeviceDenyCommand() *cobra.Command {
	var via string
	cmd := &cobra.Command{
		Use:   "deny <code>",
		Short: "Delete a pending pairing request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cloudsync.DenyPairing(cmd.Context(), args[0], via); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Denied pairing request %s.\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&via, "via", "", "Deny through one cloud remote alias")
	return cmd
}

func newCloudDeviceCompleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "complete <code>",
		Short: "Finish pairing after an authorized device approves it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := cloudsync.CompletePairing(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Device pairing completed.")
			fmt.Fprintf(cmd.OutOrStdout(), "Remote: %s\n", result.Alias)
			fmt.Fprintf(cmd.OutOrStdout(), "Profile: %s\n", shortSyncID(result.ProfileID))
			fmt.Fprintf(cmd.OutOrStdout(), "Device: %s\n", result.DeviceID)
			return nil
		},
	}
}

func newCloudDevicePendingCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "pending",
		Short: "Show pairing requests created on this device",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			requests, err := cloudsync.PendingPairingRequests()
			if err != nil {
				return err
			}
			if len(requests) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No local pending pairing requests.")
				return nil
			}
			for _, request := range requests {
				fmt.Fprintf(
					cmd.OutOrStdout(), "- %s  via %s  expires %s\n",
					request.Code, request.Alias,
					request.ExpiresAt.Format("2006-01-02 15:04:05Z"),
				)
			}
			return nil
		},
	}
}

func init() {
	// Canonical tree.
	rootCmd.AddCommand(newCloudCommand())
	// Root compatibility aliases (same command objects would double-register;
	// construct fresh instances so cobra parent pointers stay unique).
	rootCmd.AddCommand(
		newCloudLoginCommand(),
		newCloudLogoutCommand(),
		newCloudPushCommand(),
		newCloudPullCommand(),
		newCloudTagCommand(),
		newCloudStatusCommand(),
		newCloudKeyCommand(),
		newCloudDeviceCommand(),
	)
}
