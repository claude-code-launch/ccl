package cloudsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Entry points the CLI no longer calls. They stay here as fixtures: the tests
// use them to build single-remote, passphrase, and legacy Keychain profiles
// and to push snapshots, so migration and sync paths keep their coverage.

// LoginICloud retains the original passphrase API for callers that explicitly
// select the legacy-compatible passphrase mode.
func LoginICloud(passphrase string) (LoginResult, error) {
	return LoginICloudWithPassphrase(passphrase)
}

func LoginICloudKeychain() (LoginResult, error) {
	remoteDir, err := defaultICloudDirectory()
	if err != nil {
		return LoginResult{}, err
	}
	if err := os.MkdirAll(filepath.Join(remoteDir, snapshotsDirectory), 0o700); err != nil {
		return LoginResult{}, fmt.Errorf("create iCloud sync directory: %w", err)
	}

	profilePath := filepath.Join(remoteDir, profileFileName)
	var profile remoteProfile
	if err := readJSONFile(profilePath, &profile); err != nil {
		if !os.IsNotExist(err) {
			return LoginResult{}, err
		}
		var key []byte
		profile, key, err = newMasterKeyProfile()
		if err != nil {
			return LoginResult{}, err
		}
		if err := platformKeyStore(profile.ID, key); err != nil {
			return LoginResult{}, keychainLoginError(err)
		}
		if err := writeJSONAtomic(profilePath, profile, 0o600); err != nil {
			return LoginResult{}, fmt.Errorf("write iCloud sync profile: %w", err)
		}
		return finishLogin(remoteDir, profile, key, false, keyModeKeychain, false)
	}
	if err := validateRemoteProfile(profile); err != nil {
		return LoginResult{}, err
	}

	key, keychainErr := platformKeyLoad(profile.ID)
	migrated := false
	if keychainErr != nil {
		localKey, localErr := existingLocalProfileKey(profile.ID)
		if localErr != nil {
			if errors.Is(localErr, os.ErrNotExist) {
				uninitialized, checkErr := isUninitializedRemoteProfile(remoteDir)
				if checkErr != nil {
					return LoginResult{}, checkErr
				}
				if uninitialized && errors.Is(keychainErr, ErrKeychainItemMissing) {
					return replaceUninitializedProfile(remoteDir)
				}
				return LoginResult{}, keychainLoginError(keychainErr)
			}
			return LoginResult{}, localErr
		}
		// Authenticate the old file key before replacing any Keychain item.
		localDir, dirErr := cclDirectory()
		if dirErr != nil {
			return LoginResult{}, dirErr
		}
		probe := &Manager{localDir: localDir, remoteDir: remoteDir, profileID: profile.ID, key: localKey}
		if err := probe.verifyOrCreateProfileKey(profile.ID, false); err != nil {
			return LoginResult{}, fmt.Errorf("verify existing local sync key: %w", err)
		}
		if err := platformKeyStore(profile.ID, localKey); err != nil {
			return LoginResult{}, keychainLoginError(err)
		}
		key = localKey
		migrated = true
	}
	return finishLogin(remoteDir, profile, key, true, keyModeKeychain, migrated)
}

func replaceUninitializedProfile(remoteDir string) (LoginResult, error) {
	profile, key, err := newMasterKeyProfile()
	if err != nil {
		return LoginResult{}, err
	}
	if err := platformKeyStore(profile.ID, key); err != nil {
		return LoginResult{}, keychainLoginError(err)
	}
	if err := writeJSONAtomic(filepath.Join(remoteDir, profileFileName), profile, 0o600); err != nil {
		return LoginResult{}, fmt.Errorf("replace uninitialized iCloud sync profile: %w", err)
	}
	return finishLogin(remoteDir, profile, key, false, keyModeKeychain, false)
}

func finishLogin(
	remoteDir string,
	profile remoteProfile,
	key []byte,
	existing bool,
	keyMode string,
	migrated bool,
) (LoginResult, error) {
	return finishLoginForProvider(
		remoteDir, profile, key, existing, keyMode, migrated,
		providerICloud, remoteDir,
	)
}

func ImportRecoveryKey(value string) (KeyImportResult, error) {
	return ImportRecoveryKeyForProvider(value, "")
}

func (m *Manager) Push(force bool) (PushResult, error) {
	prepared, err := m.preparePush()
	if err != nil {
		return PushResult{}, err
	}
	plan, err := m.planPush(prepared, force)
	if err != nil {
		return PushResult{}, err
	}
	result, err := plan.commit(prepared, true)
	if err != nil {
		return PushResult{}, err
	}
	return result, nil
}

func HasDiagnosticErrors(report DiagnosticReport) bool {
	for _, check := range report.Checks {
		if strings.EqualFold(check.Level, "error") {
			return true
		}
	}
	return false
}

func IsNotConfiguredDiagnostic(report DiagnosticReport) bool {
	return !report.Configured && len(report.Checks) == 1 &&
		report.Checks[0].Level == "info" &&
		strings.Contains(report.Checks[0].Message, "not configured")
}

func authorizeGoogleDrive(ctx context.Context) (*googleDriveRemote, error) {
	return authorizeGoogleDriveWithNotice(ctx, nil)
}
