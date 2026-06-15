package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update ccl to the latest version",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("Current version: %s\n", Version)
		fmt.Println("Checking for updates...")

		latestVersion, err := fetchLatestNpmVersion()
		if err != nil {
			fmt.Printf("⚠️  Could not check for latest version: %v\n", err)
			latestVersion = "unknown"
		} else {
			fmt.Printf("Latest version: %s\n\n", latestVersion)
		}

		cleanCurrent := strings.TrimPrefix(Version, "v")
		cleanLatest := strings.TrimPrefix(latestVersion, "v")

		if Version != "dev" && latestVersion != "unknown" && cleanCurrent == cleanLatest {
			fmt.Println("✨ You are already on the latest version!")
			return nil
		}

		// Prompt user for update method
		var method string
		var options []huh.Option[string]

		// Check if npm is available
		if _, err := exec.LookPath("npm"); err == nil {
			options = append(options, huh.NewOption("Update via npm (Global install)", "npm"))
		}

		// Check if go is available
		if _, err := exec.LookPath("go"); err == nil {
			options = append(options, huh.NewOption("Update via Go (go install)", "go"))
		}

		options = append(options, huh.NewOption("View installation instructions", "manual"))
		options = append(options, huh.NewOption("Cancel", "cancel"))

		err = huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Choose Update Method").
					Options(options...).
					Value(&method),
			),
		).Run()
		if err != nil {
			return err
		}

		switch method {
		case "cancel", "":
			fmt.Println("Update cancelled.")
			return nil
		case "npm":
			fmt.Println("Updating via npm... Running 'npm install -g @claudecodelaunch/ccl@latest'")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "npm", "install", "-g", "@claudecodelaunch/ccl@latest")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Stdin = os.Stdin
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("npm update failed: %w", err)
			}
			fmt.Println("\n🎉 Successfully updated ccl to the latest version via npm!")
		case "go":
			fmt.Println("Updating via Go... Running 'go install github.com/claude-code-launch/ccl@latest'")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "go", "install", "github.com/claude-code-launch/ccl@latest")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Stdin = os.Stdin
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("go update failed: %w", err)
			}
			fmt.Println("\n🎉 Successfully updated ccl to the latest version via Go!")
		case "manual":
			fmt.Println("\nPlease visit the following link to check alternative installation methods:")
			fmt.Println("🔗 https://github.com/claude-code-launch/ccl#安装与编译")
		}

		return nil
	},
}

func fetchLatestNpmVersion() (string, error) {
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get("https://registry.npmjs.org/@claudecodelaunch/ccl/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	var result struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if result.Version == "" {
		return "", fmt.Errorf("empty version in response")
	}

	return "v" + result.Version, nil
}

func init() {
	rootCmd.AddCommand(updateCmd)
}
