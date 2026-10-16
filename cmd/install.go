package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		fmt.Println(locale.T("正在通过官方 PowerShell 安装脚本安装 Claude Code……", "Installing Claude Code with the official PowerShell installer..."))
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
		fmt.Println(locale.T("正在用 curl 下载官方安装脚本……", "Downloading the official installer with curl..."))
		// show progress with -# (no -s)
		if err := runCmdStream(ctx, curlPath, "-#", "-fSL", "https://claude.ai/install.sh", "-o", tmpFile); err != nil {
			return fmt.Errorf("curl download failed: %w", err)
		}
	} else {
		fmt.Println(locale.T("正在用 wget 下载官方安装脚本……", "Downloading the official installer with wget..."))
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
	fmt.Println(locale.T("正在运行安装脚本……", "Running the installer..."))
	if err := runCmdStream(ctx, "bash", tmpFile); err != nil {
		return fmt.Errorf("installer execution failed: %w", err)
	}

	// cleanup (optional)
	_ = os.Remove(tmpFile)
	return nil
}

var errInstallNeedsTerminal = errors.New(
	"claude is not installed and stdin is not a terminal, so the installer was not run")

// manualInstallHint lists the commands that install Claude Code by hand.
func manualInstallHint() string {
	official := "curl -fsSL https://claude.ai/install.sh | bash"
	if runtime.GOOS == "windows" {
		official = "irm https://claude.ai/install.ps1 | iex"
	}
	return fmt.Sprintf(locale.T(
		"手动安装 Claude Code 后重新运行 ccl：\n  %s\n或使用 npm：\n  npm install -g @anthropic-ai/claude-code\n详见 https://code.claude.com/",
		"Install Claude Code, then run ccl again:\n  %s\nor with npm:\n  npm install -g @anthropic-ai/claude-code\nSee https://code.claude.com/",
	), official)
}

// checkInstallConsent interprets the answer to the "(Y/n)" install prompt and
// returns nil only when the installer may run. The prompt defaults to yes,
// which is only a safe default when a person is there to see it: with no
// terminal an empty answer is EOF, not consent, and treating it as yes would
// download and execute an installer unattended.
func checkInstallConsent(answer string, stdinIsTerminal bool) error {
	if !stdinIsTerminal {
		return fmt.Errorf("%w.\n%s", errInstallNeedsTerminal, manualInstallHint())
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "n", "no":
		return fmt.Errorf("%s\n%s", locale.T("已取消安装。", "Installation cancelled."), manualInstallHint())
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
		fmt.Println(locale.T(
			"未找到 Claude Code（PATH 中没有 claude）。可以运行 Anthropic 官方安装脚本自动安装，进度会显示在下方。",
			"Claude Code was not found (no claude in PATH). ccl can run Anthropic's official installer; its progress is shown below.",
		))
		fmt.Print(locale.T("是否自动安装？(Y/n): ", "Automatically install? (Y/n): "))
		_, _ = fmt.Scanln(&answer)
	}

	if err := checkInstallConsent(answer, stdinIsTerminal); err != nil {
		return err
	}

	if err := RunInstallerScript(); err != nil {
		return fmt.Errorf("%w\n%s", err, manualInstallHint())
	}
	if err := ensureInstalledClaudeOnPath(os.Stdout); err != nil {
		return err
	}
	fmt.Println(locale.T("✓ Claude Code 安装完成", "✓ Claude Code installed"))
	return nil
}

// useLocalClaudeInstall finds the official installer's claude in ~/.local/bin
// when the shell's PATH lacks that directory (Cloud Shell, a fresh login, a
// shell profile that was not reloaded). Without this, every later ccl run
// would offer to install Claude Code again. Only this process's PATH changes;
// it returns the directory it added, or "".
func useLocalClaudeInstall() string {
	if IsInstalled() {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".local", "bin")
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	if !fileExists(filepath.Join(dir, name)) {
		return ""
	}
	_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// localClaudeDir is set when Execute put ~/.local/bin on PATH for claude.
var localClaudeDir string

func printLocalClaudeHint(out io.Writer, dir string) {
	fmt.Fprintf(out, locale.T(
		"ccl：Claude Code 位于 %s，但该目录不在 PATH 中；ccl 已直接使用它。要在 shell 里直接运行 claude，请把它加入 PATH，例如（zsh 用 ~/.zshrc）：\n  echo 'export PATH=\"$HOME/.local/bin:$PATH\"' >> ~/.bashrc\n",
		"ccl: Claude Code is in %s, which is not in PATH; ccl uses it directly. To run claude from your shell, add it to PATH, e.g. (~/.zshrc for zsh):\n  echo 'export PATH=\"$HOME/.local/bin:$PATH\"' >> ~/.bashrc\n",
	), dir)
}

// ensureInstalledClaudeOnPath lets this launch continue when the installer put
// claude outside PATH, and tells the user how to make that permanent.
func ensureInstalledClaudeOnPath(out io.Writer) error {
	if dir := useLocalClaudeInstall(); dir != "" {
		printLocalClaudeHint(out, dir)
		return nil
	}
	if IsInstalled() {
		return nil
	}
	return errors.New(locale.T(
		"安装脚本已结束，但仍找不到 claude；请打开新的终端后重新运行 ccl",
		"the installer finished but claude is still not found; open a new terminal and run ccl again",
	))
}
