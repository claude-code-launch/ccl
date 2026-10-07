package cmd

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
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

// releaseArchiveSuffix is appended to a binary's asset name on GitHub releases:
// binaries ship gzip-compressed (about a third of their size). The npm package
// still carries them uncompressed.
const releaseArchiveSuffix = ".gz"

// maxReleaseBinaryBytes caps the decompressed download, so a corrupt or
// hostile archive cannot fill the disk. Release binaries are about 30 MiB.
var maxReleaseBinaryBytes int64 = 256 << 20

var updateMethod string

var updateCmd = func() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update ccl to the latest version",
		Long: `Update ccl to the latest release.

ccl detects how it was installed (npm, go install, or a downloaded binary)
and offers that method first. --method self|npm|go skips the prompt, which
is required when stdin is not a terminal. The self method downloads the
release archive for this platform, checks it against the release's
SHA256SUMS, and keeps the previous binary as a backup.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(updateMethod)
		},
	}
	cmd.Flags().StringVar(&updateMethod, "method", "", "Update method: self, npm, or go (skips the prompt)")
	return cmd
}()

func runUpdate(method string) error {
	fmt.Printf("Current version: %s\n", Version)
	fmt.Println("Checking for updates...")

	latestVersion, err := fetchLatestNpmVersion()
	if err != nil {
		fmt.Printf("⚠️  Could not check for latest version: %v\n", err)
		latestVersion = "unknown"
	} else {
		fmt.Printf("Latest version: %s\n\n", latestVersion)
	}
	if Version != "dev" && latestVersion != "unknown" && canonicalReleaseTag(Version) == canonicalReleaseTag(latestVersion) {
		fmt.Println("✨ You are already on the latest version!")
		return nil
	}

	detected := detectInstallMethod()
	method = strings.ToLower(strings.TrimSpace(method))
	switch method {
	case "":
		if !term.IsTerminal(os.Stdin.Fd()) {
			return fmt.Errorf("no terminal to ask on; run ccl update --method %s (detected) or self|npm|go", detected)
		}
		method = promptUpdateMethod(detected)
	case "self", "npm", "go":
	default:
		return fmt.Errorf("unknown update method %q; use self, npm, or go", method)
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
	case "npm":
		fmt.Println("Updating via npm... Running 'npm install -g @claudecodelaunch/ccl@latest'")
		if err := runUpdateCommand("npm", "install", "-g", "@claudecodelaunch/ccl@latest"); err != nil {
			return fmt.Errorf("npm update failed: %w", err)
		}
		fmt.Println("\n🎉 Successfully updated ccl to the latest version via npm!")
	case "go":
		fmt.Println("Updating via Go... Running 'go install github.com/claude-code-launch/ccl@latest'")
		if err := runUpdateCommand("go", "install", "github.com/claude-code-launch/ccl@latest"); err != nil {
			return fmt.Errorf("go update failed: %w", err)
		}
		fmt.Println("\n🎉 Successfully updated ccl to the latest version via Go!")
	case "manual":
		fmt.Println("\nPlease visit the following link to check alternative installation methods:")
		fmt.Println("🔗 https://github.com/claude-code-launch/ccl#安装与编译")
	default:
		fmt.Println("Update cancelled.")
	}
	return nil
}

// promptUpdateMethod asks which method to use; Enter picks the detected one.
func promptUpdateMethod(detected string) string {
	labels := map[string]string{"self": "1", "npm": "2", "go": "3"}
	fmt.Println(locale.T("选择更新方式:", "Choose update method:"))
	for _, row := range []struct{ key, text string }{
		{"self", "1. Auto update (download latest binary)"},
		{"npm", "2. Update via npm (Global install)"},
		{"go", "3. Update via Go (go install)"},
	} {
		if row.key == detected {
			fmt.Println(row.text + "  ← detected")
		} else {
			fmt.Println(row.text)
		}
	}
	fmt.Println("4. View installation instructions")
	fmt.Println("5. Cancel")
	fmt.Printf("Choose [1-5] (Enter = %s): ", labels[detected])
	var choice string
	_, _ = fmt.Scanln(&choice)
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "":
		return detected
	case "1", "auto", "self":
		return "self"
	case "2", "npm":
		return "npm"
	case "3", "go":
		return "go"
	case "4", "manual":
		return "manual"
	default:
		return "cancel"
	}
}

func runUpdateCommand(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout, command.Stderr, command.Stdin = os.Stdout, os.Stderr, os.Stdin
	return command.Run()
}

// detectInstallMethod infers how this ccl was installed from where its binary
// lives: inside an npm package, in a Go bin directory, or anywhere else
// (a downloaded release binary).
func detectInstallMethod() string {
	path, err := os.Executable()
	if err != nil {
		return "self"
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return installMethodForPath(path, goBinDirs())
}

func installMethodForPath(path string, goBins []string) string {
	slashed := filepath.ToSlash(path)
	if strings.Contains(slashed, "/node_modules/@claudecodelaunch/ccl/") {
		return "npm"
	}
	dir := filepath.Dir(path)
	for _, bin := range goBins {
		if bin != "" && filepath.Clean(bin) == dir {
			return "go"
		}
	}
	return "self"
}

func goBinDirs() []string {
	var dirs []string
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			gopath = filepath.Join(home, "go")
		}
	}
	for _, entry := range filepath.SplitList(gopath) {
		dirs = append(dirs, filepath.Join(entry, "bin"))
	}
	return dirs
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

// releaseChecksum fetches the release's SHA256SUMS and returns the digest
// listed for asset. A release without one for the asset is refused rather than
// installed unchecked.
func releaseChecksum(ctx context.Context, version, asset string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseDownloadURL(version, "SHA256SUMS"), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download SHA256SUMS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download SHA256SUMS: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	return checksumFor(string(data), asset)
}

// checksumFor finds asset in sha256sum output ("<hex>  <name>" per line).
func checksumFor(sums, asset string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset && len(fields[0]) == 64 {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS lists no checksum for %s", asset)
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

// downloadReleaseBinary streams the gzip-compressed release asset at url into
// dest, decompressing on the way. A progress bar (of compressed bytes) is drawn
// on stderr when stderr is a terminal and the server reports a content length.
func downloadReleaseBinary(ctx context.Context, url, dest, wantSHA256 string) (err error) {
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
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(dest)
		}
	}()

	progress := &downloadProgress{
		reader: resp.Body,
		total:  resp.ContentLength,
		isTerm: term.IsTerminal(os.Stderr.Fd()),
	}
	digest := sha256.New()
	archive, err := gzip.NewReader(io.TeeReader(progress, digest))
	if err != nil {
		return fmt.Errorf("download is not a gzip release archive: %w", err)
	}
	// Read one byte past the limit so an oversized archive is detected rather
	// than silently truncated.
	written, err := io.Copy(f, io.LimitReader(archive, maxReleaseBinaryBytes+1))
	if err != nil {
		return fmt.Errorf("decompress release archive: %w", err)
	}
	if written > maxReleaseBinaryBytes {
		return fmt.Errorf("release archive expands beyond %s", humanSize(maxReleaseBinaryBytes))
	}
	if err = archive.Close(); err != nil {
		return fmt.Errorf("decompress release archive: %w", err)
	}
	// gzip stops at the end of its stream; drain any trailing bytes so the
	// digest covers the whole downloaded file.
	if _, err = io.Copy(digest, progress); err != nil {
		return fmt.Errorf("read release archive: %w", err)
	}
	if wantSHA256 != "" {
		if got := hex.EncodeToString(digest.Sum(nil)); !strings.EqualFold(got, wantSHA256) {
			return fmt.Errorf("release archive checksum mismatch: got %s, want %s", got, wantSHA256)
		}
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = validateReleaseBinary(dest); err != nil {
		return err
	}

	if progress.isTerm {
		renderDownloadProgress(progress.done, progress.total)
		fmt.Fprintln(os.Stderr)
	} else {
		fmt.Fprintf(os.Stderr, "Downloaded %s (%s unpacked)\n", humanSize(progress.done), humanSize(written))
	}
	return nil
}

// downloadProgress counts the compressed bytes read from the response and
// redraws the progress bar at most every 60ms.
type downloadProgress struct {
	reader   io.Reader
	total    int64
	done     int64
	isTerm   bool
	lastDraw time.Time
}

func (p *downloadProgress) Read(buf []byte) (int, error) {
	n, err := p.reader.Read(buf)
	p.done += int64(n)
	if p.isTerm && n > 0 && time.Since(p.lastDraw) >= 60*time.Millisecond {
		renderDownloadProgress(p.done, p.total)
		p.lastDraw = time.Now()
	}
	return n, err
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

	archive := asset + releaseArchiveSuffix
	wantSHA256, err := releaseChecksum(ctx, version, archive)
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := downloadReleaseBinary(ctx, releaseDownloadURL(version, archive), tmp, wantSHA256); err != nil {
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
