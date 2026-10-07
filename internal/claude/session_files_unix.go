//go:build !windows

package claude

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether pid is still running. EPERM means it exists but
// belongs to another user, which still counts as alive.
func processAlive(pid int, _ string) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// launchSignals are forwarded to Claude Code so ccl outlives it and can clean
// up. SIGINT is caught but not forwarded: the terminal already delivers it to
// the whole foreground group, and a second copy would make Claude Code quit.
var launchSignals = []os.Signal{syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT, os.Interrupt}

func forwardLaunchSignal(sig os.Signal) bool {
	return sig != os.Interrupt
}
