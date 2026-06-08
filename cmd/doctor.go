package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/haiboyuwen/claude-code-launch/internal/claude"
	"github.com/haiboyuwen/claude-code-launch/internal/config"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check system prerequisites and provider connectivity",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Diagnosing ccl environment:")

		// 1. Check Node.js
		nodePath, err := exec.LookPath("node")
		if err != nil {
			fmt.Println("- Info: Node.js is not installed (no longer required by newer native Claude Code binaries)")
		} else {
			fmt.Printf("✓ Node.js found at: %s\n", nodePath)
		}

		// 2. Check Claude CLI
		claudeInstalled := claude.IsInstalled()
		if !claudeInstalled {
			fmt.Println("✗ Claude Code CLI is not installed or not in PATH.")
			// Prompt to install automatically
			err := claude.AutoInstall()
			if err != nil {
				fmt.Printf("✗ Auto-installation failed: %v. Please install manually by visiting: https://code.claude.com/\n", err)
			}
		} else {
			claudePath, _ := exec.LookPath("claude")
			fmt.Printf("✓ Claude Code CLI installed at: %s\n", claudePath)
		}

		// 3. Check Configuration File
		cfg, err := config.Load()
		if err != nil {
			fmt.Printf("✗ Failed to load config: %v\n", err)
			return nil
		}
		fmt.Printf("✓ Config file found at: %s\n", config.ConfigPath())

		// 4. Check Active Provider
		if cfg.ActiveProvider == "" {
			fmt.Println("✗ Active provider is not selected. Use 'ccl add' or 'ccl use'")
			return nil
		}
		fmt.Printf("✓ Active provider: %s\n", cfg.ActiveProvider)

		p, ok := cfg.Providers[cfg.ActiveProvider]
		if !ok {
			fmt.Printf("✗ Selected provider %q does not exist in config\n", cfg.ActiveProvider)
			return nil
		}

		fmt.Printf("  - Type: %s\n", p.Type)
		fmt.Printf("  - Endpoint: %s\n", p.Endpoint)
		fmt.Printf("  - Model: %s\n", p.Model)

		// 5. Test Endpoint reachability and API Authentication key
		if p.Endpoint != "" {
			fmt.Printf("  - Testing reachability and auth key validation...\n")
			client := http.Client{
				Timeout: 5 * time.Second,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			// Determine models URL precisely following how internal/proxy/server.go's fetchAvailableModels checks models
			endpoint := strings.TrimSuffix(p.Endpoint, "/")
			modelsURL := endpoint + "/models"
			if !strings.HasSuffix(endpoint, "/v1") {
				modelsURL = endpoint + "/v1/models"
				modelsURL = strings.Replace(modelsURL, "/v1/v1", "/v1", 1)
			}

			req, err := http.NewRequestWithContext(ctx, "GET", modelsURL, nil)
			if err != nil {
				fmt.Printf("\n✗ Failed to create validation request: %v\n", err)
				return nil
			}

			// Add API Key headers
			if p.Type == "openai" {
				req.Header.Set("Authorization", "Bearer "+p.APIKey)
			} else {
				req.Header.Set("x-api-key", p.APIKey)
				req.Header.Set("anthropic-version", "2023-06-01")
			}

			resp, err := client.Do(req)
			if err != nil {
				fmt.Printf("\n✗ Endpoint is unreachable: %v\n", err)
			} else {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					fmt.Printf(" Success! Connected and verified. (HTTP %d)\n", resp.StatusCode)
				} else if resp.StatusCode == http.StatusUnauthorized {
					fmt.Printf("\n✗ Authentication failed! (HTTP %d). Please verify your API Key.\n", resp.StatusCode)
				} else if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
					// Fallback strategy if GET models returns 404 or 403 on third-party proxies
					fallbackCtx, fallbackCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer fallbackCancel()

					fallbackReq, fallbackErr := http.NewRequestWithContext(fallbackCtx, "GET", p.Endpoint, nil)
					if fallbackErr != nil {
						fmt.Printf("\n✗ Models discovery returned HTTP %d. Failed to create fallback request: %v\n", resp.StatusCode, fallbackErr)
						return nil
					}

					if p.Type == "openai" {
						fallbackReq.Header.Set("Authorization", "Bearer "+p.APIKey)
					} else {
						fallbackReq.Header.Set("x-api-key", p.APIKey)
						fallbackReq.Header.Set("anthropic-version", "2023-06-01")
					}

					fallbackResp, fallbackErr := client.Do(fallbackReq)
					if fallbackErr != nil {
						fmt.Printf("\n✗ Models discovery returned HTTP %d, and base endpoint fallback is unreachable: %v\n", resp.StatusCode, fallbackErr)
					} else {
						defer fallbackResp.Body.Close()
						if fallbackResp.StatusCode == http.StatusUnauthorized || fallbackResp.StatusCode == http.StatusForbidden {
							fmt.Printf("\n✗ Authentication failed! Base endpoint returned HTTP %d. Please verify your API Key.\n", fallbackResp.StatusCode)
						} else {
							fmt.Printf(" Success! Connected and verified. (HTTP %d, models discovery bypassed)\n", resp.StatusCode)
						}
					}
				} else {
					fmt.Printf(" Connected, but returned unexpected status (HTTP %d)\n", resp.StatusCode)
				}
			}
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
