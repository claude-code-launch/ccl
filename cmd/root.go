package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/haiboyuwen/claude-code-launch/internal/claude"
)

var rootCmd = &cobra.Command{
	Use:   "ccl",
	Short: "ccl is a multi-provider launcher for Claude Code",
	Long:  `ccl manages different LLM providers for Claude Code and runs Claude Code with injected configurations.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !claude.IsInstalled() {
			err := claude.AutoInstall()
			if err != nil {
				return err
			}
		}

		p, err := resolveProvider()
		if err != nil {
			return err
		}

		return claude.Run(p, args)
	},
}

func Execute() {
	rootCmd.FParseErrWhitelist.UnknownFlags = true

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}