//go:build !windows

package claude

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRunForwardsTerminationToClaude pins S7: a SIGTERM aimed at ccl reaches
// Claude Code and does not kill ccl first, so the deferred cleanup still runs.
func TestRunForwardsTerminationToClaude(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	got := filepath.Join(dir, "got")
	child := exec.Command("sh", "-c",
		`trap 'echo TERM > "$GOT"; exit 0' TERM; : > "$READY"; while :; do sleep 0.05; done`)
	child.Env = append(os.Environ(), "READY="+ready, "GOT="+got)

	result := make(chan error, 1)
	go func() { result <- runForwardingSignals(child) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runForwardingSignals() = %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = child.Process.Kill()
		t.Fatal("child did not exit after the forwarded SIGTERM")
	}
	if data, err := os.ReadFile(got); err != nil || string(data) != "TERM\n" {
		t.Fatalf("child saw %q, %v", data, err)
	}
}
