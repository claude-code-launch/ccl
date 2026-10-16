package cmd

import (
	"runtime"
	"strings"
	"testing"
)

func TestHeadlessHostDetectsSessionsWithoutABrowser(t *testing.T) {
	for _, key := range []string{"CLOUD_SHELL", "SSH_CONNECTION", "SSH_TTY", "DISPLAY", "WAYLAND_DISPLAY"} {
		t.Setenv(key, "")
	}
	t.Setenv("DISPLAY", ":0")
	if headlessHost() {
		t.Fatal("a local desktop session was reported headless")
	}
	t.Setenv("CLOUD_SHELL", "true")
	if !headlessHost() {
		t.Fatal("Cloud Shell was not detected")
	}
	t.Setenv("CLOUD_SHELL", "")
	t.Setenv("SSH_CONNECTION", "10.0.0.1 22 10.0.0.2 51234")
	if !headlessHost() {
		t.Fatal("an SSH session was not detected")
	}
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("DISPLAY", "")
	if got, want := headlessHost(), runtime.GOOS == "linux"; got != want {
		t.Fatalf("no display: headless = %v, want %v", got, want)
	}
}

func TestCloudLoginDefaultsToGoogleDrive(t *testing.T) {
	// Without the device client the Google path stops before any network
	// request, which identifies the platform that was chosen.
	t.Setenv("CCL_GOOGLE_DEVICE_CLIENT_ID", "")
	t.Setenv("CCL_SYNC_PASSPHRASE", "test-passphrase")
	_, err := executeCommand(RootCmd(), "cloud", "login", "--device")
	if err == nil || !strings.Contains(err.Error(), "Google device-flow client") {
		t.Fatalf("ccl cloud login without a platform: %v", err)
	}
}
