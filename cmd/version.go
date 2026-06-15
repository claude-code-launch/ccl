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
}
