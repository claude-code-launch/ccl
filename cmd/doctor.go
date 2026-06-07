package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"time"

	"github.com/spf13/cobra"
	"github.com/haiboyuwen/cc/internal/claude"
	"github.com/haiboyuwen/cc/internal/config"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check system prerequisites and provider connectivity",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Diagnosing cc environment:")

		// 1. Check Node.js
		nodePath, err := exec.LookPath("node")
		if err != nil {
			fmt.Println("✗ Node.js is not installed or not in PATH")
		} else {
			fmt.Printf("✓ Node.js installed at: %s\n", nodePath)
		}

		// 2. Check Claude CLI
		claudeInstalled := claude.IsInstalled()
		if !claudeInstalled {
			fmt.Println("✗ Claude Code CLI is not installed or not in PATH.")
			// Prompt to install automatically
			err := claude.AutoInstall()
			if err != nil {
				fmt.Printf("✗ Auto-installation failed: %v. Please install manually using: npm install -g @anthropic-ai/claude-code\n", err)
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
			fmt.Println("✗ Active provider is not selected. Use 'cc add' or 'cc use'")
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
			fmt.Printf("  - Testing reachability and auth key validation...")
			client := http.Client{
				Timeout: 5 * time.Second,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			testURL := p.Endpoint
			if p.Type == "openai" {
				// We test using OpenAI models endpoint
				testURL = p.Endpoint + "/models"
				if !time.Now().IsZero() { // generic path override
					testURL = stringsReplaceV1(p.Endpoint) + "/models"
				}
			}

			req, err := http.NewRequestWithContext(ctx, "GET", testURL, nil)
			if err != nil {
				fmt.Printf("\n✗ Failed to create validation request: %v\n", err)
				return nil
			}

			// Add API Key headers
			if p.Type == "openai" {
				req.Header.Set("Authorization", "Bearer "+p.APIKey)
			} else {
				req.Header.Set("x-api-key", p.APIKey)
			}

			resp, err := client.Do(req)
			if err != nil {
				fmt.Printf("\n✗ Endpoint is unreachable: %v\n", err)
			} else {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					fmt.Printf(" Success! Connected and verified. (HTTP %d)\n", resp.StatusCode)
				} else if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
					fmt.Printf("\n✗ Authentication failed! (HTTP %d). Please verify your API Key.\n", resp.StatusCode)
				} else {
					fmt.Printf(" Connected, but returned unexpected status (HTTP %d)\n", resp.StatusCode)
				}
			}
		}

		return nil
	},
}

// Quick helper to format /v1 path mapping to OpenAI standards.
func stringsReplaceV1(endpoint string) string {
	endpoint = pFormat(endpoint)
	if !pHasSuffix(endpoint, "/v1") && !pHasSuffix(endpoint, "/v1/chat/completions") {
		return endpoint + "/v1"
	}
	return endpoint
}

func pFormat(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func pHasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
