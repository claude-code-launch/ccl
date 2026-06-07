package claude

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/charmbracelet/huh"
)

// IsInstalled returns true if the 'claude' CLI executable is found in system PATH.
func IsInstalled() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

// AutoInstall attempts to install Claude CLI globally via official script.
// It uses curl/bash on macOS/Linux and powershell on Windows.
func AutoInstall() error {
	var confirm bool
	err := huh.NewConfirm().
		Title("Claude Code is not installed").
		Description("Would you like cc to automatically install it via the official installer script?").
		Value(&confirm).
		Run()

	if err != nil {
		return err
	}

	if !confirm {
		return fmt.Errorf("installation cancelled. You can install it manually by referring to https://code.claude.com/")
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		fmt.Println("Installing Claude Code CLI via official PowerShell installer...")
		// Official Windows installer command: irm https://claude.ai/install.ps1 | iex
		cmd = exec.Command("powershell", "-ExecutionPolicy", "Bypass", "-Command", "irm https://claude.ai/install.ps1 | iex")
	} else {
		// Ensure curl or wget is installed
		_, curlErr := exec.LookPath("curl")
		_, wgetErr := exec.LookPath("wget")
		if curlErr != nil && wgetErr != nil {
			return fmt.Errorf("either 'curl' or 'wget' is required but neither was found in PATH. Please install one of them first")
		}

		fmt.Println("Installing Claude Code CLI via official Shell installer (curl -fsSL https://claude.ai/install.sh | bash)...")
		cmd = exec.Command("bash", "-c", "curl -fsSL https://claude.ai/install.sh | bash")
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("official installation script failed: %w", err)
	}

	fmt.Println("✓ Claude Code CLI installed successfully!")
	return nil
}
