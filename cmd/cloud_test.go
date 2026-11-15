package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCloudLoginICloudCreatesAProfileWithItsKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", t.TempDir())
	command := newCloudLoginCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"icloud"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"key stored with the cloud profile", "A new encrypted sync profile was created."} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("login output missing %q: %q", want, output.String())
		}
	}
	keys, err := filepath.Glob(filepath.Join(home, ".ccl", "cloud", "profiles", "*", "key"))
	if err != nil || len(keys) != 1 {
		t.Fatalf("profile key glob = %v, %v", keys, err)
	}
	if info, err := os.Stat(keys[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("profile key mode/error = %v, %v", info, err)
	}
}

func TestCloudCommandTreeShape(t *testing.T) {
	cloudCommand := newCloudCommand()
	var names []string
	for _, child := range cloudCommand.Commands() {
		names = append(names, child.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "login,logout,pull,push,remote,status,tag" {
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
