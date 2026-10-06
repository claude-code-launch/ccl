package cloudsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newCloudDoctorFixture builds a healthy single-remote installation: a home
// with local data, an iCloud drive override, and one logged-in remote.
type cloudDoctorFixture struct {
	home     string
	local    string
	drive    string
	profile  string
	remoteID string
	registry cloudRegistry
}

func newCloudDoctorFixture(t *testing.T) cloudDoctorFixture {
	t.Helper()
	home := t.TempDir()
	drive := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", drive)
	writeSyncFixture(t, home)
	if _, err := LoginICloudNamed("personal", false, ""); err != nil {
		t.Fatalf("login: %v", err)
	}
	local := filepath.Join(home, ".ccl")
	registry, err := loadRegistry(local, false)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return cloudDoctorFixture{
		home:     home,
		local:    local,
		drive:    drive,
		profile:  registry.ActiveProfileID,
		remoteID: registry.Aliases["personal"],
		registry: registry,
	}
}

// reportHas asserts one check at the given level mentions the text.
func reportHas(t *testing.T, report DiagnosticReport, level, want string) {
	t.Helper()
	for _, check := range report.Checks {
		if check.Level == level && strings.Contains(check.Message, want) {
			return
		}
	}
	t.Fatalf("no %s check contains %q: %+v", level, want, report)
}

func TestCloudDoctorReportsAHealthyInstallation(t *testing.T) {
	fixture := newCloudDoctorFixture(t)
	report := DiagnoseLocal()
	if !report.Configured || report.ProfileID != fixture.profile || report.Remotes != 1 {
		t.Fatalf("healthy report = %+v", report)
	}
	if HasDiagnosticErrors(report) {
		t.Fatalf("healthy installation has errors: %+v", report)
	}
	if IsNotConfiguredDiagnostic(report) {
		t.Fatal("a logged-in installation was reported as unconfigured")
	}
	reportHas(t, report, "ok", "registry v2")
}

// TestCloudDoctorReportsAnUnconfiguredInstallation covers both shapes of "no
// cloud sync here": nothing at all, and a leftover v1 configuration that the
// next cloud command would migrate.
func TestCloudDoctorReportsAnUnconfiguredInstallation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	report := DiagnoseLocal()
	if report.Configured || HasDiagnosticErrors(report) || !IsNotConfiguredDiagnostic(report) {
		t.Fatalf("fresh installation report = %+v", report)
	}
	if len(report.Checks) != 1 || report.Checks[0].Level != "info" {
		t.Fatalf("fresh installation checks = %+v", report.Checks)
	}

	if err := os.MkdirAll(filepath.Join(home, ".ccl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(home, ".ccl", cloudConfigName),
		[]byte(`{"version":1}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	report = DiagnoseLocal()
	if report.Configured || IsNotConfiguredDiagnostic(report) {
		t.Fatalf("legacy installation report = %+v", report)
	}
	reportHas(t, report, "warning", "legacy cloud sync v1")
	if HasDiagnosticErrors(report) {
		t.Fatalf("a migratable legacy config must not be an error: %+v", report)
	}
}

// TestCloudDoctorDetectsAnUnusableRegistry walks the registry failures a user
// can actually produce: a swapped symlink, unreadable JSON, and a file from a
// future version.
func TestCloudDoctorDetectsAnUnusableRegistry(t *testing.T) {
	fixture := newCloudDoctorFixture(t)
	path := registryPath(fixture.local)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(path, original, 0o600) })

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fixture.home, "elsewhere.json"), path); err != nil {
		t.Fatal(err)
	}
	report := DiagnoseLocal()
	if !report.Configured {
		t.Fatalf("a present registry must count as configured: %+v", report)
	}
	reportHas(t, report, "error", "cloud registry is not a regular file")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	report = DiagnoseLocal()
	reportHas(t, report, "error", "decode registry.json")

	registry := fixture.registry
	registry.Version = 99
	if err := writeJSONAtomic(path, registry, 0o600); err != nil {
		t.Fatal(err)
	}
	report = DiagnoseLocal()
	reportHas(t, report, "error", "invalid cloud registry")
}

// TestCloudDoctorDetectsAProfileThatCannotUnlock covers the profile-level
// failures: a key of the wrong length, the legacy root key, and a profile still
// parked on the macOS Keychain.
func TestCloudDoctorDetectsAProfileThatCannotUnlock(t *testing.T) {
	fixture := newCloudDoctorFixture(t)
	statePath := profileStatePath(fixture.local, fixture.profile)
	keyPath := profileKeyPath(fixture.local, fixture.profile)
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(statePath, stateBytes, 0o600)
		_ = os.WriteFile(keyPath, keyBytes, 0o600)
	})

	var state localProfileStateV2
	if err := readJSONFile(statePath, &state); err != nil {
		t.Fatal(err)
	}
	state.KeyMode = keyModeKeychain
	if err := writeJSONAtomic(statePath, state, 0o600); err != nil {
		t.Fatal(err)
	}
	report := DiagnoseLocal()
	reportHas(t, report, "info", "legacy macOS Keychain mode")

	if err := os.WriteFile(statePath, stateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyBytes[:16], 0o600); err != nil {
		t.Fatal(err)
	}
	report = DiagnoseLocal()
	reportHas(t, report, "error", "invalid length 16")

	legacy := filepath.Join(fixture.local, cloudKeyName)
	if err := os.WriteFile(legacy, keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(legacy) })
	report = DiagnoseLocal()
	reportHas(t, report, "warning", "cloud.key still present")
}

// TestCloudDoctorDetectsRemoteProblems covers both remote providers: an iCloud
// directory that vanished, and a Google Drive remote with a missing, unusable,
// and then healthy authorization.
func TestCloudDoctorDetectsRemoteProblems(t *testing.T) {
	fixture := newCloudDoctorFixture(t)
	configPath := remoteConfigPath(fixture.local, fixture.remoteID)
	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(configPath, original, 0o600) })

	icloudDir := filepath.Join(fixture.drive, remoteDirectory)
	if err := os.RemoveAll(icloudDir); err != nil {
		t.Fatal(err)
	}
	report := DiagnoseLocal()
	reportHas(t, report, "error", "iCloud directory unavailable")

	// A file where the remote directory belongs is not a directory either.
	if err := os.WriteFile(icloudDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	report = DiagnoseLocal()
	reportHas(t, report, "error", "iCloud path is not a directory")
	if err := os.Remove(icloudDir); err != nil {
		t.Fatal(err)
	}

	var remote localRemoteConfigV2
	if err := readJSONFile(configPath, &remote); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(fixture.local, googleCacheName)
	remote.Provider = providerGoogleDrive
	remote.RemoteDir = cacheDir
	if err := writeJSONAtomic(configPath, remote, 0o600); err != nil {
		t.Fatal(err)
	}

	report = DiagnoseLocal()
	reportHas(t, report, "error", "OAuth token is unavailable")
	reportHas(t, report, "error", "invalid Google authorization")

	authPath := remoteAuthPath(fixture.local, fixture.remoteID)
	if err := os.WriteFile(authPath, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	report = DiagnoseLocal()
	reportHas(t, report, "error", "invalid Google authorization")
	reportHas(t, report, "error", "cache is unavailable")

	if err := os.WriteFile(authPath, []byte(
		`{"version":1,"token":{"access_token":"a","refresh_token":"r","token_type":"Bearer"}}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	report = DiagnoseLocal()
	reportHas(t, report, "error", "cache is unavailable")

	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	report = DiagnoseLocal()
	if HasDiagnosticErrors(report) {
		t.Fatalf("a healthy Google Drive remote has errors: %+v", report)
	}
}

// TestCloudDoctorReportsPendingOperationsAndPairings covers the two journal
// directories: interrupted pushes and pairing requests waiting for approval.
func TestCloudDoctorReportsPendingOperationsAndPairings(t *testing.T) {
	fixture := newCloudDoctorFixture(t)
	operations := operationsDirectory(fixture.local)
	if err := os.MkdirAll(filepath.Join(operations, "not-an-id"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(operations, "stray.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	brokenID := strings.Repeat("a", 32)
	if err := os.MkdirAll(filepath.Join(operations, brokenID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(operationMetadataPath(fixture.local, brokenID), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	report := DiagnoseLocal()
	reportHas(t, report, "warning", "unexpected cloud operation entry")
	reportHas(t, report, "error", "cannot read push operation")

	operation := pushOperation{
		Version:    pushOperationVersion,
		ID:         strings.Repeat("b", 32),
		ProfileID:  fixture.profile,
		SnapshotID: strings.Repeat("c", 64),
		Hash:       strings.Repeat("d", 64),
		Tag:        "latest",
		TargetIDs:  []string{fixture.remoteID},
		CreatedAt:  time.Now().UTC(),
	}
	if err := writeJSONAtomic(
		operationMetadataPath(fixture.local, operation.ID), operation, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	stale := operation
	stale.ID = strings.Repeat("e", 32)
	stale.ProfileID = strings.Repeat("f", 32)
	if err := writeJSONAtomic(
		operationMetadataPath(fixture.local, stale.ID), stale, 0o600,
	); err != nil {
		t.Fatal(err)
	}

	pairing := pendingPairingDirectory(fixture.local)
	expired := pendingPairing{Request: pairingRequestEnvelope{
		RequestID:          "req-expired",
		ProfileID:          fixture.profile,
		EphemeralPublicKey: "public-key",
		ExpiresAt:          time.Now().UTC().Add(-time.Hour),
	}}
	if err := writeJSONAtomic(filepath.Join(pairing, "expired.json"), expired, 0o600); err != nil {
		t.Fatal(err)
	}
	waiting := pendingPairing{Request: pairingRequestEnvelope{
		RequestID:          "req-waiting",
		ProfileID:          fixture.profile,
		EphemeralPublicKey: "public-key",
		ExpiresAt:          time.Now().UTC().Add(time.Hour),
	}}
	if err := writeJSONAtomic(filepath.Join(pairing, "waiting.json"), waiting, 0o600); err != nil {
		t.Fatal(err)
	}
	// Ignored shapes: unreadable JSON is reported, a directory named *.json and
	// a remote-side object are skipped without a word.
	if err := os.WriteFile(filepath.Join(pairing, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pairing, "nested.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(pairing, "remote-mirror.json"), []byte("{"), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	report = DiagnoseLocal()
	reportHas(t, report, "warning", "partial push")
	reportHas(t, report, "error", "belongs to another profile")
	reportHas(t, report, "warning", "expired local pairing request")
	reportHas(t, report, "info", "waiting for approval")
	reportHas(t, report, "error", "invalid pending pairing")
}

// TestDefaultICloudDirectoryResolvesTheExpectedPath pins the path the login
// would use, including the two ways it can fail before any network call.
func TestDefaultICloudDirectoryResolvesTheExpectedPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv("CCL_ICLOUD_DRIVE_DIR", "relative/path")
	if _, err := defaultICloudDirectory(); err == nil ||
		!strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("relative override error = %v", err)
	}

	drive := t.TempDir()
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", drive)
	resolved, err := defaultICloudDirectory()
	if err != nil || resolved != filepath.Join(drive, remoteDirectory) {
		t.Fatalf("override path = %q, %v", resolved, err)
	}

	t.Setenv("CCL_ICLOUD_DRIVE_DIR", "")
	if _, err := defaultICloudDirectory(); err == nil ||
		!strings.Contains(err.Error(), "iCloud Drive is unavailable") {
		t.Fatalf("missing iCloud error = %v", err)
	}

	icloud := filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs")
	if err := os.MkdirAll(icloud, 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err = defaultICloudDirectory()
	if err != nil || resolved != filepath.Join(icloud, remoteDirectory) {
		t.Fatalf("iCloud path = %q, %v", resolved, err)
	}

	if err := os.RemoveAll(icloud); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(icloud, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := defaultICloudDirectory(); err == nil ||
		!strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("file in place of iCloud Drive error = %v", err)
	}
}
