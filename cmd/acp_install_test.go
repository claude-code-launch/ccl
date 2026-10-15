package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolveACPAdapterPrefersExplicitThenManagedThenPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CCL_ACP_ADAPTER", "")
	if _, err := resolveACPAdapter(""); err == nil || !strings.Contains(err.Error(), "ccl acp install") {
		t.Fatalf("missing adapter error = %v", err)
	}
	dir, _ := acpAdapterDir()
	managed := managedACPAdapter(dir)
	writeExecutable(t, managed)
	if got, err := resolveACPAdapter(""); err != nil || got != managed {
		t.Fatalf("managed adapter = %q, %v", got, err)
	}
	fromEnv := filepath.Join(t.TempDir(), filepath.Base(managed))
	writeExecutable(t, fromEnv)
	t.Setenv("CCL_ACP_ADAPTER", fromEnv)
	if got, err := resolveACPAdapter(""); err != nil || got != fromEnv {
		t.Fatalf("CCL_ACP_ADAPTER = %q, %v", got, err)
	}
	if got, err := resolveACPAdapter(managed); err != nil || got != managed {
		t.Fatalf("--adapter = %q, %v", got, err)
	}
}

func TestResolveNodeSkipsTooOldVersions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	name := "node"
	if runtime.GOOS == "windows" {
		name = "node.exe"
	}
	onPath := filepath.Join(t.TempDir(), name)
	writeExecutable(t, onPath)
	t.Setenv("PATH", filepath.Dir(onPath))
	recorded := filepath.Join(t.TempDir(), name)
	writeExecutable(t, recorded)
	dir, _ := acpAdapterDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node-path"), []byte(recorded+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{onPath: "v20.11.0\n", recorded: "v24.16.0\n"}
	previous := nodeVersionOutput
	nodeVersionOutput = func(node string) (string, error) { return versions[node], nil }
	t.Cleanup(func() { nodeVersionOutput = previous })
	if got, err := resolveNode(); err != nil || got != recorded {
		t.Fatalf("node = %q, %v; want the recorded Node.js 24", got, err)
	}
	versions[recorded] = "v18.0.0"
	if _, err := resolveNode(); err == nil || !strings.Contains(err.Error(), "v20.11.0") || !strings.Contains(err.Error(), ">=22") {
		t.Fatalf("old node error = %v", err)
	}
}

func TestPrependPathKeepsTheEntryName(t *testing.T) {
	sep := string(os.PathListSeparator)
	got := prependPath([]string{"A=1", "Path=old"}, "node-dir")
	if got[1] != "Path=node-dir"+sep+"old" {
		t.Fatalf("PATH entry = %q", got[1])
	}
	if got := prependPath([]string{"A=1"}, "node-dir"); got[1] != "PATH=node-dir" {
		t.Fatalf("missing PATH = %q", got)
	}
}
