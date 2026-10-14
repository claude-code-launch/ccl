package fsutil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestFileLockContentionCanBeCanceledAndReacquired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.lock")
	release, err := AcquireFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if _, err := AcquireFileLock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contending lock: %v", err)
	}
	release()
	release() // shutdown and cleanup can overlap
	again, err := AcquireFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestFileLockProcessHelper(t *testing.T) {
	path := os.Getenv("CCL_TEST_FILE_LOCK")
	if path == "" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := AcquireFileLock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("another process did not hold the lock: %v", err)
	}
}

func TestFileLockSerializesSeparateProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.lock")
	release, err := AcquireFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	command := exec.Command(os.Args[0], "-test.run=^TestFileLockProcessHelper$", "-test.v")
	command.Env = append(os.Environ(), "CCL_TEST_FILE_LOCK="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cross-process lock: %v\n%s", err, output)
	}
}
