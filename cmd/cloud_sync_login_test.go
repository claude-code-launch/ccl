package cmd

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
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

func TestCloudLoginDefaultsToGoogleDriveAndReadsThePastedRedirect(t *testing.T) {
	// --no-browser prints the Google authorization URL and waits for the
	// pasted address; empty input times out only after that, so the notice
	// identifies the platform and mode without any network request.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CCL_SYNC_PASSPHRASE", "test-passphrase")
	t.Setenv("CCL_GOOGLE_OAUTH_CLIENT_ID", "test-client.apps.googleusercontent.com")
	t.Setenv("CCL_GOOGLE_OAUTH_CLIENT_SECRET", "test-secret")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := RootCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetIn(strings.NewReader(""))
	command.SetArgs([]string{"cloud", "login", "--no-browser"})
	err := command.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("ccl cloud login --no-browser: %v", err)
	}
	for _, text := range []string{"accounts.google.com", "drive.appdata", "paste it here"} {
		if !strings.Contains(output.String(), text) {
			t.Fatalf("output missing %q:\n%s", text, output.String())
		}
	}
}
