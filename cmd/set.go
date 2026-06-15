package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

var setCmd = &cobra.Command{
	Use:   "set [name]",
	Short: "Add or update an LLM provider configuration",
	Long: `Add a new provider or update an existing one.
You can automatically discover models from the API endpoint, or enter them manually.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// 1. 加载配置
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		if cfg.Providers == nil {
			cfg.Providers = make(map[string]provider.Provider)
		}

		// 2. 获取 provider 名称
		var targetName string
		if len(args) > 0 {
			targetName = strings.TrimSpace(args[0])
		}
		if targetName == "" {
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title("Provider Name").
						Description("A unique identifier (e.g., openrouter, deepseek, my-company)").
						Value(&targetName).
						Validate(func(str string) error {
							if strings.TrimSpace(str) == "" {
								return errors.New("provider name cannot be empty")
							}
							return nil
						}),
				),
			).Run()
			if err != nil {
				return err
			}
		}

		// 3. 新建或加载已有配置
		var p provider.Provider
		isUpdate := false
		if existing, exists := cfg.Providers[targetName]; exists {
			p = existing
			isUpdate = true
			fmt.Printf("🔄 Updating existing provider %q...\n\n", targetName)
		} else {
			p.Name = targetName
			fmt.Printf("✨ Creating new provider %q...\n\n", targetName)
		}

		// Step 1: 基础凭据
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title("Endpoint URL").
					Description("Base API endpoint, e.g. https://api.openai.com/v1").
					Value(&p.Endpoint).
					Validate(func(str string) error {
						if strings.TrimSpace(str) == "" {
							return errors.New("endpoint cannot be empty")
						}
						return nil
					}),

				huh.NewInput().
					Title("API Key").
					Description("Your API key, stored locally only").
					Value(&p.APIKey).
					EchoMode(huh.EchoModePassword).
					Validate(func(str string) error {
						if strings.TrimSpace(str) == "" {
							return errors.New("API Key cannot be empty")
						}
						return nil
					}),
			),
		).Run()
		if err != nil {
			return err
		}

		// Step 2: 自动探测协议与模型
		fmt.Println("\n🔍 Connecting to endpoint to detect protocol and models...")
		detectedType, discoveredModelsRaw := detectProtocolAndModels(p.Endpoint, p.APIKey)
		p.Type = detectedType

		if detectedType != "" {
			fmt.Printf("✅ Detected Protocol: %s\n", strings.ToUpper(p.Type))
		} else {
			fmt.Println("⚠️  Could not detect protocol automatically")
		}

		var confirmType = true
		if detectedType != "" {
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewConfirm().
						Title(fmt.Sprintf("Use %s protocol?", strings.ToUpper(p.Type))).
						Value(&confirmType),
				),
			).Run()
			if err != nil {
				return err
			}
		}

		if !confirmType || detectedType == "" {
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewSelect[string]().
						Title("Select Provider Type").
						Options(
							huh.NewOption("OpenAI Compatible", "openai"),
							huh.NewOption("Anthropic Native", "anthropic"),
						).
						Value(&p.Type),
				),
			).Run()
			if err != nil {
				return err
			}
		}

		// -----------------------------
		// 构建 model pool（不再提示选择模型）
		// -----------------------------
		var discoveredModels []string
		if discoveredModelsRaw != "" {
			for _, m := range strings.Split(discoveredModelsRaw, ",") {
				m = strings.TrimSpace(m)
				if m != "" {
					discoveredModels = append(discoveredModels, m)
				}
			}
		}

		// 如果用户已有 p.Model（旧配置），也把它加入 pool
		var selectedModels []string
		if p.Model != "" {
			for _, m := range strings.Split(p.Model, ",") {
				m = strings.TrimSpace(m)
				if m != "" && !stringInSlice(m, selectedModels) {
					selectedModels = append(selectedModels, m)
				}
			}
		}
		// 把探测到的模型加入 pool（优先）
		for _, m := range discoveredModels {
			if !stringInSlice(m, selectedModels) {
				selectedModels = append(selectedModels, m)
			}
		}

		// 如果没有任何模型（既没探测到也没旧配置），提示用户输入模型池（逗号分隔）
		if len(selectedModels) == 0 {
			fmt.Println("⚠️  No models discovered and no existing model list found.")
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title("Model List").
						Description("Comma separated list of model IDs to populate the model pool").
						Value(&p.Model),
				),
			).Run()
			if err != nil {
				return err
			}
			for _, m := range strings.Split(p.Model, ",") {
				m = strings.TrimSpace(m)
				if m != "" && !stringInSlice(m, selectedModels) {
					selectedModels = append(selectedModels, m)
				}
			}
		} else {
			// 把 pool 写回 p.Model（保持兼容）
			p.Model = strings.Join(selectedModels, ",")
		}

		// ------------------------------------------------------------
		// 直接显示 Claude Code 高级配置确认（不再单独做“选择模型”步骤）
		// ------------------------------------------------------------
		var configAdvanced bool
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Configure Claude Code advanced model options?").
					Description("Includes per-slot mapping (default/opus/sonnet/haiku/custom), effort level, and model lock.").
					Value(&configAdvanced),
			),
		).Run()
		if err != nil {
			return err
		}

		if configAdvanced {
			fmt.Println("\n--- Claude Code Advanced Model Configuration ---")

			sortedModels := make([]string, len(selectedModels))
			copy(sortedModels, selectedModels)
			sort.Strings(sortedModels)

			slots := []slotEntry{
				{name: "Default (general)", model: &p.CustomModelID},
				{name: "Opus", model: &p.OpusModel},
				{name: "Sonnet", model: &p.SonnetModel},
				{name: "Haiku", model: &p.HaikuModel},
				{name: "Custom (user-defined slot)", model: &p.LockModel},
			}

			setEnv := func(key, val string) {
				if p.Env == nil {
					p.Env = make(map[string]string)
				}
				p.Env[key] = val
			}

			if err = RunSlotListTUI(slots, sortedModels, setEnv); err != nil {
				return err
			}

			// Effort Level
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewSelect[string]().
						Title("Effort Level").
						Description("CLAUDE_CODE_EFFORT_LEVEL").
						Options(
							huh.NewOption("(unset)", ""),
							huh.NewOption("Low", "low"),
							huh.NewOption("medium", "medium"),
							huh.NewOption("high", "high"),
							huh.NewOption("xhigh", "xhigh"),
							huh.NewOption("max", "max"),
							huh.NewOption("ultracode", "ultracode"),
						).
						Value(&p.EffortLevel),
				),
			).Run()
			if err != nil {
				return err
			}
		}

		// Step 4: 保存
		cfg.Providers[p.Name] = p

		if cfg.ActiveProvider != p.Name {
			var activateNow bool
			_ = huh.NewForm(
				huh.NewGroup(
					huh.NewConfirm().
						Title("Set this as your active provider now?").
						Value(&activateNow),
				),
			).Run()
			if activateNow {
				cfg.ActiveProvider = p.Name
			}
		}
		if cfg.ActiveProvider == "" {
			cfg.ActiveProvider = p.Name
		}

		err = config.Save(cfg)
		if err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Println("")
		if isUpdate {
			fmt.Printf("✅ Successfully updated provider %q\n", p.Name)
		} else {
			fmt.Printf("✅ Successfully added provider %q\n", p.Name)
		}
		if cfg.ActiveProvider == p.Name {
			fmt.Println("🔥 This provider is now active")
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(setCmd)
}

func SetCMD() *cobra.Command {
	return setCmd
}

// 辅助：字符串是否在切片中
func stringInSlice(s string, slice []string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func detectProtocolAndModels(endpoint, apiKey string) (string, string) {
	endpoint = strings.TrimSuffix(endpoint, "/")

	// 优先探测 Anthropic
	if models, err := protocol.GetAnthropicModels(endpoint, apiKey); err == nil {
		return "anthropic", models
	}

	// 再探测 OpenAI，顺便解析模型列表
	if models, err := protocol.GetOpenAIModels(endpoint, apiKey); err == nil {
		return "openai", models
	}

	// 兜底 URL 启发式，模型列表为空
	if strings.Contains(endpoint, "anthropic.com") {
		return "anthropic", ""
	}
	return "openai", ""
}
