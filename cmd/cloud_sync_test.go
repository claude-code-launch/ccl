package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCloudLoginPassphraseMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", t.TempDir())
	t.Setenv("CCL_SYNC_PASSPHRASE", "correct horse battery staple")
	if err := os.MkdirAll(filepath.Join(home, ".ccl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ccl", "config.yaml"), []byte("providers: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := newCloudLoginCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"icloud", "--passphrase"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "passphrase-derived local key") {
		t.Fatalf("login output = %q", got)
	}
	// After login the v2 registry is active: profile key is authoritative and
	// root cloud.key should not linger.
	profileDirs, err := filepath.Glob(filepath.Join(home, ".ccl", "cloud", "profiles", "*", "key"))
	if err != nil || len(profileDirs) != 1 {
		t.Fatalf("profile key glob = %v, %v", profileDirs, err)
	}
	if info, err := os.Stat(profileDirs[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("profile key mode/error = %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ccl", "cloud.key")); !os.IsNotExist(err) {
		t.Fatalf("legacy cloud.key should be removed after registry setup: %v", err)
	}

	recoveryPath := filepath.Join(home, "ccl-recovery.txt")
	exportCommand := newCloudKeyExportCommand()
	exportCommand.SetOut(&output)
	exportCommand.SetErr(&output)
	exportCommand.SetArgs([]string{"--output", recoveryPath})
	if err := exportCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	recovery, err := os.ReadFile(recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(recovery), "CCL1-") {
		t.Fatalf("exported recovery key = %q", recovery)
	}
	if info, err := os.Stat(recoveryPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("recovery file mode/error = %v, %v", info, err)
	}
	if err := exportCommand.Execute(); err == nil || !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("second export should refuse overwrite: %v", err)
	}
}

func TestCloudKeyCommandShape(t *testing.T) {
	command := newCloudKeyCommand()
	if command.Use != "key" {
		t.Fatalf("key command use = %q", command.Use)
	}
	var names []string
	for _, child := range command.Commands() {
		names = append(names, child.Name())
	}
	if strings.Join(names, ",") != "export,import" {
		t.Fatalf("key subcommands = %v", names)
	}
}

func TestCloudCommandTreeShape(t *testing.T) {
	cloudCommand := newCloudCommand()
	var names []string
	for _, child := range cloudCommand.Commands() {
		names = append(names, child.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "device,key,login,logout,pull,push,remote,status,tag" {
		t.Fatalf("cloud subcommands = %v", names)
	}
	remote := newCloudRemoteCommand()
	var remoteNames []string
	for _, child := range remote.Commands() {
		remoteNames = append(remoteNames, child.Name())
	}
	sort.Strings(remoteNames)
	if strings.Join(remoteNames, ",") != "ls,rename,set,use" {
		t.Fatalf("cloud remote subcommands = %v", remoteNames)
	}
	logout := newCloudLogoutCommand()
	for _, name := range []string{"all", "revoke", "delete-remote", "force-local", "yes"} {
		if logout.Flags().Lookup(name) == nil {
			t.Fatalf("logout missing --%s", name)
		}
	}
}

func TestReadRecoveryKeyFromArgumentFileEnvironmentAndStdin(t *testing.T) {
	const recovery = "CCL1-ABCDE"
	got, err := readRecoveryKey(strings.NewReader(""), &bytes.Buffer{}, []string{recovery}, "")
	if err != nil || got != recovery {
		t.Fatalf("argument = %q, %v", got, err)
	}

	path := filepath.Join(t.TempDir(), "recovery.txt")
	if err := os.WriteFile(path, []byte(recovery+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = readRecoveryKey(strings.NewReader(""), &bytes.Buffer{}, nil, path)
	if err != nil || got != recovery {
		t.Fatalf("file = %q, %v", got, err)
	}

	t.Setenv("CCL_SYNC_RECOVERY_KEY", recovery)
	got, err = readRecoveryKey(strings.NewReader(""), &bytes.Buffer{}, nil, "")
	if err != nil || got != recovery {
		t.Fatalf("environment = %q, %v", got, err)
	}
	t.Setenv("CCL_SYNC_RECOVERY_KEY", "")
	got, err = readRecoveryKey(strings.NewReader(recovery+"\n"), &bytes.Buffer{}, nil, "")
	if err != nil || got != recovery {
		t.Fatalf("stdin = %q, %v", got, err)
	}
}
