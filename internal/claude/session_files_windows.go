//go:build windows

package claude

import (
	"os"
	"time"
)

// processAlive cannot cheaply probe a PID on Windows, so a settings file is
// presumed alive until it is a day old.
func processAlive(_ int, path string) bool {
	info, err := os.Stat(path)
	return err == nil && time.Since(info.ModTime()) < legacyTempSettingsAge
}

// On Windows only Ctrl+C reaches ccl; Claude Code receives it from the console
// itself, so it is caught (keeping ccl alive to clean up) but not forwarded.
var launchSignals = []os.Signal{os.Interrupt}

func forwardLaunchSignal(os.Signal) bool { return false }
