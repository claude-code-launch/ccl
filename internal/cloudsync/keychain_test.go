package cloudsync

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyModeDescriptionsCoverEveryMode(t *testing.T) {
	for mode, want := range map[string]string{
		keyModeKeychain:   "legacy macOS Keychain",
		keyModeLocal:      "local profile key",
		keyModePassphrase: "passphrase-derived local key",
		keyModeRecovery:   "imported recovery key",
		keyModePairing:    "approved device pairing",
		"":                "local key file",
		"future-mode":     "local key file",
	} {
		if got := KeyModeDescription(mode); got != want {
			t.Fatalf("KeyModeDescription(%q) = %q, want %q", mode, got, want)
		}
	}
}

func TestLoadProfileKeyFilePromotesTheLegacyRootKey(t *testing.T) {
	local := t.TempDir()
	profileID := strings.Repeat("a", 32)
	key := bytes.Repeat([]byte{3}, 32)
	if err := writeAtomic(filepath.Join(local, cloudKeyName), key, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadProfileKeyFile(local, profileID)
	if err != nil || !bytes.Equal(loaded, key) {
		t.Fatalf("loadProfileKeyFile() = %d bytes, %v", len(loaded), err)
	}
	// The rescue writes a copy into the profile directory, leaving the root key
	// untouched for a rollback.
	promoted, err := os.ReadFile(profileKeyPath(local, profileID))
	if err != nil || !bytes.Equal(promoted, key) {
		t.Fatalf("promoted key = %d bytes, %v", len(promoted), err)
	}
	if _, err := os.Stat(filepath.Join(local, cloudKeyName)); err != nil {
		t.Fatalf("the rescue removed the root key: %v", err)
	}

	// Once promoted, the profile copy is authoritative even if the root key
	// changes.
	replacement := bytes.Repeat([]byte{4}, 32)
	if err := writeAtomic(filepath.Join(local, cloudKeyName), replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadProfileKeyFile(local, profileID)
	if err != nil || !bytes.Equal(loaded, key) {
		t.Fatalf("profile key was not authoritative: %d bytes, %v", len(loaded), err)
	}
}

func TestLoadProfileKeyFileReportsUnusableKeys(t *testing.T) {
	local := t.TempDir()
	profileID := strings.Repeat("b", 32)

	// Neither copy exists: the caller sees the original profile-key error.
	if _, err := loadProfileKeyFile(local, profileID); err == nil {
		t.Fatal("a missing profile key was accepted")
	}

	// A short profile key is rejected rather than padded.
	if err := os.MkdirAll(profileDirectory(local, profileID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profileKeyPath(local, profileID), bytes.Repeat([]byte{1}, 16), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProfileKeyFile(local, profileID); err == nil ||
		!strings.Contains(err.Error(), "invalid local cloud encryption key") {
		t.Fatalf("short profile key error = %v", err)
	}

	// A short legacy root key cannot seed the promotion either.
	if err := os.Remove(profileKeyPath(local, profileID)); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(local, cloudKeyName), bytes.Repeat([]byte{2}, 16), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProfileKeyFile(local, profileID); err == nil {
		t.Fatal("a short legacy key was promoted")
	}
	if _, err := loadLocalKeyFile(local); err == nil ||
		!strings.Contains(err.Error(), "invalid local encryption key") {
		t.Fatalf("loadLocalKeyFile() with a short key = %v", err)
	}
}

// TestLoadProfileKeyFileRefusesASymlinkedCopy pins that the promotion follows
// the same regular-file rule as every other read.
func TestLoadProfileKeyFileRefusesASymlinkedCopy(t *testing.T) {
	local := t.TempDir()
	profileID := strings.Repeat("c", 32)
	key := bytes.Repeat([]byte{5}, 32)
	if err := writeAtomic(filepath.Join(local, cloudKeyName), key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(profileDirectory(local, profileID), 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(local, "elsewhere.key")
	if err := os.WriteFile(elsewhere, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, profileKeyPath(local, profileID)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProfileKeyFile(local, profileID); err == nil ||
		!strings.Contains(err.Error(), "non-regular file") {
		t.Fatalf("symlinked profile key error = %v", err)
	}
}
