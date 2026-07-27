package cloudsync

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

var ErrNoLocalData = errors.New("no ccl configuration or OAuth credentials found to sync")

func cclDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".ccl"), nil
}

func defaultICloudDirectory() (string, error) {
	if override := strings.TrimSpace(os.Getenv("CCL_ICLOUD_DRIVE_DIR")); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("CCL_ICLOUD_DRIVE_DIR must be an absolute path")
		}
		return filepath.Join(override, remoteDirectory), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	drive := filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs")
	info, err := os.Stat(drive)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("iCloud Drive is unavailable; sign in and enable iCloud Drive in macOS System Settings")
		}
		return "", fmt.Errorf("check iCloud Drive: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("iCloud Drive path is not a directory: %s", drive)
	}
	return filepath.Join(drive, remoteDirectory), nil
}

func collectLocalFiles() ([]snapshotFile, string, error) {
	root, err := cclDirectory()
	if err != nil {
		return nil, "", err
	}
	var files []snapshotFile
	var totalSize int64
	add := func(path, archivePath string) error {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				return nil
			}
			return statErr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refuse to sync non-regular file %s", path)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		totalSize += int64(len(data))
		if totalSize > maxEncryptedSize {
			return fmt.Errorf("ccl configuration exceeds the %d MiB sync safety limit", maxEncryptedSize>>20)
		}
		files = append(files, snapshotFile{Path: archivePath, Mode: 0o600, Data: data})
		return nil
	}
	if err := add(filepath.Join(root, "config.yaml"), "config.yaml"); err != nil {
		return nil, "", fmt.Errorf("read ccl config: %w", err)
	}
	authDir := filepath.Join(root, "auth")
	entries, readErr := os.ReadDir(authDir)
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, "", fmt.Errorf("read auth directory: %w", readErr)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		if strings.Contains(entry.Name(), `\`) {
			return nil, "", fmt.Errorf("refuse to sync auth filename containing a backslash: %s", entry.Name())
		}
		if err := add(filepath.Join(authDir, entry.Name()), path.Join("auth", entry.Name())); err != nil {
			return nil, "", fmt.Errorf("read auth credential %s: %w", entry.Name(), err)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	if len(files) == 0 {
		return nil, "", ErrNoLocalData
	}
	return files, hashSnapshotFiles(files), nil
}

func hashSnapshotFiles(files []snapshotFile) string {
	h := sha256.New()
	var size [8]byte
	for _, file := range files {
		binary.BigEndian.PutUint64(size[:], uint64(len(file.Path)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(file.Path))
		binary.BigEndian.PutUint64(size[:], uint64(len(file.Data)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(file.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validateSnapshotFiles(files []snapshotFile) error {
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		clean := path.Clean(file.Path)
		valid := clean == "config.yaml"
		if !valid && path.Dir(clean) == "auth" &&
			clean == path.Join("auth", path.Base(clean)) {
			valid = strings.EqualFold(path.Ext(clean), ".json")
		}
		if !valid || path.IsAbs(clean) || file.Path != clean || strings.Contains(file.Path, `\`) {
			return fmt.Errorf("snapshot contains unsupported path %q", file.Path)
		}
		if seen[clean] {
			return fmt.Errorf("snapshot contains duplicate path %q", file.Path)
		}
		seen[clean] = true
	}
	return nil
}

func readJSONFile(path string, target any) error {
	data, err := readRegularFile(path, maxEncryptedSize)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refuse to read non-regular file %s", path)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file %s exceeds safety limit", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file %s exceeds safety limit", path)
	}
	return data, nil
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomic(path, data, mode)
}

func writeAtomic(path string, data []byte, mode os.FileMode) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	parentInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to write through non-directory path %s", filepath.Dir(path))
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err = tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
