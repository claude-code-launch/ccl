package cloudsync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var (
	ErrKeychainUnavailable = errors.New("macOS Keychain is unavailable")
	ErrKeychainItemMissing = errors.New("sync key is not present in macOS Keychain")

	platformKeyStore = storePlatformKey
	platformKeyLoad  = loadPlatformKey
)

func KeyModeDescription(mode string) string {
	switch mode {
	case keyModeKeychain:
		return "legacy macOS Keychain"
	case keyModeLocal:
		return "local profile key"
	case keyModePassphrase:
		return "passphrase-derived local key"
	case keyModeRecovery:
		return "imported recovery key"
	case keyModePairing:
		return "approved device pairing"
	default:
		return "local key file"
	}
}

func loadLocalKeyFile(localDir string) ([]byte, error) {
	key, err := readRegularFile(filepath.Join(localDir, cloudKeyName), 32)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid local encryption key; log in or import a recovery key again")
	}
	return key, nil
}

func loadProfileKeyFile(localDir, profileID string) ([]byte, error) {
	key, err := readRegularFile(profileKeyPath(localDir, profileID), 32)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		// One-shot rescue for dual-write lag / partial upgrades: promote root
		// cloud.key into the profile directory when it is still the only copy.
		legacy, legacyErr := loadLocalKeyFile(localDir)
		if legacyErr != nil {
			return nil, err
		}
		if mkErr := ensureLocalCloudDirectory(profileDirectory(localDir, profileID)); mkErr != nil {
			return nil, mkErr
		}
		if writeErr := writeAtomic(profileKeyPath(localDir, profileID), legacy, 0o600); writeErr != nil {
			return nil, writeErr
		}
		return append([]byte(nil), legacy...), nil
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid local cloud encryption key; log in or import a recovery key again")
	}
	return append([]byte(nil), key...), nil
}
