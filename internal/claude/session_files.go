package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
)

// Session settings files can carry a direct provider's API key, so they live
// in ~/.ccl/run (0700) rather than the shared temp directory, and each name
// carries the owning ccl process ID. A session killed before it could clean up
// leaves its file behind; the next launch removes every file whose process is
// gone.
const (
	settingsFileSuffix = "_settings.json"
	// legacyTempSettingsAge is how old a settings file in the system temp
	// directory (where ccl used to write them) must be before it is swept.
	legacyTempSettingsAge = 24 * time.Hour
)

// runDirectory is var so tests can redirect it.
var runDirectory = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ccl", "run"), nil
}

// sessionSettingsPath returns where this process writes a session's settings,
// creating ~/.ccl/run if needed. It falls back to the temp directory when the
// home directory cannot be used.
func sessionSettingsPath(session string) string {
	name := fmt.Sprintf("%s-%d%s", session, os.Getpid(), settingsFileSuffix)
	dir, err := runDirectory()
	if err == nil {
		if err = os.MkdirAll(dir, 0o700); err == nil {
			_ = os.Chmod(dir, 0o700)
			return filepath.Join(dir, name)
		}
	}
	oauthproxy.LogWarnf("session run directory unavailable (%v); using the temp directory", err)
	return filepath.Join(os.TempDir(), name)
}

// sweepStaleSessionFiles removes settings files left by ccl processes that no
// longer run, plus legacy ones from the temp directory. Best effort: it never
// fails a launch.
func sweepStaleSessionFiles() {
	if dir, err := runDirectory(); err == nil {
		sweepSettingsDir(dir, func(path string, pid int) bool {
			return pid != os.Getpid() && !processAlive(pid, path)
		})
	}
	sweepSettingsDir(os.TempDir(), func(path string, _ int) bool {
		info, err := os.Lstat(path)
		return err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) > legacyTempSettingsAge
	})
}

func sweepSettingsDir(dir string, stale func(path string, pid int) bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "claude_") || !strings.HasSuffix(name, settingsFileSuffix) {
			continue
		}
		path := filepath.Join(dir, name)
		if stale(path, settingsFilePID(name)) {
			if err := os.Remove(path); err == nil {
				oauthproxy.LogDebugf("removed stale session settings %s", name)
			}
		}
	}
}

// settingsFilePID extracts the owning process ID from
// claude_<id>-<pid>_settings.json; legacy names without one return 0.
func settingsFilePID(name string) int {
	stem := strings.TrimSuffix(name, settingsFileSuffix)
	dash := strings.LastIndexByte(stem, '-')
	if dash < 0 {
		return 0
	}
	pid, err := strconv.Atoi(stem[dash+1:])
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
