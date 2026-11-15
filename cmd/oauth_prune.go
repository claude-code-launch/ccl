package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

// credentialUsers counts the providers bound to each credential file name.
func credentialUsers(cfg *provider.Config) map[string]int {
	users := make(map[string]int)
	for _, p := range cfg.Providers {
		if name := strings.TrimSpace(p.OAuthAccountCredential); name != "" {
			users[filepath.Base(name)]++
		}
	}
	return users
}

// orphanCredentials lists credential files in authDir that no provider binds.
func orphanCredentials(authDir string, cfg *provider.Config) ([]string, error) {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	users := credentialUsers(cfg)
	var orphans []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" || strings.HasPrefix(name, ".") {
			continue
		}
		if info, err := entry.Info(); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if users[name] == 0 {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}

// removeCredentialFile deletes one credential from authDir, refusing anything
// that is not a plain file directly inside it.
func removeCredentialFile(authDir, name string) error {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return fmt.Errorf("invalid credential name %q", name)
	}
	path := filepath.Join(authDir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refuse to delete non-regular credential %s", name)
	}
	return os.Remove(path)
}

func newOAuthPruneCommand() *cobra.Command {
	yes := false
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete OAuth credentials no provider uses",
		Long: `List credential files under ~/.ccl/auth that no provider is bound to
(left behind by ccl provider rm or by logging in again), and delete them after
confirmation.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOAuthPrune(cmd.OutOrStdout(), yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Delete without confirmation")
	return cmd
}

func runOAuthPrune(out io.Writer, yes bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}
	authDir, err := oauthproxy.AuthDir()
	if err != nil {
		return err
	}
	orphans, err := orphanCredentials(authDir, cfg)
	if err != nil {
		return err
	}
	if len(orphans) == 0 {
		fmt.Fprintln(out, locale.T("没有未被使用的凭据。", "No unused credentials."))
		return nil
	}
	fmt.Fprintf(out, locale.T("%d 个凭据没有任何 Provider 使用：\n", "%d credential(s) are not used by any provider:\n"), len(orphans))
	for _, name := range orphans {
		fmt.Fprintf(out, "  %s\n", name)
	}
	if !confirmed(locale.T("全部删除？(y/N): ", "Delete them all? (y/N): "), yes) {
		fmt.Fprintln(out, locale.T("已取消。", "Cancelled."))
		return nil
	}
	removed := 0
	for _, name := range orphans {
		if err := removeCredentialFile(authDir, name); err != nil {
			fmt.Fprintf(out, "  ✗ %s: %v\n", name, err)
			continue
		}
		removed++
	}
	fmt.Fprintf(out, locale.T("✅ 已删除 %d 个凭据\n", "✅ Deleted %d credential(s)\n"), removed)
	return nil
}
