package oauthproxy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneLogDirKeepsRecentOwnLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := now.Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return path
	}
	old := write("ccl-debug-claude_old.log", 40*24*time.Hour)
	foreign := write("notes.log", 40*24*time.Hour)
	other := write("ccl-debug-claude_old.txt", 40*24*time.Hour)
	var recent []string
	for i := range 4 {
		recent = append(recent, write(fmt.Sprintf("ccl-debug-claude_%d.log", i), time.Duration(i+1)*time.Hour))
	}

	pruneLogDir(dir, "ccl-debug", 30*24*time.Hour, 3, now)

	gone := func(path string) bool { _, err := os.Stat(path); return os.IsNotExist(err) }
	if !gone(old) {
		t.Error("a log past the age limit survived")
	}
	if gone(foreign) || gone(other) {
		t.Error("a file that is not a ccl session log was removed")
	}
	// Newest three kept; the fourth (oldest of the recent) dropped by count.
	for i, path := range recent {
		if want := i < 3; gone(path) == want {
			t.Errorf("%s: removed=%t", filepath.Base(path), gone(path))
		}
	}
}
