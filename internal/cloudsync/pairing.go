package cloudsync

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func pendingPairingDirectory(localDir string) string {
	return filepath.Join(cloudRoot(localDir), pairingDirName)
}

func pendingRemotePath(localDir, alias string) string {
	return filepath.Join(pendingPairingDirectory(localDir), "remote-"+alias+".json")
}

func pendingRequestPath(localDir, requestID string) string {
	return filepath.Join(pendingPairingDirectory(localDir), requestID+".json")
}

func savePendingRemoteConnection(
	localDir, alias, provider, remoteDir, remoteLabel, authPath string,
) (pendingRemoteConnection, error) {
	normalized, err := normalizeRemoteAlias(alias)
	if err != nil {
		return pendingRemoteConnection{}, err
	}
	if !filepath.IsAbs(remoteDir) || (authPath != "" && !filepath.IsAbs(authPath)) {
		return pendingRemoteConnection{}, fmt.Errorf("invalid pending cloud remote path")
	}
	remoteID, err := randomHexIdentifier(16)
	if err != nil {
		return pendingRemoteConnection{}, err
	}
	if existing, err := loadPendingRemoteConnection(localDir, normalized); err == nil {
		remoteID = existing.RemoteID
	} else if !os.IsNotExist(err) {
		return pendingRemoteConnection{}, err
	}
	connection := pendingRemoteConnection{
		Version: registryVersion, Alias: normalized, Provider: provider,
		RemoteID: remoteID, RemoteDir: remoteDir,
		RemoteLabel: remoteLabel, AuthPath: authPath,
	}
	if err := writeJSONAtomic(pendingRemotePath(localDir, normalized), connection, 0o600); err != nil {
		return pendingRemoteConnection{}, err
	}
	return connection, nil
}

func loadPendingRemoteConnection(localDir, alias string) (pendingRemoteConnection, error) {
	normalized, err := normalizeRemoteAlias(alias)
	if err != nil {
		return pendingRemoteConnection{}, err
	}
	var connection pendingRemoteConnection
	if err := readJSONFile(pendingRemotePath(localDir, normalized), &connection); err != nil {
		return pendingRemoteConnection{}, err
	}
	if connection.Version != registryVersion || connection.Alias != normalized ||
		!validIdentifier(connection.RemoteID, 32) ||
		!filepath.IsAbs(connection.RemoteDir) ||
		(connection.AuthPath != "" && !filepath.IsAbs(connection.AuthPath)) ||
		(connection.Provider != providerICloud && connection.Provider != providerGoogleDrive) {
		return pendingRemoteConnection{}, fmt.Errorf("invalid pending cloud remote connection")
	}
	return connection, nil
}

func rememberFailedPairingLogin(
	alias, provider, remoteDir, remoteLabel, authPath string,
	loginErr error,
) error {
	localDir, err := cclDirectory()
	if err != nil {
		return loginErr
	}
	var profile remoteProfile
	if readErr := readJSONFile(filepath.Join(remoteDir, profileFileName), &profile); readErr != nil ||
		validateRemoteProfile(profile) != nil || profile.PairingPublicKey == "" {
		return loginErr
	}
	if _, saveErr := savePendingRemoteConnection(
		localDir, alias, provider, remoteDir, remoteLabel, authPath,
	); saveErr != nil {
		return fmt.Errorf("%v; also failed to save pending device pairing: %w", loginErr, saveErr)
	}
	return fmt.Errorf(
		"%v; this profile supports device pairing, run `ccl cloud device request --via %s`",
		loginErr, alias,
	)
}

func StartPairing(
	ctx context.Context,
	via, deviceName string,
) (PairingRequestResult, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return PairingRequestResult{}, err
	}
	if _, err := loadRegistry(localDir, false); err == nil {
		return PairingRequestResult{}, fmt.Errorf("this device already has an active cloud profile")
	} else if !errors.Is(err, errRegistryNotConfigured) {
		return PairingRequestResult{}, err
	}
	alias, err := normalizeRemoteAlias(via)
	if err != nil {
		return PairingRequestResult{}, err
	}
	connection, err := loadPendingRemoteConnection(localDir, alias)
	if err != nil {
		if !os.IsNotExist(err) {
			return PairingRequestResult{}, err
		}
		connection, err = discoverPendingRemote(localDir, alias)
		if err != nil {
			return PairingRequestResult{}, err
		}
	}
	store, err := pairingStoreForPending(ctx, connection, true)
	if err != nil {
		return PairingRequestResult{}, err
	}
	var profile remoteProfile
	if err := readJSONFile(filepath.Join(connection.RemoteDir, profileFileName), &profile); err != nil {
		return PairingRequestResult{}, err
	}
	if err := validateRemoteProfile(profile); err != nil {
		return PairingRequestResult{}, err
	}
	deviceID, err := randomDeviceID()
	if err != nil {
		return PairingRequestResult{}, err
	}
	if strings.TrimSpace(deviceName) == "" {
		deviceName = defaultDeviceName()
	}
	request, privateKey, err := newPairingRequest(profile, deviceID, deviceName)
	if err != nil {
		return PairingRequestResult{}, err
	}
	if err := store.PutRequest(ctx, request); err != nil {
		return PairingRequestResult{}, err
	}
	pending := pendingPairing{
		Version: pairingProtocolVersion, Alias: alias,
		Provider: connection.Provider, RemoteID: connection.RemoteID,
		RemoteDir: connection.RemoteDir, RemoteLabel: connection.RemoteLabel,
		AuthPath: connection.AuthPath, Profile: profile,
		DeviceID: deviceID, DeviceName: strings.TrimSpace(deviceName),
		EphemeralPrivateKey: base64.RawStdEncoding.EncodeToString(privateKey),
		Request:             request,
	}
	if err := writeJSONAtomic(
		pendingRequestPath(localDir, request.RequestID), pending, 0o600,
	); err != nil {
		_ = store.Delete(ctx, request.RequestID)
		return PairingRequestResult{}, err
	}
	return PairingRequestResult{
		Code: pairingCode(request), RequestID: request.RequestID,
		Alias: alias, ExpiresAt: request.ExpiresAt,
	}, nil
}

func discoverPendingRemote(localDir, alias string) (pendingRemoteConnection, error) {
	switch alias {
	case providerGoogleDrive:
		authPath := filepath.Join(localDir, googleAuthName)
		if _, err := loadGoogleToken(authPath); err != nil {
			return pendingRemoteConnection{}, fmt.Errorf(
				"Google Drive is not authorized; run `ccl cloud login google-drive %s` first",
				alias,
			)
		}
		cacheDir, err := googleCacheDirectory()
		if err != nil {
			return pendingRemoteConnection{}, err
		}
		return savePendingRemoteConnection(
			localDir, alias, providerGoogleDrive, cacheDir,
			"Google Drive appDataFolder", authPath,
		)
	case providerICloud:
		remoteDir, err := defaultICloudDirectory()
		if err != nil {
			return pendingRemoteConnection{}, err
		}
		return savePendingRemoteConnection(
			localDir, alias, providerICloud, remoteDir, remoteDir, "",
		)
	default:
		return pendingRemoteConnection{}, fmt.Errorf(
			"no pending cloud login exists for %q; run `ccl cloud login <provider> %s` first",
			alias, alias,
		)
	}
}

func pairingStoreForPending(
	ctx context.Context,
	connection pendingRemoteConnection,
	refresh bool,
) (pairingStore, error) {
	switch connection.Provider {
	case providerICloud:
		return newFilePairingStore(connection.RemoteDir)
	case providerGoogleDrive:
		remote, err := loadAuthorizedGoogleDriveAt(ctx, connection.AuthPath, connection.Alias)
		if err != nil {
			return nil, err
		}
		if refresh {
			if _, err := remote.downloadBundle(connection.RemoteDir); err != nil {
				return nil, err
			}
		}
		return &googlePairingStore{remote: remote}, nil
	default:
		return nil, fmt.Errorf("unsupported pairing provider %q", connection.Provider)
	}
}

func ListPairingRequests(
	ctx context.Context,
	via string,
	all bool,
) ([]PairingRequestInfo, error) {
	managers, err := pairingManagers(via, all)
	if err != nil {
		return nil, err
	}
	var result []PairingRequestInfo
	seenCodes := make(map[string]bool)
	for _, manager := range managers {
		store, err := pairingStoreForManager(ctx, manager)
		if err != nil {
			return nil, err
		}
		requests, err := store.ListRequests(ctx)
		if err != nil {
			return nil, err
		}
		for _, request := range requests {
			if nowUTC().After(request.ExpiresAt) {
				_ = store.Delete(ctx, request.RequestID)
				continue
			}
			if request.ProfileID != manager.profileID {
				continue
			}
			payload, err := openPairingRequest(manager.key, request)
			if err != nil {
				return nil, fmt.Errorf("open pairing request from %s: %w", manager.alias, err)
			}
			code := pairingCode(request)
			if seenCodes[code] {
				return nil, fmt.Errorf("duplicate pairing approval code; deny both requests and try again")
			}
			seenCodes[code] = true
			result = append(result, PairingRequestInfo{
				Code: code, RequestID: request.RequestID, Alias: manager.alias,
				DeviceID: payload.DeviceID, DeviceName: payload.DeviceName,
				CreatedAt: request.CreatedAt, ExpiresAt: request.ExpiresAt,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

func ApprovePairing(
	ctx context.Context,
	code, via string,
) (PairingApproveResult, error) {
	manager, store, request, payload, err := findPairingRequest(ctx, code, via)
	if err != nil {
		return PairingApproveResult{}, err
	}
	response, err := newPairingResponse(manager.key, request, payload)
	if err != nil {
		return PairingApproveResult{}, err
	}
	if err := store.PutResponse(ctx, response); err != nil {
		return PairingApproveResult{}, err
	}
	return PairingApproveResult{
		Code: pairingCode(request), RequestID: request.RequestID,
		DeviceID: payload.DeviceID, DeviceName: payload.DeviceName,
		ExpiresAt: response.ExpiresAt,
	}, nil
}

func DenyPairing(ctx context.Context, code, via string) error {
	_, store, request, _, err := findPairingRequest(ctx, code, via)
	if err != nil {
		return err
	}
	return store.Delete(ctx, request.RequestID)
}

func findPairingRequest(
	ctx context.Context,
	code, via string,
) (*Manager, pairingStore, pairingRequestEnvelope, pairingRequestPayload, error) {
	normalizedCode := normalizePairingCode(code)
	if normalizedCode == "" {
		return nil, nil, pairingRequestEnvelope{}, pairingRequestPayload{},
			fmt.Errorf("invalid pairing approval code")
	}
	managers, err := pairingManagers(via, strings.TrimSpace(via) == "")
	if err != nil {
		return nil, nil, pairingRequestEnvelope{}, pairingRequestPayload{}, err
	}
	var (
		foundManager *Manager
		foundStore   pairingStore
		foundRequest pairingRequestEnvelope
		foundPayload pairingRequestPayload
	)
	for _, manager := range managers {
		store, err := pairingStoreForManager(ctx, manager)
		if err != nil {
			return nil, nil, pairingRequestEnvelope{}, pairingRequestPayload{}, err
		}
		requests, err := store.ListRequests(ctx)
		if err != nil {
			return nil, nil, pairingRequestEnvelope{}, pairingRequestPayload{}, err
		}
		for _, request := range requests {
			if pairingCode(request) != normalizedCode || request.ProfileID != manager.profileID {
				continue
			}
			payload, err := openPairingRequest(manager.key, request)
			if err != nil {
				return nil, nil, pairingRequestEnvelope{}, pairingRequestPayload{}, err
			}
			if foundManager != nil {
				return nil, nil, pairingRequestEnvelope{}, pairingRequestPayload{},
					fmt.Errorf("pairing code is ambiguous; specify --via")
			}
			foundManager, foundStore = manager, store
			foundRequest, foundPayload = request, payload
		}
	}
	if foundManager == nil {
		return nil, nil, pairingRequestEnvelope{}, pairingRequestPayload{},
			fmt.Errorf("pairing request %s was not found or has expired", normalizedCode)
	}
	return foundManager, foundStore, foundRequest, foundPayload, nil
}

func pairingManagers(via string, all bool) ([]*Manager, error) {
	if strings.TrimSpace(via) != "" {
		manager, err := LoadRemote(via)
		if err != nil {
			return nil, err
		}
		return []*Manager{manager}, nil
	}
	if !all {
		manager, err := Load()
		if err != nil {
			return nil, err
		}
		return []*Manager{manager}, nil
	}
	remotes, err := ListRemotes()
	if err != nil {
		return nil, err
	}
	managers := make([]*Manager, 0, len(remotes))
	for _, remote := range remotes {
		manager, err := LoadRemote(remote.Alias)
		if err != nil {
			return nil, err
		}
		managers = append(managers, manager)
	}
	return managers, nil
}

func CompletePairing(
	ctx context.Context,
	code string,
) (PairingCompleteResult, error) {
	normalizedCode := normalizePairingCode(code)
	if normalizedCode == "" {
		return PairingCompleteResult{}, fmt.Errorf("invalid pairing approval code")
	}
	localDir, err := cclDirectory()
	if err != nil {
		return PairingCompleteResult{}, err
	}
	if _, err := loadRegistry(localDir, false); err == nil {
		return PairingCompleteResult{}, fmt.Errorf("this device already has an active cloud profile")
	} else if !errors.Is(err, errRegistryNotConfigured) {
		return PairingCompleteResult{}, err
	}
	pending, pendingPath, err := findPendingPairing(localDir, normalizedCode)
	if err != nil {
		return PairingCompleteResult{}, err
	}
	connection := pendingRemoteConnection{
		Version: registryVersion, Alias: pending.Alias,
		Provider: pending.Provider, RemoteID: pending.RemoteID,
		RemoteDir: pending.RemoteDir, RemoteLabel: pending.RemoteLabel,
		AuthPath: pending.AuthPath,
	}
	store, err := pairingStoreForPending(ctx, connection, true)
	if err != nil {
		return PairingCompleteResult{}, err
	}
	response, found, err := store.GetResponse(ctx, pending.Request.RequestID)
	if err != nil {
		return PairingCompleteResult{}, err
	}
	if !found {
		return PairingCompleteResult{}, fmt.Errorf("pairing request has not been approved yet")
	}
	privateKey, err := base64.RawStdEncoding.DecodeString(pending.EphemeralPrivateKey)
	if err != nil || len(privateKey) != 32 {
		return PairingCompleteResult{}, fmt.Errorf("invalid local pending pairing key")
	}
	key, err := openPairingResponse(
		privateKey, pending.Profile.PairingPublicKey,
		pending.Request, response, pending.DeviceID,
	)
	if err != nil {
		return PairingCompleteResult{}, err
	}
	var currentProfile remoteProfile
	if err := readJSONFile(
		filepath.Join(connection.RemoteDir, profileFileName), &currentProfile,
	); err != nil {
		return PairingCompleteResult{}, err
	}
	if err := validateRemoteProfile(currentProfile); err != nil {
		return PairingCompleteResult{}, err
	}
	if currentProfile != pending.Profile {
		return PairingCompleteResult{}, fmt.Errorf("cloud profile changed while device pairing was pending")
	}
	pending.Profile = currentProfile
	manager := &Manager{
		localDir: localDir, remoteDir: connection.RemoteDir,
		profileID: pending.Profile.ID, key: key,
	}
	if err := manager.verifyProfileKey(pending.Profile.ID); err != nil {
		return PairingCompleteResult{}, fmt.Errorf("verify paired cloud profile: %w", err)
	}
	if err := activatePairedProfile(localDir, pending, key); err != nil {
		return PairingCompleteResult{}, err
	}
	_ = store.Delete(ctx, pending.Request.RequestID)
	if err := os.Remove(pendingPath); err != nil && !os.IsNotExist(err) {
		return PairingCompleteResult{}, err
	}
	_ = os.Remove(pendingRemotePath(localDir, pending.Alias))
	return PairingCompleteResult{
		Alias: pending.Alias, ProfileID: pending.Profile.ID,
		DeviceID: pending.DeviceID, KeyMode: keyModePairing,
	}, nil
}

func findPendingPairing(
	localDir, code string,
) (pendingPairing, string, error) {
	entries, err := os.ReadDir(pendingPairingDirectory(localDir))
	if err != nil {
		if os.IsNotExist(err) {
			return pendingPairing{}, "", fmt.Errorf("no pending device pairing request")
		}
		return pendingPairing{}, "", err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" ||
			strings.HasPrefix(entry.Name(), "remote-") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validIdentifier(id, 32) {
			continue
		}
		path := filepath.Join(pendingPairingDirectory(localDir), entry.Name())
		var pending pendingPairing
		if err := readLimitedJSONFile(path, &pending); err != nil {
			return pendingPairing{}, "", err
		}
		if pairingCode(pending.Request) == code {
			if pending.Version != pairingProtocolVersion ||
				pending.Request.RequestID != id {
				return pendingPairing{}, "", fmt.Errorf("invalid local pending pairing request")
			}
			return pending, path, nil
		}
	}
	return pendingPairing{}, "", fmt.Errorf("pending pairing request %s was not found", code)
}

func activatePairedProfile(localDir string, pending pendingPairing, key []byte) error {
	remoteDir := pending.RemoteDir
	if pending.Provider == providerGoogleDrive {
		cacheDir := remoteCachePath(localDir, pending.RemoteID)
		bundle, err := createCloudBundle(pending.RemoteDir, filepath.Base(pending.RemoteDir))
		if err != nil {
			return err
		}
		if err := replaceCloudCache(cacheDir, remoteCacheName, bundle); err != nil {
			return err
		}
		if err := copyRegularFileReplace(
			pending.AuthPath, remoteAuthPath(localDir, pending.RemoteID),
		); err != nil {
			return err
		}
		remoteDir = cacheDir
	}
	if err := writeJSONAtomic(
		profileMetadataPath(localDir, pending.Profile.ID), pending.Profile, 0o600,
	); err != nil {
		return err
	}
	if err := writeAtomic(profileKeyPath(localDir, pending.Profile.ID), key, 0o600); err != nil {
		return err
	}
	if err := writeJSONAtomic(
		profileStatePath(localDir, pending.Profile.ID),
		localProfileStateV2{
			Version: registryVersion, ProfileID: pending.Profile.ID,
			KeyMode: keyModePairing,
		},
		0o600,
	); err != nil {
		return err
	}
	remote := localRemoteConfigV2{
		Version: registryVersion, ID: pending.RemoteID, Alias: pending.Alias,
		Provider: pending.Provider, ProfileID: pending.Profile.ID,
		RemoteDir: remoteDir, RemoteLabel: pending.RemoteLabel,
		Enabled: true, Mirror: true,
	}
	if err := writeJSONAtomic(
		remoteConfigPath(localDir, pending.RemoteID), remote, 0o600,
	); err != nil {
		return err
	}
	if err := writeJSONAtomic(
		remoteStatePath(localDir, pending.RemoteID),
		localRemoteStateV2{Version: registryVersion}, 0o600,
	); err != nil {
		return err
	}
	registry := cloudRegistry{
		Version: registryVersion, ActiveProfileID: pending.Profile.ID,
		PrimaryRemoteID: pending.RemoteID,
		Device:          localDevice{ID: pending.DeviceID, Name: pending.DeviceName},
		Aliases:         map[string]string{pending.Alias: pending.RemoteID},
		RemoteOrder:     []string{pending.RemoteID},
	}
	return saveRegistry(localDir, registry)
}

func defaultDeviceName() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "CCL device"
	}
	name = strings.TrimSpace(name)
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

func PendingPairingRequests() ([]PairingRequestResult, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(pendingPairingDirectory(localDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var result []PairingRequestResult
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "remote-") ||
			filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validIdentifier(id, 32) {
			continue
		}
		var pending pendingPairing
		if err := readLimitedJSONFile(
			filepath.Join(pendingPairingDirectory(localDir), entry.Name()), &pending,
		); err != nil {
			return nil, err
		}
		result = append(result, PairingRequestResult{
			Code: pairingCode(pending.Request), RequestID: id,
			Alias: pending.Alias, ExpiresAt: pending.Request.ExpiresAt,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ExpiresAt.Before(result[j].ExpiresAt)
	})
	return result, nil
}

func pairingTimeRemaining(expiresAt time.Time) time.Duration {
	remaining := time.Until(expiresAt)
	if remaining < 0 {
		return 0
	}
	return remaining.Round(time.Second)
}
