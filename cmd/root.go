package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/haiboyuwen/claude-code-launch/internal/claude"
	"github.com/spf13/cobra"
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
	if len(os.Args) > 1 {
		firstArg := os.Args[1]
		isCclCmd := false
		switch firstArg {
		case "add", "use", "list", "doctor", "settings", "models", "update", "upgrade", "run", "help", "completion", "-h", "--help":
			isCclCmd = true
		}

		if !isCclCmd {
			if !claude.IsInstalled() {
				err := claude.AutoInstall()
				if err != nil {
					fmt.Println(err)
					os.Exit(1)
				}
			}

			p, err := resolveProvider()
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}

			var argsToPass []string
			if firstArg == "claude" {
				argsToPass = os.Args[2:]
			} else {
				argsToPass = os.Args[1:]
			}

			if err := claude.Run(p, argsToPass); err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					os.Exit(exitErr.ExitCode())
				}
				fmt.Println(err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}

	rootCmd.FParseErrWhitelist.UnknownFlags = true

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
