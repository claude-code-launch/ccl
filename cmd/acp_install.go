package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/spf13/cobra"
)

const (
	acpAdapterPackage = "@agentclientprotocol/claude-agent-acp"
	// The version CI runs the real-adapter integration tests against.
	acpAdapterVersion = "0.90.0"
	acpMinNodeMajor   = 22
)

// acpAdapterDir is the npm prefix `ccl acp install` manages. It also records
// the absolute node path used for the install, because a GUI client such as
// Xcode starts ccl acp with a PATH that rarely contains Node.js.
func acpAdapterDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ccl", "acp-adapter"), nil
}

func managedACPAdapter(dir string) string {
	name := "claude-agent-acp"
	if runtime.GOOS == "windows" {
		name += ".cmd"
	}
	return filepath.Join(dir, "node_modules", ".bin", name)
}

func acpInstallHint() string {
	return fmt.Sprintf(locale.T(
		"在终端运行 ccl acp install（需要 Node.js >=%d），或手动安装：npm install -g %s",
		"run ccl acp install in a terminal (requires Node.js >=%d), or install it yourself: npm install -g %s",
	), acpMinNodeMajor, acpAdapterPackage)
}

// resolveACPAdapter prefers an explicit choice, then the managed install,
// then a global npm install on PATH.
func resolveACPAdapter(flag string) (string, error) {
	adapter := flag
	if adapter == "" {
		adapter = os.Getenv("CCL_ACP_ADAPTER")
	}
	if adapter == "" {
		if dir, err := acpAdapterDir(); err == nil {
			if managed := managedACPAdapter(dir); fileExists(managed) {
				return managed, nil
			}
		}
		adapter = "claude-agent-acp"
	}
	path, err := exec.LookPath(adapter)
	if err != nil {
		return "", fmt.Errorf(locale.T("未找到 claude-agent-acp：%s：%w", "claude-agent-acp not found: %s: %w"), acpInstallHint(), err)
	}
	return path, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

var nodeVersionOutput = func(node string) (string, error) {
	out, err := process.Command(node, "--version").Output()
	return string(out), err
}

func nodeMajor(version string) (int, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	major, _, _ := strings.Cut(version, ".")
	return strconv.Atoi(major)
}

// resolveNode returns the first Node.js >= acpMinNodeMajor among PATH and the
// node recorded by `ccl acp install`.
func resolveNode() (string, error) {
	var candidates []string
	if path, err := exec.LookPath("node"); err == nil {
		candidates = append(candidates, path)
	}
	if dir, err := acpAdapterDir(); err == nil {
		if data, err := os.ReadFile(filepath.Join(dir, "node-path")); err == nil {
			if recorded := strings.TrimSpace(string(data)); recorded != "" && fileExists(recorded) {
				candidates = append(candidates, recorded)
			}
		}
	}
	var found []string
	for _, node := range candidates {
		version, err := nodeVersionOutput(node)
		if err != nil {
			continue
		}
		if major, err := nodeMajor(version); err == nil && major >= acpMinNodeMajor {
			return node, nil
		}
		found = append(found, node+" "+strings.TrimSpace(version))
	}
	if len(found) > 0 {
		return "", fmt.Errorf(locale.T("claude-agent-acp 需要 Node.js >=%d，当前为 %s", "claude-agent-acp requires Node.js >=%d; found %s"),
			acpMinNodeMajor, strings.Join(found, ", "))
	}
	return "", fmt.Errorf(locale.T("claude-agent-acp 需要 Node.js >=%d，但未找到 node；请从 https://nodejs.org 安装", "claude-agent-acp requires Node.js >=%d, but node was not found; install it from https://nodejs.org"), acpMinNodeMajor)
}

func newACPInstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: locale.T("安装 ccl acp 使用的 claude-agent-acp 适配器", "Install the claude-agent-acp adapter ccl acp uses"),
		Long: fmt.Sprintf(locale.T(`用 npm 把 %s@%s（CI 验证的版本）安装到 ~/.ccl/acp-adapter，安装进度由 npm 显示。
ccl acp 会自动使用这份安装，并记住安装时的 node 路径，供 Xcode 等 GUI 客户端使用。
重新运行即可修复或重装。`, `Install %s@%s (the version CI verifies) into ~/.ccl/acp-adapter with npm; npm shows the progress.
ccl acp uses this install automatically and remembers the node used for it, for GUI clients such as Xcode.
Run it again to repair or reinstall.`), acpAdapterPackage, acpAdapterVersion),
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runACPInstall(cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func runACPInstall(out, errw io.Writer) error {
	node, err := resolveNode()
	if err != nil {
		return err
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return errors.New(locale.T("未找到 npm；它随 Node.js 一起安装：https://nodejs.org", "npm was not found; it ships with Node.js: https://nodejs.org"))
	}
	dir, err := acpAdapterDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	spec := acpAdapterPackage + "@" + acpAdapterVersion
	fmt.Fprintf(out, locale.T("正在安装 %s 到 %s（使用 %s）……\n", "Installing %s into %s (using %s)...\n"), spec, dir, node)
	install := process.Command(npm, "install", "--prefix", dir, "--no-audit", "--no-fund", "--ignore-scripts", spec)
	install.Stdin, install.Stdout, install.Stderr = os.Stdin, out, errw
	if err := install.Run(); err != nil {
		return fmt.Errorf(locale.T("npm install 失败：%w", "npm install failed: %w"), err)
	}
	adapter := managedACPAdapter(dir)
	if !fileExists(adapter) {
		return fmt.Errorf(locale.T("npm 已完成，但缺少 %s", "npm finished, but %s is missing"), adapter)
	}
	if err := os.WriteFile(filepath.Join(dir, "node-path"), []byte(node+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, locale.T("✓ 已安装 claude-agent-acp %s；ccl acp 会自动使用它\n", "✓ Installed claude-agent-acp %s; ccl acp uses it automatically\n"), acpAdapterVersion)
	return nil
}
