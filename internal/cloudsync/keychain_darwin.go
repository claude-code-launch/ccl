//go:build darwin

package cloudsync

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const keychainService = "com.claude-code-launch.ccl.sync"

func storePlatformKey(profileID string, key []byte) error {
	if len(key) != 32 {
		return fmt.Errorf("refuse to store an invalid encryption key")
	}
	value := base64.RawStdEncoding.EncodeToString(key)
	cmd := exec.Command(
		"/usr/bin/security", "add-generic-password",
		"-U",
		"-a", profileID,
		"-s", keychainService,
		"-l", "ccl iCloud sync key",
		"-T", "",
		"-w",
	)
	// With -w as the final option, security(1) reads the secret without placing
	// it in argv. New items request the value twice; updates safely ignore the
	// second line.
	cmd.Stdin = strings.NewReader(value + "\n" + value + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: store sync key: %s", ErrKeychainUnavailable, securityError(stderr.String(), err))
	}
	return nil
}

func loadPlatformKey(profileID string) ([]byte, error) {
	cmd := exec.Command(
		"/usr/bin/security", "find-generic-password",
		"-a", profileID,
		"-s", keychainService,
		"-w",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		message := stderr.String()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 ||
			strings.Contains(message, "could not be found") ||
			strings.Contains(message, "errSecItemNotFound") {
			return nil, ErrKeychainItemMissing
		}
		return nil, fmt.Errorf("%w: read sync key: %s", ErrKeychainUnavailable, securityError(message, err))
	}
	key, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(output)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("invalid ccl sync key in macOS Keychain")
	}
	return key, nil
}

func securityError(message string, fallback error) string {
	if message = strings.TrimSpace(message); message != "" {
		return message
	}
	return fallback.Error()
}
