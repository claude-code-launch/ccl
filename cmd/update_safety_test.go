package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if err := runPreview(); err == nil {
		t.Fatal("preview reported success after setup failure")
	}
}
