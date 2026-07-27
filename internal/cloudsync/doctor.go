package cloudsync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func DiagnoseLocal() DiagnosticReport {
	var report DiagnosticReport
	localDir, err := cclDirectory()
	if err != nil {
		report.add("error", err.Error())
		return report
	}
	registryInfo, err := os.Lstat(registryPath(localDir))
	if err != nil {
		if !os.IsNotExist(err) {
			report.add("error", fmt.Sprintf("cannot inspect cloud registry: %v", err))
			return report
		}
		if _, legacyErr := os.Stat(filepath.Join(localDir, cloudConfigName)); legacyErr == nil {
			report.add("warning", "legacy cloud sync v1 is configured and will migrate on the next cloud command")
		} else if !os.IsNotExist(legacyErr) {
			report.add("error", fmt.Sprintf("cannot inspect legacy cloud configuration: %v", legacyErr))
		} else {
			report.add("info", "not configured")
		}
		return report
	}
	report.Configured = true
	if !registryInfo.Mode().IsRegular() || registryInfo.Mode()&os.ModeSymlink != 0 {
		report.add("error", "cloud registry is not a regular file")
		return report
	}
	if insecurePermissions(registryInfo.Mode()) {
		report.add("error", fmt.Sprintf("cloud registry permissions are too broad: %o", registryInfo.Mode().Perm()))
	}
	registry, err := loadRegistry(localDir, false)
	if err != nil {
		report.add("error", err.Error())
		return report
	}
	report.ProfileID = registry.ActiveProfileID
	report.Remotes = len(registry.Aliases)
	report.add("ok", fmt.Sprintf(
		"registry v%d · profile %s · %d remote(s)",
		registry.Version, shortIdentifier(registry.ActiveProfileID), len(registry.Aliases),
	))
	checkSecureDirectory(&report, cloudRoot(localDir), "cloud directory")
	checkSecureFile(
		&report, profileMetadataPath(localDir, registry.ActiveProfileID),
		"profile metadata", false,
	)
	var profile localProfileStateV2
	if err := readJSONFile(profileStatePath(localDir, registry.ActiveProfileID), &profile); err != nil {
		report.add("error", fmt.Sprintf("cannot read profile state: %v", err))
	} else {
		checkSecureFile(
			&report, profileStatePath(localDir, registry.ActiveProfileID),
			"profile state", false,
		)
		if profile.KeyMode != keyModeKeychain {
			keyPath := profileKeyPath(localDir, registry.ActiveProfileID)
			checkSecureFile(&report, keyPath, "profile key", false)
			if key, err := readRegularFile(keyPath, 32); err == nil && len(key) != 32 {
				report.add("error", fmt.Sprintf("profile key has invalid length %d", len(key)))
			}
			rootKey := filepath.Join(localDir, cloudKeyName)
			if _, err := os.Stat(rootKey); err == nil {
				report.add("warning", "legacy ~/.ccl/cloud.key still present; profile key is authoritative — safe to remove after backup")
			} else if !os.IsNotExist(err) {
				report.add("warning", fmt.Sprintf("cannot stat legacy cloud.key: %v", err))
			}
		} else {
			report.add("info", "profile key uses the legacy macOS Keychain mode")
		}
	}
	for _, alias := range sortedRemoteAliases(registry) {
		id := registry.Aliases[alias]
		var remote localRemoteConfigV2
		if err := readJSONFile(remoteConfigPath(localDir, id), &remote); err != nil {
			report.add("error", fmt.Sprintf("%s: cannot read remote config: %v", alias, err))
			continue
		}
		checkSecureFile(&report, remoteConfigPath(localDir, id), alias+" config", false)
		var state localRemoteStateV2
		if err := readJSONFile(remoteStatePath(localDir, id), &state); err != nil {
			report.add("error", fmt.Sprintf("%s: cannot read remote state: %v", alias, err))
		} else if state.Version != registryVersion {
			report.add("error", fmt.Sprintf("%s: unsupported remote state version %d", alias, state.Version))
		}
		checkSecureFile(&report, remoteStatePath(localDir, id), alias+" state", false)
		switch remote.Provider {
		case providerGoogleDrive:
			authPath := remoteAuthPath(localDir, id)
			checkSecureFile(&report, authPath, alias+" OAuth token", false)
			if _, err := loadGoogleToken(authPath); err != nil {
				report.add("error", fmt.Sprintf("%s: invalid Google authorization: %v", alias, err))
			}
			checkSecureDirectory(&report, remote.RemoteDir, alias+" cache")
		case providerICloud:
			info, err := os.Stat(remote.RemoteDir)
			if err != nil {
				report.add("error", fmt.Sprintf("%s: iCloud directory unavailable: %v", alias, err))
			} else if !info.IsDir() {
				report.add("error", fmt.Sprintf("%s: iCloud path is not a directory", alias))
			}
		}
		role := "mirror"
		if id == registry.PrimaryRemoteID {
			role = "primary"
		}
		report.add("ok", fmt.Sprintf(
			"%s · %s · %s · mirror=%t",
			alias, remote.Provider, role, remote.Mirror,
		))
	}
	diagnoseOperations(&report, localDir, registry)
	diagnosePendingPairing(&report, localDir)
	return report
}

func (report *DiagnosticReport) add(level, message string) {
	report.Checks = append(report.Checks, Diagnostic{Level: level, Message: message})
}

func checkSecureDirectory(report *DiagnosticReport, path, label string) {
	info, err := os.Lstat(path)
	if err != nil {
		report.add("error", fmt.Sprintf("%s is unavailable: %v", label, err))
		return
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		report.add("error", label+" is not a regular directory")
		return
	}
	if insecurePermissions(info.Mode()) {
		report.add("warning", fmt.Sprintf("%s permissions are broader than 0700: %o", label, info.Mode().Perm()))
	}
}

func checkSecureFile(
	report *DiagnosticReport,
	path, label string,
	optional bool,
) {
	info, err := os.Lstat(path)
	if err != nil {
		if optional && os.IsNotExist(err) {
			return
		}
		report.add("error", fmt.Sprintf("%s is unavailable: %v", label, err))
		return
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		report.add("error", label+" is not a regular file")
		return
	}
	if insecurePermissions(info.Mode()) {
		report.add("error", fmt.Sprintf("%s permissions are broader than 0600: %o", label, info.Mode().Perm()))
	}
}

func insecurePermissions(mode os.FileMode) bool {
	return mode.Perm()&0o077 != 0
}

func diagnoseOperations(
	report *DiagnosticReport,
	localDir string,
	registry cloudRegistry,
) {
	entries, err := os.ReadDir(operationsDirectory(localDir))
	if err != nil {
		if !os.IsNotExist(err) {
			report.add("error", fmt.Sprintf("cannot inspect push operations: %v", err))
		}
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validIdentifier(entry.Name(), 32) {
			report.add("warning", fmt.Sprintf("unexpected cloud operation entry %q", entry.Name()))
			continue
		}
		var operation pushOperation
		if err := readJSONFile(operationMetadataPath(localDir, entry.Name()), &operation); err != nil {
			report.add("error", fmt.Sprintf("cannot read push operation %s: %v", entry.Name(), err))
			continue
		}
		if err := validatePushOperation(operation); err != nil {
			report.add("error", fmt.Sprintf("invalid push operation %s: %v", entry.Name(), err))
			continue
		}
		if operation.ProfileID != registry.ActiveProfileID {
			report.add("error", fmt.Sprintf("push operation %s belongs to another profile", entry.Name()))
			continue
		}
		report.add("warning", fmt.Sprintf(
			"partial push %s is pending (%d/%d remotes complete)",
			shortIdentifier(operation.SnapshotID),
			len(operation.CompletedIDs), len(operation.TargetIDs),
		))
	}
}

func diagnosePendingPairing(report *DiagnosticReport, localDir string) {
	entries, err := os.ReadDir(pendingPairingDirectory(localDir))
	if err != nil {
		if !os.IsNotExist(err) {
			report.add("error", fmt.Sprintf("cannot inspect pending pairing: %v", err))
		}
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" ||
			strings.HasPrefix(entry.Name(), "remote-") {
			continue
		}
		var pending pendingPairing
		path := filepath.Join(pendingPairingDirectory(localDir), entry.Name())
		if err := readLimitedJSONFile(path, &pending); err != nil {
			report.add("error", fmt.Sprintf("invalid pending pairing %q: %v", entry.Name(), err))
			continue
		}
		if nowUTC().After(pending.Request.ExpiresAt) {
			report.add("warning", fmt.Sprintf(
				"expired local pairing request %s can be removed",
				pairingCode(pending.Request),
			))
		} else {
			report.add("info", fmt.Sprintf(
				"local pairing request %s is waiting for approval",
				pairingCode(pending.Request),
			))
		}
	}
}

func HasDiagnosticErrors(report DiagnosticReport) bool {
	for _, check := range report.Checks {
		if strings.EqualFold(check.Level, "error") {
			return true
		}
	}
	return false
}

func IsNotConfiguredDiagnostic(report DiagnosticReport) bool {
	return !report.Configured && len(report.Checks) == 1 &&
		report.Checks[0].Level == "info" &&
		strings.Contains(report.Checks[0].Message, "not configured")
}
