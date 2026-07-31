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

	"github.com/charmbracelet/x/term"
	"github.com/claude-code-launch/ccl/internal/locale"
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

var errInstallNeedsTerminal = errors.New(
	"claude is not installed and stdin is not a terminal, so the installer was not run. " +
		"Install it manually (https://code.claude.com/) or run ccl from a terminal to be prompted")

// checkInstallConsent interprets the answer to the "(Y/n)" install prompt and
// returns nil only when the installer may run. The prompt defaults to yes,
// which is only a safe default when a person is there to see it: with no
// terminal an empty answer is EOF, not consent, and treating it as yes would
// download and execute an installer unattended.
func checkInstallConsent(answer string, stdinIsTerminal bool) error {
	if !stdinIsTerminal {
		return errInstallNeedsTerminal
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "n", "no":
		return errors.New("installation cancelled. You can install it manually by referring to https://code.claude.com/")
	default:
		return nil
	}
}

// AutoInstall attempts to install Claude CLI globally via official script.
// It uses curl/bash on macOS/Linux and powershell on Windows.
func AutoInstall() error {
	stdinIsTerminal := term.IsTerminal(os.Stdin.Fd())

	var answer string
	if stdinIsTerminal {
		fmt.Println("Claude Code is not installed.")
		fmt.Print(locale.T("是否自动安装？(Y/n): ", "Automatically install? (Y/n): "))
		_, _ = fmt.Scanln(&answer)
	}

	if err := checkInstallConsent(answer, stdinIsTerminal); err != nil {
		return err
	}

	if err := RunInstallerScript(); err != nil {
		return err
	}

	fmt.Println("✓ Claude Code CLI installed successfully!")
	return nil
}
