package cmd

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/spf13/cobra"
)

// cclRepoReleases is the base URL for ccl's GitHub release asset downloads. Asset
// names are produced by .github/workflows/release.yml and consumed by bin/wrapper.js.
const cclRepoReleases = "https://github.com/claude-code-launch/ccl/releases/download"

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

		cleanCurrent := canonicalReleaseTag(Version)
		cleanLatest := canonicalReleaseTag(latestVersion)

		if Version != "dev" && latestVersion != "unknown" && cleanCurrent == cleanLatest {
			fmt.Println("✨ You are already on the latest version!")
			return nil
		}

		// Prompt user for update method
		var method string
		fmt.Println(locale.T("选择更新方式:", "Choose update method:"))
		fmt.Println("1. Auto update (download latest binary)")
		fmt.Println("2. Update via npm (Global install)")
		fmt.Println("3. Update via Go (go install)")
		fmt.Println("4. View installation instructions")
		fmt.Println("5. Cancel")
		fmt.Print("Choose [1-5]: ")
		var choiceStr string
		fmt.Scanln(&choiceStr)
		choiceStr = strings.ToLower(strings.TrimSpace(choiceStr))

		switch choiceStr {
		case "1", "auto", "self":
			method = "self"
		case "2", "npm":
			method = "npm"
		case "3", "go":
			method = "go"
		case "4", "manual":
			method = "manual"
		case "5", "cancel", "":
			method = "cancel"
		default:
			method = "cancel"
		}

		switch method {
		case "self":
			fmt.Println("Downloading latest ccl binary...")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := selfUpdate(ctx, latestVersion); err != nil {
				return fmt.Errorf("self-update failed (try the npm or go method instead): %w", err)
			}
			fmt.Println("\n🎉 Successfully updated ccl to the latest version!")
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

// releaseAssetName returns the GitHub release asset name for the current
// GOOS/GOARCH, matching the names produced by .github/workflows/release.yml and
// consumed by bin/wrapper.js.
func releaseAssetName() (string, error) {
	platform, arch := runtime.GOOS, runtime.GOARCH
	switch platform {
	case "darwin", "linux":
		if arch != "amd64" && arch != "arm64" {
			return "", fmt.Errorf("unsupported architecture for self-update: %s/%s", runtime.GOOS, runtime.GOARCH)
		}
	case "windows":
		platform = "win32"
		if arch == "amd64" {
			arch = "x64"
		}
	default:
		return "", fmt.Errorf("unsupported platform for self-update: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("ccl-%s-%s%s", platform, arch, ext), nil
}

// releaseDownloadURL resolves a release asset download URL. When the version is
// known it pins the exact release; otherwise it follows GitHub's /latest redirect.
func releaseDownloadURL(version, asset string) string {
	if version != "" && version != "unknown" {
		return fmt.Sprintf("%s/%s/%s", cclRepoReleases, canonicalReleaseTag(version), asset)
	}
	return fmt.Sprintf("https://github.com/claude-code-launch/ccl/releases/latest/download/%s", asset)
}

var legacyNpmRelease = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)-([0-9]+)$`)

func canonicalReleaseTag(version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	version = legacyNpmRelease.ReplaceAllString(version, "$1.$2")
	return "v" + version
}

// downloadReleaseBinary streams url into dest, rendering a progress bar on stderr
// when stderr is a terminal (and the server reports a content length).
func downloadReleaseBinary(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}

	total := resp.ContentLength
	isTerm := term.IsTerminal(os.Stderr.Fd())

	buf := make([]byte, 32*1024)
	var done int64
	var lastDraw time.Time
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(dest)
				return werr
			}
			done += int64(n)
			if isTerm && time.Since(lastDraw) >= 60*time.Millisecond {
				renderDownloadProgress(done, total)
				lastDraw = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(dest)
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(dest)
		return err
	}
	if err := validateReleaseBinary(dest); err != nil {
		_ = os.Remove(dest)
		return err
	}

	if isTerm {
		renderDownloadProgress(done, total)
		fmt.Fprintln(os.Stderr)
	} else {
		fmt.Fprintf(os.Stderr, "Downloaded %s\n", humanSize(done))
	}
	return nil
}

// Inspect without executing downloaded code. Accept both package builds and
// release builds made from main.go (which list this module as a dependency).
func validateReleaseBinary(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("download is not a Go executable: %w", err)
	}
	const module = "github.com/claude-code-launch/ccl"
	ours := info.Path == module || info.Main.Path == module
	if info.Path == "command-line-arguments" {
		for _, dep := range info.Deps {
			if dep.Path == module {
				ours = true
			}
		}
	}
	if !ours {
		return fmt.Errorf("download is not a ccl executable")
	}
	settings := make(map[string]string)
	for _, entry := range info.Settings {
		settings[entry.Key] = entry.Value
	}
	if settings["GOOS"] != runtime.GOOS || settings["GOARCH"] != runtime.GOARCH {
		return fmt.Errorf("download targets %s/%s, expected %s/%s", settings["GOOS"], settings["GOARCH"], runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

// renderDownloadProgress draws a single-line progress bar. A missing content
// length falls back to an unbounded byte counter.
func renderDownloadProgress(done, total int64) {
	const width = 30
	if total <= 0 {
		fmt.Fprintf(os.Stderr, "\rDownloading... %s", humanSize(done))
		return
	}
	frac := float64(done) / float64(total)
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * width)
	bar := strings.Repeat("=", filled) + strings.Repeat("-", width-filled)
	fmt.Fprintf(os.Stderr, "\r[%s] %5.1f%% (%s / %s)", bar, frac*100, humanSize(done), humanSize(total))
}

// humanSize renders a byte count with a binary-prefix suffix.
func humanSize(n int64) string {
	const unit = 1024.0
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	i := -1
	for v >= unit && i < len(suffixes)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, suffixes[i])
}

// selfUpdate downloads the current platform's release binary and atomically
// replaces the running ccl binary at os.Executable(). It is a no-op on Windows,
// where the running executable cannot be replaced in place.
func selfUpdate(ctx context.Context, version string) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("self-update is not supported on Windows; use the npm or go method instead")
	}

	asset, err := releaseAssetName()
	if err != nil {
		return err
	}

	target, err := os.Executable()
	if err != nil {
		return err
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}

	dir := filepath.Dir(target)
	staging, err := os.CreateTemp(dir, "."+filepath.Base(target)+".download-*")
	if err != nil {
		return err
	}
	tmp := staging.Name()
	if err := staging.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	defer os.Remove(tmp)

	if err := downloadReleaseBinary(ctx, releaseDownloadURL(version, asset), tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}

	backup, err := installReleaseBinary(target, tmp)
	if err != nil {
		return err
	}
	fmt.Printf("Previous version retained at: %s\n", backup)
	return nil
}

func installReleaseBinary(target, downloaded string) (backupPath string, err error) {
	if err := validateReleaseBinary(downloaded); err != nil {
		return "", err
	}
	current, err := os.Open(target)
	if err != nil {
		return "", err
	}
	defer current.Close()
	info, err := current.Stat()
	if err != nil {
		return "", err
	}
	backup, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".backup-*")
	if err != nil {
		return "", err
	}
	backupPath = backup.Name()
	defer func() {
		_ = backup.Close()
		if err != nil {
			_ = os.Remove(backupPath)
		}
	}()
	if err = backup.Chmod(info.Mode().Perm()); err != nil {
		return backupPath, err
	}
	if _, err = io.Copy(backup, current); err != nil {
		return backupPath, err
	}
	if err = backup.Sync(); err != nil {
		return backupPath, err
	}
	if err = backup.Close(); err != nil {
		return backupPath, err
	}
	if err = os.Rename(downloaded, target); err != nil {
		return backupPath, fmt.Errorf("install new binary: %w", err)
	}
	return backupPath, nil
}

func init() {
	rootCmd.AddCommand(updateCmd)
}
