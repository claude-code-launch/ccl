package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultSessionStorePathLivesUnderTheUserHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := DefaultSessionStorePath()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, ".ccl", "acp", "sessions") {
		t.Fatalf("DefaultSessionStorePath() = %q", path)
	}
}

func TestSessionStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	store := newSessionStore(dir)
	if store == nil {
		t.Fatal("newSessionStore returned nil for a path")
	}
	if newSessionStore("") != nil {
		t.Fatal("an empty path did not disable the store")
	}

	entry := persistedSession{
		SessionID: "acp-1", ClaudeSessionID: "claude-1", CWD: t.TempDir(),
	}
	if err := store.save(entry); err != nil {
		t.Fatal(err)
	}
	// The store is private: it must not be readable by other users.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("session store mode = %o", perm)
	}
	fileInfo, err := os.Stat(store.path(entry.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("session file mode = %o", perm)
	}
	// No staging file is left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != entry.SessionID+".json" {
		t.Fatalf("session store contents = %v", entries)
	}

	loaded, err := store.load(entry.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != entry {
		t.Fatalf("loaded %+v, want %+v", loaded, entry)
	}

	// Saving again replaces the mapping, which is how a resumed session
	// records its new Claude session id.
	updated := entry
	updated.ClaudeSessionID = "claude-2"
	if err := store.save(updated); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.load(entry.SessionID)
	if err != nil || loaded.ClaudeSessionID != "claude-2" {
		t.Fatalf("after update: %+v, %v", loaded, err)
	}
}

// TestSessionStoreIgnoresIncompleteEntries pins that a session without both
// ids is simply not persisted, rather than written as an unusable mapping.
func TestSessionStoreIgnoresIncompleteEntries(t *testing.T) {
	store := newSessionStore(t.TempDir())
	for name, entry := range map[string]persistedSession{
		"no session id": {ClaudeSessionID: "claude-1", CWD: "/tmp"},
		"no claude id":  {SessionID: "acp-1", CWD: "/tmp"},
		"empty":         {},
	} {
		if err := store.save(entry); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var nilStore *sessionStore
	if err := nilStore.save(persistedSession{SessionID: "a", ClaudeSessionID: "b"}); err != nil {
		t.Fatalf("nil store save = %v", err)
	}
	if _, err := nilStore.load("a"); !os.IsNotExist(err) {
		t.Fatalf("nil store load = %v", err)
	}
}

// TestSessionStoreRejectsUnusableMappings covers the load-side guards: a path
// escape, unreadable JSON, a mapping that does not describe this session, and
// a working directory that is not absolute.
func TestSessionStoreRejectsUnusableMappings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	store := newSessionStore(dir)
	cwd := t.TempDir()

	// A traversing id is treated as "not found" rather than resolved.
	for _, id := range []string{"", "../escape", "a/b", `a\b`, "."} {
		if _, err := store.load(id); !os.IsNotExist(err) {
			t.Fatalf("load(%q) error = %v", id, err)
		}
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(id, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("broken", "{")
	if _, err := store.load("broken"); err == nil ||
		!strings.Contains(err.Error(), "decode ACP session") {
		t.Fatalf("broken session error = %v", err)
	}

	write("mismatch", `{"sessionId":"other","claudeSessionId":"c","cwd":"`+cwd+`"}`)
	if _, err := store.load("mismatch"); err == nil ||
		!strings.Contains(err.Error(), "invalid ACP session mapping") {
		t.Fatalf("mismatched session error = %v", err)
	}

	write("relative", `{"sessionId":"relative","claudeSessionId":"c","cwd":"somewhere"}`)
	if _, err := store.load("relative"); err == nil ||
		!strings.Contains(err.Error(), "invalid ACP session cwd") {
		t.Fatalf("relative cwd error = %v", err)
	}

	if _, err := store.load("missing"); !os.IsNotExist(err) {
		t.Fatalf("missing session error = %v", err)
	}
}

// TestSessionStoreSurfacesWriteFailures pins that a store that cannot be
// created reports it instead of silently losing the mapping.
func TestSessionStoreSurfacesWriteFailures(t *testing.T) {
	parent := t.TempDir()
	// A file where the directory belongs makes MkdirAll fail.
	blocked := filepath.Join(parent, "blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newSessionStore(blocked)
	if err := store.save(persistedSession{
		SessionID: "a", ClaudeSessionID: "b", CWD: "/tmp",
	}); err == nil || !strings.Contains(err.Error(), "create ACP session store") {
		t.Fatalf("blocked store error = %v", err)
	}
}

// TestSessionStoreValidPathKeepsFilesInItsOwnDirectory pins the traversal rule
// that protects every load.
func TestSessionStoreValidPathKeepsFilesInItsOwnDirectory(t *testing.T) {
	store := newSessionStore("/sessions")
	path, err := store.validPath("acp-1")
	if err != nil || path != filepath.Join("/sessions", "acp-1.json") {
		t.Fatalf("validPath() = %q, %v", path, err)
	}
	if _, err := store.validPath("../etc/passwd"); !os.IsNotExist(err) {
		t.Fatalf("traversing id error = %v", err)
	}
}

// TestPersistedSessionSerializesItsThreeFields pins the on-disk shape a future
// ccl version must keep reading.
func TestPersistedSessionSerializesItsThreeFields(t *testing.T) {
	data, err := json.Marshal(persistedSession{
		SessionID: "a", ClaudeSessionID: "b", CWD: "/c",
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"sessionId": "a", "claudeSessionId": "b", "cwd": "/c"}
	if len(fields) != len(want) {
		t.Fatalf("serialized fields = %v", fields)
	}
	for key, value := range want {
		if fields[key] != value {
			t.Fatalf("field %s = %v, want %q", key, fields[key], value)
		}
	}
}

func TestSessionStorePruneDropsOnlyStaleMappings(t *testing.T) {
	dir := t.TempDir()
	store := newSessionStore(dir)
	now := time.Now()
	write := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := now.Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return path
	}
	stale := write("old.json", 40*24*time.Hour)
	tempLeft := write(".session-123.tmp", 40*24*time.Hour)
	fresh := write("new.json", time.Hour)
	foreign := write("README", 40*24*time.Hour)

	store.prune(sessionStoreMaxAge, now)
	for path, survive := range map[string]bool{stale: false, tempLeft: false, fresh: true, foreign: true} {
		_, err := os.Stat(path)
		if exists := err == nil; exists != survive {
			t.Errorf("%s: exists=%t, want %t", filepath.Base(path), exists, survive)
		}
	}
	var nilStore *sessionStore
	nilStore.prune(time.Hour, now) // must not panic
}
