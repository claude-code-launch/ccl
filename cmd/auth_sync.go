package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

type authSyncResult struct {
	Credentials   int
	Added         int
	Removed       int
	GroupsPruned  int
	GroupsUpdated int
	Cleaned       int
	CleanInvalid  bool
	CleanQuota    bool

	// Invalid account diagnostics.
	Disabled         []string
	Unavailable      []string
	QuotaExhausted   []string
	MissingInGroup   []string
	RemovedProviders []string
	DeletedFiles     []string
}

func newOAuthSyncCommand() *cobra.Command {
	var keepInvalid bool
	var cleanQuota bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Reconcile OAuth config and delete invalid credential files",
		Long: `Reconcile single-account OAuth providers and auth groups with ~/.ccl/auth,
and delete invalid credential files by default.

Invalid accounts (deleted from ~/.ccl/auth unless --keep-invalid):

  - disabled
  - unavailable (bad/expired token markers written by CPA)

Also always:

  - prunes missing/wrong-type members from auth groups
  - removes single-account providers whose credential file is gone
  - reports quota-exhausted accounts (kept by default; may recover)

Examples:
  ccl oauth sync
  ccl oauth sync --clean-quota          # also delete quota-exhausted files
  ccl oauth sync --keep-invalid         # report only; do not delete files
  ccl sync                              # compatibility alias

After cleaning, re-run ccl doctor or start ccl to use the remaining healthy pool.
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cleanInvalid := !keepInvalid
			result, err := syncAuthConfigWithOptions(nil, cleanInvalid, cleanQuota)
			if err != nil {
				return err
			}
			writeAuthSyncReport(cmd.OutOrStdout(), result)
			return nil
		},
	}
	cmd.Flags().BoolVar(&keepInvalid, "keep-invalid", false,
		"Do not delete disabled/unavailable credential files (report only)")
	cmd.Flags().BoolVar(&cleanQuota, "clean-quota", false,
		"Also delete quota-exhausted credential files")
	return cmd
}

func writeAuthSyncReport(out io.Writer, result authSyncResult) {
	fmt.Fprintf(out,
		"Synced %d credential file(s): %d provider(s) added, %d removed, %d group member(s) pruned",
		result.Credentials, result.Added, result.Removed, result.GroupsPruned)
	if result.CleanInvalid {
		fmt.Fprintf(out, ", %d invalid file(s) deleted", result.Cleaned)
	}
	fmt.Fprintln(out, ".")
	if len(result.RemovedProviders) > 0 {
		fmt.Fprintln(out, "Removed providers:")
		for _, name := range result.RemovedProviders {
			fmt.Fprintf(out, "  - %s\n", name)
		}
	}
	printSyncAccountList(out, "Deleted credential files", result.DeletedFiles)
	if !result.CleanInvalid {
		printSyncAccountList(out, "Disabled accounts (kept on disk)", result.Disabled)
		printSyncAccountList(out, "Unavailable accounts (kept on disk)", result.Unavailable)
		printSyncAccountList(out, "Quota-exhausted accounts (kept on disk)", result.QuotaExhausted)
	} else if !result.CleanQuota {
		printSyncAccountList(out, "Quota-exhausted accounts (kept on disk)", result.QuotaExhausted)
	}
	printSyncAccountList(out, "Missing/invalid group members (pruned from config)", result.MissingInGroup)
	if !result.CleanInvalid && len(result.Disabled)+len(result.Unavailable) > 0 {
		fmt.Fprintln(out, "Invalid files were kept (--keep-invalid). Re-run without it to delete them.")
	}
	if result.CleanInvalid && len(result.QuotaExhausted) > 0 && !result.CleanQuota {
		fmt.Fprintln(out, "Quota-exhausted files were kept; add --clean-quota to delete them too.")
	}
	if result.Cleaned == 0 && len(result.Disabled)+len(result.Unavailable)+len(result.QuotaExhausted)+len(result.MissingInGroup) == 0 {
		fmt.Fprintln(out, "No invalid accounts found.")
	}
}

func printSyncAccountList(out io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(out, "%s (%d):\n", title, len(items))
	for _, item := range items {
		fmt.Fprintf(out, "  - %s\n", item)
	}
}

// syncAuthConfig makes config.yaml reflect the current one-level contents of
// ~/.ccl/auth. preferredProviders preserves an import-time GPT/Copilot hint for
// newly copied codex credentials. Invalid (disabled/unavailable) credential
// files are deleted by default; pass keep-only behavior via
// syncAuthConfigWithOptions(..., false, false) from tests that only reconcile.
func syncAuthConfig(preferredProviders map[string]string) (authSyncResult, error) {
	// Default CLI behavior deletes invalid files. Tests that must keep fixtures
	// on disk should call syncAuthConfigWithOptions directly with cleanInvalid=false.
	return syncAuthConfigWithOptions(preferredProviders, true, false)
}

func syncAuthConfigWithOptions(preferredProviders map[string]string, cleanInvalid, cleanQuota bool) (authSyncResult, error) {
	if cleanQuota && !cleanInvalid {
		return authSyncResult{}, fmt.Errorf("--clean-quota requires cleaning invalid accounts (omit --keep-invalid)")
	}
	credentials, err := oauthproxy.ListCredentials()
	if err != nil {
		return authSyncResult{}, err
	}
	cfg, err := config.Load()
	if err != nil {
		return authSyncResult{}, fmt.Errorf("load ccl config: %w", err)
	}

	result := authSyncResult{
		Credentials:  len(credentials),
		CleanInvalid: cleanInvalid,
		CleanQuota:   cleanQuota,
	}
	authDir, err := oauthproxy.AuthDir()
	if err != nil {
		return authSyncResult{}, err
	}
	available := make(map[string]oauthproxy.CredentialInfo, len(credentials))
	var deleteFiles []oauthproxy.CredentialInfo
	for _, credential := range credentials {
		name := credential.FileName
		switch {
		case credential.Disabled:
			result.Disabled = append(result.Disabled, formatSyncAccount(credential))
			if cleanInvalid {
				deleteFiles = append(deleteFiles, credential)
				continue
			}
			// Disabled files stay on disk but are excluded from providers/groups.
			continue
		case credential.QuotaExceeded:
			result.QuotaExhausted = append(result.QuotaExhausted, formatSyncAccount(credential))
			if cleanInvalid && cleanQuota {
				deleteFiles = append(deleteFiles, credential)
				continue
			}
		case credential.Unavailable:
			result.Unavailable = append(result.Unavailable, formatSyncAccount(credential))
			if cleanInvalid {
				deleteFiles = append(deleteFiles, credential)
				continue
			}
		}
		available[strings.ToLower(name)] = credential
	}
	for _, credential := range deleteFiles {
		path := filepath.Join(authDir, filepath.Base(credential.FileName))
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return authSyncResult{}, fmt.Errorf("delete invalid credential %s: %w", credential.FileName, err)
		}
		result.Cleaned++
		result.DeletedFiles = append(result.DeletedFiles, formatSyncAccount(credential))
	}
	sort.Strings(result.Disabled)
	sort.Strings(result.Unavailable)
	sort.Strings(result.QuotaExhausted)
	sort.Strings(result.DeletedFiles)

	claimed := make(map[string]bool)
	for name, p := range cfg.Providers {
		if groupName := strings.TrimSpace(p.AuthGroup); groupName != "" {
			if _, ok := cfg.AuthGroups[groupName]; !ok {
				delete(cfg.Providers, name)
				result.Removed++
				result.RemovedProviders = append(result.RemovedProviders, name+" (missing auth group)")
				if cfg.ActiveProvider == name {
					cfg.ActiveProvider = ""
				}
			}
			continue
		}
		if strings.TrimSpace(p.OAuthAccountCredential) == "" {
			continue
		}
		key := strings.ToLower(p.OAuthAccountCredential)
		credential, ok := available[key]
		if !ok {
			delete(cfg.Providers, name)
			result.Removed++
			if cfg.ActiveProvider == name {
				cfg.ActiveProvider = ""
			}
			continue
		}
		expectedBackend, backendErr := oauthproxy.BackendProvider(p.OAuthProvider)
		if backendErr != nil || !strings.EqualFold(expectedBackend, credential.Backend) {
			delete(cfg.Providers, name)
			result.Removed++
			if cfg.ActiveProvider == name {
				cfg.ActiveProvider = ""
			}
			continue
		}
		claimed[key] = true
		// Preserve the public GPT/Copilot identity already attached to the
		// provider while normalizing the rest of its OAuth fields.
		cfg.Providers[name] = configureOAuthProvider(p, name, p.OAuthProvider, credential.FileName)
	}

	for _, credential := range credentials {
		if credential.Disabled {
			continue
		}
		key := strings.ToLower(credential.FileName)
		if claimed[key] {
			continue
		}
		oauthProvider := credential.OAuthProvider
		if preferred := strings.TrimSpace(preferredProviders[key]); preferred != "" {
			oauthProvider = preferred
		}
		name := availableProviderName(cfg, derivedProviderName(oauthProvider, credential.FileName), credential.FileName)
		cfg.Providers[name] = configureOAuthProvider(provider.Provider{}, name, oauthProvider, credential.FileName)
		claimed[key] = true
		result.Added++
	}

	for name, group := range cfg.AuthGroups {
		backend, backendErr := oauthproxy.BackendProvider(group.OAuthProvider)
		if backendErr != nil {
			continue
		}
		seen := make(map[string]bool)
		kept := make([]string, 0, len(group.Credentials))
		for _, file := range group.Credentials {
			key := strings.ToLower(strings.TrimSpace(file))
			credential, ok := available[key]
			if !ok || !strings.EqualFold(credential.Backend, backend) || seen[key] {
				result.GroupsPruned++
				continue
			}
			seen[key] = true
			kept = append(kept, credential.FileName)
		}
		sort.Strings(kept)
		group.Credentials = kept
		cfg.AuthGroups[name] = group
		if err := ensureGroupProvider(cfg, name); err != nil {
			return authSyncResult{}, err
		}
		result.GroupsUpdated++
	}

	sort.Strings(result.RemovedProviders)
	sort.Strings(result.MissingInGroup)
	if err := config.Save(cfg); err != nil {
		return authSyncResult{}, fmt.Errorf("save synchronized config: %w", err)
	}
	return result, nil
}

func formatSyncAccount(credential oauthproxy.CredentialInfo) string {
	label := credential.FileName
	if email := strings.TrimSpace(credential.Email); email != "" {
		label += " <" + email + ">"
	}
	if msg := strings.TrimSpace(credential.StatusMessage); msg != "" {
		// Keep one line; truncate very long upstream JSON errors.
		msg = strings.ReplaceAll(msg, "\n", " ")
		if len(msg) > 120 {
			msg = msg[:117] + "..."
		}
		label += " · " + msg
	} else if st := strings.TrimSpace(credential.Status); st != "" {
		label += " · status=" + st
	}
	return label
}

func availableProviderName(cfg *provider.Config, base, credentialFile string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "oauth-account"
	}
	for n := 1; ; n++ {
		candidate := base
		if n > 1 {
			candidate = fmt.Sprintf("%s-%d", base, n)
		}
		existing, ok := cfg.Providers[candidate]
		if !ok || strings.EqualFold(existing.OAuthAccountCredential, credentialFile) {
			return candidate
		}
	}
}

func defaultGroupProviderName(groupName string) string {
	return strings.TrimSpace(groupName)
}

// groupProviderName resolves a group by its explicit AuthGroup marker instead
// of relying on its name. This keeps renamed group providers stable across
// `ccl sync`.
func groupProviderName(cfg *provider.Config, groupName string) string {
	var matches []string
	for name, p := range cfg.Providers {
		if strings.TrimSpace(p.AuthGroup) == groupName {
			matches = append(matches, name)
		}
	}
	if len(matches) == 0 {
		return defaultGroupProviderName(groupName)
	}
	sort.Strings(matches)
	recommended := defaultGroupProviderName(groupName)
	for _, name := range matches {
		if name == recommended {
			return name
		}
	}
	return matches[0]
}

func ensureGroupProvider(cfg *provider.Config, groupName string) error {
	group, ok := cfg.AuthGroups[groupName]
	if !ok {
		return fmt.Errorf("auth group %q not found", groupName)
	}
	name := groupProviderName(cfg, groupName)
	p, exists := cfg.Providers[name]
	if exists && strings.TrimSpace(p.AuthGroup) == "" {
		return fmt.Errorf("provider %q already exists and is not an auth group", name)
	}
	p.AuthGroup = groupName
	p = configureOAuthProvider(p, name, group.OAuthProvider, "")
	p.AuthGroup = groupName
	p.OAuthAccountCredential = ""
	p.OAuthAccountCredentials = append([]string{}, group.Credentials...)
	cfg.Providers[name] = p
	return nil
}
