package cmd

var useCmd = newProviderUseCommand("use [provider]")

func init() {
	rootCmd.AddCommand(useCmd)
}
