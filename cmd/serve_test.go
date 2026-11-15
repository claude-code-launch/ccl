package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestServeCommandFlagsAndValidation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// --host was retired: ccl serve always binds 127.0.0.1 (or a local Unix socket).
	cmd := newServeCommand()
	if cmd.Flags().Lookup("host") != nil {
		t.Fatal("ccl serve still registers --host")
	}

	for _, args := range [][]string{
		{"--port", "70000", "--no-open"},
		{"--socket", "/tmp/x.sock", "--port", "8099", "--no-open"},
		{"--base-path", "../bad", "--no-open"},
		{"extra-arg", "--no-open"},
	} {
		c := newServeCommand()
		c.SetArgs(args)
		c.SetOut(&bytes.Buffer{})
		c.SetErr(&bytes.Buffer{})
		if err := c.Execute(); err == nil {
			t.Fatalf("expected error for args %v", args)
		}
	}
}

func TestServeCommandSocketLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX socket mode test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	shortDir, err := os.MkdirTemp("", "ccl-serve")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
	sock := filepath.Join(shortDir, "ccl.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout syncBuffer
	cmd := newServeCommand()
	cmd.SetContext(ctx)
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--socket", sock, "--workspace", t.TempDir(), "--no-shell"})

	done := make(chan error, 1)
	go func() {
		done <- cmd.Execute()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stdout.String(), "\n") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("ccl serve --socket did not print ready JSON")
		}
		time.Sleep(20 * time.Millisecond)
	}

	var ready struct {
		Socket string `json:"socket"`
		PID    int    `json:"pid"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &ready); err != nil {
		t.Fatalf("ready JSON: %v (%q)", err, stdout.String())
	}
	if ready.Socket != sock || ready.PID == 0 {
		t.Fatalf("ready = %+v", ready)
	}

	cancel()
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("serve exit = %v", runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ccl serve did not exit on context cancel")
	}
}
