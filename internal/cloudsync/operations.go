package cloudsync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const pushOperationVersion = 1

func operationsDirectory(localDir string) string {
	return filepath.Join(cloudRoot(localDir), operationsDirName)
}

func operationDirectory(localDir, operationID string) string {
	return filepath.Join(operationsDirectory(localDir), operationID)
}

func operationMetadataPath(localDir, operationID string) string {
	return filepath.Join(operationDirectory(localDir, operationID), "operation.json")
}

func operationSnapshotPath(localDir, operationID string) string {
	return filepath.Join(operationDirectory(localDir, operationID), "snapshot.ccl")
}

func targetRemoteIDs(localDir string, aliases []string) ([]string, error) {
	registry, err := loadRegistry(localDir, false)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		_, id, err := resolveRemote(registry, alias)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func resumePushOperation(
	manager *Manager,
	aliases []string,
	prepared *preparedPush,
) (pushOperation, bool, error) {
	targetIDs, err := targetRemoteIDs(manager.localDir, aliases)
	if err != nil {
		return pushOperation{}, false, err
	}
	entries, err := os.ReadDir(operationsDirectory(manager.localDir))
	if err != nil {
		if os.IsNotExist(err) {
			return pushOperation{}, false, nil
		}
		return pushOperation{}, false, err
	}
	var match *pushOperation
	for _, entry := range entries {
		if !entry.IsDir() || !validIdentifier(entry.Name(), 32) {
			continue
		}
		var operation pushOperation
		if err := readJSONFile(operationMetadataPath(manager.localDir, entry.Name()), &operation); err != nil {
			return pushOperation{}, false, err
		}
		if err := validatePushOperation(operation); err != nil {
			return pushOperation{}, false, err
		}
		if operation.ProfileID != manager.profileID || operation.Hash != prepared.hash ||
			operation.Tag != prepared.tag || !sameStrings(operation.TargetIDs, targetIDs) {
			continue
		}
		if match != nil {
			return pushOperation{}, false, fmt.Errorf("multiple matching cloud push operations require `ccl doctor`")
		}
		copy := operation
		match = &copy
	}
	if match == nil {
		return pushOperation{}, false, nil
	}
	ciphertext, err := readRegularFile(
		operationSnapshotPath(manager.localDir, match.ID), maxEncryptedSize,
	)
	if err != nil {
		return pushOperation{}, false, err
	}
	if len(ciphertext) > maxEncryptedSize {
		return pushOperation{}, false, fmt.Errorf("pending encrypted snapshot exceeds safety limit")
	}
	plain, err := openCompressed(manager.key, ciphertext)
	if err != nil {
		return pushOperation{}, false, fmt.Errorf("open pending encrypted snapshot: %w", err)
	}
	var payload snapshotPayload
	if err := json.Unmarshal(plain, &payload); err != nil {
		return pushOperation{}, false, fmt.Errorf("decode pending encrypted snapshot: %w", err)
	}
	if payload.Version != formatVersion || payload.Hash != match.Hash ||
		payload.Tag != match.Tag || !payload.CreatedAt.Equal(match.CreatedAt) ||
		hashSnapshotFiles(payload.Files) != match.Hash {
		return pushOperation{}, false, fmt.Errorf("pending cloud push operation does not match its encrypted snapshot")
	}
	prepared.id = match.SnapshotID
	prepared.createdAt = match.CreatedAt
	prepared.encrypted = ciphertext
	prepared.files = payload.Files
	return *match, true, nil
}

func createPushOperation(
	manager *Manager,
	aliases []string,
	prepared preparedPush,
) (pushOperation, error) {
	targetIDs, err := targetRemoteIDs(manager.localDir, aliases)
	if err != nil {
		return pushOperation{}, err
	}
	operationID, err := randomHexIdentifier(16)
	if err != nil {
		return pushOperation{}, err
	}
	operation := pushOperation{
		Version: pushOperationVersion, ID: operationID,
		ProfileID: manager.profileID, SnapshotID: prepared.id,
		Hash: prepared.hash, Tag: prepared.tag,
		TargetIDs: targetIDs, CreatedAt: prepared.createdAt,
	}
	if err := writeAtomic(
		operationSnapshotPath(manager.localDir, operationID),
		prepared.encrypted, 0o600,
	); err != nil {
		return pushOperation{}, err
	}
	if err := writeJSONAtomic(
		operationMetadataPath(manager.localDir, operationID),
		operation, 0o600,
	); err != nil {
		return pushOperation{}, err
	}
	return operation, nil
}

func validatePushOperation(operation pushOperation) error {
	if operation.Version != pushOperationVersion ||
		!validIdentifier(operation.ID, 32) ||
		!validIdentifier(operation.ProfileID, 32) ||
		!validIdentifier(operation.SnapshotID, 64) ||
		!validIdentifier(operation.Hash, 64) ||
		operation.Tag == "" || operation.CreatedAt.IsZero() ||
		len(operation.TargetIDs) == 0 {
		return fmt.Errorf("invalid pending cloud push operation")
	}
	targets := make(map[string]bool, len(operation.TargetIDs))
	for _, id := range operation.TargetIDs {
		if !validIdentifier(id, 32) || targets[id] {
			return fmt.Errorf("invalid pending cloud push target")
		}
		targets[id] = true
	}
	completed := make(map[string]bool, len(operation.CompletedIDs))
	for _, id := range operation.CompletedIDs {
		if !targets[id] || completed[id] {
			return fmt.Errorf("invalid completed cloud push target")
		}
		completed[id] = true
	}
	return nil
}

func markPushOperationComplete(localDir string, operation *pushOperation, remoteID string) error {
	for _, id := range operation.CompletedIDs {
		if id == remoteID {
			return nil
		}
	}
	operation.CompletedIDs = append(operation.CompletedIDs, remoteID)
	sort.Strings(operation.CompletedIDs)
	return writeJSONAtomic(operationMetadataPath(localDir, operation.ID), operation, 0o600)
}

func pushOperationCompleted(operation pushOperation, remoteID string) bool {
	for _, id := range operation.CompletedIDs {
		if id == remoteID {
			return true
		}
	}
	return false
}

func removePushOperation(localDir string, operation pushOperation) error {
	path := operationDirectory(localDir, operation.ID)
	if filepath.Dir(path) != operationsDirectory(localDir) ||
		filepath.Base(path) != operation.ID || !validIdentifier(operation.ID, 32) {
		return fmt.Errorf("refuse to remove invalid cloud operation path")
	}
	return os.RemoveAll(path)
}

func sameStrings(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	firstCopy := append([]string(nil), first...)
	secondCopy := append([]string(nil), second...)
	sort.Strings(firstCopy)
	sort.Strings(secondCopy)
	for index := range firstCopy {
		if firstCopy[index] != secondCopy[index] {
			return false
		}
	}
	return true
}
