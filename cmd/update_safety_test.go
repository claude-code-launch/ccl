package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestUpdateRejectsInvalidDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>maintenance</html>")) }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "download")
	if err := downloadReleaseBinary(context.Background(), server.URL, path); err == nil {
		t.Fatal("HTML accepted as executable")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid download not removed")
	}
}

func TestUpdatePreservesPreviousExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("in-place update is disabled on Windows")
	}
	dir := t.TempDir()
	downloaded := filepath.Join(dir, "download")
	build := exec.Command("go", "build", "-o", downloaded, "../main.go")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build release fixture: %v %s", err, output)
	}
	if err := validateReleaseBinary(downloaded); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "ccl")
	if err := os.WriteFile(target, []byte("old executable"), 0755); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(dir, "invalid")
	if err := os.WriteFile(invalid, []byte("html"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := installReleaseBinary(target, invalid); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	if data, _ := os.ReadFile(target); string(data) != "old executable" {
		t.Fatal("invalid update modified current executable")
	}
	backup, err := installReleaseBinary(target, downloaded)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(backup); string(data) != "old executable" {
		t.Fatal("old executable was not retained")
	}
	if err := validateReleaseBinary(target); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyNpmTagMapping(t *testing.T) {
	for input, want := range map[string]string{"1.2.3-4": "v1.2.3.4", "v1.2.3.4": "v1.2.3.4", "v1.2.3": "v1.2.3", "1.2.3-beta.1": "v1.2.3-beta.1"} {
		if got := canonicalReleaseTag(input); got != want {
			t.Fatalf("%s => %s want %s", input, got, want)
		}
	}
	if got := releaseDownloadURL("v1.2.3-4", "asset"); got != cclRepoReleases+"/v1.2.3.4/asset" {
		t.Fatal(got)
	}
}

func TestPreviewReturnsSetupError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["broken"] = provider.Provider{Name: "broken", Type: "openai", Endpoint: "://invalid", Model: ""}
	cfg.ActiveProvider = "broken"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := runPreview(providerTarget{}); err == nil {
		t.Fatal("preview reported success after setup failure")
	}
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func serveBytes(t *testing.T, body []byte) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(server.Close)
	return server.URL
}

// TestUpdateDownloadsAndUnpacksTheGzipArchive pins the release format: GitHub
// releases ship <asset>.gz, and self-update unpacks it into a runnable ccl.
func TestUpdateDownloadsAndUnpacksTheGzipArchive(t *testing.T) {
	dir := t.TempDir()
	built := filepath.Join(dir, "built")
	if output, err := exec.Command("go", "build", "-o", built, "../main.go").CombinedOutput(); err != nil {
		t.Fatalf("build release fixture: %v %s", err, output)
	}
	raw, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "download")
	if err := downloadReleaseBinary(context.Background(), serveBytes(t, gzipBytes(t, raw)), dest); err != nil {
		t.Fatalf("downloadReleaseBinary() = %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("unpacked binary differs from the original (%d vs %d bytes, err %v)", len(got), len(raw), err)
	}

	// The old raw asset is no longer a valid download: it is not gzip.
	if err := downloadReleaseBinary(context.Background(), serveBytes(t, raw), dest); err == nil ||
		!strings.Contains(err.Error(), "not a gzip release archive") {
		t.Fatalf("raw binary error = %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("rejected download was not removed")
	}
}

// TestUpdateRejectsUnusableArchives covers a gzip archive whose content is not
// ccl, a truncated archive, and one that expands past the size cap.
func TestUpdateRejectsUnusableArchives(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "download")

	if err := downloadReleaseBinary(context.Background(), serveBytes(t, gzipBytes(t, []byte("<html>maintenance</html>"))), dest); err == nil {
		t.Fatal("a gzip archive of HTML was accepted")
	}

	archive := gzipBytes(t, bytes.Repeat([]byte("x"), 4096))
	if err := downloadReleaseBinary(context.Background(), serveBytes(t, archive[:len(archive)/2]), dest); err == nil ||
		!strings.Contains(err.Error(), "decompress release archive") {
		t.Fatalf("truncated archive error = %v", err)
	}

	previous := maxReleaseBinaryBytes
	maxReleaseBinaryBytes = 1024
	t.Cleanup(func() { maxReleaseBinaryBytes = previous })
	if err := downloadReleaseBinary(context.Background(), serveBytes(t, archive), dest); err == nil ||
		!strings.Contains(err.Error(), "expands beyond") {
		t.Fatalf("oversized archive error = %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("rejected download was not removed")
	}
}

func TestDownloadProgressCountsCompressedBytes(t *testing.T) {
	progress := &downloadProgress{reader: strings.NewReader("abcdef"), total: 6}
	buf := make([]byte, 4)
	n, _ := progress.Read(buf)
	if n != 4 || progress.done != 4 {
		t.Fatalf("first read = %d, done = %d", n, progress.done)
	}
	n, _ = progress.Read(buf)
	if n != 2 || progress.done != 6 {
		t.Fatalf("second read = %d, done = %d", n, progress.done)
	}
}
