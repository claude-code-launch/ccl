package cloudsync

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	login, err := LoginICloud("correct horse battery staple")
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

func TestICloudLoginRejectsWrongPassphraseForExistingData(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", t.TempDir())
	cclDir, err := cclDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cclDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cclDir, "config.yaml"), []byte("providers: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoginICloud("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoginICloud("this passphrase is incorrect"); err == nil ||
		!strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("wrong passphrase before first push = %v", err)
	}
	manager, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Push(false); err != nil {
		t.Fatal(err)
	}
	if _, err := LoginICloud("this passphrase is incorrect"); err == nil ||
		!strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("wrong passphrase error = %v", err)
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

func TestRecoveryKeyRoundTripAndChecksum(t *testing.T) {
	profileID := strings.Repeat("ab", 16)
	key := bytes.Repeat([]byte{0x42}, 32)
	encoded, err := encodeRecoveryKey(profileID, key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "CCL1-") {
		t.Fatalf("recovery key = %q", encoded)
	}
	gotProfile, gotKey, err := decodeRecoveryKey(strings.ToLower(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if gotProfile != profileID || !bytes.Equal(gotKey, key) {
		t.Fatalf("decoded recovery key = %q, %x", gotProfile, gotKey)
	}
	last := encoded[len(encoded)-1]
	replacement := byte('A')
	if last == replacement {
		replacement = 'B'
	}
	tampered := encoded[:len(encoded)-1] + string(replacement)
	if _, _, err := decodeRecoveryKey(tampered); err == nil {
		t.Fatal("tampered recovery key was accepted")
	}
}

func TestKeychainLoginExportAndImport(t *testing.T) {
	originalStore, originalLoad := platformKeyStore, platformKeyLoad
	t.Cleanup(func() {
		platformKeyStore, platformKeyLoad = originalStore, originalLoad
	})
	keys := make(map[string][]byte)
	platformKeyStore = func(profileID string, key []byte) error {
		keys[profileID] = append([]byte(nil), key...)
		return nil
	}
	platformKeyLoad = func(profileID string) ([]byte, error) {
		key := keys[profileID]
		if key == nil {
			return nil, ErrKeychainItemMissing
		}
		return append([]byte(nil), key...), nil
	}

	firstHome := t.TempDir()
	drive := t.TempDir()
	t.Setenv("HOME", firstHome)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", drive)
	if err := os.MkdirAll(filepath.Join(firstHome, ".ccl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firstHome, ".ccl", "config.yaml"), []byte("providers: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	login, err := LoginICloudKeychain()
	if err != nil {
		t.Fatal(err)
	}
	if login.Existing || login.KeyMode != keyModeKeychain {
		t.Fatalf("login = %+v", login)
	}
	if _, err := os.Stat(filepath.Join(firstHome, ".ccl", cloudKeyName)); !os.IsNotExist(err) {
		t.Fatalf("keychain login wrote %s: %v", cloudKeyName, err)
	}
	exported, err := ExportRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a new device: it can see iCloud Drive but has no local config or
	// Keychain item. The recovery key must authenticate before local state is written.
	secondHome := t.TempDir()
	t.Setenv("HOME", secondHome)
	clear(keys)
	wrongRecovery, err := encodeRecoveryKey(exported.ProfileID, bytes.Repeat([]byte{0x99}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRecoveryKey(wrongRecovery); err == nil ||
		!strings.Contains(err.Error(), "wrong passphrase/recovery key") {
		t.Fatalf("wrong recovery key error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(secondHome, ".ccl", cloudConfigName)); !os.IsNotExist(err) {
		t.Fatalf("wrong recovery key created local login state: %v", err)
	}
	imported, err := ImportRecoveryKey(exported.RecoveryKey)
	if err != nil {
		t.Fatal(err)
	}
	if imported.KeyMode != keyModeRecovery {
		t.Fatalf("import mode = %q", imported.KeyMode)
	}
	manager, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if manager.profileID != exported.ProfileID {
		t.Fatalf("loaded imported profile = %q", manager.profileID)
	}
}

func TestLocalKeyLoginMigratesLegacyKeychainMode(t *testing.T) {
	originalStore, originalLoad := platformKeyStore, platformKeyLoad
	t.Cleanup(func() {
		platformKeyStore, platformKeyLoad = originalStore, originalLoad
	})
	keys := make(map[string][]byte)
	platformKeyStore = func(profileID string, key []byte) error {
		keys[profileID] = append([]byte(nil), key...)
		return nil
	}
	platformKeyLoad = func(profileID string) ([]byte, error) {
		if key := keys[profileID]; key != nil {
			return append([]byte(nil), key...), nil
		}
		return nil, ErrKeychainItemMissing
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", t.TempDir())
	_, err := LoginICloudKeychain()
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := LoginICloudLocalKey()
	if err != nil {
		t.Fatal(err)
	}
	if !migrated.Migrated || migrated.KeyMode != keyModeLocal {
		t.Fatalf("local migration = %+v", migrated)
	}
	key, err := os.ReadFile(filepath.Join(home, ".ccl", cloudKeyName))
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 {
		t.Fatalf("migrated local key length = %d", len(key))
	}
	var cfg localCloudConfig
	if err := readJSONFile(filepath.Join(home, ".ccl", cloudConfigName), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.KeyMode != keyModeLocal {
		t.Fatalf("local config mode = %q", cfg.KeyMode)
	}
	profileKey, err := os.ReadFile(profileKeyPath(filepath.Join(home, ".ccl"), cfg.ProfileID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(profileKey, key) {
		t.Fatal("profile key differs from root cloud.key after local migration")
	}
	platformKeyLoad = func(string) ([]byte, error) {
		return nil, errors.New("legacy Keychain should not be read")
	}
	if _, err := Load(); err != nil {
		t.Fatalf("load migrated local key: %v", err)
	}
}

func TestKeychainLoginMigratesExistingLocalKey(t *testing.T) {
	originalStore, originalLoad := platformKeyStore, platformKeyLoad
	t.Cleanup(func() {
		platformKeyStore, platformKeyLoad = originalStore, originalLoad
	})
	keys := make(map[string][]byte)
	platformKeyStore = func(profileID string, key []byte) error {
		keys[profileID] = append([]byte(nil), key...)
		return nil
	}
	platformKeyLoad = func(profileID string) ([]byte, error) {
		if key := keys[profileID]; key != nil {
			return append([]byte(nil), key...), nil
		}
		return nil, ErrKeychainItemMissing
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", t.TempDir())
	if _, err := LoginICloudWithPassphrase("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(home, ".ccl", cloudKeyName))
	if err != nil {
		t.Fatal(err)
	}
	login, err := LoginICloudKeychain()
	if err != nil {
		t.Fatal(err)
	}
	if !login.Migrated || login.KeyMode != keyModeKeychain {
		t.Fatalf("migration login = %+v", login)
	}
	if _, err := os.Stat(filepath.Join(home, ".ccl", cloudKeyName)); !os.IsNotExist(err) {
		t.Fatalf("migrated key file still exists: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("stored key count = %d", len(keys))
	}
	for _, key := range keys {
		if !bytes.Equal(key, before) {
			t.Fatal("migrated Keychain key differs from original")
		}
	}
}

func TestKeychainLoginRequiresRecoveryOnNewDevice(t *testing.T) {
	originalStore, originalLoad := platformKeyStore, platformKeyLoad
	t.Cleanup(func() {
		platformKeyStore, platformKeyLoad = originalStore, originalLoad
	})
	keys := make(map[string][]byte)
	platformKeyStore = func(profileID string, key []byte) error {
		keys[profileID] = append([]byte(nil), key...)
		return nil
	}
	platformKeyLoad = func(profileID string) ([]byte, error) {
		if key := keys[profileID]; key != nil {
			return append([]byte(nil), key...), nil
		}
		return nil, ErrKeychainItemMissing
	}

	firstHome := t.TempDir()
	t.Setenv("HOME", firstHome)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", t.TempDir())
	if _, err := LoginICloudKeychain(); err != nil {
		t.Fatal(err)
	}
	clear(keys)
	t.Setenv("HOME", t.TempDir())
	if _, err := LoginICloudKeychain(); err == nil ||
		!strings.Contains(err.Error(), "ccl cloud key import") {
		t.Fatalf("missing recovery error = %v", err)
	}
}

func TestKeychainLoginReplacesUninitializedPassphraseProfile(t *testing.T) {
	originalStore, originalLoad := platformKeyStore, platformKeyLoad
	t.Cleanup(func() {
		platformKeyStore, platformKeyLoad = originalStore, originalLoad
	})
	keys := make(map[string][]byte)
	platformKeyStore = func(profileID string, key []byte) error {
		keys[profileID] = append([]byte(nil), key...)
		return nil
	}
	platformKeyLoad = func(string) ([]byte, error) {
		return nil, ErrKeychainItemMissing
	}

	home := t.TempDir()
	drive := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", drive)
	remoteDir := filepath.Join(drive, remoteDirectory)
	if err := os.MkdirAll(filepath.Join(remoteDir, snapshotsDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	orphan, err := newPassphraseProfile()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(remoteDir, profileFileName), orphan, 0o600); err != nil {
		t.Fatal(err)
	}

	login, err := LoginICloudKeychain()
	if err != nil {
		t.Fatal(err)
	}
	if login.Existing || login.KeyMode != keyModeKeychain {
		t.Fatalf("replacement login = %+v", login)
	}
	var replacement remoteProfile
	if err := readJSONFile(filepath.Join(remoteDir, profileFileName), &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.ID == orphan.ID || replacement.KDF != kdfMasterKey {
		t.Fatalf("replacement profile = %+v, orphan = %+v", replacement, orphan)
	}
	if len(keys[replacement.ID]) != 32 {
		t.Fatal("replacement key was not stored")
	}
	if _, err := os.Stat(filepath.Join(remoteDir, verifierFileName)); err != nil {
		t.Fatalf("replacement verifier: %v", err)
	}
}

func TestKeychainUnavailableErrorIsClassified(t *testing.T) {
	err := keychainLoginError(fmt.Errorf("%w: denied", ErrKeychainUnavailable))
	if !errors.Is(err, ErrKeychainUnavailable) {
		t.Fatalf("wrapped error lost sentinel: %v", err)
	}
}
