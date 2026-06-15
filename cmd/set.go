package cmd

import (
	"errors"
	"fmt"
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

			// 列表式槽位配置
			baseSlotLabels := []string{
				"Default (general)",
				"Opus",
				"Sonnet",
				"Haiku",
				"Custom (user-defined slot)",
				"Done",
			}

			slotMap := map[string]*string{
				"Default (general)":          &p.CustomModelID,
				"Opus":                       &p.OpusModel,
				"Sonnet":                     &p.SonnetModel,
				"Haiku":                      &p.HaikuModel,
				"Custom (user-defined slot)": &p.LockModel,
			}

			// Ordered slot names (no "Done") for 1M MultiSelect
			slotNames := []string{
				"Default (general)",
				"Opus",
				"Sonnet",
				"Haiku",
				"Custom (user-defined slot)",
			}

			// poolOptions（从 selectedModels）
			var poolOptions []huh.Option[string]
			for _, m := range selectedModels {
				poolOptions = append(poolOptions, huh.NewOption(m, m))
			}
			manualToken := "(Enter custom model ID)"

			// formatLabel shows slot name with current model (no 1M indicator — managed by right column)
			formatLabel := func(base string) string {
				if base == "Done" {
					return base
				}
				if ptr, ok := slotMap[base]; ok && ptr != nil && *ptr != "" {
					return fmt.Sprintf("%s — current: %s", base, strings.TrimSuffix(*ptr, "[1m]"))
				}
				return fmt.Sprintf("%s — current: (not set)", base)
			}

			// get1MSlots returns slot names that currently have [1m] suffix
			get1MSlots := func() []string {
				var active []string
				for _, name := range slotNames {
					if ptr, ok := slotMap[name]; ok && ptr != nil && strings.HasSuffix(*ptr, "[1m]") {
						active = append(active, name)
					}
				}
				return active
			}

			// apply1MSlots applies 1M MultiSelect state to all configured slots
			apply1MSlots := func(selected []string) {
				hasAny := false
				for _, name := range slotNames {
					ptr, ok := slotMap[name]
					if !ok || ptr == nil || *ptr == "" {
						continue
					}
					base := strings.TrimSuffix(*ptr, "[1m]")
					if stringInSlice(name, selected) {
						*ptr = base + "[1m]"
						hasAny = true
					} else {
						*ptr = base
					}
				}
				if hasAny {
					if p.Env == nil {
						p.Env = make(map[string]string)
					}
					p.Env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] = "1000000"
				}
			}

			// Build 1M options (all slots)
			var oneMOpts []huh.Option[string]
			for _, name := range slotNames {
				oneMOpts = append(oneMOpts, huh.NewOption(name, name))
			}

			for {
				// 构造槽位选项
				var opts []huh.Option[string]
				for _, base := range baseSlotLabels {
					opts = append(opts, huh.NewOption(formatLabel(base), base))
				}

				var pick string
				current1M := get1MSlots()

				// 外层：左列=槽位选择，右列=1M MultiSelect
				err = huh.NewForm(
					huh.NewGroup(
						huh.NewSelect[string]().
							Title("Claude Slot Mapping").
							Description("Select a slot to configure. Choose Done to finish.").
							Options(opts...).
							Value(&pick),
					),
					huh.NewGroup(
						huh.NewMultiSelect[string]().
							Title("1M context").
							Options(oneMOpts...).
							Value(&current1M),
					),
				).WithLayout(huh.LayoutColumns(2)).Run()
				if err != nil {
					return err
				}

				// Apply 1M changes from right column
				apply1MSlots(current1M)

				if pick == "Done" || pick == "" {
					break
				}

				fieldPtr, ok := slotMap[pick]
				if !ok {
					fmt.Printf("⚠️  No mapping for slot %q, skipping.\n", pick)
					continue
				}

				// 记录当前槽位的 1M 状态，model 更换后保留
				wasEnabled1M := stringInSlice(pick, current1M)

				// 内层：单列选模型（不含 1M，由外层管理）
				modelOpts := append([]huh.Option[string]{}, poolOptions...)
				modelOpts = append(modelOpts, huh.NewOption(manualToken, manualToken))

				defaultChoice := ""
				if fieldPtr != nil && *fieldPtr != "" {
					baseVal := strings.TrimSuffix(*fieldPtr, "[1m]")
					if stringInSlice(baseVal, selectedModels) {
						defaultChoice = baseVal
					} else {
						defaultChoice = manualToken
					}
				}

				chosen := defaultChoice
				err = huh.NewForm(
					huh.NewGroup(
						huh.NewSelect[string]().
							Title(fmt.Sprintf("Set model for %s", pick)).
							Description("Choose from model pool or select manual entry to type a model ID").
							Options(modelOpts...).
							Value(&chosen),
					),
				).Run()
				if err != nil {
					return err
				}

				if chosen == manualToken || chosen == "" {
					var manual string
					if fieldPtr != nil && *fieldPtr != "" && !stringInSlice(strings.TrimSuffix(*fieldPtr, "[1m]"), selectedModels) {
						manual = strings.TrimSuffix(*fieldPtr, "[1m]")
					}
					err = huh.NewForm(
						huh.NewGroup(
							huh.NewInput().
								Title(fmt.Sprintf("%s - Manual Entry", pick)).
								Description("Enter model ID for this slot").
								Value(&manual),
						),
					).Run()
					if err != nil {
						return err
					}
					*fieldPtr = strings.TrimSpace(manual)
				} else {
					*fieldPtr = chosen
				}

				// Re-apply 1M suffix for this slot (preserve state after model change)
				if *fieldPtr != "" {
					base := strings.TrimSuffix(*fieldPtr, "[1m]")
					if wasEnabled1M {
						*fieldPtr = base + "[1m]"
					} else {
						*fieldPtr = base
					}
				}

				fmt.Printf("✅ %s set to %q\n\n", pick, *fieldPtr)
				// 配置完成后循环回到槽位列表
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
