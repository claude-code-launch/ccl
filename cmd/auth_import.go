package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/spf13/cobra"
)

type authImportOptions struct {
	providerHint string
}

func newAuthImportCommand() *cobra.Command {
	opts := authImportOptions{}
	cmd := &cobra.Command{
		Use:   "import <file-or-directory>",
		Short: "Import existing OAuth credential JSON files",
		Long: `Import one credential JSON file or every JSON file directly inside a directory.

Directories are scanned one level only; subdirectories are never traversed.
ccl validates each file and stores an independent, canonically named 0600 copy
under ~/.ccl/auth before refreshing providers and auth groups.

Examples:
  ccl oauth import ~/xai-user@example.com.json
  ccl oauth import ~/credentials
  ccl oauth import ~/codex.json --provider copilot
  ccl oauth import ~/.aws/sso/cache/kiro-auth-token.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuthImport(cmd.OutOrStdout(), args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.providerHint, "provider", "", "Public provider hint (normally only needed to distinguish gpt from copilot)")
	return cmd
}

func runAuthImport(out io.Writer, source string, opts authImportOptions) error {
	paths, err := authImportPaths(source)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no JSON credential files found in %q", source)
	}

	preferred := make(map[string]string)
	var importErrors []error
	imported := 0
	for _, path := range paths {
		credential, targetPath, importErr := oauthproxy.ImportCredential(path, opts.providerHint)
		if importErr != nil {
			importErrors = append(importErrors, importErr)
			fmt.Fprintf(out, "Skipped %s: %v\n", filepath.Base(path), importErr)
			continue
		}
		imported++
		preferred[strings.ToLower(credential.FileName)] = credential.OAuthProvider
		fmt.Fprintf(out, "Imported %s -> %s (%s)\n", filepath.Base(path), targetPath, credential.OAuthProvider)
	}
	if imported == 0 {
		return errors.Join(importErrors...)
	}
	if _, err := syncAuthConfig(preferred); err != nil {
		return err
	}
	fmt.Fprintf(out, "Imported %d credential(s) and refreshed ccl providers.\n", imported)
	if len(importErrors) > 0 {
		return fmt.Errorf("%d credential file(s) could not be imported: %w", len(importErrors), errors.Join(importErrors...))
	}
	return nil
}

func authImportPaths(source string) ([]string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, fmt.Errorf("import path is empty")
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("stat import path %q: %w", source, err)
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("import path %q is not a regular file or directory", source)
		}
		return []string{source}, nil
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return nil, fmt.Errorf("read import directory %q: %w", source, err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		paths = append(paths, filepath.Join(source, entry.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

func init() {
	authCmd.AddCommand(newAuthImportCommand())
}
