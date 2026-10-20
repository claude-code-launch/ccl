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

// latestReleaseAPI is the GitHub endpoint naming the newest published release.
// It is the version source of truth: the GitHub release and its assets exist
// before the npm job that publishes the matching package has run.
var latestReleaseAPI = "https://api.github.com/repos/claude-code-launch/ccl/releases/latest"

var updateHTTPClient = &http.Client{Timeout: 5 * time.Second}

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
	method = strings.ToLower(strings.TrimSpace(method))
	if method != "" && method != "self" && method != "npm" && method != "go" {
		return fmt.Errorf("unknown update method %q; use self, npm, or go", method)
	}
	fmt.Printf("Current version: %s\n", Version)
	fmt.Println("Checking for updates...")

	latestVersion, npmReady := latestVersions()
	if latestVersion == "unknown" {
		fmt.Println("⚠️  Could not check for latest version: neither the GitHub release nor the npm registry answered")
	} else {
		fmt.Printf("Latest version: %s\n\n", latestVersion)
	}
	if Version != "dev" && latestVersion != "unknown" && canonicalReleaseTag(Version) == canonicalReleaseTag(latestVersion) && method != "npm" {
		fmt.Println("✨ You are already on the latest version!")
		return nil
	}
	// The npm package is published by a later job than the GitHub release, so a
	// brand-new version can exist for hours without an npm package. Offering npm
	// then installs an older version (or fails), and a launcher replaced by npm
	// has to be reinstalled by hand.

	detected := detectInstallMethod()
	suggested := detected
	if detected == "npm" && !npmReady {
		// npm can only install its own latest, which is still the old release.
		// Replacing the binary in place leaves the npm package installed; on
		// Windows a running executable cannot be replaced, so there is nothing
		// to offer yet.
		if runtime.GOOS == "windows" && method == "" {
			return fmt.Errorf("npm has not published %s yet (the registry lags the GitHub release) and Windows cannot replace a running binary; retry later or run ccl update --method go", latestVersion)
		}
		suggested = "self"
	}
	switch method {
	case "":
		if !term.IsTerminal(os.Stdin.Fd()) {
			available := "self|go"
			if npmReady {
				available = "self|npm|go"
			}
			return fmt.Errorf("no terminal to ask on; run ccl update --method %s (suggested) or %s", suggested, available)
		}
		if detected == "npm" && !npmReady {
			fmt.Println(locale.T(
				"npm 上还没有这个版本；本次只替换二进制文件，不重装 npm 包。",
				"npm does not carry this version yet; replacing the binary in place instead of reinstalling the npm package."))
		}
		method = promptUpdateMethod(suggested, npmReady)
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
			return fmt.Errorf("self-update failed (try the go method instead): %w", err)
		}
		fmt.Println("\n🎉 Successfully updated ccl to the latest version!")
	case "npm":
		if !npmReady {
			return fmt.Errorf("npm does not have %s yet (publishing lags the GitHub release); use --method self or go", latestVersion)
		}
		return updateViaNpm(latestVersion)
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

// updateChoice is one row of the update prompt; its number is its position.
type updateChoice struct{ method, text string }

// updateChoices lists the methods this run can actually use: npm is left out
// until the registry carries the version being installed.
func updateChoices(npmReady bool) []updateChoice {
	choices := []updateChoice{
		{"self", "Auto update (download latest binary)"},
	}
	if npmReady {
		choices = append(choices, updateChoice{"npm", "Update via npm (Global install)"})
	}
	return append(choices, updateChoice{"go", "Update via Go (go install)"})
}

// promptUpdateMethod asks which method to use; Enter picks the detected one.
func promptUpdateMethod(detected string, npmReady bool) string {
	choices := updateChoices(npmReady)
	labels := make(map[string]string, len(choices))
	fmt.Println(locale.T("选择更新方式:", "Choose update method:"))
	defaultChoice := choices[0].method
	for i, row := range choices {
		number := fmt.Sprintf("%d", i+1)
		labels[row.method] = number
		text := number + ". " + row.text
		if row.method == detected {
			text += "  ← detected"
			defaultChoice = detected
		}
		fmt.Println(text)
	}
	manual := len(choices) + 1
	cancel := len(choices) + 2
	fmt.Printf("%d. View installation instructions\n%d. Cancel\n", manual, cancel)
	fmt.Printf("Choose [1-%d] (Enter = %s): ", cancel, labels[defaultChoice])
	var choice string
	_, _ = fmt.Scanln(&choice)
	trimmed := strings.ToLower(strings.TrimSpace(choice))
	if trimmed == "" {
		return defaultChoice
	}
	for i, row := range choices {
		if trimmed == fmt.Sprintf("%d", i+1) || trimmed == row.method ||
			(row.method == "self" && trimmed == "auto") {
			return row.method
		}
	}
	switch trimmed {
	case fmt.Sprintf("%d", manual), "manual":
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
	for _, platform := range []string{"darwin-x64", "darwin-arm64", "linux-x64", "linux-arm64", "win32-x64", "win32-arm64"} {
		if strings.Contains(slashed, "/node_modules/@claudecodelaunch/ccl-"+platform+"/") {
			return "npm"
		}
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

// latestVersions resolves the version ccl update installs and whether npm
// carries it. The GitHub release is authoritative and appears first; npm is
// both the fallback when the GitHub API is unreachable (it is blocked on some
// networks) and the gate on the npm method.
func latestVersions() (latest string, npmReady bool) {
	release, releaseErr := fetchLatestReleaseVersion()
	npm, npmErr := fetchLatestNpmVersion()
	switch {
	case releaseErr == nil:
		latest = release
	case npmErr == nil:
		latest = npm
	default:
		return "unknown", false
	}
	return latest, npmErr == nil && npm != "unknown" && canonicalReleaseTag(npm) == canonicalReleaseTag(latest)
}

// fetchLatestReleaseVersion names the newest published GitHub release.
func fetchLatestReleaseVersion() (string, error) {
	req, err := http.NewRequest(http.MethodGet, latestReleaseAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	var result struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.TagName == "" {
		return "", fmt.Errorf("empty tag in response")
	}
	if result.Draft || result.Prerelease {
		return "", fmt.Errorf("latest release %s is not a stable release", result.TagName)
	}
	return canonicalReleaseTag(result.TagName), nil
}

// fetchLatestNpmVersion names the newest version of the npm launcher package.
func fetchLatestNpmVersion() (string, error) {
	resp, err := updateHTTPClient.Get("https://registry.npmjs.org/@claudecodelaunch/ccl/latest")
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.Version == "" {
		return "", fmt.Errorf("empty version in response")
	}
	return canonicalReleaseTag(result.Version), nil
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

func canonicalReleaseTag(version string) string {
	return "v" + strings.TrimPrefix(strings.TrimSpace(version), "v")
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
// replaces the running ccl binary at os.Executable().
func selfUpdate(ctx context.Context, version string) error {
	target, err := os.Executable()
	if err != nil {
		return err
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	return selfUpdateTo(ctx, version, target)
}

// selfUpdateTo also supports npm recovery: its target is resolved before npm
// starts, rather than looking up the running executable after npm removed it.
func selfUpdateTo(ctx context.Context, version, target string) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("self-update is not supported on Windows; use the npm or go method instead")
	}
	asset, err := releaseAssetName()
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
	if backup != "" {
		fmt.Printf("Previous version retained at: %s\n", backup)
	}
	return nil
}

func installReleaseBinary(target, downloaded string) (backupPath string, err error) {
	if err := validateReleaseBinary(downloaded); err != nil {
		return "", err
	}
	current, err := os.Open(target)
	if os.IsNotExist(err) {
		// A failed npm install may already have removed the old binary.
		if err := os.Rename(downloaded, target); err != nil {
			return "", fmt.Errorf("install missing binary: %w", err)
		}
		return "", nil
	}
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
