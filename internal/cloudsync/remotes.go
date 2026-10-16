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
	registry, err := loadRegistry(localDir)
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
	_, err = loadRegistry(localDir)
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
	registry, err := loadRegistry(localDir)
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
	registry, err := loadRegistry(localDir)
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
	registry, err := loadRegistry(localDir)
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

// loginExistingRemote reuses the already-configured remote under alias. It
// refuses when the remote belongs to a different provider and refreshes the
// profile key before reporting the remote as existing.
func loginExistingRemote(
	localDir string,
	registry cloudRegistry,
	normalized, provider string,
) (LoginResult, error) {
	var existing localRemoteConfigV2
	if err := readJSONFile(remoteConfigPath(localDir, registry.Aliases[normalized]), &existing); err != nil {
		return LoginResult{}, err
	}
	if existing.Provider != provider {
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
		Alias: normalized, Provider: provider,
		Existing: true, KeyMode: manager.keyMode,
	}, nil
}

// verifyActiveProfileBeforeAttach requires the active profile to load before
// a second remote is attached to it, so a broken profile is surfaced early
// instead of after the provider's own login flow has already run.
func verifyActiveProfileBeforeAttach(registry cloudRegistry) error {
	if registry.PrimaryRemoteID != "" {
		if _, err := Load(); err != nil {
			return fmt.Errorf("verify active cloud profile before adding a remote: %w", err)
		}
	}
	return nil
}

// LoginGoogleDriveNamed connects a Google account under a local alias. On a
// device without a profile it opens (or creates) the one stored in that
// account; otherwise it attaches the account to the active profile.
func LoginGoogleDriveNamed(
	ctx context.Context,
	alias string,
	notice interface{ Write([]byte) (int, error) },
) (LoginResult, error) {
	normalized, localDir, registry, configured, err := prepareLogin(alias)
	if err != nil {
		return LoginResult{}, err
	}
	if configured && registry.Aliases[normalized] != "" {
		return loginExistingRemote(localDir, registry, normalized, providerGoogleDrive)
	}
	if configured {
		if err := verifyActiveProfileBeforeAttach(registry); err != nil {
			return LoginResult{}, err
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
	google, err := authorizeGoogleDriveAt(ctx, remoteAuthPath(localDir, remoteID), notice)
	if err != nil {
		return LoginResult{}, err
	}
	cacheDir := remoteCachePath(localDir, remoteID)
	if _, err := google.downloadBundle(cacheDir); err != nil {
		return LoginResult{}, err
	}
	result, err := connectRemote(
		localDir, registry, configured, normalized, remoteID, providerGoogleDrive,
		cacheDir, googleDriveLabel, func() error { return google.uploadBundle(cacheDir) },
	)
	if err != nil {
		return LoginResult{}, err
	}
	activated = true
	return result, nil
}

const googleDriveLabel = "Google Drive appDataFolder"

func LoginICloudNamed(alias string) (LoginResult, error) {
	normalized, localDir, registry, configured, err := prepareLogin(alias)
	if err != nil {
		return LoginResult{}, err
	}
	if configured && registry.Aliases[normalized] != "" {
		return loginExistingRemote(localDir, registry, normalized, providerICloud)
	}
	remoteDir, err := defaultICloudDirectory()
	if err != nil {
		return LoginResult{}, err
	}
	if configured {
		if err := verifyActiveProfileBeforeAttach(registry); err != nil {
			return LoginResult{}, err
		}
		for existingAlias, id := range registry.Aliases {
			var existing localRemoteConfigV2
			if readJSONFile(remoteConfigPath(localDir, id), &existing) == nil &&
				existing.Provider == providerICloud && filepath.Clean(existing.RemoteDir) == filepath.Clean(remoteDir) {
				return LoginResult{}, fmt.Errorf("iCloud directory is already connected as %q", existingAlias)
			}
		}
	}
	remoteID, err := randomHexIdentifier(16)
	if err != nil {
		return LoginResult{}, err
	}
	return connectRemote(localDir, registry, configured, normalized, remoteID, providerICloud, remoteDir, remoteDir, nil)
}

// prepareLogin normalizes alias and loads the registry; configured is false on
// a device that has no cloud profile yet.
func prepareLogin(alias string) (string, string, cloudRegistry, bool, error) {
	normalized, err := normalizeRemoteAlias(alias)
	if err != nil {
		return "", "", cloudRegistry{}, false, err
	}
	localDir, err := cclDirectory()
	if err != nil {
		return "", "", cloudRegistry{}, false, err
	}
	registry, err := loadRegistry(localDir)
	if errors.Is(err, errRegistryNotConfigured) {
		return normalized, localDir, cloudRegistry{}, false, nil
	}
	return normalized, localDir, registry, err == nil, err
}

func connectRemote(
	localDir string, registry cloudRegistry, configured bool,
	alias, remoteID, provider, remoteDir, remoteLabel string, flush func() error,
) (LoginResult, error) {
	if configured {
		return attachRemote(localDir, registry, alias, remoteID, provider, remoteDir, remoteLabel, flush)
	}
	return loginFirstRemote(localDir, alias, remoteID, provider, remoteDir, remoteLabel, flush)
}

// loginFirstRemote sets this device up from the profile at remoteDir. The key
// stored in the cloud profile opens it, so the cloud account is all a new
// device needs; a remote without a profile gets a new one.
func loginFirstRemote(localDir, alias, remoteID, provider, remoteDir, remoteLabel string, flush func() error) (LoginResult, error) {
	profile, key, existing, err := openRemoteProfile(remoteDir)
	if err != nil {
		return LoginResult{}, err
	}
	deviceID, err := randomDeviceID()
	if err != nil {
		return LoginResult{}, err
	}
	profileDir := profileDirectory(localDir, profile.ID)
	for _, dir := range []string{cloudRoot(localDir), filepath.Dir(profileDir), profileDir} {
		if err := ensureLocalCloudDirectory(dir); err != nil {
			return LoginResult{}, err
		}
	}
	state := localProfileStateV2{Version: registryVersion, ProfileID: profile.ID, KeyMode: keyModeLocal}
	if err := writeJSONAtomic(profileMetadataPath(localDir, profile.ID), profile, 0o600); err != nil {
		return LoginResult{}, err
	}
	if err := writeJSONAtomic(profileStatePath(localDir, profile.ID), state, 0o600); err != nil {
		return LoginResult{}, err
	}
	if err := writeAtomic(profileKeyPath(localDir, profile.ID), key, 0o600); err != nil {
		return LoginResult{}, fmt.Errorf("save profile encryption key: %w", err)
	}
	registry := cloudRegistry{
		Version: registryVersion, ActiveProfileID: profile.ID,
		Device: localDevice{ID: deviceID}, Aliases: map[string]string{},
	}
	result, err := attachRemote(localDir, registry, alias, remoteID, provider, remoteDir, remoteLabel, flush)
	if err != nil {
		_ = os.RemoveAll(profileDir)
		return LoginResult{}, err
	}
	result.Existing = existing
	return result, nil
}

// openRemoteProfile reads the profile at remoteDir and the key stored in it,
// or writes a new profile there when the remote holds none (or only an
// unused one).
func openRemoteProfile(remoteDir string) (remoteProfile, []byte, bool, error) {
	var profile remoteProfile
	err := readJSONFile(filepath.Join(remoteDir, profileFileName), &profile)
	switch {
	case err == nil:
		if err := validateRemoteProfile(profile); err != nil {
			return remoteProfile{}, nil, false, err
		}
		if key, ok := profileAccountKey(profile); ok {
			return profile, key, true, nil
		}
		unused, err := isUninitializedRemoteProfile(remoteDir)
		if err != nil {
			return remoteProfile{}, nil, false, err
		}
		if !unused {
			return remoteProfile{}, nil, false, fmt.Errorf("the cloud sync profile stores no key; delete ccl's cloud data (ccl cloud logout --delete-remote) and log in again")
		}
	case !os.IsNotExist(err):
		return remoteProfile{}, nil, false, err
	}
	profile, key, err := newMasterKeyProfile()
	if err != nil {
		return remoteProfile{}, nil, false, err
	}
	if _, err := ensureProfileAccountKey(&profile, key); err != nil {
		return remoteProfile{}, nil, false, err
	}
	if err := os.MkdirAll(filepath.Join(remoteDir, snapshotsDirectory), 0o700); err != nil {
		return remoteProfile{}, nil, false, err
	}
	if err := writeJSONAtomic(filepath.Join(remoteDir, profileFileName), profile, 0o600); err != nil {
		return remoteProfile{}, nil, false, fmt.Errorf("write cloud sync profile: %w", err)
	}
	return profile, key, false, nil
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
	key, err := loadProfileKeyFile(localDir, registry.ActiveProfileID)
	if err != nil {
		return LoginResult{}, err
	}
	if changed, err := ensureProfileAccountKey(&metadata, key); err != nil {
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
		changed, err := ensureProfileAccountKey(&remoteProfileValue, key)
		if err != nil {
			return LoginResult{}, fmt.Errorf("cloud remote %q: %w", alias, err)
		}
		remoteProfileChanged = changed
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
	registry, err := loadRegistry(localDir)
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
		if err := deleteRemoteData(ctx, manager, localDir, remoteID, resolvedAlias, remote); err != nil {
			return LogoutResult{}, err
		}
		result.RemoteDeleted = true
		result.LocalOnly = false
	}
	if options.Revoke && remote.Provider == providerGoogleDrive {
		revoked, err := revokeRemoteToken(ctx, localDir, remoteID, options.ForceLocal)
		if err != nil {
			return LogoutResult{}, err
		}
		result.TokenRevoked = revoked
	}
	if err := removeRemoteFromRegistry(localDir, registry, remoteID, resolvedAlias, &result); err != nil {
		return LogoutResult{}, err
	}
	if err := removeLocalRemoteDir(localDir, remoteID); err != nil {
		return LogoutResult{}, err
	}
	return result, nil
}

// deleteRemoteData removes all cloud-side data for a remote before its local
// configuration is dropped. The manager is required so the iCloud branch can
// refuse to proceed when the on-disk remote directory no longer matches the
// configured path.
func deleteRemoteData(
	ctx context.Context,
	manager *Manager,
	localDir, remoteID, alias string,
	remote localRemoteConfigV2,
) error {
	switch remote.Provider {
	case providerGoogleDrive:
		google, loadErr := loadAuthorizedGoogleDriveAt(ctx, remoteAuthPath(localDir, remoteID), alias)
		if loadErr != nil {
			return loadErr
		}
		return google.deleteAllApplicationData(ctx)
	case providerICloud:
		if manager.remoteDir != remote.RemoteDir {
			return fmt.Errorf("iCloud remote path changed during deletion")
		}
		return deleteICloudRemoteData(remote.RemoteDir, remote.ProfileID)
	}
	return nil
}

// revokeRemoteToken revokes the stored Google OAuth authorization. When
// forceLocal is set, a revocation failure is tolerated and revoked is false;
// otherwise the error is returned to the caller.
func revokeRemoteToken(ctx context.Context, localDir, remoteID string, forceLocal bool) (bool, error) {
	if err := revokeGoogleAuthorization(ctx, remoteAuthPath(localDir, remoteID)); err != nil {
		if !forceLocal {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// removeRemoteFromRegistry drops the alias and remote ID from the registry,
// reassigns the primary remote if needed, and persists the result. The new
// primary alias (if any) is written back through result.NewPrimary.
func removeRemoteFromRegistry(
	localDir string,
	registry cloudRegistry,
	remoteID, alias string,
	result *LogoutResult,
) error {
	delete(registry.Aliases, alias)
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
	return saveRegistry(localDir, registry)
}

// removeLocalRemoteDir deletes the on-disk remote directory after validating
// that it lives directly under the remotes root with the remote ID as its base
// name, refusing to touch anything that fails that shape check.
func removeLocalRemoteDir(localDir, remoteID string) error {
	localRemoteDir := remoteDirectoryPath(localDir, remoteID)
	if filepath.Dir(localRemoteDir) != filepath.Join(cloudRoot(localDir), remotesDirName) ||
		filepath.Base(localRemoteDir) != remoteID {
		return fmt.Errorf("refuse to remove invalid local remote path")
	}
	if err := os.RemoveAll(localRemoteDir); err != nil {
		return fmt.Errorf("remove local cloud remote: %w", err)
	}
	return nil
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
	registry, err := loadRegistry(localDir)
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
	registry, err := loadRegistry(localDir)
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
