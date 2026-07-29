package cloudsync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Manager struct {
	localDir         string
	remoteDir        string
	remoteLabel      string
	alias            string
	remoteID         string
	provider         string
	deviceID         string
	profileID        string
	keyMode          string
	key              []byte
	primary          bool
	mirror           bool
	profileStateFile string
	remoteStateFile  string
	flushRemote      func() error
}

var errRemoteVerifierMissing = errors.New("cloud sync profile has no encrypted verifier")

// LoginICloud retains the original passphrase API for callers that explicitly
// select the legacy-compatible passphrase mode.
func LoginICloud(passphrase string) (LoginResult, error) {
	return LoginICloudWithPassphrase(passphrase)
}

func LoginICloudWithPassphrase(passphrase string) (LoginResult, error) {
	remoteDir, err := defaultICloudDirectory()
	if err != nil {
		return LoginResult{}, err
	}
	return loginPassphraseAt(remoteDir, passphrase)
}

func loginPassphraseAt(remoteDir, passphrase string) (LoginResult, error) {
	return loginPassphraseAtProvider(remoteDir, passphrase, providerICloud, remoteDir)
}

func loginPassphraseAtProvider(remoteDir, passphrase, provider, remoteLabel string) (LoginResult, error) {
	if err := os.MkdirAll(filepath.Join(remoteDir, snapshotsDirectory), 0o700); err != nil {
		return LoginResult{}, fmt.Errorf("create sync directory: %w", err)
	}

	profilePath := filepath.Join(remoteDir, profileFileName)
	var profile remoteProfile
	existing := true
	if err := readJSONFile(profilePath, &profile); err != nil {
		if !os.IsNotExist(err) {
			return LoginResult{}, err
		}
		existing = false
		profile, err = newPassphraseProfile()
		if err != nil {
			return LoginResult{}, err
		}
	} else if err := validateRemoteProfile(profile); err != nil {
		return LoginResult{}, err
	}
	key, err := deriveKey(passphrase, profile)
	if err != nil {
		return LoginResult{}, err
	}
	if !existing {
		if err := writeJSONAtomic(profilePath, profile, 0o600); err != nil {
			return LoginResult{}, fmt.Errorf("write cloud sync profile: %w", err)
		}
	}
	return finishLoginForProvider(
		remoteDir, profile, key, existing, keyModePassphrase, false,
		provider, remoteLabel,
	)
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

// LoginICloudLocalKey is the default macOS mode. It uses a random 256-bit
// master key stored under ~/.ccl/cloud/profiles/<id>/key (with a temporary
// ~/.ccl/cloud.key only until the v2 registry is created), and migrates the
// short-lived Keychain mode used by earlier ccl builds.
func LoginICloudLocalKey() (LoginResult, error) {
	remoteDir, err := defaultICloudDirectory()
	if err != nil {
		return LoginResult{}, err
	}
	return loginLocalKeyAt(remoteDir)
}

func loginLocalKeyAt(remoteDir string) (LoginResult, error) {
	return loginLocalKeyAtProvider(remoteDir, providerICloud, remoteDir)
}

func loginLocalKeyAtProvider(remoteDir, provider, remoteLabel string) (LoginResult, error) {
	if err := os.MkdirAll(filepath.Join(remoteDir, snapshotsDirectory), 0o700); err != nil {
		return LoginResult{}, fmt.Errorf("create sync directory: %w", err)
	}

	profilePath := filepath.Join(remoteDir, profileFileName)
	var profile remoteProfile
	if err := readJSONFile(profilePath, &profile); err != nil {
		if !os.IsNotExist(err) {
			return LoginResult{}, err
		}
		profile, key, createErr := newMasterKeyProfile()
		if createErr != nil {
			return LoginResult{}, createErr
		}
		if writeErr := writeJSONAtomic(profilePath, profile, 0o600); writeErr != nil {
			return LoginResult{}, fmt.Errorf("write cloud sync profile: %w", writeErr)
		}
		return finishLoginForProvider(
			remoteDir, profile, key, false, keyModeLocal, false,
			provider, remoteLabel,
		)
	}
	if err := validateRemoteProfile(profile); err != nil {
		return LoginResult{}, err
	}

	if key, localErr := existingLocalProfileKey(profile.ID); localErr == nil {
		return finishLoginForProvider(
			remoteDir, profile, key, true, keyModeLocal, false,
			provider, remoteLabel,
		)
	} else if !errors.Is(localErr, os.ErrNotExist) {
		return LoginResult{}, localErr
	}

	// Migrate the previous macOS Keychain mode without rotating the encryption
	// key or changing the remote profile.
	localDir, err := cclDirectory()
	if err != nil {
		return LoginResult{}, err
	}
	var previous localCloudConfig
	if provider == providerICloud &&
		readJSONFile(filepath.Join(localDir, cloudConfigName), &previous) == nil &&
		previous.ProfileID == profile.ID && previous.KeyMode == keyModeKeychain {
		key, loadErr := platformKeyLoad(profile.ID)
		if loadErr != nil {
			return LoginResult{}, keychainLoginError(loadErr)
		}
		probe := &Manager{localDir: localDir, remoteDir: remoteDir, profileID: profile.ID, key: key}
		if err := probe.verifyOrCreateProfileKey(profile.ID, false); err != nil {
			return LoginResult{}, fmt.Errorf("verify legacy Keychain sync key: %w", err)
		}
		return finishLoginForProvider(
			remoteDir, profile, key, true, keyModeLocal, true,
			provider, remoteLabel,
		)
	}

	uninitialized, err := isUninitializedRemoteProfile(remoteDir)
	if err != nil {
		return LoginResult{}, err
	}
	if uninitialized {
		return replaceUninitializedProfileLocalForProvider(remoteDir, provider, remoteLabel)
	}
	return LoginResult{}, fmt.Errorf("this device has no local key for the cloud sync profile; run `ccl cloud key import` or log in with `--passphrase` for a passphrase profile")
}

func isUninitializedRemoteProfile(remoteDir string) (bool, error) {
	for _, name := range []string{verifierFileName, indexFileName} {
		if _, err := os.Lstat(filepath.Join(remoteDir, name)); err == nil {
			return false, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	entries, err := os.ReadDir(filepath.Join(remoteDir, snapshotsDirectory))
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	return len(entries) == 0, nil
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

func replaceUninitializedProfileLocal(remoteDir string) (LoginResult, error) {
	return replaceUninitializedProfileLocalForProvider(remoteDir, providerICloud, remoteDir)
}

func replaceUninitializedProfileLocalForProvider(remoteDir, provider, remoteLabel string) (LoginResult, error) {
	profile, key, err := newMasterKeyProfile()
	if err != nil {
		return LoginResult{}, err
	}
	if err := writeJSONAtomic(filepath.Join(remoteDir, profileFileName), profile, 0o600); err != nil {
		return LoginResult{}, fmt.Errorf("replace uninitialized cloud sync profile: %w", err)
	}
	return finishLoginForProvider(
		remoteDir, profile, key, false, keyModeLocal, false,
		provider, remoteLabel,
	)
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

func finishLoginForProvider(
	remoteDir string,
	profile remoteProfile,
	key []byte,
	existing bool,
	keyMode string,
	migrated bool,
	provider string,
	remoteLabel string,
) (LoginResult, error) {
	if len(key) != 32 {
		return LoginResult{}, fmt.Errorf("invalid sync encryption key")
	}

	localDir, err := cclDirectory()
	if err != nil {
		return LoginResult{}, err
	}
	deviceID := ""
	var previous localCloudConfig
	if readJSONFile(filepath.Join(localDir, cloudConfigName), &previous) == nil &&
		previous.ProfileID == profile.ID && previous.DeviceID != "" {
		deviceID = previous.DeviceID
	}
	if deviceID == "" {
		deviceID, err = randomDeviceID()
		if err != nil {
			return LoginResult{}, err
		}
	}
	manager := &Manager{
		localDir: localDir, remoteDir: remoteDir, remoteLabel: remoteLabel,
		provider: provider, deviceID: deviceID,
		profileID: profile.ID, keyMode: keyMode, key: key,
	}
	if err := manager.verifyOrCreateProfileKey(profile.ID, !existing); err != nil {
		return LoginResult{}, fmt.Errorf("verify cloud login: %w", err)
	}
	if changed, err := ensureProfilePairingPublicKey(&profile, key); err != nil {
		return LoginResult{}, err
	} else if changed {
		if err := writeJSONAtomic(filepath.Join(remoteDir, profileFileName), profile, 0o600); err != nil {
			return LoginResult{}, fmt.Errorf("enable device pairing for cloud profile: %w", err)
		}
	}
	localConfig := localCloudConfig{
		Version: formatVersion, Provider: provider,
		RemoteDir: remoteDir, DeviceID: deviceID, ProfileID: profile.ID,
		KeyMode: keyMode, RemoteLabel: remoteLabel,
	}
	if previous.ProfileID != "" && previous.ProfileID != profile.ID {
		if err := os.Remove(filepath.Join(localDir, cloudStateName)); err != nil && !os.IsNotExist(err) {
			return LoginResult{}, fmt.Errorf("reset local sync state: %w", err)
		}
	}
	if err := writeJSONAtomic(filepath.Join(localDir, cloudConfigName), localConfig, 0o600); err != nil {
		return LoginResult{}, fmt.Errorf("save local cloud configuration: %w", err)
	}
	// Persist the active key under the v2 profile path. Keep root cloud.key only
	// as a short-lived legacy handoff for migration; remove it once the profile
	// key is in place so Load/Export never disagree with finishLogin.
	keyPath := filepath.Join(localDir, cloudKeyName)
	if keyMode == keyModeKeychain {
		if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
			return LoginResult{}, fmt.Errorf("remove migrated local encryption key: %w", err)
		}
	} else {
		if err := ensureLocalCloudDirectory(profileDirectory(localDir, profile.ID)); err != nil {
			return LoginResult{}, err
		}
		if err := writeAtomic(profileKeyPath(localDir, profile.ID), key, 0o600); err != nil {
			return LoginResult{}, fmt.Errorf("save profile encryption key: %w", err)
		}
		// Root cloud.key remains only for first-load migration of bare v1 state.
		// When a registry already exists, drop the root copy to avoid dual-key drift.
		if _, regErr := os.Stat(registryPath(localDir)); regErr == nil {
			_ = os.Remove(keyPath)
		} else if err := writeAtomic(keyPath, key, 0o600); err != nil {
			return LoginResult{}, fmt.Errorf("save local encryption key: %w", err)
		}
	}
	return LoginResult{
		RemoteDir: remoteDir, DeviceID: deviceID, Existing: existing,
		Alias: defaultRemoteAlias(provider), Provider: provider,
		KeyMode: keyMode, Migrated: migrated,
	}, nil
}

// LoginGoogleDrive starts the native browser OAuth flow, downloads the
// application-private sync bundle, then opens or creates its encrypted profile.
// The OAuth client identity is built into ccl; callers never supply a JSON file.
func LoginGoogleDrive(
	ctx context.Context,
	usePassphrase bool,
	passphrase string,
	notice io.Writer,
) (LoginResult, error) {
	remote, err := authorizeGoogleDriveWithNotice(ctx, notice)
	if err != nil {
		return LoginResult{}, err
	}
	cacheDir, err := googleCacheDirectory()
	if err != nil {
		return LoginResult{}, err
	}
	if _, err := remote.downloadBundle(cacheDir); err != nil {
		return LoginResult{}, err
	}
	const label = "Google Drive appDataFolder"
	var result LoginResult
	if usePassphrase {
		result, err = loginPassphraseAtProvider(
			cacheDir, passphrase, providerGoogleDrive, label,
		)
	} else {
		result, err = loginLocalKeyAtProvider(
			cacheDir, providerGoogleDrive, label,
		)
	}
	if err != nil {
		return LoginResult{}, err
	}
	if err := remote.uploadBundle(cacheDir); err != nil {
		return LoginResult{}, err
	}
	return result, nil
}

func (m *Manager) verifyOrCreateProfileKey(profileID string, create bool) error {
	if !create {
		err := m.verifyProfileKey(profileID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errRemoteVerifierMissing) {
			return err
		}
		create = true
	}
	if !create {
		return nil
	}
	path := filepath.Join(m.remoteDir, verifierFileName)
	expected := []byte("ccl-sync-profile:" + profileID)
	encrypted, err := sealCompressed(m.key, expected)
	if err != nil {
		return err
	}
	return writeAtomic(path, encrypted, 0o600)
}

func (m *Manager) verifyProfileKey(profileID string) error {
	path := filepath.Join(m.remoteDir, verifierFileName)
	expected := []byte("ccl-sync-profile:" + profileID)
	info, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// Profiles created before verifier.ccl can authenticate through their
		// encrypted index without mutating the remote during Load.
		if _, statErr := os.Lstat(filepath.Join(m.remoteDir, indexFileName)); statErr == nil {
			if _, loadErr := m.loadRemoteIndex(); loadErr != nil {
				return loadErr
			}
			return nil
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		return errRemoteVerifierMissing
	}
	if info.Size() > maxEncryptedSize {
		return fmt.Errorf("encrypted cloud verifier exceeds safety limit")
	}
	encrypted, err := readRegularFile(path, maxEncryptedSize)
	if err != nil {
		return err
	}
	plain, err := openCompressed(m.key, encrypted)
	if err != nil {
		return err
	}
	if string(plain) != string(expected) {
		return fmt.Errorf("cloud sync verifier does not match the selected profile")
	}
	return nil
}

func (m *Manager) ensureRemotePairingPublicKey() (bool, error) {
	path := filepath.Join(m.remoteDir, profileFileName)
	var profile remoteProfile
	if err := readJSONFile(path, &profile); err != nil {
		return false, err
	}
	if err := validateRemoteProfile(profile); err != nil {
		return false, err
	}
	if profile.ID != m.profileID {
		return false, fmt.Errorf("cloud profile does not match the active profile")
	}
	changed, err := ensureProfilePairingPublicKey(&profile, m.key)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := writeJSONAtomic(path, profile, 0o600); err != nil {
		return false, err
	}
	if m.profileStateFile != "" {
		if err := writeJSONAtomic(
			profileMetadataPath(m.localDir, m.profileID), profile, 0o600,
		); err != nil {
			return false, err
		}
	}
	return true, nil
}

func Load() (*Manager, error) {
	return LoadRemote("")
}

// LoadRemote loads a selected v2 cloud remote. An empty alias selects the
// primary remote. A legacy v1 configuration is migrated locally before use.
func LoadRemote(alias string) (*Manager, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return nil, err
	}
	registry, err := loadRegistry(localDir, true)
	if err != nil {
		if errors.Is(err, errRegistryNotConfigured) {
			return nil, fmt.Errorf("not logged in; run `ccl cloud login icloud` or `ccl cloud login google-drive` first")
		}
		return nil, err
	}
	resolvedAlias, remoteID, err := resolveRemote(registry, alias)
	if err != nil {
		return nil, err
	}
	var cfg localRemoteConfigV2
	if err := readJSONFile(remoteConfigPath(localDir, remoteID), &cfg); err != nil {
		return nil, err
	}
	if err := validateRemoteConfig(cfg, remoteID, resolvedAlias, registry.ActiveProfileID); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, fmt.Errorf("cloud remote %q is disabled", resolvedAlias)
	}
	var profileState localProfileStateV2
	if err := readJSONFile(profileStatePath(localDir, registry.ActiveProfileID), &profileState); err != nil {
		return nil, err
	}
	var flushRemote func() error
	if cfg.Provider == providerGoogleDrive {
		remote, remoteErr := loadAuthorizedGoogleDriveAt(
			context.Background(), remoteAuthPath(localDir, remoteID), resolvedAlias,
		)
		if remoteErr != nil {
			return nil, remoteErr
		}
		if _, remoteErr = remote.downloadBundle(cfg.RemoteDir); remoteErr != nil {
			return nil, remoteErr
		}
		flushRemote = func() error {
			return remote.uploadBundle(cfg.RemoteDir)
		}
	}
	keyMode := profileState.KeyMode
	var key []byte
	switch keyMode {
	case keyModeKeychain:
		key, err = platformKeyLoad(registry.ActiveProfileID)
		if err != nil {
			return nil, keychainLoginError(err)
		}
	case keyModeLocal, keyModePassphrase, keyModeRecovery, keyModePairing:
		key, err = loadProfileKeyFile(localDir, registry.ActiveProfileID)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("local encryption key is missing; log in with `--passphrase` or run `ccl cloud key import`")
			}
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported local sync key mode %q", keyMode)
	}
	manager := &Manager{
		localDir: localDir, remoteDir: cfg.RemoteDir,
		remoteLabel: cfg.RemoteLabel, provider: cfg.Provider,
		alias: resolvedAlias, remoteID: remoteID,
		deviceID: registry.Device.ID, profileID: registry.ActiveProfileID,
		keyMode: keyMode, key: append([]byte(nil), key...),
		primary: remoteID == registry.PrimaryRemoteID, mirror: cfg.Mirror,
		profileStateFile: profileStatePath(localDir, registry.ActiveProfileID),
		remoteStateFile:  remoteStatePath(localDir, remoteID),
		flushRemote:      flushRemote,
	}
	if err := manager.verifyProfileKey(manager.profileID); err != nil {
		return nil, fmt.Errorf("verify cloud profile for remote %q: %w", resolvedAlias, err)
	}
	return manager, nil
}

func existingLocalProfileKey(profileID string) ([]byte, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return nil, err
	}
	if registry, registryErr := loadRegistry(localDir, false); registryErr == nil {
		if registry.ActiveProfileID != profileID {
			return nil, os.ErrNotExist
		}
		return loadProfileKeyFile(localDir, profileID)
	} else if !errors.Is(registryErr, errRegistryNotConfigured) {
		return nil, registryErr
	}
	var cfg localCloudConfig
	if err := readJSONFile(filepath.Join(localDir, cloudConfigName), &cfg); err != nil {
		return nil, err
	}
	if cfg.ProfileID != profileID {
		return nil, os.ErrNotExist
	}
	return loadLocalKeyFile(localDir)
}

func keychainLoginError(err error) error {
	switch {
	case errors.Is(err, ErrKeychainItemMissing):
		return fmt.Errorf("this device has no Keychain key for the iCloud sync profile; run `ccl cloud key import` or `ccl cloud login icloud --passphrase` for a passphrase profile")
	case errors.Is(err, ErrKeychainUnavailable):
		return fmt.Errorf("macOS Keychain is unavailable: %w; use `ccl cloud login icloud --passphrase` if Keychain access is not possible", err)
	default:
		return err
	}
}

func ExportRecoveryKey() (KeyExportResult, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return KeyExportResult{}, err
	}
	registry, err := loadRegistry(localDir, true)
	if err != nil {
		return KeyExportResult{}, err
	}
	var profile localProfileStateV2
	if err := readJSONFile(
		profileStatePath(localDir, registry.ActiveProfileID), &profile,
	); err != nil {
		return KeyExportResult{}, err
	}
	var key []byte
	switch profile.KeyMode {
	case keyModeKeychain:
		key, err = platformKeyLoad(registry.ActiveProfileID)
	default:
		key, err = loadProfileKeyFile(localDir, registry.ActiveProfileID)
	}
	if err != nil {
		return KeyExportResult{}, err
	}
	recoveryKey, err := encodeRecoveryKey(registry.ActiveProfileID, key)
	if err != nil {
		return KeyExportResult{}, err
	}
	return KeyExportResult{
		ProfileID:   registry.ActiveProfileID,
		RecoveryKey: recoveryKey, KeyMode: profile.KeyMode,
	}, nil
}

func ImportRecoveryKey(value string) (KeyImportResult, error) {
	return ImportRecoveryKeyForProvider(value, "")
}

// ImportRecoveryKeyForProvider restores a profile after its cloud provider has
// been authorized. An empty provider uses the active cloud configuration, then
// falls back to a saved Google authorization and finally iCloud.
func ImportRecoveryKeyForProvider(value, requestedProvider string) (KeyImportResult, error) {
	profileID, key, err := decodeRecoveryKey(value)
	if err != nil {
		return KeyImportResult{}, err
	}
	provider, err := recoveryProvider(requestedProvider)
	if err != nil {
		return KeyImportResult{}, err
	}
	var (
		remoteDir   string
		remoteLabel string
		flushRemote func() error
	)
	switch provider {
	case providerICloud:
		remoteDir, err = defaultICloudDirectory()
		remoteLabel = remoteDir
	case providerGoogleDrive:
		var remote *googleDriveRemote
		remote, err = loadAuthorizedGoogleDrive(context.Background())
		if err == nil {
			remoteDir, err = googleCacheDirectory()
		}
		if err == nil {
			_, err = remote.downloadBundle(remoteDir)
		}
		if err == nil {
			remoteLabel = "Google Drive appDataFolder"
			flushRemote = func() error { return remote.uploadBundle(remoteDir) }
		}
	}
	if err != nil {
		return KeyImportResult{}, err
	}
	var profile remoteProfile
	if err := readJSONFile(filepath.Join(remoteDir, profileFileName), &profile); err != nil {
		if os.IsNotExist(err) {
			return KeyImportResult{}, fmt.Errorf("no ccl sync profile exists in %s", provider)
		}
		return KeyImportResult{}, err
	}
	if err := validateRemoteProfile(profile); err != nil {
		return KeyImportResult{}, err
	}
	if profile.ID != profileID {
		return KeyImportResult{}, fmt.Errorf("recovery key belongs to a different %s sync profile", provider)
	}
	if _, verifierErr := os.Stat(filepath.Join(remoteDir, verifierFileName)); os.IsNotExist(verifierErr) {
		if _, indexErr := os.Stat(filepath.Join(remoteDir, indexFileName)); os.IsNotExist(indexErr) {
			return KeyImportResult{}, fmt.Errorf("cloud sync profile has no encrypted verifier; log in on the original device before importing its recovery key")
		} else if indexErr != nil {
			return KeyImportResult{}, indexErr
		}
	} else if verifierErr != nil {
		return KeyImportResult{}, verifierErr
	}

	localDir, err := cclDirectory()
	if err != nil {
		return KeyImportResult{}, err
	}
	probe := &Manager{localDir: localDir, remoteDir: remoteDir, profileID: profile.ID, key: key}
	if err := probe.verifyOrCreateProfileKey(profile.ID, false); err != nil {
		return KeyImportResult{}, fmt.Errorf("verify recovery key: %w", err)
	}

	mode := keyModeRecovery
	result, err := finishLoginForProvider(
		remoteDir, profile, key, true, mode, false, provider, remoteLabel,
	)
	if err != nil {
		return KeyImportResult{}, err
	}
	if flushRemote != nil {
		if err := flushRemote(); err != nil {
			return KeyImportResult{}, fmt.Errorf("publish recovered Google Drive sync profile: %w", err)
		}
	}
	return KeyImportResult{
		RemoteDir: result.RemoteDir, DeviceID: result.DeviceID, KeyMode: result.KeyMode,
	}, nil
}

func recoveryProvider(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "auto":
		localDir, err := cclDirectory()
		if err != nil {
			return "", err
		}
		var cfg localCloudConfig
		if readJSONFile(filepath.Join(localDir, cloudConfigName), &cfg) == nil &&
			(cfg.Provider == providerICloud ||
				cfg.Provider == providerGoogleDrive) {
			return cfg.Provider, nil
		}
		if _, err := os.Stat(filepath.Join(localDir, googleAuthName)); err == nil {
			return providerGoogleDrive, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		return providerICloud, nil
	case "icloud":
		return providerICloud, nil
	case "google-drive", "google", "drive":
		return providerGoogleDrive, nil
	default:
		return "", fmt.Errorf("unsupported cloud provider %q; use icloud or google-drive", value)
	}
}

func randomDeviceID() (string, error) {
	return randomHexIdentifier(16)
}

func randomSnapshotID() (string, error) {
	return randomHexIdentifier(32)
}

func randomHexIdentifier(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (m *Manager) loadState() (localSyncState, error) {
	if m.profileStateFile != "" && m.remoteStateFile != "" {
		var profile localProfileStateV2
		if err := readJSONFile(m.profileStateFile, &profile); err != nil {
			return localSyncState{}, err
		}
		var remote localRemoteStateV2
		if err := readJSONFile(m.remoteStateFile, &remote); err != nil {
			if !os.IsNotExist(err) {
				return localSyncState{}, err
			}
			remote = localRemoteStateV2{Version: registryVersion}
		}
		return localSyncState{
			LastRemoteID: remote.LastRemoteID, LastLocalHash: remote.LastLocalHash,
			PendingTag: profile.PendingTag, PendingHash: profile.PendingHash,
			ExplicitTag:   profile.ExplicitTag,
			LastOperation: remote.LastOperation, LastSyncAt: remote.LastSyncAt,
		}, nil
	}
	var state localSyncState
	err := readJSONFile(filepath.Join(m.localDir, cloudStateName), &state)
	if os.IsNotExist(err) {
		return state, nil
	}
	return state, err
}

func (m *Manager) saveState(state localSyncState) error {
	if m.profileStateFile != "" && m.remoteStateFile != "" {
		var profile localProfileStateV2
		if err := readJSONFile(m.profileStateFile, &profile); err != nil {
			return err
		}
		profile.PendingTag = state.PendingTag
		profile.PendingHash = state.PendingHash
		profile.ExplicitTag = state.ExplicitTag
		profile.LastOperation = state.LastOperation
		profile.LastSyncAt = state.LastSyncAt
		if err := writeJSONAtomic(m.profileStateFile, profile, 0o600); err != nil {
			return err
		}
		var remote localRemoteStateV2
		if err := readJSONFile(m.remoteStateFile, &remote); err != nil && !os.IsNotExist(err) {
			return err
		}
		remote.Version = registryVersion
		remote.LastSeenRemoteID = state.LastRemoteID
		remote.LastRemoteID = state.LastRemoteID
		remote.LastLocalHash = state.LastLocalHash
		remote.LastOperation = state.LastOperation
		remote.LastSyncAt = state.LastSyncAt
		switch state.LastOperation {
		case "push":
			remote.LastPushedSnapshotID = state.LastRemoteID
		case "pull":
			remote.LastPulledSnapshotID = state.LastRemoteID
		}
		return writeJSONAtomic(m.remoteStateFile, remote, 0o600)
	}
	return writeJSONAtomic(filepath.Join(m.localDir, cloudStateName), state, 0o600)
}

func emptyRemoteIndex() remoteIndex {
	return remoteIndex{
		Version: formatVersion,
		Tags:    make(map[string]string), Snapshots: make(map[string]snapshotRecord),
	}
}

func (m *Manager) loadRemoteIndex() (remoteIndex, error) {
	path := filepath.Join(m.remoteDir, indexFileName)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyRemoteIndex(), nil
		}
		return remoteIndex{}, err
	}
	if info.Size() > maxEncryptedSize {
		return remoteIndex{}, fmt.Errorf("encrypted cloud index exceeds safety limit")
	}
	encrypted, err := readRegularFile(path, maxEncryptedSize)
	if err != nil {
		return remoteIndex{}, err
	}
	plain, err := openCompressed(m.key, encrypted)
	if err != nil {
		return remoteIndex{}, err
	}
	var index remoteIndex
	if err := json.Unmarshal(plain, &index); err != nil {
		return remoteIndex{}, fmt.Errorf("decode encrypted cloud index: %w", err)
	}
	if index.Version != formatVersion {
		return remoteIndex{}, fmt.Errorf("unsupported cloud index version %d", index.Version)
	}
	if index.Tags == nil {
		index.Tags = make(map[string]string)
	}
	if index.Snapshots == nil {
		index.Snapshots = make(map[string]snapshotRecord)
	}
	return index, nil
}

func (m *Manager) saveRemoteIndex(index remoteIndex) error {
	index.Version = formatVersion
	plain, err := json.Marshal(index)
	if err != nil {
		return err
	}
	encrypted, err := sealCompressed(m.key, plain)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(m.remoteDir, indexFileName), encrypted, 0o600)
}

func normalizeTag(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = defaultTag
	}
	if len(value) > 64 {
		return "", fmt.Errorf("tag is too long")
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("invalid tag %q; use letters, numbers, dot, dash, or underscore", value)
	}
	return value, nil
}

func (m *Manager) Tag(value string) (TagResult, error) {
	tag, err := normalizeTag(value)
	if err != nil {
		return TagResult{}, err
	}
	_, hash, err := collectLocalFiles()
	if err != nil {
		return TagResult{}, err
	}
	state, err := m.loadState()
	if err != nil {
		return TagResult{}, err
	}
	state.PendingTag = tag
	state.PendingHash = hash
	state.ExplicitTag = true
	state.LastOperation = "tag"
	if err := m.saveState(state); err != nil {
		return TagResult{}, err
	}
	return TagResult{Tag: tag, Hash: hash}, nil
}

func (m *Manager) Status() (Status, error) {
	_, localHash, localErr := collectLocalFiles()
	if localErr != nil {
		if !errors.Is(localErr, ErrNoLocalData) {
			return Status{}, localErr
		}
		localHash = ""
	}
	state, err := m.loadState()
	if err != nil {
		return Status{}, err
	}
	index, err := m.loadRemoteIndex()
	if err != nil {
		return Status{}, err
	}
	remoteID := index.Tags[defaultTag]
	record := index.Snapshots[remoteID]
	status := Status{
		Alias: m.alias, Primary: m.primary, Mirror: m.mirror,
		Provider: m.provider, KeyMode: m.keyMode,
		RemoteDir: m.remoteDisplay(), DeviceID: m.deviceID,
		LocalHash: localHash, PendingTag: state.PendingTag,
		RemoteTag: defaultTag, RemoteID: remoteID, RemoteHash: record.Hash,
		RemoteDeviceID: record.DeviceID, RemoteCreated: record.CreatedAt,
		LastOperation: state.LastOperation, LastSyncAt: state.LastSyncAt,
	}
	switch {
	case remoteID == "" && localHash == "":
		status.State = "empty"
	case remoteID == "":
		status.State = "local only"
	case localHash == record.Hash:
		status.State = "up to date"
	case state.LastRemoteID == "" || state.LastLocalHash == "":
		status.State = "not synchronized"
	case remoteID != state.LastRemoteID && localHash != state.LastLocalHash:
		status.State = "diverged"
	case remoteID != state.LastRemoteID:
		status.State = "remote changes available"
	default:
		status.State = "local changes pending"
	}
	return status, nil
}

func (m *Manager) remoteDisplay() string {
	if strings.TrimSpace(m.remoteLabel) != "" {
		return m.remoteLabel
	}
	return m.remoteDir
}

func nowUTC() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}
