package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestNpmUpdateArgsPinVersionAndIncludeOptional(t *testing.T) {
	want := []string{"install", "-g", "@claudecodelaunch/ccl@1.6.7", "--include=optional", "--prefer-online"}
	if got := npmUpdateArgs("v1.6.7"); !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestNpmGlobalPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "prefix", "lib", "node_modules")
	paths, err := npmGlobalPaths(root+"\n", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if paths.binary != filepath.Join(root, "@claudecodelaunch", "ccl", "bin", "ccl.exe") ||
		paths.command != filepath.Join(filepath.Dir(filepath.Dir(root)), "bin", "ccl") {
		t.Fatalf("unexpected paths: %+v", paths)
	}
	root = filepath.Join(t.TempDir(), "prefix", "node_modules")
	paths, err = npmGlobalPaths(root, "windows")
	if err != nil || paths.command != filepath.Join(filepath.Dir(root), "ccl.cmd") {
		t.Fatalf("Windows paths = %+v, %v", paths, err)
	}
	for _, root := range []string{"", "node_modules", t.TempDir()} {
		if _, err := npmGlobalPaths(root, "linux"); err == nil {
			t.Fatalf("invalid npm root %q accepted", root)
		}
	}
}

func TestCompleteNpmUpdateHandlesSilentAndPostinstallFailures(t *testing.T) {
	installFailure := errors.New("npm postinstall exited 1")
	missing := errors.New("native binary missing")
	for _, tc := range []struct {
		name       string
		installErr error
		verifyErr  error
		recoverErr error
		afterErr   error
		wantRepair bool
		wantOK     bool
	}{
		{name: "normal install", wantOK: true},
		{name: "silent optional skip", verifyErr: missing, wantRepair: true, wantOK: true},
		{name: "nonzero postinstall", installErr: installFailure, verifyErr: missing, wantRepair: true, wantOK: true},
		{name: "working binary but npm failed", installErr: installFailure},
		{name: "download failed", installErr: installFailure, verifyErr: missing, recoverErr: errors.New("download failed"), wantRepair: true},
		{name: "recovery still broken", verifyErr: missing, afterErr: missing, wantRepair: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installs, verifies, repairs := 0, 0, 0
			recovered, err := completeNpmUpdate(
				func() error { installs++; return tc.installErr },
				func() error {
					verifies++
					if verifies > 1 {
						return tc.afterErr
					}
					return tc.verifyErr
				},
				func() error { repairs++; return tc.recoverErr },
			)
			if (err == nil) != tc.wantOK || recovered != (tc.wantRepair && tc.wantOK) {
				t.Fatalf("recovered=%v, error=%v", recovered, err)
			}
			if installs != 1 || (repairs == 1) != tc.wantRepair {
				t.Fatalf("installs=%d verifies=%d repairs=%d", installs, verifies, repairs)
			}
			if tc.installErr != nil && tc.verifyErr == nil && !errors.Is(err, tc.installErr) {
				t.Fatal("npm failure was lost")
			}
		})
	}
}

// Build a native fixture rather than a Unix shell script: these checks also run
// on Windows, and must never execute the developer's actual ccl from PATH.
func TestVerifyNpmInstallChecksOnlyNpmDestination(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	binary := filepath.Join(dir, "ccl.exe")
	if err := os.WriteFile(source, []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"ccl version v1.6.7\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", binary, source).CombinedOutput(); err != nil {
		t.Fatalf("build version fixture: %v\n%s", err, out)
	}
	if err := verifyNpmInstall(binary, "1.6.7"); err != nil {
		t.Fatal(err)
	}
	if err := verifyNpmInstall(binary, "v1.6.8"); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("old version accepted: %v", err)
	}
	// Another working ccl on PATH cannot mask a missing npm target.
	t.Setenv("PATH", dir)
	if err := verifyNpmInstall(filepath.Join(dir, "missing.exe"), "v1.6.7"); err == nil {
		t.Fatal("missing npm binary was accepted")
	}
}

func TestRunUpdateNpmRepairsPackageWhenBinaryIsAlreadyCurrent(t *testing.T) {
	withReleaseAndNpm(t, 200, `{"tag_name":"v1.6.7"}`, 200, `{"version":"1.6.7"}`)
	previous := Version
	Version = "v1.6.7"
	t.Cleanup(func() { Version = previous })
	dir := t.TempDir()
	root := filepath.Join(dir, "prefix", "lib", "node_modules")
	if runtime.GOOS == "windows" {
		root = filepath.Join(dir, "prefix", "node_modules")
	}
	paths, err := npmGlobalPaths(root, runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	build := func(name, source string) string {
		t.Helper()
		file := filepath.Join(dir, name+".go")
		if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(dir, name)
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		if out, err := exec.Command("go", "build", "-o", binary, file).CombinedOutput(); err != nil {
			t.Fatalf("build npm fixture: %v\n%s", err, out)
		}
		return binary
	}
	native := build("native", "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"ccl version v1.6.7\") }\n")
	build("npm", `package main
import ("fmt"; "os"; "path/filepath"; "runtime"; "strings")
func main() {
    if len(os.Args) == 3 && os.Args[1] == "root" && os.Args[2] == "-g" {
        fmt.Println(os.Getenv("CCL_TEST_NPM_ROOT")); return
    }
    if err := os.WriteFile(os.Getenv("CCL_TEST_NPM_ARGS"), []byte(strings.Join(os.Args[1:], "\n")), 0600); err != nil { panic(err) }
    data, err := os.ReadFile(os.Getenv("CCL_TEST_NPM_NATIVE")); if err != nil { panic(err) }
    target := os.Getenv("CCL_TEST_NPM_TARGET")
    if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil { panic(err) }
    if err := os.WriteFile(target, data, 0755); err != nil { panic(err) }
    command := os.Getenv("CCL_TEST_NPM_COMMAND")
    if err := os.MkdirAll(filepath.Dir(command), 0755); err != nil { panic(err) }
    if runtime.GOOS == "windows" {
        if err := os.WriteFile(command, []byte("npm shim fixture"), 0755); err != nil { panic(err) }
    } else if err := os.Symlink(target, command); err != nil { panic(err) }
}
`)
	t.Setenv("PATH", dir)
	t.Setenv("CCL_TEST_NPM_ROOT", root)
	t.Setenv("CCL_TEST_NPM_NATIVE", native)
	t.Setenv("CCL_TEST_NPM_TARGET", paths.binary)
	t.Setenv("CCL_TEST_NPM_COMMAND", paths.command)
	argsPath := filepath.Join(dir, "npm-args")
	t.Setenv("CCL_TEST_NPM_ARGS", argsPath)
	if err := runUpdate("npm"); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil || string(args) != strings.Join(npmUpdateArgs("v1.6.7"), "\n") {
		t.Fatalf("npm repair was skipped or called incorrectly: %q, %v", args, err)
	}
}

func TestRestoreNpmCommandAfterInstallRemovedIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows recovery does not create symlinks")
	}
	root := filepath.Join(t.TempDir(), "lib", "node_modules")
	paths, err := npmGlobalPaths(root, runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.binary, []byte("recovered executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := restoreNpmCommand(paths); err != nil {
			t.Fatal(err)
		}
	}
	if data, err := os.ReadFile(paths.command); err != nil || string(data) != "recovered executable" {
		t.Fatalf("recovered command = %q, %v", data, err)
	}
	if err := os.Remove(paths.command); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.command, []byte("another install"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := restoreNpmCommand(paths); err == nil {
		t.Fatal("another installation was overwritten")
	}
}
