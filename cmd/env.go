package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/spf13/cobra"
)

var deleteFlag bool

var envCmd = &cobra.Command{
	Use:   "env [provider] [KEY=VALUE... / KEY...]",
	Short: "Manage environment variables for a provider",
	Long: `Manage provider-specific environment variables.
These variables will be injected when launching Claude.

To run the interactive variable manager:
  ccl env [provider]

To set/modify variables via CLI:
  ccl env [provider] KEY=VALUE [KEY2=VALUE2...]

To delete variables via CLI:
  ccl env [provider] --del KEY [KEY2...]
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		var targetProvider string
		var actionArgs []string

		// Determine which provider and which arguments are action arguments
		if len(args) > 0 {
			firstArg := args[0]
			// Check if first arg is a known provider
			if _, exists := cfg.Providers[firstArg]; exists {
				targetProvider = firstArg
				actionArgs = args[1:]
			} else {
				// If not a known provider, default to active provider
				if cfg.ActiveProvider == "" {
					return fmt.Errorf("no active provider set and %q is not a known provider", firstArg)
				}
				targetProvider = cfg.ActiveProvider
				actionArgs = args
			}
		} else {
			if cfg.ActiveProvider == "" {
				return fmt.Errorf("no active provider set. Specify a provider name: ccl env <provider>")
			}
			targetProvider = cfg.ActiveProvider
		}

		p := cfg.Providers[targetProvider]
		if p.Env == nil {
			p.Env = make(map[string]string)
		}

		// If we do not have action args and the user did not specify --del flag,
		// launch the interactive env manager!
		if len(actionArgs) == 0 && !deleteFlag {
			for {
				var choice string
				var choices []huh.Option[string]

				choices = append(choices, huh.NewOption("List all variables", "list"))
				choices = append(choices, huh.NewOption("Add new variable", "add"))
				if len(p.Env) > 0 {
					choices = append(choices, huh.NewOption("Edit existing variable", "edit"))
					choices = append(choices, huh.NewOption("Delete variable", "delete"))
				}
				choices = append(choices, huh.NewOption("Save and Exit", "exit"))

				err = huh.NewForm(
					huh.NewGroup(
						huh.NewSelect[string]().
							Title(fmt.Sprintf("Manage Env for %q", targetProvider)).
							Options(choices...).
							Value(&choice),
					),
				).Run()
				if err != nil {
					return err
				}

				if choice == "exit" {
					break
				}

				switch choice {
				case "list":
					if len(p.Env) == 0 {
						fmt.Println("\nNo custom environment variables configured.")
					} else {
						fmt.Printf("\nEnvironment variables for %q:\n", targetProvider)
						var keys []string
						for k := range p.Env {
							keys = append(keys, k)
						}
						sort.Strings(keys)
						for _, k := range keys {
							fmt.Printf("  %s=%s\n", k, p.Env[k])
						}
					}
					fmt.Println("\nPress Enter to return to menu...")
					var dummy string
					_ = huh.NewForm(
						huh.NewGroup(
							huh.NewInput().
								Title("Press Enter to continue").
								Value(&dummy),
						),
					).Run()

				case "add":
					var k, v string
					err = huh.NewForm(
						huh.NewGroup(
							huh.NewInput().
								Title("Key Name").
								Description("e.g. CLAUDE_AUTOCOMPACT_PCT_OVERRIDE").
								Value(&k).
								Validate(func(str string) error {
									if strings.TrimSpace(str) == "" {
										return fmt.Errorf("key cannot be empty")
									}
									if strings.Contains(str, "=") {
										return fmt.Errorf("key cannot contain '='")
									}
									return nil
								}),
							huh.NewInput().
								Title("Value").
								Value(&v),
						),
					).Run()
					if err != nil {
						return err
					}

					k = strings.TrimSpace(k)
					v = strings.TrimSpace(v)
					p.Env[k] = v
					fmt.Printf("\n✅ Added: %s=%s\n\n", k, v)

				case "edit":
					var keys []string
					for k := range p.Env {
						keys = append(keys, k)
					}
					sort.Strings(keys)

					var editOptions []huh.Option[string]
					for _, k := range keys {
						editOptions = append(editOptions, huh.NewOption(fmt.Sprintf("%s (%s)", k, p.Env[k]), k))
					}

					var selectedKey string
					err = huh.NewForm(
						huh.NewGroup(
							huh.NewSelect[string]().
								Title("Select Key to Edit").
								Options(editOptions...).
								Value(&selectedKey),
						),
					).Run()
					if err != nil {
						return err
					}

					if selectedKey == "" {
						continue
					}

					var newValue string = p.Env[selectedKey]
					err = huh.NewForm(
						huh.NewGroup(
							huh.NewInput().
								Title(fmt.Sprintf("New value for %s", selectedKey)).
								Value(&newValue),
						),
					).Run()
					if err != nil {
						return err
					}

					p.Env[selectedKey] = strings.TrimSpace(newValue)
					fmt.Printf("\n✅ Updated: %s=%s\n\n", selectedKey, p.Env[selectedKey])

				case "delete":
					var keys []string
					for k := range p.Env {
						keys = append(keys, k)
					}
					sort.Strings(keys)

					var delOptions []huh.Option[string]
					for _, k := range keys {
						delOptions = append(delOptions, huh.NewOption(fmt.Sprintf("%s (%s)", k, p.Env[k]), k))
					}

					var selectedKey string
					err = huh.NewForm(
						huh.NewGroup(
							huh.NewSelect[string]().
								Title("Select Key to Delete").
								Options(delOptions...).
								Value(&selectedKey),
						),
					).Run()
					if err != nil {
						return err
					}

					if selectedKey == "" {
						continue
					}

					var confirm bool
					err = huh.NewForm(
						huh.NewGroup(
							huh.NewConfirm().
								Title(fmt.Sprintf("Delete %s?", selectedKey)).
								Value(&confirm),
						),
					).Run()
					if err != nil {
						return err
					}

					if confirm {
						delete(p.Env, selectedKey)
						fmt.Printf("\n✅ Deleted %s\n\n", selectedKey)
					}
				}
			}

			// Save config after exit
			cfg.Providers[targetProvider] = p
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}
			fmt.Printf("✅ Saved all changes for provider %q.\n", targetProvider)
			return nil
		}

		if deleteFlag {
			if len(actionArgs) == 0 {
				return fmt.Errorf("please specify at least one key to delete: ccl env %s --del KEY", targetProvider)
			}
			deletedKeys := []string{}
			for _, key := range actionArgs {
				if _, exists := p.Env[key]; exists {
					delete(p.Env, key)
					deletedKeys = append(deletedKeys, key)
				} else {
					fmt.Printf("⚠️  Key %q not found in provider %q env\n", key, targetProvider)
				}
			}
			if len(deletedKeys) > 0 {
				cfg.Providers[targetProvider] = p
				if err := config.Save(cfg); err != nil {
					return fmt.Errorf("failed to save config: %w", err)
				}
				fmt.Printf("✅ Deleted environment variables from %q: %s\n", targetProvider, strings.Join(deletedKeys, ", "))
			}
			return nil
		}

		// If we have action args, we should set/modify them
		if len(actionArgs) > 0 {
			updated := make(map[string]string)
			for _, arg := range actionArgs {
				k, v, ok := strings.Cut(arg, "=")
				if !ok {
					return fmt.Errorf("invalid environment variable format: %q (expected KEY=VALUE)", arg)
				}
				k = strings.TrimSpace(k)
				v = strings.TrimSpace(v)
				if k == "" {
					return fmt.Errorf("environment variable key cannot be empty in %q", arg)
				}
				p.Env[k] = v
				updated[k] = v
			}

			cfg.Providers[targetProvider] = p
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("✅ Updated environment variables for %q:\n", targetProvider)
			var keys []string
			for k := range updated {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Printf("  %s=%s\n", k, updated[k])
			}
			return nil
		}

		return nil
	},
}

func init() {
	envCmd.Flags().BoolVarP(&deleteFlag, "del", "d", false, "Delete specified environment variable keys")
	rootCmd.AddCommand(envCmd)
}
