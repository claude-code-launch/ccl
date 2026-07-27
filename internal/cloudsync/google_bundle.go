package cloudsync

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func createGoogleBundle(cacheDir string) ([]byte, error) {
	return createCloudBundle(cacheDir, filepath.Base(cacheDir))
}

func createCloudBundle(cacheDir, cacheName string) ([]byte, error) {
	if err := ensureCloudCacheDirectory(cacheDir, cacheName); err != nil {
		return nil, err
	}
	var names []string
	for _, name := range []string{profileFileName, verifierFileName, indexFileName} {
		if info, err := os.Lstat(filepath.Join(cacheDir, name)); err == nil {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("refuse to bundle non-regular sync file %s", name)
			}
			names = append(names, name)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	entries, err := os.ReadDir(filepath.Join(cacheDir, snapshotsDirectory))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		name := path.Join(snapshotsDirectory, entry.Name())
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil, infoErr
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !validBundlePath(name) {
			return nil, fmt.Errorf("refuse to bundle unsupported sync cache entry %q", name)
		}
		names = append(names, name)
	}
	if !containsString(names, profileFileName) {
		return nil, fmt.Errorf("cloud sync cache has no %s", profileFileName)
	}
	sort.Strings(names)

	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, name := range names {
		data, err := readRegularFile(
			filepath.Join(cacheDir, filepath.FromSlash(name)), maxEncryptedSize,
		)
		if err != nil {
			_ = writer.Close()
			return nil, err
		}
		if output.Len()+len(data) > maxBundleSize {
			_ = writer.Close()
			return nil, fmt.Errorf("cloud sync bundle exceeds the %d MiB safety limit", maxBundleSize>>20)
		}
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(0o600)
		part, err := writer.CreateHeader(header)
		if err != nil {
			_ = writer.Close()
			return nil, err
		}
		if _, err := part.Write(data); err != nil {
			_ = writer.Close()
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if output.Len() > maxBundleSize {
		return nil, fmt.Errorf("cloud sync bundle exceeds the %d MiB safety limit", maxBundleSize>>20)
	}
	return output.Bytes(), nil
}

func replaceGoogleCache(cacheDir string, bundle []byte) error {
	return replaceCloudCache(cacheDir, filepath.Base(cacheDir), bundle)
}

func replaceCloudCache(cacheDir, cacheName string, bundle []byte) error {
	if !filepath.IsAbs(cacheDir) || filepath.Base(cacheDir) != cacheName {
		return fmt.Errorf("invalid cloud cache directory")
	}
	parent := filepath.Dir(cacheDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, "."+cacheName+"-staging-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.MkdirAll(filepath.Join(staging, snapshotsDirectory), 0o700); err != nil {
		return err
	}
	if len(bundle) != 0 {
		if err := extractGoogleBundle(staging, bundle); err != nil {
			return err
		}
	}

	backup := cacheDir + ".old"
	_ = os.RemoveAll(backup)
	hadCache := false
	if _, err := os.Lstat(cacheDir); err == nil {
		if err := os.Rename(cacheDir, backup); err != nil {
			return fmt.Errorf("replace Google Drive cache: %w", err)
		}
		hadCache = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(staging, cacheDir); err != nil {
		if hadCache {
			_ = os.Rename(backup, cacheDir)
		}
		return fmt.Errorf("activate Google Drive cache: %w", err)
	}
	if hadCache {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func ensureCloudCacheDirectory(cacheDir, cacheName string) error {
	if !filepath.IsAbs(cacheDir) || filepath.Base(cacheDir) != cacheName {
		return fmt.Errorf("invalid cloud cache directory")
	}
	if info, err := os.Lstat(cacheDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refuse to use non-directory cloud cache")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, snapshotsDirectory), 0o700); err != nil {
		return fmt.Errorf("create cloud cache: %w", err)
	}
	return nil
}

func extractGoogleBundle(destination string, bundle []byte) error {
	reader, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		return fmt.Errorf("open Google Drive sync bundle: %w", err)
	}
	if len(reader.File) > 4096 {
		return fmt.Errorf("Google Drive sync bundle contains too many files")
	}
	seen := make(map[string]bool, len(reader.File))
	var total int64
	for _, file := range reader.File {
		if !validBundlePath(file.Name) || seen[file.Name] || file.FileInfo().IsDir() {
			return fmt.Errorf("Google Drive sync bundle contains unsupported path %q", file.Name)
		}
		seen[file.Name] = true
		if file.UncompressedSize64 > maxEncryptedSize {
			return fmt.Errorf("Google Drive sync bundle entry %q exceeds the safety limit", file.Name)
		}
		total += int64(file.UncompressedSize64)
		if total > maxBundleSize {
			return fmt.Errorf("Google Drive sync bundle exceeds the %d MiB safety limit", maxBundleSize>>20)
		}
		input, err := file.Open()
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(input, maxEncryptedSize+1))
		closeErr := input.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(data) > maxEncryptedSize {
			return fmt.Errorf("Google Drive sync bundle entry %q exceeds the safety limit", file.Name)
		}
		target := filepath.Join(destination, filepath.FromSlash(file.Name))
		if err := writeAtomic(target, data, 0o600); err != nil {
			return err
		}
	}
	if !seen[profileFileName] {
		return fmt.Errorf("Google Drive sync bundle has no %s", profileFileName)
	}
	return nil
}

func validBundlePath(name string) bool {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name ||
		strings.Contains(name, `\`) {
		return false
	}
	if name == profileFileName || name == verifierFileName || name == indexFileName {
		return true
	}
	if path.Dir(name) != snapshotsDirectory || path.Ext(name) != ".ccl" {
		return false
	}
	id := strings.TrimSuffix(path.Base(name), ".ccl")
	if len(id) != 64 {
		return false
	}
	for _, char := range id {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
