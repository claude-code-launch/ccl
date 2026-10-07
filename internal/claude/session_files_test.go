package claude

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func useRunDirectory(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "run")
	previous := runDirectory
	runDirectory = func() (string, error) { return dir, nil }
	t.Cleanup(func() { runDirectory = previous })
	return dir
}

func TestSessionSettingsLiveInAPrivateRunDirectory(t *testing.T) {
	dir := useRunDirectory(t)
	path := sessionSettingsPath("claude_abc123")
	if filepath.Dir(path) != dir {
		t.Fatalf("settings path %q is outside the run directory", path)
	}
	if got := settingsFilePID(filepath.Base(path)); got != os.Getpid() {
		t.Fatalf("settings file does not carry the owner PID: %q", path)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("run directory mode = %o", info.Mode().Perm())
	}
}

func TestSettingsFilePID(t *testing.T) {
	for name, want := range map[string]int{
		"claude_abc-1234_settings.json": 1234,
		"claude_abc_settings.json":      0, // legacy, no PID
		"claude_abc-x_settings.json":    0,
		"claude_abc-0_settings.json":    0,
	} {
		if got := settingsFilePID(name); got != want {
			t.Fatalf("settingsFilePID(%q) = %d, want %d", name, got, want)
		}
	}
}

// TestSweepRemovesOnlyAbandonedSettings covers the leftovers a killed session
// used to leave in the temp directory, some carrying plaintext API keys.
func TestSweepRemovesOnlyAbandonedSettings(t *testing.T) {
	dir := useRunDirectory(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Skipf("cannot start a helper process: %v", err)
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Skipf("cannot run a helper process: %v", err)
	}

	write := func(path string, age time.Duration) {
		t.Helper()
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	name := func(pid int) string { return "claude_x-" + strconv.Itoa(pid) + settingsFileSuffix }
	files := map[string]bool{ // path -> should survive
		filepath.Join(dir, name(os.Getpid())):              true,
		filepath.Join(dir, name(sleeper.Process.Pid)):      true,
		filepath.Join(dir, name(gone.Process.Pid)):         false,
		filepath.Join(dir, "notes.json"):                   true,
		filepath.Join(temp, "claude_old_settings.json"):    false,
		filepath.Join(temp, "claude_recent_settings.json"): true,
		filepath.Join(temp, "unrelated_settings.json"):     true,
	}
	for path := range files {
		age := time.Minute
		if filepath.Base(path) == "claude_old_settings.json" {
			age = 48 * time.Hour
		}
		write(path, age)
	}

	sweepStaleSessionFiles()
	for path, survive := range files {
		_, err := os.Stat(path)
		if exists := err == nil; exists != survive {
			t.Errorf("%s: exists=%t, want %t", filepath.Base(path), exists, survive)
		}
	}
}
