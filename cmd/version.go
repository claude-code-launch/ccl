package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version of ccl",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("ccl version v1.0.0")
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
