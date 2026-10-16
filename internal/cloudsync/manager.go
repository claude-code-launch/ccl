package cloudsync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// ensureRemoteAccountKey adds the key to a cloud profile created before
// profiles carried it, so the next push lets other devices log in.
func (m *Manager) ensureRemoteAccountKey() (bool, error) {
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
	changed, err := ensureProfileAccountKey(&profile, m.key)
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

// LoadRemote loads a cloud remote. An empty alias selects the primary remote.
func LoadRemote(alias string) (*Manager, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return nil, err
	}
	registry, err := loadRegistry(localDir)
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
	key, err := loadProfileKeyFile(localDir, registry.ActiveProfileID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("local encryption key is missing; run `ccl cloud logout` and `ccl cloud login` again")
		}
		return nil, err
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
		LastRemoteCreatedAt: remote.LastRemoteCreatedAt,
		PendingTag:          profile.PendingTag, PendingHash: profile.PendingHash,
		ExplicitTag:   profile.ExplicitTag,
		LastOperation: remote.LastOperation, LastSyncAt: remote.LastSyncAt,
	}, nil
}

func (m *Manager) saveState(state localSyncState) error {
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
	remote.LastRemoteCreatedAt = state.LastRemoteCreatedAt
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
	case remoteID != state.LastRemoteID && !record.CreatedAt.IsZero() &&
		!state.LastRemoteCreatedAt.IsZero() && record.CreatedAt.Before(state.LastRemoteCreatedAt):
		status.State = "remote rollback suspected"
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
