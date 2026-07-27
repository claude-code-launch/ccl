package cloudsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ListRemotes returns the locally configured cloud remotes in stable order.
// It does not contact any provider.
func ListRemotes() ([]RemoteInfo, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return nil, err
	}
	registry, err := loadRegistry(localDir, true)
	if err != nil {
		if errors.Is(err, errRegistryNotConfigured) {
			return nil, nil
		}
		return nil, err
	}
	infos := make([]RemoteInfo, 0, len(registry.Aliases))
	for _, alias := range sortedRemoteAliases(registry) {
		id := registry.Aliases[alias]
		var remote localRemoteConfigV2
		if err := readJSONFile(remoteConfigPath(localDir, id), &remote); err != nil {
			return nil, err
		}
		signedIn := false
		switch remote.Provider {
		case providerGoogleDrive:
			_, err := loadGoogleToken(remoteAuthPath(localDir, id))
			signedIn = err == nil
		case providerICloud:
			info, err := os.Stat(remote.RemoteDir)
			signedIn = err == nil && info.IsDir()
		}
		infos = append(infos, RemoteInfo{
			ID: id, Alias: alias, Provider: remote.Provider,
			ProfileID: remote.ProfileID, Primary: id == registry.PrimaryRemoteID,
			Mirror: remote.Mirror, Enabled: remote.Enabled, SignedIn: signedIn,
			RemoteLabel: remote.RemoteLabel, AccountHint: remote.AccountHint,
		})
	}
	return infos, nil
}

func HasActiveProfile() (bool, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return false, err
	}
	_, err = loadRegistry(localDir, true)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, errRegistryNotConfigured) {
		return false, nil
	}
	return false, err
}

func SetPrimaryRemote(alias string) error {
	localDir, err := cclDirectory()
	if err != nil {
		return err
	}
	registry, err := loadRegistry(localDir, true)
	if err != nil {
		return err
	}
	_, remoteID, err := resolveRemote(registry, alias)
	if err != nil {
		return err
	}
	registry.PrimaryRemoteID = remoteID
	return saveRegistry(localDir, registry)
}

func RenameRemote(alias, newAlias string) error {
	localDir, err := cclDirectory()
	if err != nil {
		return err
	}
	registry, err := loadRegistry(localDir, true)
	if err != nil {
		return err
	}
	oldAlias, remoteID, err := resolveRemote(registry, alias)
	if err != nil {
		return err
	}
	normalized, err := normalizeRemoteAlias(newAlias)
	if err != nil {
		return err
	}
	if oldAlias == normalized {
		return nil
	}
	if registry.Aliases[normalized] != "" {
		return fmt.Errorf("cloud remote %q already exists", normalized)
	}
	var remote localRemoteConfigV2
	path := remoteConfigPath(localDir, remoteID)
	if err := readJSONFile(path, &remote); err != nil {
		return err
	}
	previous := remote
	remote.Alias = normalized
	if err := writeJSONAtomic(path, remote, 0o600); err != nil {
		return err
	}
	delete(registry.Aliases, oldAlias)
	registry.Aliases[normalized] = remoteID
	if err := saveRegistry(localDir, registry); err != nil {
		_ = writeJSONAtomic(path, previous, 0o600)
		return err
	}
	return nil
}

func SetRemoteMirror(alias string, enabled bool) error {
	localDir, err := cclDirectory()
	if err != nil {
		return err
	}
	registry, err := loadRegistry(localDir, true)
	if err != nil {
		return err
	}
	resolvedAlias, remoteID, err := resolveRemote(registry, alias)
	if err != nil {
		return err
	}
	var remote localRemoteConfigV2
	path := remoteConfigPath(localDir, remoteID)
	if err := readJSONFile(path, &remote); err != nil {
		return err
	}
	if err := validateRemoteConfig(remote, remoteID, resolvedAlias, registry.ActiveProfileID); err != nil {
		return err
	}
	remote.Mirror = enabled
	return writeJSONAtomic(path, remote, 0o600)
}

// LoginGoogleDriveNamed connects a Google account under a local alias. The
// first login reuses the v1 implementation and immediately migrates it to v2;
// later logins attach an independent OAuth token/cache to the active profile.
func LoginGoogleDriveNamed(
	ctx context.Context,
	alias string,
	usePassphrase bool,
	passphrase string,
	notice interface{ Write([]byte) (int, error) },
) (LoginResult, error) {
	normalized, err := normalizeRemoteAlias(alias)
	if err != nil {
		return LoginResult{}, err
	}
	localDir, err := cclDirectory()
	if err != nil {
		return LoginResult{}, err
	}
	registry, err := loadRegistry(localDir, true)
	if errors.Is(err, errRegistryNotConfigured) {
		result, loginErr := LoginGoogleDrive(ctx, usePassphrase, passphrase, notice)
		if loginErr != nil {
			cacheDir, cacheErr := googleCacheDirectory()
			if cacheErr != nil {
				return LoginResult{}, loginErr
			}
			return LoginResult{}, rememberFailedPairingLogin(
				normalized, providerGoogleDrive, cacheDir,
				"Google Drive appDataFolder",
				filepath.Join(localDir, googleAuthName), loginErr,
			)
		}
		registry, loginErr = loadRegistry(localDir, true)
		if loginErr != nil {
			return LoginResult{}, loginErr
		}
		defaultAlias, _, resolveErr := resolveRemote(registry, "")
		if resolveErr != nil {
			return LoginResult{}, resolveErr
		}
		if defaultAlias != normalized {
			if renameErr := RenameRemote(defaultAlias, normalized); renameErr != nil {
				return LoginResult{}, renameErr
			}
		}
		result.Alias = normalized
		result.Provider = providerGoogleDrive
		return result, nil
	}
	if err != nil {
		return LoginResult{}, err
	}
	if id := registry.Aliases[normalized]; id != "" {
		var existing localRemoteConfigV2
		if err := readJSONFile(remoteConfigPath(localDir, id), &existing); err != nil {
			return LoginResult{}, err
		}
		if existing.Provider != providerGoogleDrive {
			return LoginResult{}, fmt.Errorf("cloud remote %q already uses provider %s", normalized, existing.Provider)
		}
		manager, err := LoadRemote(normalized)
		if err != nil {
			return LoginResult{}, err
		}
		if err := manager.verifyOrCreateProfileKey(manager.profileID, false); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{
			RemoteDir: manager.remoteDisplay(), DeviceID: manager.deviceID,
			Alias: normalized, Provider: providerGoogleDrive,
			Existing: true, KeyMode: manager.keyMode,
		}, nil
	}
	if registry.PrimaryRemoteID != "" {
		if _, err := Load(); err != nil {
			return LoginResult{}, fmt.Errorf("verify active cloud profile before adding a remote: %w", err)
		}
	}

	remoteID, err := randomHexIdentifier(16)
	if err != nil {
		return LoginResult{}, err
	}
	localRemoteDir := remoteDirectoryPath(localDir, remoteID)
	if err := os.MkdirAll(localRemoteDir, 0o700); err != nil {
		return LoginResult{}, err
	}
	activated := false
	defer func() {
		if !activated {
			_ = os.RemoveAll(localRemoteDir)
		}
	}()
	authPath := remoteAuthPath(localDir, remoteID)
	google, err := authorizeGoogleDriveAt(ctx, authPath, notice)
	if err != nil {
		return LoginResult{}, err
	}
	cacheDir := remoteCachePath(localDir, remoteID)
	if _, err := google.downloadBundle(cacheDir); err != nil {
		return LoginResult{}, err
	}
	result, err := attachRemote(
		localDir, registry, normalized, remoteID, providerGoogleDrive,
		cacheDir, "Google Drive appDataFolder",
		func() error { return google.uploadBundle(cacheDir) },
	)
	if err != nil {
		return LoginResult{}, err
	}
	activated = true
	return result, nil
}

func LoginICloudNamed(alias string, usePassphrase bool, passphrase string) (LoginResult, error) {
	normalized, err := normalizeRemoteAlias(alias)
	if err != nil {
		return LoginResult{}, err
	}
	localDir, err := cclDirectory()
	if err != nil {
		return LoginResult{}, err
	}
	registry, err := loadRegistry(localDir, true)
	if errors.Is(err, errRegistryNotConfigured) {
		var result LoginResult
		if usePassphrase {
			result, err = LoginICloudWithPassphrase(passphrase)
		} else {
			result, err = LoginICloudLocalKey()
		}
		if err != nil {
			remoteDir, remoteErr := defaultICloudDirectory()
			if remoteErr != nil {
				return LoginResult{}, err
			}
			return LoginResult{}, rememberFailedPairingLogin(
				normalized, providerICloud, remoteDir, remoteDir, "", err,
			)
		}
		registry, err = loadRegistry(localDir, true)
		if err != nil {
			return LoginResult{}, err
		}
		defaultAlias, _, resolveErr := resolveRemote(registry, "")
		if resolveErr != nil {
			return LoginResult{}, resolveErr
		}
		if defaultAlias != normalized {
			if err := RenameRemote(defaultAlias, normalized); err != nil {
				return LoginResult{}, err
			}
		}
		result.Alias = normalized
		result.Provider = providerICloud
		return result, nil
	}
	if err != nil {
		return LoginResult{}, err
	}
	if id := registry.Aliases[normalized]; id != "" {
		var existing localRemoteConfigV2
		if err := readJSONFile(remoteConfigPath(localDir, id), &existing); err != nil {
			return LoginResult{}, err
		}
		if existing.Provider != providerICloud {
			return LoginResult{}, fmt.Errorf("cloud remote %q already uses provider %s", normalized, existing.Provider)
		}
		manager, err := LoadRemote(normalized)
		if err != nil {
			return LoginResult{}, err
		}
		if err := manager.verifyOrCreateProfileKey(manager.profileID, false); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{
			RemoteDir: manager.remoteDisplay(), DeviceID: manager.deviceID,
			Alias: normalized, Provider: providerICloud,
			Existing: true, KeyMode: manager.keyMode,
		}, nil
	}
	if registry.PrimaryRemoteID != "" {
		if _, err := Load(); err != nil {
			return LoginResult{}, fmt.Errorf("verify active cloud profile before adding a remote: %w", err)
		}
	}
	remoteDir, err := defaultICloudDirectory()
	if err != nil {
		return LoginResult{}, err
	}
	for existingAlias, id := range registry.Aliases {
		var existing localRemoteConfigV2
		if readJSONFile(remoteConfigPath(localDir, id), &existing) == nil &&
			existing.Provider == providerICloud && filepath.Clean(existing.RemoteDir) == filepath.Clean(remoteDir) {
			return LoginResult{}, fmt.Errorf("iCloud directory is already connected as %q", existingAlias)
		}
	}
	remoteID, err := randomHexIdentifier(16)
	if err != nil {
		return LoginResult{}, err
	}
	return attachRemote(
		localDir, registry, normalized, remoteID, providerICloud,
		remoteDir, remoteDir, nil,
	)
}

func attachRemote(
	localDir string,
	registry cloudRegistry,
	alias, remoteID, provider, remoteDir, remoteLabel string,
	flush func() error,
) (LoginResult, error) {
	var profileState localProfileStateV2
	if err := readJSONFile(profileStatePath(localDir, registry.ActiveProfileID), &profileState); err != nil {
		return LoginResult{}, err
	}
	var metadata remoteProfile
	if err := readJSONFile(profileMetadataPath(localDir, registry.ActiveProfileID), &metadata); err != nil {
		return LoginResult{}, err
	}
	var key []byte
	var err error
	switch profileState.KeyMode {
	case keyModeKeychain:
		key, err = platformKeyLoad(registry.ActiveProfileID)
	default:
		key, err = loadProfileKeyFile(localDir, registry.ActiveProfileID)
	}
	if err != nil {
		return LoginResult{}, err
	}
	if changed, err := ensureProfilePairingPublicKey(&metadata, key); err != nil {
		return LoginResult{}, err
	} else if changed {
		if err := writeJSONAtomic(
			profileMetadataPath(localDir, registry.ActiveProfileID), metadata, 0o600,
		); err != nil {
			return LoginResult{}, err
		}
	}
	if err := os.MkdirAll(filepath.Join(remoteDir, snapshotsDirectory), 0o700); err != nil {
		return LoginResult{}, err
	}
	var remoteProfileValue remoteProfile
	existing := true
	remoteProfileChanged := false
	if err := readJSONFile(filepath.Join(remoteDir, profileFileName), &remoteProfileValue); err != nil {
		if !os.IsNotExist(err) {
			return LoginResult{}, err
		}
		existing = false
		remoteProfileValue = metadata
		if err := writeJSONAtomic(filepath.Join(remoteDir, profileFileName), metadata, 0o600); err != nil {
			return LoginResult{}, err
		}
	} else {
		if err := validateRemoteProfile(remoteProfileValue); err != nil {
			return LoginResult{}, err
		}
		if remoteProfileValue.ID != registry.ActiveProfileID {
			return LoginResult{}, fmt.Errorf(
				"cloud remote %q belongs to profile %s, but the active profile is %s",
				alias, shortIdentifier(remoteProfileValue.ID), shortIdentifier(registry.ActiveProfileID),
			)
		}
		expectedPublicKey := metadata.PairingPublicKey
		switch {
		case remoteProfileValue.PairingPublicKey == "":
			remoteProfileValue.PairingPublicKey = expectedPublicKey
			remoteProfileChanged = true
		case remoteProfileValue.PairingPublicKey != expectedPublicKey:
			return LoginResult{}, fmt.Errorf("cloud remote %q has a different pairing public key", alias)
		}
	}
	manager := &Manager{
		localDir: localDir, remoteDir: remoteDir, remoteLabel: remoteLabel,
		alias: alias, remoteID: remoteID, provider: provider,
		deviceID: registry.Device.ID, profileID: registry.ActiveProfileID,
		keyMode: profileState.KeyMode, key: append([]byte(nil), key...),
		mirror: true, profileStateFile: profileStatePath(localDir, registry.ActiveProfileID),
		remoteStateFile: remoteStatePath(localDir, remoteID), flushRemote: flush,
	}
	if err := manager.verifyOrCreateProfileKey(registry.ActiveProfileID, !existing); err != nil {
		return LoginResult{}, fmt.Errorf("verify cloud login: %w", err)
	}
	if remoteProfileChanged {
		if err := writeJSONAtomic(
			filepath.Join(remoteDir, profileFileName), remoteProfileValue, 0o600,
		); err != nil {
			return LoginResult{}, err
		}
	}
	if flush != nil {
		if err := flush(); err != nil {
			return LoginResult{}, fmt.Errorf("publish encrypted cloud profile: %w", err)
		}
	}
	remoteConfig := localRemoteConfigV2{
		Version: registryVersion, ID: remoteID, Alias: alias,
		Provider: provider, ProfileID: registry.ActiveProfileID,
		RemoteDir: remoteDir, RemoteLabel: remoteLabel,
		Enabled: true, Mirror: true,
	}
	if err := writeJSONAtomic(remoteConfigPath(localDir, remoteID), remoteConfig, 0o600); err != nil {
		return LoginResult{}, err
	}
	if err := writeJSONAtomic(remoteStatePath(localDir, remoteID), localRemoteStateV2{
		Version: registryVersion,
	}, 0o600); err != nil {
		return LoginResult{}, err
	}
	registry.Aliases[alias] = remoteID
	registry.RemoteOrder = append(registry.RemoteOrder, remoteID)
	if registry.PrimaryRemoteID == "" {
		registry.PrimaryRemoteID = remoteID
		manager.primary = true
	}
	if err := saveRegistry(localDir, registry); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{
		RemoteDir: remoteLabel, DeviceID: registry.Device.ID,
		Alias: alias, Provider: provider, Existing: existing,
		KeyMode: profileState.KeyMode,
	}, nil
}

func shortIdentifier(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func LogoutRemote(ctx context.Context, alias string, options LogoutOptions) (LogoutResult, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return LogoutResult{}, err
	}
	registry, err := loadRegistry(localDir, true)
	if err != nil {
		return LogoutResult{}, err
	}
	resolvedAlias, remoteID, err := resolveRemote(registry, alias)
	if err != nil {
		return LogoutResult{}, err
	}
	var remote localRemoteConfigV2
	if err := readJSONFile(remoteConfigPath(localDir, remoteID), &remote); err != nil {
		return LogoutResult{}, err
	}
	result := LogoutResult{Alias: resolvedAlias, Provider: remote.Provider, LocalOnly: true}
	if options.DeleteRemote {
		manager, loadErr := LoadRemote(resolvedAlias)
		if loadErr != nil {
			return LogoutResult{}, fmt.Errorf("verify remote before deletion: %w", loadErr)
		}
		switch remote.Provider {
		case providerGoogleDrive:
			google, loadErr := loadAuthorizedGoogleDriveAt(ctx, remoteAuthPath(localDir, remoteID), resolvedAlias)
			if loadErr != nil {
				return LogoutResult{}, loadErr
			}
			if err := google.deleteAllApplicationData(ctx); err != nil {
				return LogoutResult{}, err
			}
		case providerICloud:
			if manager.remoteDir != remote.RemoteDir {
				return LogoutResult{}, fmt.Errorf("iCloud remote path changed during deletion")
			}
			if err := deleteICloudRemoteData(remote.RemoteDir, remote.ProfileID); err != nil {
				return LogoutResult{}, err
			}
		}
		result.RemoteDeleted = true
		result.LocalOnly = false
	}
	if options.Revoke && remote.Provider == providerGoogleDrive {
		if err := revokeGoogleAuthorization(ctx, remoteAuthPath(localDir, remoteID)); err != nil {
			if !options.ForceLocal {
				return LogoutResult{}, err
			}
		} else {
			result.TokenRevoked = true
		}
	}

	delete(registry.Aliases, resolvedAlias)
	filtered := registry.RemoteOrder[:0]
	for _, id := range registry.RemoteOrder {
		if id != remoteID {
			filtered = append(filtered, id)
		}
	}
	registry.RemoteOrder = filtered
	if registry.PrimaryRemoteID == remoteID {
		registry.PrimaryRemoteID = ""
		if len(registry.RemoteOrder) > 0 {
			registry.PrimaryRemoteID = registry.RemoteOrder[0]
			for name, id := range registry.Aliases {
				if id == registry.PrimaryRemoteID {
					result.NewPrimary = name
					break
				}
			}
		}
	}
	if err := saveRegistry(localDir, registry); err != nil {
		return LogoutResult{}, err
	}
	localRemoteDir := remoteDirectoryPath(localDir, remoteID)
	if filepath.Dir(localRemoteDir) != filepath.Join(cloudRoot(localDir), remotesDirName) ||
		filepath.Base(localRemoteDir) != remoteID {
		return LogoutResult{}, fmt.Errorf("refuse to remove invalid local remote path")
	}
	if err := os.RemoveAll(localRemoteDir); err != nil {
		return LogoutResult{}, fmt.Errorf("remove local cloud remote: %w", err)
	}
	return result, nil
}

func deleteICloudRemoteData(path, profileID string) error {
	if !filepath.IsAbs(path) || filepath.Base(filepath.Clean(path)) != remoteDirectory {
		return fmt.Errorf("refuse to delete invalid iCloud sync directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("refuse to delete non-directory iCloud sync path")
	}
	var profile remoteProfile
	if err := readJSONFile(filepath.Join(path, profileFileName), &profile); err != nil {
		return fmt.Errorf("verify iCloud profile before deletion: %w", err)
	}
	if profile.ID != profileID {
		return fmt.Errorf("refuse to delete iCloud data for a different profile")
	}
	return os.RemoveAll(path)
}

func PrimaryRemoteAlias() (string, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return "", err
	}
	registry, err := loadRegistry(localDir, true)
	if err != nil {
		return "", err
	}
	alias, _, err := resolveRemote(registry, "")
	return alias, err
}

func remoteAliasesForPush(all bool, alias string) ([]string, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return nil, err
	}
	registry, err := loadRegistry(localDir, true)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(alias) != "" {
		resolved, _, err := resolveRemote(registry, alias)
		if err != nil {
			return nil, err
		}
		return []string{resolved}, nil
	}
	if !all {
		resolved, _, err := resolveRemote(registry, "")
		if err != nil {
			return nil, err
		}
		return []string{resolved}, nil
	}
	var result []string
	for _, name := range sortedRemoteAliases(registry) {
		id := registry.Aliases[name]
		var remote localRemoteConfigV2
		if err := readJSONFile(remoteConfigPath(localDir, id), &remote); err != nil {
			return nil, err
		}
		if remote.Enabled && remote.Mirror {
			result = append(result, name)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no mirror-enabled cloud remotes")
	}
	return result, nil
}
