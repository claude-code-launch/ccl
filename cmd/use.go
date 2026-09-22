package cmd

var useCmd = newProviderUseCommand("use [--acp] [provider]")

func init() {
	rootCmd.AddCommand(useCmd)
}
