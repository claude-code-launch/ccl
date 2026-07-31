package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/spf13/cobra"
)

// errLangNeedsChoice is returned when there is nothing to act on: no argument,
// and no answer to the prompt. Reporting the current language is then the whole
// job, so the config must not be rewritten as a side effect of being asked.
var errLangNeedsChoice = errors.New("no language given")

// parseLangArg maps what a user may type to the code stored in config.yaml.
// Anything unrecognized is an error rather than a silent fallback: `ccl lang zn`
// used to save English, reporting success for a language nobody asked for.
func parseLangArg(arg string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "zh", "cn", "zh-cn", "zh_cn", "chinese", "中文":
		return "zh-CN", nil
	case "zh-tw", "zh_tw", "zh-hk", "zh_hk", "tw", "hk":
		return "zh-TW", nil
	case "en", "en-us", "en_us", "english":
		return "en-US", nil
	default:
		return "", fmt.Errorf("unknown language %q: use zh, zh-TW or en", strings.TrimSpace(arg))
	}
}

// parseLangChoice maps the answer to the numbered prompt. An empty answer means
// the prompt was never answered (EOF, or a bare Enter), which keeps the current
// language instead of picking one.
func parseLangChoice(answer string) (string, error) {
	switch strings.TrimSpace(answer) {
	case "":
		return "", errLangNeedsChoice
	case "1":
		return "zh-CN", nil
	case "2":
		return "en-US", nil
	default:
		return "", fmt.Errorf("invalid choice %q: enter 1 or 2", strings.TrimSpace(answer))
	}
}

func langDisplayName(code string) string {
	if strings.HasPrefix(strings.ToLower(code), "zh") {
		return "中文"
	}
	return "English"
}

var langCmd = &cobra.Command{
	Use:   "lang [zh|zh-TW|en]",
	Short: "Set the display language",
	Long: `Set the display language for ccl prompts.

Without arguments, shows the current language and offers a choice. Answering
nothing leaves the configuration untouched.

CCL_LANG overrides the saved language, so an export in your shell wins over what
this command writes; ccl says so when that is the case.

Examples:
  ccl lang        # show current and choose
  ccl lang zh     # switch to Chinese
  ccl lang en     # switch to English
`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()

		if len(args) > 0 {
			code, err := parseLangArg(args[0])
			if err != nil {
				return err
			}
			return applyLang(out, code)
		}

		current := locale.Current()
		fmt.Fprintln(out, locale.Tf("当前语言: %s", "Current language: %s", langDisplayName(current)))

		// Without a terminal there is nobody to answer the prompt, so reporting
		// the current language is the whole job. Writing a language chosen by
		// an EOF is how `ccl lang` used to silently switch a shell to English.
		if !term.IsTerminal(os.Stdin.Fd()) {
			warnIfLangEnvOverrides(out)
			fmt.Fprintln(out, locale.T(
				"未指定语言，配置未改动。用 `ccl lang zh` 或 `ccl lang en` 切换。",
				"No language given, configuration left unchanged. Use `ccl lang zh` or `ccl lang en` to switch."))
			return nil
		}

		fmt.Fprintln(out, "1. 中文")
		fmt.Fprintln(out, "2. English")
		fmt.Fprint(out, locale.T("请输入 (1/2，留空保持不变): ", "Enter (1/2, blank keeps current): "))

		var answer string
		_, _ = fmt.Scanln(&answer)

		code, err := parseLangChoice(answer)
		if errors.Is(err, errLangNeedsChoice) {
			fmt.Fprintln(out, locale.T("配置未改动。", "Configuration left unchanged."))
			return nil
		}
		if err != nil {
			return err
		}
		return applyLang(out, code)
	},
}

// applyLang saves the language and reports the outcome, skipping the write when
// it would change nothing.
func applyLang(out io.Writer, code string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if cfg.Lang != code {
		cfg.Lang = code
		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}
	}
	locale.SetLanguage(code)

	if strings.HasPrefix(code, "zh") {
		fmt.Fprintln(out, "✅ 已切换为中文")
	} else {
		fmt.Fprintln(out, "✅ Switched to English")
	}
	warnIfLangEnvOverrides(out)
	return nil
}

// warnIfLangEnvOverrides reports a CCL_LANG export, which locale resolves ahead
// of config.yaml: without this the command claims to have switched languages
// while the next run still reads the one from the environment.
func warnIfLangEnvOverrides(out io.Writer) {
	env := strings.TrimSpace(os.Getenv("CCL_LANG"))
	if env == "" {
		return
	}
	fmt.Fprintln(out, locale.Tf(
		"⚠️  CCL_LANG=%s 优先于配置文件，实际显示语言仍由它决定。取消 export 后本设置才生效。",
		"⚠️  CCL_LANG=%s takes precedence over the config file and still decides the display language. Unset it for this setting to take effect.",
		env))
}

func init() {
	rootCmd.AddCommand(langCmd)
}
