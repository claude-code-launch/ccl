package cloudsync

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var errRegistryNotConfigured = errors.New("cloud sync registry is not configured")

func cloudRoot(localDir string) string {
	return filepath.Join(localDir, cloudDirectoryName)
}

func registryPath(localDir string) string {
	return filepath.Join(cloudRoot(localDir), registryFileName)
}

func profileDirectory(localDir, profileID string) string {
	return filepath.Join(cloudRoot(localDir), profilesDirName, profileID)
}

func profileMetadataPath(localDir, profileID string) string {
	return filepath.Join(profileDirectory(localDir, profileID), profileFileName)
}

func profileKeyPath(localDir, profileID string) string {
	return filepath.Join(profileDirectory(localDir, profileID), "key")
}

func profileStatePath(localDir, profileID string) string {
	return filepath.Join(profileDirectory(localDir, profileID), profileStateName)
}

func remoteDirectoryPath(localDir, remoteID string) string {
	return filepath.Join(cloudRoot(localDir), remotesDirName, remoteID)
}

func remoteConfigPath(localDir, remoteID string) string {
	return filepath.Join(remoteDirectoryPath(localDir, remoteID), remoteConfigName)
}

func remoteStatePath(localDir, remoteID string) string {
	return filepath.Join(remoteDirectoryPath(localDir, remoteID), remoteStateName)
}

func remoteAuthPath(localDir, remoteID string) string {
	return filepath.Join(remoteDirectoryPath(localDir, remoteID), remoteAuthName)
}

func remoteCachePath(localDir, remoteID string) string {
	return filepath.Join(remoteDirectoryPath(localDir, remoteID), remoteCacheName)
}

func loadRegistry(localDir string, migrate bool) (cloudRegistry, error) {
	var registry cloudRegistry
	err := readJSONFile(registryPath(localDir), &registry)
	if err == nil {
		if validateErr := validateRegistry(localDir, registry); validateErr != nil {
			return cloudRegistry{}, validateErr
		}
		return registry, nil
	}
	if !os.IsNotExist(err) {
		return cloudRegistry{}, err
	}
	if !migrate {
		return cloudRegistry{}, errRegistryNotConfigured
	}
	if _, statErr := os.Stat(filepath.Join(localDir, cloudConfigName)); statErr != nil {
		if os.IsNotExist(statErr) {
			return cloudRegistry{}, errRegistryNotConfigured
		}
		return cloudRegistry{}, statErr
	}
	if err := migrateLegacyRegistry(localDir); err != nil {
		return cloudRegistry{}, err
	}
	if err := readJSONFile(registryPath(localDir), &registry); err != nil {
		return cloudRegistry{}, err
	}
	if err := validateRegistry(localDir, registry); err != nil {
		return cloudRegistry{}, err
	}
	return registry, nil
}

func validateRegistry(localDir string, registry cloudRegistry) error {
	if err := validateLocalCloudDirectory(cloudRoot(localDir)); err != nil {
		return err
	}
	if registry.Version != registryVersion || !validIdentifier(registry.ActiveProfileID, 32) ||
		!validIdentifier(registry.Device.ID, 32) || registry.Aliases == nil {
		return fmt.Errorf("invalid cloud registry; restore %s or run `ccl doctor`", registryPath(localDir))
	}
	seenIDs := make(map[string]bool, len(registry.Aliases))
	if err := validateLocalCloudDirectory(
		profileDirectory(localDir, registry.ActiveProfileID),
	); err != nil {
		return err
	}
	for alias, remoteID := range registry.Aliases {
		normalized, err := normalizeRemoteAlias(alias)
		if err != nil || normalized != alias || !validIdentifier(remoteID, 32) || seenIDs[remoteID] {
			return fmt.Errorf("invalid cloud registry remote %q", alias)
		}
		seenIDs[remoteID] = true
		if err := validateLocalCloudDirectory(remoteDirectoryPath(localDir, remoteID)); err != nil {
			return fmt.Errorf("invalid cloud remote directory for %q: %w", alias, err)
		}
		var remote localRemoteConfigV2
		if err := readJSONFile(remoteConfigPath(localDir, remoteID), &remote); err != nil {
			return fmt.Errorf("load cloud remote %q: %w", alias, err)
		}
		if err := validateRemoteConfig(remote, remoteID, alias, registry.ActiveProfileID); err != nil {
			return err
		}
	}
	if registry.PrimaryRemoteID != "" && !seenIDs[registry.PrimaryRemoteID] {
		return fmt.Errorf("cloud registry primary remote does not exist")
	}
	orderSeen := make(map[string]bool, len(registry.RemoteOrder))
	for _, id := range registry.RemoteOrder {
		if !seenIDs[id] || orderSeen[id] {
			return fmt.Errorf("invalid cloud registry remote order")
		}
		orderSeen[id] = true
	}
	if len(orderSeen) != len(seenIDs) {
		return fmt.Errorf("cloud registry remote order is incomplete")
	}
	var profile localProfileStateV2
	if err := readJSONFile(profileStatePath(localDir, registry.ActiveProfileID), &profile); err != nil {
		return fmt.Errorf("load active cloud profile: %w", err)
	}
	if profile.Version != registryVersion || profile.ProfileID != registry.ActiveProfileID ||
		!validKeyMode(profile.KeyMode) {
		return fmt.Errorf("invalid active cloud profile")
	}
	return nil
}

func validateRemoteConfig(remote localRemoteConfigV2, expectedID, expectedAlias, profileID string) error {
	if remote.Version != registryVersion || remote.ID != expectedID ||
		remote.Alias != expectedAlias || remote.ProfileID != profileID ||
		!validIdentifier(remote.ID, 32) || !filepath.IsAbs(remote.RemoteDir) {
		return fmt.Errorf("invalid cloud remote configuration for %q", expectedAlias)
	}
	if remote.Provider != providerICloud && remote.Provider != providerGoogleDrive {
		return fmt.Errorf("unsupported cloud provider %q", remote.Provider)
	}
	return nil
}

func validKeyMode(mode string) bool {
	switch mode {
	case keyModeKeychain, keyModeLocal, keyModePassphrase, keyModeRecovery, keyModePairing:
		return true
	default:
		return false
	}
}

func validIdentifier(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func normalizeRemoteAlias(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", fmt.Errorf("cloud remote alias is required")
	}
	if len(value) > 48 {
		return "", fmt.Errorf("cloud remote alias is too long")
	}
	switch value {
	case "all", "primary", "latest":
		return "", fmt.Errorf("cloud remote alias %q is reserved", value)
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') ||
			char == '.' || char == '_' || char == '-' {
			continue
		}
		return "", fmt.Errorf("invalid cloud remote alias %q; use letters, numbers, dot, dash, or underscore", value)
	}
	return value, nil
}

func defaultRemoteAlias(provider string) string {
	switch provider {
	case providerGoogleDrive:
		return "google-drive"
	default:
		return providerICloud
	}
}

func deterministicLegacyRemoteID(cfg localCloudConfig) string {
	sum := sha256.Sum256([]byte("ccl-cloud-v1\x00" + cfg.ProfileID + "\x00" + cfg.Provider + "\x00" + cfg.RemoteDir))
	return hex.EncodeToString(sum[:16])
}

func migrateLegacyRegistry(localDir string) error {
	var cfg localCloudConfig
	if err := readJSONFile(filepath.Join(localDir, cloudConfigName), &cfg); err != nil {
		return err
	}
	if cfg.Version != formatVersion || !validIdentifier(cfg.ProfileID, 32) ||
		!validIdentifier(cfg.DeviceID, 32) || !filepath.IsAbs(cfg.RemoteDir) ||
		(cfg.Provider != providerICloud && cfg.Provider != providerGoogleDrive) {
		return fmt.Errorf("cannot migrate invalid legacy cloud configuration")
	}
	keyMode := cfg.KeyMode
	if keyMode == "" {
		keyMode = keyModePassphrase
	}
	if !validKeyMode(keyMode) {
		return fmt.Errorf("cannot migrate unsupported legacy key mode %q", keyMode)
	}

	var metadata remoteProfile
	if err := readJSONFile(filepath.Join(cfg.RemoteDir, profileFileName), &metadata); err != nil {
		return fmt.Errorf("read legacy cloud profile: %w", err)
	}
	if err := validateRemoteProfile(metadata); err != nil {
		return err
	}
	if metadata.ID != cfg.ProfileID {
		return fmt.Errorf("legacy local and remote cloud profile IDs do not match")
	}

	var legacyState localSyncState
	if err := readJSONFile(filepath.Join(localDir, cloudStateName), &legacyState); err != nil &&
		!os.IsNotExist(err) {
		return err
	}
	remoteID := deterministicLegacyRemoteID(cfg)
	alias := defaultRemoteAlias(cfg.Provider)
	newCache := cfg.RemoteDir
	if cfg.Provider == providerGoogleDrive {
		newCache = remoteCachePath(localDir, remoteID)
		bundle, err := createCloudBundle(cfg.RemoteDir, filepath.Base(cfg.RemoteDir))
		if err != nil {
			return fmt.Errorf("copy legacy Google Drive cache: %w", err)
		}
		if err := replaceCloudCache(newCache, remoteCacheName, bundle); err != nil {
			return fmt.Errorf("activate migrated Google Drive cache: %w", err)
		}
		if err := copyRegularFileReplace(
			filepath.Join(localDir, googleAuthName),
			remoteAuthPath(localDir, remoteID),
		); err != nil {
			return fmt.Errorf("copy legacy Google Drive authorization: %w", err)
		}
	}

	if err := writeJSONAtomic(profileMetadataPath(localDir, cfg.ProfileID), metadata, 0o600); err != nil {
		return err
	}
	if keyMode != keyModeKeychain {
		if err := copyRegularFileReplace(
			filepath.Join(localDir, cloudKeyName),
			profileKeyPath(localDir, cfg.ProfileID),
		); err != nil {
			return fmt.Errorf("copy legacy cloud key: %w", err)
		}
	}
	profileState := localProfileStateV2{
		Version: registryVersion, ProfileID: cfg.ProfileID, KeyMode: keyMode,
		PendingTag: legacyState.PendingTag, PendingHash: legacyState.PendingHash,
		ExplicitTag:   legacyState.ExplicitTag,
		LastOperation: legacyState.LastOperation, LastSyncAt: legacyState.LastSyncAt,
	}
	if err := writeJSONAtomic(profileStatePath(localDir, cfg.ProfileID), profileState, 0o600); err != nil {
		return err
	}
	remote := localRemoteConfigV2{
		Version: registryVersion, ID: remoteID, Alias: alias,
		Provider: cfg.Provider, ProfileID: cfg.ProfileID,
		RemoteDir: newCache, RemoteLabel: cfg.RemoteLabel,
		Enabled: true, Mirror: true,
	}
	if err := writeJSONAtomic(remoteConfigPath(localDir, remoteID), remote, 0o600); err != nil {
		return err
	}
	remoteState := localRemoteStateV2{
		Version:          registryVersion,
		LastSeenRemoteID: legacyState.LastRemoteID,
		LastRemoteID:     legacyState.LastRemoteID,
		LastLocalHash:    legacyState.LastLocalHash,
		LastOperation:    legacyState.LastOperation,
		LastSyncAt:       legacyState.LastSyncAt,
	}
	switch legacyState.LastOperation {
	case "push":
		remoteState.LastPushedSnapshotID = legacyState.LastRemoteID
	case "pull":
		remoteState.LastPulledSnapshotID = legacyState.LastRemoteID
	}
	if err := writeJSONAtomic(remoteStatePath(localDir, remoteID), remoteState, 0o600); err != nil {
		return err
	}
	var (
		key    []byte
		keyErr error
	)
	if keyMode == keyModeKeychain {
		key, keyErr = platformKeyLoad(cfg.ProfileID)
		if keyErr != nil {
			return keychainLoginError(keyErr)
		}
	} else {
		key, keyErr = loadProfileKeyFile(localDir, cfg.ProfileID)
		if keyErr != nil {
			return keyErr
		}
	}
	probe := &Manager{
		localDir: localDir, remoteDir: newCache,
		profileID: cfg.ProfileID, key: key,
	}
	if err := probe.verifyProfileKey(cfg.ProfileID); err != nil {
		return fmt.Errorf("verify legacy cloud key before migration: %w", err)
	}

	for _, legacyPath := range []string{
		filepath.Join(localDir, cloudConfigName),
		filepath.Join(localDir, cloudStateName),
		filepath.Join(localDir, cloudKeyName),
		filepath.Join(localDir, googleAuthName),
	} {
		if _, err := os.Stat(legacyPath); err == nil {
			if err := copyRegularFile(legacyPath, legacyPath+".v1.bak", true); err != nil {
				return fmt.Errorf("back up legacy cloud file: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	// Drop the live root cloud.key after a successful profile-key copy so v2
	// has a single authoritative key path. The .v1.bak remains for recovery.
	if keyMode != keyModeKeychain {
		if err := os.Remove(filepath.Join(localDir, cloudKeyName)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove legacy cloud key after migration: %w", err)
		}
	}

	registry := cloudRegistry{
		Version: registryVersion, ActiveProfileID: cfg.ProfileID,
		PrimaryRemoteID:  remoteID,
		Device:           localDevice{ID: cfg.DeviceID},
		Aliases:          map[string]string{alias: remoteID},
		RemoteOrder:      []string{remoteID},
		MigratedFromV1At: nowUTC(),
	}
	if err := writeJSONAtomic(registryPath(localDir), registry, 0o600); err != nil {
		return err
	}
	return nil
}

func copyRegularFile(source, destination string, allowExisting bool) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refuse to copy non-regular file %s", source)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	data, err := io.ReadAll(io.LimitReader(input, maxEncryptedSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxEncryptedSize {
		return fmt.Errorf("file %s exceeds safety limit", source)
	}
	if existing, err := readRegularFile(destination, maxEncryptedSize); err == nil {
		if allowExisting && string(existing) == string(data) {
			return nil
		}
		return fmt.Errorf("destination already exists: %s", destination)
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeAtomic(destination, data, 0o600)
}

func copyRegularFileReplace(source, destination string) error {
	data, err := readRegularFile(source, maxEncryptedSize)
	if err != nil {
		return err
	}
	return writeAtomic(destination, data, 0o600)
}

func saveRegistry(localDir string, registry cloudRegistry) error {
	if err := validateRegistryInMemory(registry); err != nil {
		return err
	}
	if err := ensureLocalCloudDirectory(cloudRoot(localDir)); err != nil {
		return err
	}
	return writeJSONAtomic(registryPath(localDir), registry, 0o600)
}

func ensureLocalCloudDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refuse to use non-directory cloud path %s", path)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return validateLocalCloudDirectory(path)
}

func validateLocalCloudDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to use non-directory cloud path %s", path)
	}
	return nil
}

func validateRegistryInMemory(registry cloudRegistry) error {
	if registry.Version != registryVersion || !validIdentifier(registry.ActiveProfileID, 32) ||
		!validIdentifier(registry.Device.ID, 32) || registry.Aliases == nil {
		return fmt.Errorf("invalid cloud registry")
	}
	seen := make(map[string]bool, len(registry.Aliases))
	for alias, id := range registry.Aliases {
		normalized, err := normalizeRemoteAlias(alias)
		if err != nil || normalized != alias || !validIdentifier(id, 32) || seen[id] {
			return fmt.Errorf("invalid cloud registry remote %q", alias)
		}
		seen[id] = true
	}
	if registry.PrimaryRemoteID != "" && !seen[registry.PrimaryRemoteID] {
		return fmt.Errorf("cloud registry primary remote does not exist")
	}
	if len(registry.RemoteOrder) != len(seen) {
		return fmt.Errorf("cloud registry remote order is incomplete")
	}
	orderSeen := make(map[string]bool, len(registry.RemoteOrder))
	for _, id := range registry.RemoteOrder {
		if !seen[id] || orderSeen[id] {
			return fmt.Errorf("invalid cloud registry remote order")
		}
		orderSeen[id] = true
	}
	return nil
}

func resolveRemote(registry cloudRegistry, alias string) (string, string, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" || strings.EqualFold(alias, "primary") {
		if registry.PrimaryRemoteID == "" {
			return "", "", fmt.Errorf("no primary cloud remote is selected")
		}
		for name, id := range registry.Aliases {
			if id == registry.PrimaryRemoteID {
				return name, id, nil
			}
		}
		return "", "", fmt.Errorf("primary cloud remote is missing")
	}
	normalized, err := normalizeRemoteAlias(alias)
	if err != nil {
		return "", "", err
	}
	id := registry.Aliases[normalized]
	if id == "" {
		return "", "", fmt.Errorf("cloud remote %q does not exist", alias)
	}
	return normalized, id, nil
}

func sortedRemoteAliases(registry cloudRegistry) []string {
	byID := make(map[string]string, len(registry.Aliases))
	for alias, id := range registry.Aliases {
		byID[id] = alias
	}
	aliases := make([]string, 0, len(registry.RemoteOrder))
	for _, id := range registry.RemoteOrder {
		if alias := byID[id]; alias != "" {
			aliases = append(aliases, alias)
		}
	}
	if len(aliases) != len(registry.Aliases) {
		aliases = aliases[:0]
		for alias := range registry.Aliases {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
	}
	return aliases
}
