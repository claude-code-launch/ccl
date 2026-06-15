package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
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

			baseSlotNames := []string{
				"Default (general)",
				"Opus",
				"Sonnet",
				"Haiku",
				"Custom (user-defined slot)",
			}

			slotMap := map[string]*string{
				"Default (general)":          &p.CustomModelID,
				"Opus":                       &p.OpusModel,
				"Sonnet":                     &p.SonnetModel,
				"Haiku":                      &p.HaikuModel,
				"Custom (user-defined slot)": &p.LockModel,
			}

			var poolOptions []huh.Option[string]
			sortedModels := make([]string, len(selectedModels))
			copy(sortedModels, selectedModels)
			sort.Strings(sortedModels)
			for _, m := range sortedModels {
				poolOptions = append(poolOptions, huh.NewOption(m, m))
			}
			manualToken := "(Enter custom model ID)"

			red := lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
			dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

			// buildOpts builds the flat list: slot row + 1M toggle row per slot, then Done
			buildOpts := func() []huh.Option[string] {
				var opts []huh.Option[string]
				for _, name := range baseSlotNames {
					// Slot row
					slotLabel := fmt.Sprintf("%s — current: (not set)", name)
					if ptr, ok := slotMap[name]; ok && ptr != nil && *ptr != "" {
						slotLabel = fmt.Sprintf("%s — current: %s", name, strings.TrimSuffix(*ptr, "[1m]"))
					}
					opts = append(opts, huh.NewOption(slotLabel, "slot:"+name))

					// 1M toggle row
					ptr := slotMap[name]
					if ptr != nil && strings.HasSuffix(*ptr, "[1m]") {
						opts = append(opts, huh.NewOption("  "+red.Render("[x] 1m"), "toggle:"+name))
					} else {
						opts = append(opts, huh.NewOption("  "+dim.Render("[ ] 1m"), "toggle:"+name))
					}
				}
				opts = append(opts, huh.NewOption("Done", "done"))
				return opts
			}

			pick := "slot:Default (general)" // default cursor position
			for {
				err = huh.NewForm(
					huh.NewGroup(
						huh.NewSelect[string]().
							Title("Claude Slot Mapping").
							Description("Select a slot to configure its model, or select [ ] 1m to toggle.").
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

				if strings.HasPrefix(pick, "toggle:") {
					slotName := strings.TrimPrefix(pick, "toggle:")
					fieldPtr, ok := slotMap[slotName]
					if ok && fieldPtr != nil {
						base := strings.TrimSuffix(*fieldPtr, "[1m]")
						if strings.HasSuffix(*fieldPtr, "[1m]") {
							*fieldPtr = base
						} else if base != "" {
							*fieldPtr = base + "[1m]"
							if p.Env == nil {
								p.Env = make(map[string]string)
							}
							p.Env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] = "1000000"
						}
					}
					// Stay on the toggle row to show updated state
					continue
				}

				// pick is "slot:Name"
				slotName := strings.TrimPrefix(pick, "slot:")
				fieldPtr, ok := slotMap[slotName]
				if !ok {
					fmt.Printf("⚠️  No mapping for slot %q, skipping.\n", slotName)
					continue
				}

				var chosenOpts []huh.Option[string]
				chosenOpts = append(chosenOpts, poolOptions...)
				chosenOpts = append(chosenOpts, huh.NewOption(manualToken, manualToken))

				defaultChoice := ""
				if fieldPtr != nil && *fieldPtr != "" {
					baseVal := strings.TrimSuffix(*fieldPtr, "[1m]")
					if stringInSlice(baseVal, sortedModels) {
						defaultChoice = baseVal
					} else {
						defaultChoice = manualToken
					}
				}

				chosen := defaultChoice
				err = huh.NewForm(
					huh.NewGroup(
						huh.NewSelect[string]().
							Title(fmt.Sprintf("Set model for %s", slotName)).
							Description("Type to filter. Choose from model pool or select manual entry.").
							Options(chosenOpts...).
							Filtering(true).
							Value(&chosen),
					),
				).Run()
				if err != nil {
					return err
				}

				wasEnabled1M := fieldPtr != nil && strings.HasSuffix(*fieldPtr, "[1m]")

				if chosen == manualToken || chosen == "" {
					var manual string
					baseVal := strings.TrimSuffix(*fieldPtr, "[1m]")
					if fieldPtr != nil && baseVal != "" && !stringInSlice(baseVal, sortedModels) {
						manual = baseVal
					}
					err = huh.NewForm(
						huh.NewGroup(
							huh.NewInput().
								Title(fmt.Sprintf("%s - Manual Entry", slotName)).
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

				// Restore 1M suffix after model change
				if *fieldPtr != "" {
					base := strings.TrimSuffix(*fieldPtr, "[1m]")
					if wasEnabled1M {
						*fieldPtr = base + "[1m]"
					} else {
						*fieldPtr = base
					}
				}

				fmt.Printf("✅ %s set to %q\n\n", slotName, *fieldPtr)
				// Return cursor to the slot row after model config
				pick = "slot:" + slotName
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
