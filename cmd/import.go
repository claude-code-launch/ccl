package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

// oauthImport is stubbed in tests, mirroring oauthLogin.
var oauthImport = oauthproxy.ImportCredential

var importCmd = newImportCommand()

// newImportCommand builds `ccl import`, the non-browser counterpart of
// `ccl oauth`: it imports a long-lived credential an official CLI already
// stored, validates it, and binds it to a provider. Prefer `ccl oauth` when a
// backend has a browser flow; import exists for credentials the official CLI
// owns.
func newImportCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <commandcode> [alias]",
		Short: "Import credentials from an official CLI",
		Long: `Import credentials stored by a provider's official CLI.

The only supported source is Command Code, which issues one long-lived user_
API key instead of third-party OAuth:

  ccl import commandcode        # reads ~/.commandcode/auth.json
  ccl import commandcode work   # same backend, provider name "work"

Notes:
  - Prefer ccl oauth commandcode when you can authorize in a browser; import
    only reuses a key the official CLI already stored
  - ccl validates the imported key through /alpha/whoami before storing it
  - The imported credential lives at ~/.ccl/auth/commandcode.json (0600)
  - The official CLI must be signed in once (it owns the browser login)
`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImport(cmd.Context(), cmd.OutOrStdout(), args)
		},
	}
	return cmd
}

func runImport(ctx context.Context, out io.Writer, args []string) error {
	target := strings.ToLower(strings.TrimSpace(args[0]))
	var alias string
	if len(args) > 1 {
		alias = strings.TrimSpace(args[1])
		if err := validateProviderAlias(alias); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "Importing %s credential...\n", target)
	result, err := oauthImport(ctx, target)
	if err != nil {
		return fmt.Errorf("import %s credential: %w", target, err)
	}

	providerName, _, err := activateProvider(target, alias, result)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Imported %s credential as provider %q and switched active provider.\n", target, providerName)
	fmt.Fprintf(out, "Credentials: %s\n", result.Path)
	fmt.Fprintf(out, "Protocol: %s (fixed for this backend)\n", provider.ProtocolLabel(oauthRuntimeType(target)))
	return nil
}

func init() {
	rootCmd.AddCommand(importCmd)
}
