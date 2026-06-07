package claude

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/charmbracelet/huh"
)

// IsInstalled returns true if the 'claude' CLI executable is found in system PATH.
func IsInstalled() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

// AutoInstall attempts to install Claude CLI globally via npm.
// It returns an error if npm is missing or the installation command fails.
func AutoInstall() error {
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		return fmt.Errorf("npm is not installed or not found in system PATH. Please install Node.js/npm first: https://nodejs.org/")
	}

	var confirm bool
	err = huh.NewConfirm().
		Title("Claude Code is not installed").
		Description("Would you like cc to automatically install it globally via npm?").
		Value(&confirm).
		Run()

	if err != nil {
		return err
	}

	if !confirm {
		return fmt.Errorf("installation cancelled. You can install it manually using: npm install -g @anthropic-ai/claude-code")
	}

	fmt.Println("Installing Claude Code CLI globally (@anthropic-ai/claude-code)...")
	cmd := exec.Command(npmPath, "install", "-g", "@anthropic-ai/claude-code")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm installation failed: %w", err)
	}

	fmt.Println("✓ Claude Code CLI installed successfully!")
	return nil
}
