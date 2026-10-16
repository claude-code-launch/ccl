package cloudsync

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSyncFixture(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".ccl", "auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(home, ".ccl", "config.yaml"),
		[]byte("active_provider: test\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(home, ".ccl", "auth", "credential.json"),
		[]byte(`{"type":"xai","token":"secret"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}

func TestMultipleRemotesReuseOneEncryptedSnapshot(t *testing.T) {
	home := t.TempDir()
	firstDrive := t.TempDir()
	secondDrive := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", firstDrive)
	writeSyncFixture(t, home)

	first, err := LoginICloudNamed("personal")
	if err != nil {
		t.Fatal(err)
	}
	if first.Alias != "personal" {
		t.Fatalf("first alias = %q", first.Alias)
	}
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", secondDrive)
	second, err := LoginICloudNamed("backup")
	if err != nil {
		t.Fatal(err)
	}
	if second.Alias != "backup" || second.Existing {
		t.Fatalf("second login = %+v", second)
	}
	remotes, err := ListRemotes()
	if err != nil {
		t.Fatal(err)
	}
	if len(remotes) != 2 || !remotes[0].Primary || remotes[0].Alias != "personal" {
		t.Fatalf("remotes = %+v", remotes)
	}

	outcomes, err := PushRemotes("", true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("push outcomes = %+v", outcomes)
	}
	firstID := outcomes[0].Result.ID
	if firstID == "" || outcomes[1].Result.ID != firstID {
		t.Fatalf("snapshot IDs differ: %+v", outcomes)
	}
	firstCipher, err := os.ReadFile(filepath.Join(
		firstDrive, remoteDirectory, snapshotsDirectory, firstID+".ccl",
	))
	if err != nil {
		t.Fatal(err)
	}
	secondCipher, err := os.ReadFile(filepath.Join(
		secondDrive, remoteDirectory, snapshotsDirectory, firstID+".ccl",
	))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstCipher, secondCipher) {
		t.Fatal("mirrors did not receive the same encrypted snapshot")
	}

	if err := SetPrimaryRemote("backup"); err != nil {
		t.Fatal(err)
	}
	if err := RenameRemote("personal", "home"); err != nil {
		t.Fatal(err)
	}
	if err := SetRemoteMirror("home", false); err != nil {
		t.Fatal(err)
	}
	remotes, err = ListRemotes()
	if err != nil {
		t.Fatal(err)
	}
	if len(remotes) != 2 || remotes[0].Alias != "home" || remotes[0].Mirror {
		t.Fatalf("updated remotes = %+v", remotes)
	}

	result, err := LogoutRemote(t.Context(), "home", LogoutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.LocalOnly {
		t.Fatalf("logout result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(firstDrive, remoteDirectory, profileFileName)); err != nil {
		t.Fatalf("local logout removed remote data: %v", err)
	}
}

func TestPartialPushJournalRetriesOnlyIncompleteRemote(t *testing.T) {
	home := t.TempDir()
	firstDrive := t.TempDir()
	secondDrive := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", firstDrive)
	writeSyncFixture(t, home)
	if _, err := LoginICloudNamed("first"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", secondDrive)
	if _, err := LoginICloudNamed("second"); err != nil {
		t.Fatal(err)
	}
	firstManager, err := LoadRemote("first")
	if err != nil {
		t.Fatal(err)
	}
	secondManager, err := LoadRemote("second")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := firstManager.preparePush()
	if err != nil {
		t.Fatal(err)
	}
	firstPlan, err := firstManager.planPush(prepared, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondManager.planPush(prepared, false); err != nil {
		t.Fatal(err)
	}
	operation, err := createPushOperation(firstManager, []string{"first", "second"}, prepared)
	if err != nil {
		t.Fatal(err)
	}
	firstResult, err := firstPlan.commit(prepared, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := markPushOperationComplete(
		filepath.Join(home, ".ccl"), &operation, firstManager.remoteID,
	); err != nil {
		t.Fatal(err)
	}

	outcomes, err := PushRemotes("", true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 2 || outcomes[0].Result.ID != firstResult.ID ||
		outcomes[1].Result.ID != firstResult.ID {
		t.Fatalf("resumed outcomes = %+v, first = %+v", outcomes, firstResult)
	}
	if outcomes[0].Result.Uploaded {
		t.Fatal("completed remote was uploaded again")
	}
	entries, err := os.ReadDir(operationsDirectory(filepath.Join(home, ".ccl")))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("completed operation journal remains: %v", entries)
	}
}

func TestCloudDoctorDetectsBroadKeyPermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", t.TempDir())
	writeSyncFixture(t, home)
	if _, err := LoginICloudNamed("personal"); err != nil {
		t.Fatal(err)
	}
	report := DiagnoseLocal()
	if HasDiagnosticErrors(report) {
		t.Fatalf("healthy diagnostics = %+v", report)
	}
	if err := os.Chmod(profileKeyPath(filepath.Join(home, ".ccl"), report.ProfileID), 0o644); err != nil {
		t.Fatal(err)
	}
	report = DiagnoseLocal()
	if !HasDiagnosticErrors(report) {
		t.Fatalf("insecure key permissions were not detected: %+v", report)
	}
}

func TestCloudLoadRejectsSymlinkedProfileKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", t.TempDir())
	writeSyncFixture(t, home)
	if _, err := LoginICloudNamed("personal"); err != nil {
		t.Fatal(err)
	}
	localDir := filepath.Join(home, ".ccl")
	registry, err := loadRegistry(localDir)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := profileKeyPath(localDir, registry.ActiveProfileID)
	target := filepath.Join(home, "replacement-key")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRemote("personal"); err == nil ||
		!strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("symlinked profile key was accepted: %v", err)
	}
}
