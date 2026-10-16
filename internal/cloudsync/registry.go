package cloudsync

import (
	"errors"
	"fmt"
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

func loadRegistry(localDir string) (cloudRegistry, error) {
	var registry cloudRegistry
	if err := readJSONFile(registryPath(localDir), &registry); err != nil {
		if os.IsNotExist(err) {
			return cloudRegistry{}, errRegistryNotConfigured
		}
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
	case keyModeLocal:
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
