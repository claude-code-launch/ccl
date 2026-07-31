package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version is set dynamically via ldflags during build.
var Version = "dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version of ccl",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("ccl version %s\n", Version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)

	// Setting Version makes cobra handle --version itself. Without it the flag
	// is unknown, and because FParseErrWhitelist.UnknownFlags is enabled it
	// would be dropped silently and the root RunE would launch a session.
	rootCmd.Version = Version
	rootCmd.SetVersionTemplate("ccl version {{.Version}}\n")
}
