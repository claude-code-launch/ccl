package cloudsync

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEncryptedICloudSyncLifecycle(t *testing.T) {
	home := t.TempDir()
	drive := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", drive)
	cclDir := filepath.Join(home, ".ccl")
	authDir := filepath.Join(cclDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	firstConfig := []byte("active_provider: grok1\nsecret: local-api-key\n")
	if err := os.WriteFile(filepath.Join(cclDir, "config.yaml"), firstConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "xai-a.json"),
		[]byte(`{"type":"xai","access_token":"super-secret-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	login, err := LoginICloudLocalKey()
	if err != nil {
		t.Fatal(err)
	}
	if login.Existing {
		t.Fatal("first login unexpectedly reused a profile")
	}
	manager, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	tagged, err := manager.Tag("")
	if err != nil {
		t.Fatal(err)
	}
	if tagged.Tag != defaultTag {
		t.Fatalf("default tag = %q", tagged.Tag)
	}
	pushed, err := manager.Push(false)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed.Uploaded {
		t.Fatal("first push did not upload")
	}

	remoteDir := filepath.Join(drive, remoteDirectory)
	indexCipher, err := os.ReadFile(filepath.Join(remoteDir, indexFileName))
	if err != nil {
		t.Fatal(err)
	}
	snapshotCipher, err := os.ReadFile(filepath.Join(remoteDir, snapshotsDirectory, pushed.ID+".ccl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{[]byte("local-api-key"), []byte("super-secret-token"), []byte("grok1")} {
		if bytes.Contains(indexCipher, secret) || bytes.Contains(snapshotCipher, secret) {
			t.Fatalf("remote encrypted files expose %q", secret)
		}
	}

	// Named tags are immutable unless the user explicitly forces replacement.
	if _, err := manager.Tag("release-1"); err != nil {
		t.Fatal(err)
	}
	tagPush, err := manager.Push(false)
	if err != nil {
		t.Fatal(err)
	}
	if tagPush.Uploaded {
		t.Fatal("tagging unchanged contents should reuse the encrypted snapshot")
	}
	secondConfig := []byte("active_provider: grok1\nsecret: newer-local-key\n")
	if err := os.WriteFile(filepath.Join(cclDir, "config.yaml"), secondConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Tag("release-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Push(false); err == nil || !strings.Contains(err.Error(), "already points to different data") {
		t.Fatalf("expected immutable tag conflict, got %v", err)
	}

	// No explicit tag means latest; a local change based on the last remote
	// snapshot can be pushed directly.
	state, err := manager.loadState()
	if err != nil {
		t.Fatal(err)
	}
	state.PendingTag = ""
	state.PendingHash = ""
	state.ExplicitTag = false
	if err := manager.saveState(state); err != nil {
		t.Fatal(err)
	}
	secondPush, err := manager.Push(false)
	if err != nil {
		t.Fatal(err)
	}
	if !secondPush.Uploaded || secondPush.Tag != defaultTag {
		t.Fatalf("automatic latest push = %+v", secondPush)
	}

	// A forced pull replaces local config/auth exactly and keeps an encrypted
	// recoverable backup of the previous local data.
	if err := os.WriteFile(filepath.Join(cclDir, "config.yaml"), []byte("local: divergent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "stale.json"), []byte(`{"type":"xai"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pulled, err := manager.Pull("release-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !pulled.Downloaded || pulled.BackupPath == "" {
		t.Fatalf("forced pull = %+v", pulled)
	}
	gotConfig, err := os.ReadFile(filepath.Join(cclDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotConfig, firstConfig) {
		t.Fatalf("pulled config = %q, want %q", gotConfig, firstConfig)
	}
	if _, err := os.Stat(filepath.Join(authDir, "stale.json")); !os.IsNotExist(err) {
		t.Fatalf("stale auth credential survived pull: %v", err)
	}
	if info, err := os.Stat(pulled.BackupPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("encrypted backup mode/error = %v, %v", info, err)
	}
	status, err := manager.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "remote changes available" {
		// release-1 was pulled while latest still points to the second push.
		t.Fatalf("status after historical pull = %+v", status)
	}
}

func TestPullRejectsReplayedLatestIndex(t *testing.T) {
	home := t.TempDir()
	drive := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", drive)
	cclDir := filepath.Join(home, ".ccl")
	if err := os.MkdirAll(cclDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(cclDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoginICloudLocalKey(); err != nil {
		t.Fatal(err)
	}
	manager, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Push(false)
	if err != nil {
		t.Fatal(err)
	}
	index, err := manager.loadRemoteIndex()
	if err != nil {
		t.Fatal(err)
	}
	record := index.Snapshots[first.ID]
	record.CreatedAt = record.CreatedAt.Add(-2 * time.Hour)
	index.Snapshots[first.ID] = record
	if err := manager.saveRemoteIndex(index); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(drive, remoteDirectory, indexFileName)
	replayedIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(configPath, []byte("version: two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Push(false)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("second push reused the first snapshot")
	}
	if err := os.WriteFile(indexPath, replayedIndex, 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := manager.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "remote rollback suspected" {
		t.Fatalf("status = %q, want rollback warning", status.State)
	}
	if _, err := manager.Pull(defaultTag, false); err == nil || !strings.Contains(err.Error(), "cloud rollback") {
		t.Fatalf("replayed latest pull error = %v", err)
	}
	if _, err := manager.Pull(defaultTag, true); err != nil {
		t.Fatalf("forced rollback recovery: %v", err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "version: one\n" {
		t.Fatalf("forced rollback restored %q", got)
	}
}

func TestSnapshotPathValidationRejectsTraversal(t *testing.T) {
	for _, path := range []string{"../config.yaml", "auth/../cloud.key", "/tmp/config.yaml", "auth/nested/a.json"} {
		err := validateSnapshotFiles([]snapshotFile{{Path: path, Data: []byte("x")}})
		if err == nil {
			t.Fatalf("path %q was accepted", path)
		}
	}
}

// TestNewDeviceOpensTheProfileWithOnlyTheCloudAccount covers the account key:
// a second device logs in with nothing but access to the same cloud account.
func TestNewDeviceOpensTheProfileWithOnlyTheCloudAccount(t *testing.T) {
	drive := t.TempDir()
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", drive)
	firstHome, secondHome := t.TempDir(), t.TempDir()
	t.Setenv("HOME", firstHome)
	first, err := LoginICloudLocalKey()
	if err != nil {
		t.Fatal(err)
	}
	firstKey, err := os.ReadFile(profileKeyPath(filepath.Join(firstHome, ".ccl"), currentProfileID(t, firstHome)))
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", secondHome)
	second, err := LoginICloudLocalKey()
	if err != nil {
		t.Fatal(err)
	}
	if !second.Existing || second.DeviceID == first.DeviceID {
		t.Fatalf("second device login = %+v (first %+v)", second, first)
	}
	secondKey, err := os.ReadFile(profileKeyPath(filepath.Join(secondHome, ".ccl"), currentProfileID(t, secondHome)))
	if err != nil || !bytes.Equal(secondKey, firstKey) {
		t.Fatalf("second device key differs: %v", err)
	}

	// The first pull on the new device may replace ccl's own defaults, but
	// not a provider configured there.
	t.Setenv("HOME", firstHome)
	if err := os.WriteFile(filepath.Join(firstHome, ".ccl", "config.yaml"), []byte("providers:\n  a: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pushFrom(t)
	t.Setenv("HOME", secondHome)
	secondConfig := filepath.Join(secondHome, ".ccl", "config.yaml")
	if err := os.WriteFile(secondConfig, []byte("providers:\n  local: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Pull(defaultTag, false); err == nil || !strings.Contains(err.Error(), "unsynchronized changes") {
		t.Fatalf("pull over a configured provider: %v", err)
	}
	if err := os.WriteFile(secondConfig, []byte("provider_off: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result, err := manager.Pull(defaultTag, false); err != nil || !result.Downloaded {
		t.Fatalf("first pull over defaults = %+v, %v", result, err)
	}
	if data, _ := os.ReadFile(secondConfig); !strings.Contains(string(data), "a: {}") {
		t.Fatalf("pulled config = %q", data)
	}
}

func pushFrom(t *testing.T) {
	t.Helper()
	manager, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Push(false); err != nil {
		t.Fatal(err)
	}
}

func currentProfileID(t *testing.T, home string) string {
	t.Helper()
	registry, err := loadRegistry(filepath.Join(home, ".ccl"))
	if err != nil {
		t.Fatal(err)
	}
	return registry.ActiveProfileID
}
