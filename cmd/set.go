package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"charm.land/bubbles/v2/key"
	"charm.land/lipgloss/v2"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

// lang is the language setting for interactive prompts, set at the start of RunConfSet.
var lang = Lang{code: "en"}

var setCmd = &cobra.Command{
	Use:   "set [name]",
	Short: "Add or update an LLM provider configuration",
	Long: `Add a new provider or update an existing one.
You can automatically discover models from the API endpoint, or enter them manually.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunConfSet(args)
	},
}


	// RunConfSet is the shared logic for "ccl set" / "ccl conf set" / "ccl conf".
	func RunConfSet(args []string) error {
		// Language selection
		_ = huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Language / 语言").
					Description("Choose your preferred language for prompts").
					Options(
						huh.NewOption("English", "en"),
						huh.NewOption("中文", "cn"),
					).
					Value(&lang.code),
			),
		).Run()

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
						Title(lang.T("Provider Name", "Provider Name")).
						Description(lang.T("例如：openrouter、deepseek、my-company", "A unique identifier (e.g., openrouter, deepseek, my-company)")).
						Value(&targetName).
						Validate(func(str string) error {
							if strings.TrimSpace(str) == "" {
								return errors.New(lang.T("provider name 不能为空", "provider name cannot be empty"))
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
			fmt.Printf("🔄 %s %q...\n\n", lang.T("正在更新", "Updating existing provider"), targetName)
		} else {
			p.Name = targetName
			fmt.Printf("✨ %s %q...\n\n", lang.T("正在创建", "Creating new provider"), targetName)
		}

		// Step 1: 基础凭据（URL 和 Key 拆为两个 Group，PageUp 可回退）
		// 创建自定义 KeyMap，添加 PageUp 支持回退到上一个 Group
		customKeyMap := huh.NewDefaultKeyMap()
		customKeyMap.Input.Prev = key.NewBinding(key.WithKeys("shift+tab", "pgup"), key.WithHelp("shift+tab/pgup", "back"))
		customKeyMap.Input.Next = key.NewBinding(key.WithKeys("enter", "tab"), key.WithHelp("enter", "next"))
		customKeyMap.Input.Submit = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "submit"))

		err = huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title(lang.T("Endpoint URL", "Endpoint URL")).
					Description(lang.T("API 端点地址，例如：https://api.openai.com/v1", "Base API endpoint, e.g. https://api.openai.com/v1")).
					Value(&p.Endpoint).
					Validate(func(str string) error {
						if strings.TrimSpace(str) == "" {
							return errors.New(lang.T("endpoint 不能为空", "endpoint cannot be empty"))
						}
						return nil
					}),
			),
			huh.NewGroup(
				huh.NewInput().
					Title(lang.T("API Key", "API Key")).
					Description(lang.T("你的 API Key，仅存储在本地 · 按 PageUp 返回修改 URL", "Your API key, stored locally only · Press PageUp to go back to URL")).
					Value(&p.APIKey).
					EchoMode(huh.EchoModePassword).
					Validate(func(str string) error {
						if strings.TrimSpace(str) == "" {
							return errors.New(lang.T("API Key 不能为空", "API Key cannot be empty"))
						}
						return nil
					}),
			),
		).WithKeyMap(customKeyMap).Run()
		if err != nil {
			return err
		}

		// Step 2: 自动探测协议与模型
		fmt.Println(lang.T("\n🔍 正在连接端点，探测协议和模型...", "\n🔍 Connecting to endpoint to detect protocol and models..."))
		detectedType, discoveredModelsRaw := detectProtocolAndModels(p.Endpoint, p.APIKey)
		p.Type = detectedType

		if detectedType != "" {
			fmt.Printf("✅ %s: %s\n", lang.T("检测到协议", "Detected Protocol"), strings.ToUpper(p.Type))
		} else {
			fmt.Println(lang.T("⚠️  无法自动检测协议", "⚠️  Could not detect protocol automatically"))
		}

		var confirmType = true
		if detectedType != "" {
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewConfirm().
						Title(lang.Tf("使用 %s 协议？", "Use %s protocol?", strings.ToUpper(p.Type))).
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
						Title(lang.T("选择 Provider 类型", "Select Provider Type")).
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
			fmt.Println(lang.T("⚠️  未探测到模型，也没有已有的模型列表。", "⚠️  No models discovered and no existing model list found."))
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title(lang.T("模型列表", "Model List")).
						Description(lang.T("逗号分隔的模型 ID 列表，用于填充模型池", "Comma separated list of model IDs to populate the model pool")).
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
		var configAdvanced = true
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(lang.T("配置 Claude Code 高级模型选项？", "Configure Claude Code advanced model options?")).
					Description(lang.T("包含每个 slot 的映射（opus/sonnet/haiku/custom）、effort level 和 model lock。", "Includes per-slot mapping (opus/sonnet/haiku/custom), effort level, and model lock.")).
					Value(&configAdvanced),
			),
		).Run()
		if err != nil {
			return err
		}

		if configAdvanced {
			fmt.Println(lang.T("\n--- Claude Code 高级模型配置 ---", "\n--- Claude Code Advanced Model Configuration ---"))

			baseSlotNames := []string{
				"Opus",
				"Opus",
				"Sonnet",
				"Haiku",
				"Custom (user-defined slot)",
			}

			slotMap := map[string]*string{
				"Opus":                       &p.OpusModel,
				"Sonnet":                     &p.SonnetModel,
				"Haiku":                      &p.HaikuModel,
				"Custom (user-defined slot)": &p.LockModel,
			}

			sortedModels := make([]string, len(selectedModels))
			copy(sortedModels, selectedModels)
			sort.Strings(sortedModels)

			red := lipgloss.NewStyle().Foreground(lipgloss.Color("9"))

			buildOpts := func() []huh.Option[string] {
				var opts []huh.Option[string]
				for _, name := range baseSlotNames {
					ptr := slotMap[name]
					var indicator string
					if ptr != nil && strings.HasSuffix(*ptr, "[1m]") {
						indicator = "  " + red.Render("⚡1M")
					}

					slotLabel := fmt.Sprintf("%s — %s%s", name, lang.T("当前: (未设置)", "current: (not set)"), indicator)
					if ptr != nil && *ptr != "" {
						slotLabel = fmt.Sprintf("%s — %s%s%s", name, lang.T("当前: ", "current: "), strings.TrimSuffix(*ptr, "[1m]"), indicator)
					}
					opts = append(opts, huh.NewOption(slotLabel, "slot:"+name))
				}
				opts = append(opts, huh.NewOption(lang.T("完成", "Done"), "done"))
				return opts
			}

			pick := "slot:Opus"
			for {
				err = huh.NewForm(
					huh.NewGroup(
						huh.NewSelect[string]().
							Title(lang.T("Claude Slot 映射", "Claude Slot Mapping")).
							Description(lang.T("选择一个 slot 配置模型，或选择 [ ] 1m 切换大上下文模式。", "Select a slot to configure its model, or select [ ] 1m to toggle.")).
							Options(buildOpts()...).
							Value(&pick),
					),
				).Run()
				if err != nil {
					return err
				}

				if pick == "done" || pick == "" {
						break
					}

					// pick is "slot:Name" → launch dual-panel TUI for model + 1M config
					slotName := strings.TrimPrefix(pick, "slot:")
				fieldPtr, ok := slotMap[slotName]
				if !ok {
					fmt.Printf("⚠️  %s %q，跳过。\n", lang.T("未找到 slot 映射", "No mapping for slot"), slotName)
					continue
				}

				currentModel := strings.TrimSuffix(*fieldPtr, "[1m]")
				wasEnabled1M := strings.HasSuffix(*fieldPtr, "[1m]")

				res, err := RunSlotConfigTUI(slotName, sortedModels, currentModel, wasEnabled1M)
				if err != nil {
					return err
				}
				if res.cancelled {
					pick = "slot:" + slotName
					continue
				}

				if res.manual {
					var manual string
					if currentModel != "" && !stringInSlice(currentModel, sortedModels) {
						manual = currentModel
					}
					err = huh.NewForm(
						huh.NewGroup(
							huh.NewInput().
								Title(lang.Tf("%s - 手动输入", "%s - Manual Entry", slotName)).
								Description(lang.T("输入该 slot 的模型 ID", "Enter model ID for this slot")).
								Value(&manual),
						),
					).Run()
					if err != nil {
						return err
					}
					*fieldPtr = strings.TrimSpace(manual)
				} else {
					*fieldPtr = res.model
				}

				// Apply 1M suffix
				if *fieldPtr != "" {
					base := strings.TrimSuffix(*fieldPtr, "[1m]")
					if res.enable1M {
						*fieldPtr = base + "[1m]"
						if p.Env == nil {
							p.Env = make(map[string]string)
						}
						p.Env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] = "1000000"
					} else {
						*fieldPtr = base
					}
				}

				fmt.Printf("✅ %s %s：%q\n\n", slotName, lang.T("设置为", "set to"), *fieldPtr)
				pick = "slot:" + slotName
			}

			// Effort Level
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewSelect[string]().
						Title(lang.T("Effort Level（思考力度）", "Effort Level")).
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
						Title(lang.T("立即设为当前使用的 Provider？", "Set this as your active provider now?")).
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
			fmt.Printf("✅ %s %q\n", lang.T("已更新 Provider", "Successfully updated provider"), p.Name)
		} else {
			fmt.Printf("✅ %s %q\n", lang.T("已添加 Provider", "Successfully added provider"), p.Name)
		}
		if cfg.ActiveProvider == p.Name {
			fmt.Println(lang.T("🔥 此 Provider 现在是当前使用的", "🔥 This provider is now active"))
		}
		return nil
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
