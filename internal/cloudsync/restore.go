package cloudsync

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func (m *Manager) backupCurrent(tag string) (string, error) {
	files, hash, err := collectLocalFiles()
	if err != nil {
		if errors.Is(err, ErrNoLocalData) {
			return "", nil
		}
		return "", err
	}
	payload := snapshotPayload{
		Version: formatVersion, Hash: hash, Tag: "backup-" + tag,
		DeviceID: m.deviceID, CreatedAt: nowUTC(), Files: files,
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encrypted, err := sealCompressed(m.key, plain)
	if err != nil {
		return "", err
	}
	name := time.Now().UTC().Format("20060102T150405.000000000Z") + ".ccl"
	path := filepath.Join(m.localDir, backupsDirectory, name)
	if err := writeAtomic(path, encrypted, 0o600); err != nil {
		return "", fmt.Errorf("create encrypted local backup: %w", err)
	}
	return path, nil
}

func (m *Manager) applySnapshot(files []snapshotFile) (err error) {
	if err := validateSnapshotFiles(files); err != nil {
		return err
	}
	if err := os.MkdirAll(m.localDir, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(m.localDir, ".pull-stage-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	stageAuth := filepath.Join(stage, "auth")
	if err := os.MkdirAll(stageAuth, 0o700); err != nil {
		return err
	}
	hasConfig := false
	for _, file := range files {
		target := filepath.Join(stage, filepath.FromSlash(file.Path))
		if file.Path == "config.yaml" {
			hasConfig = true
		}
		if err := writeAtomic(target, file.Data, 0o600); err != nil {
			return fmt.Errorf("stage %s: %w", file.Path, err)
		}
	}

	suffix, err := randomDeviceID()
	if err != nil {
		return err
	}
	configPath := filepath.Join(m.localDir, "config.yaml")
	authPath := filepath.Join(m.localDir, "auth")
	configBackup := filepath.Join(m.localDir, ".config.pull-backup-"+suffix)
	authBackup := filepath.Join(m.localDir, ".auth.pull-backup-"+suffix)
	hadConfig := pathExists(configPath)
	hadAuth := pathExists(authPath)

	rollback := func() {
		_ = os.Remove(configPath)
		_ = os.RemoveAll(authPath)
		if hadConfig {
			_ = os.Rename(configBackup, configPath)
		}
		if hadAuth {
			_ = os.Rename(authBackup, authPath)
		}
	}
	if hadConfig {
		if err := os.Rename(configPath, configBackup); err != nil {
			return fmt.Errorf("prepare config replacement: %w", err)
		}
	}
	if hadAuth {
		if err := os.Rename(authPath, authBackup); err != nil {
			if hadConfig {
				_ = os.Rename(configBackup, configPath)
			}
			return fmt.Errorf("prepare auth replacement: %w", err)
		}
	}
	if err := os.Rename(stageAuth, authPath); err != nil {
		rollback()
		return fmt.Errorf("replace auth directory: %w", err)
	}
	if hasConfig {
		if err := os.Rename(filepath.Join(stage, "config.yaml"), configPath); err != nil {
			rollback()
			return fmt.Errorf("replace config: %w", err)
		}
	}
	if hadConfig {
		_ = os.Remove(configBackup)
	}
	if hadAuth {
		_ = os.RemoveAll(authBackup)
	}
	if err := os.Chmod(authPath, 0o700); err != nil {
		return err
	}
	if hasConfig {
		if err := os.Chmod(configPath, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
