package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"charm.land/huh/v2"
)

// IsInstalled returns true if the 'claude' CLI executable is found in system PATH.
func IsInstalled() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

// helper: run command and stream output
func runCmdStream(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func fileLooksLikeInstaller(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, 1024)
	n, _ := f.Read(buf)
	content := strings.ToLower(string(buf[:n]))
	if strings.HasPrefix(content, "#!") || strings.Contains(content, "install") || strings.Contains(content, "claude") {
		return true, nil
	}
	return false, nil
}

func RunInstallerScript() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Windows: use PowerShell installer
	if runtime.GOOS == "windows" {
		fmt.Println("Installing/Updating Claude Code CLI via official PowerShell installer...")
		// prefer pwsh if available
		pwsh, _ := exec.LookPath("pwsh")
		ps, _ := exec.LookPath("powershell")
		var shell string
		if pwsh != "" {
			shell = pwsh
		} else if ps != "" {
			shell = ps
		} else {
			return errors.New("PowerShell not found (pwsh or powershell required)")
		}
		// Use -NoProfile -ExecutionPolicy Bypass -Command "<script>"
		cmdStr := "irm https://claude.ai/install.ps1 | iex"
		return runCmdStream(ctx, shell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", cmdStr)
	}

	// Non-Windows: ensure curl or wget exists
	curlPath, curlErr := exec.LookPath("curl")
	wgetPath, wgetErr := exec.LookPath("wget")
	if curlErr != nil && wgetErr != nil {
		return fmt.Errorf("either 'curl' or 'wget' is required but neither was found in PATH")
	}

	// download to temp file
	tmpDir := os.TempDir()
	tmpFile := filepath.Join(tmpDir, "claude_install.sh")
	if curlPath != "" {
		fmt.Println("Downloading installer with curl...")
		// show progress with -# (no -s)
		if err := runCmdStream(ctx, curlPath, "-#", "-fSL", "https://claude.ai/install.sh", "-o", tmpFile); err != nil {
			return fmt.Errorf("curl download failed: %w", err)
		}
	} else {
		fmt.Println("Downloading installer with wget...")
		if err := runCmdStream(ctx, wgetPath, "https://claude.ai/install.sh", "-O", tmpFile); err != nil {
			return fmt.Errorf("wget download failed: %w", err)
		}
	}

	// basic sanity check
	ok, err := fileLooksLikeInstaller(tmpFile)
	if err != nil {
		return fmt.Errorf("failed to inspect downloaded installer: %w", err)
	}
	if !ok {
		// print head for debugging
		f, _ := os.Open(tmpFile)
		defer f.Close()
		head := make([]byte, 1024)
		n, _ := f.Read(head)
		return fmt.Errorf("downloaded file does not look like an installer; first bytes: %q", string(head[:n]))
	}

	// make executable
	if err := os.Chmod(tmpFile, 0o755); err != nil {
		// not fatal; we'll still try to run with bash
		fmt.Fprintf(os.Stderr, "warning: chmod failed: %v\n", err)
	}

	// run installer with bash
	fmt.Println("Running installer script...")
	if err := runCmdStream(ctx, "bash", tmpFile); err != nil {
		return fmt.Errorf("installer execution failed: %w", err)
	}

	// cleanup (optional)
	_ = os.Remove(tmpFile)
	return nil
}

// AutoInstall attempts to install Claude CLI globally via official script.
// It uses curl/bash on macOS/Linux and powershell on Windows.
func AutoInstall() error {
	var confirm bool
	err := huh.NewConfirm().
		Title("Claude Code is not installed").
		Description("Would you like ccl to automatically install it via the official installer script?").
		Value(&confirm).
		Run()

	if err != nil {
		return err
	}

	if !confirm {
		return fmt.Errorf("installation cancelled. You can install it manually by referring to https://code.claude.com/")
	}

	if err := RunInstallerScript(); err != nil {
		return err
	}

	fmt.Println("✓ Claude Code CLI installed successfully!")
	return nil
}
