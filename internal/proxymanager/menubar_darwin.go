package proxymanager

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

//go:embed menubar.jxa
var menuBarScript string

// The helper owns AppKit's event loop. Watching authenticated readiness also
// removes the icon when the daemon is killed without running its defers.
func (m *Manager) RunMenuBar(ctx context.Context, id string) error {
	s, err := m.Read()
	if err != nil {
		return err
	}
	if s == nil || s.ID != id || !m.Running(ctx, s) {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command("/usr/bin/osascript", "-l", "JavaScript", "-", executable, s.Provider)
	command.Stdin = strings.NewReader(menuBarScript)
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() { _ = command.Process.Kill(); <-done }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			done <- err
			return err
		case <-ticker.C:
			current, err := m.Read()
			if err != nil || current == nil || current.ID != id || !m.Running(ctx, current) {
				return nil
			}
		}
	}
}

func (m *Manager) startMenuBar(id string) func() {
	if os.Getenv("CCL_PROXY_NO_MENUBAR") == "1" {
		return func() {}
	}
	executable, err := os.Executable()
	if err != nil {
		return func() {}
	}
	command := exec.Command(executable, "proxy", "menubar", "--instance", id)
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
	if err := command.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "ccl: menu bar unavailable:", err)
		return func() {}
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	return func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
	}
}

func ShowMenuError(message string) {
	if os.Getenv("CCL_PROXY_NO_MENUBAR") == "1" {
		return
	}
	script := `function run(argv) { const app = Application.currentApplication(); app.includeStandardAdditions = true; app.displayAlert('CCL Proxy', {message: argv[0], as: 'critical'}); }`
	command := exec.Command("/usr/bin/osascript", "-l", "JavaScript", "-", message)
	command.Stdin = strings.NewReader(script)
	_ = command.Run()
}
