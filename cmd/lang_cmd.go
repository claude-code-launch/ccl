package cmd

import (
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/spf13/cobra"
)

var langCmd = &cobra.Command{
	Use:   "lang [zh|en]",
	Short: "Set the display language",
	Long: `Set the display language for ccl prompts.

Without arguments, shows the current language and offers a choice.

Examples:
  ccl lang        # show current and choose
  ccl lang zh     # switch to Chinese
  ccl lang en     # switch to English
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		current := locale.Current()

		if len(args) > 0 {
			code := strings.ToLower(strings.TrimSpace(args[0]))
			if code == "zh" || code == "cn" || strings.HasPrefix(code, "zh") {
				code = "zh"
			} else {
				code = "en"
			}
			if err := saveLangConfig(code); err != nil {
				return err
			}
			locale.SetLanguage(code)
			if code == "zh" {
				fmt.Println("✅ 已切换为中文")
			} else {
				fmt.Println("✅ Switched to English")
			}
			return nil
		}

		display := "English"
		if strings.HasPrefix(current, "zh") {
			display = "中文"
		}
		fmt.Println(locale.Tf("当前语言: %s", "Current language: %s", display))
		fmt.Println(locale.T("当前语言显示", "Current display language"))
		fmt.Println("1. 中文")
		fmt.Println("2. English")
		fmt.Print(locale.T("请输入 (1/2): ", "Enter (1/2): "))

		var choice string
		fmt.Scanln(&choice)
		choice = strings.TrimSpace(choice)

		code := "en"
		if choice == "1" {
			code = "zh"
		}
		if err := saveLangConfig(code); err != nil {
			return err
		}
		locale.SetLanguage(code)
		if code == "zh" {
			fmt.Println("✅ 已切换为中文")
		} else {
			fmt.Println("✅ Switched to English")
		}
		return nil
	},
}

func saveLangConfig(code string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	cfg.Lang = code
	return config.Save(cfg)
}

func init() {
	rootCmd.AddCommand(langCmd)
}
