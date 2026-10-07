package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// deprecatedRootAlias keeps a root shortcut working for one more release while
// steering users to its canonical form. The alias is hidden from help, and the
// notice is printed from PersistentPreRun so subcommands (ccl env ls) show it
// too, not only the alias itself.
func deprecatedRootAlias(cmd *cobra.Command, canonical string) *cobra.Command {
	cmd.Hidden = true
	inner := cmd.PersistentPreRunE
	cmd.PersistentPreRunE = func(c *cobra.Command, args []string) error {
		fmt.Fprintf(c.ErrOrStderr(), "ccl: %q is deprecated; use `%s` instead. The root shortcut will be removed in the next minor release.\n",
			cmd.Name(), canonical)
		if inner != nil {
			return inner(c, args)
		}
		return nil
	}
	return cmd
}
