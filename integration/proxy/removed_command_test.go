package proxy_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetiredCommandsDoNotLaunchGrok(t *testing.T) {
	binary := os.Getenv("CCL_TEST_BINARY")
	if binary == "" {
		t.Skip("set CCL_TEST_BINARY to a built native ccl binary")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	for key, value := range map[string]string{
		"HOME": home, "USERPROFILE": home, "CODEX_HOME": filepath.Join(home, ".codex"),
		"CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"), "PATH": home, "CCL_LANG": "en",
	} {
		t.Setenv(key, value)
	}
	for _, args := range [][]string{
		{"proxy"}, {"proxy", "start"}, {"proxy", "stop"}, {"proxy", "status"}, {"proxy", "--help"},
		{"claude", "--help"}, {"acp"}, {"sign"}, {"statusline"},
	} {
		message := "ccl " + args[0] + " has been retired"
		if args[0] == "proxy" {
			message = "ccl proxy has been removed; use ccl codex start/status/stop"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		command := exec.CommandContext(ctx, binary, args...)
		command.Dir = home
		output, err := command.CombinedOutput()
		cancel()
		if err == nil || !strings.Contains(string(output), message) {
			t.Fatalf("ccl %v: err=%v output=%s", args, err, output)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("removed command touched the user profile: entries=%v err=%v", entries, err)
	}
}
