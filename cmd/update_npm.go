package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type npmInstallPaths struct {
	binary  string
	command string
}

// npmGlobalPaths uses npm's destination, not the executable that happens to be
// running. npm may remove or move that executable during an upgrade.
func npmGlobalPaths(root, goos string) (npmInstallPaths, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if !filepath.IsAbs(root) || filepath.Base(root) != "node_modules" {
		return npmInstallPaths{}, fmt.Errorf("unexpected npm global root %q", root)
	}
	prefix := filepath.Dir(root)
	command := filepath.Join(prefix, "ccl.cmd")
	if goos != "windows" {
		if filepath.Base(prefix) != "lib" {
			return npmInstallPaths{}, fmt.Errorf("unexpected npm global root %q", root)
		}
		command = filepath.Join(filepath.Dir(prefix), "bin", "ccl")
	}
	return npmInstallPaths{
		binary:  filepath.Join(root, "@claudecodelaunch", "ccl", "bin", "ccl.exe"),
		command: command,
	}, nil
}

func npmUpdateArgs(version string) []string {
	return []string{"install", "-g", "@claudecodelaunch/ccl@" + strings.TrimPrefix(canonicalReleaseTag(version), "v"), "--include=optional", "--prefer-online"}
}

func updateViaNpm(version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "npm", "root", "-g").Output()
	if err != nil {
		return fmt.Errorf("find npm's global install destination: %w", err)
	}
	paths, err := npmGlobalPaths(string(out), runtime.GOOS)
	if err != nil {
		return err
	}
	args := npmUpdateArgs(version)
	fmt.Printf("Updating via npm... Running 'npm %s'\n", strings.Join(args, " "))
	recovered, err := completeNpmUpdate(
		func() error { return runUpdateCommand("npm", args...) },
		func() error { return verifyNpmDestination(paths, version) },
		func() error {
			if runtime.GOOS == "windows" {
				return fmt.Errorf("Windows cannot repair the running executable in place")
			}
			fmt.Println("The npm-installed command is unusable; downloading the release binary directly...")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := os.MkdirAll(filepath.Dir(paths.binary), 0o755); err != nil {
				return err
			}
			if err := selfUpdateTo(ctx, version, paths.binary); err != nil {
				return err
			}
			return restoreNpmCommand(paths)
		},
	)
	if err != nil {
		return fmt.Errorf("npm update failed: %w; retry with npm %s", err, strings.Join(args, " "))
	}
	if recovered {
		fmt.Printf("\nRecovered ccl %s with the release binary; npm installation did not complete.\n", canonicalReleaseTag(version))
		fmt.Println("To repair the npm package later: npm " + strings.Join(args, " "))
	} else {
		fmt.Printf("\n🎉 Successfully updated ccl to %s via npm!\n", canonicalReleaseTag(version))
	}
	return nil
}

// completeNpmUpdate verifies even after npm exits nonzero: postinstall can fail
// after npm removed the old platform package. A recovery must itself be verified
// before reporting a working command, and must not be called an npm success.
func completeNpmUpdate(install, verify, recover func() error) (bool, error) {
	installErr := install()
	verifyErr := verify()
	if verifyErr == nil {
		return false, installErr
	}
	if err := recover(); err != nil {
		return false, errors.Join(installErr, verifyErr, fmt.Errorf("binary recovery failed: %w", err))
	}
	if err := verify(); err != nil {
		return false, fmt.Errorf("verify recovered binary: %w", err)
	}
	return true, nil
}

func verifyNpmDestination(paths npmInstallPaths, want string) error {
	if err := verifyNpmInstall(paths.binary, want); err != nil {
		return err
	}
	info, err := os.Stat(paths.command)
	if err != nil {
		return fmt.Errorf("npm command %s is missing: %w", paths.command, err)
	}
	if info.IsDir() {
		return fmt.Errorf("npm command %s is a directory", paths.command)
	}
	if runtime.GOOS != "windows" {
		target, err := filepath.EvalSymlinks(paths.command)
		if err != nil {
			return err
		}
		binary, err := filepath.EvalSymlinks(paths.binary)
		if err != nil {
			return err
		}
		if target != binary {
			return fmt.Errorf("%s points to another installation", paths.command)
		}
	}
	return nil
}

func verifyNpmInstall(binary, want string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "version").Output()
	if err != nil {
		return fmt.Errorf("npm-installed %s does not run: %w", binary, err)
	}
	got := strings.TrimSpace(string(out))
	if !strings.HasPrefix(got, "ccl version ") || canonicalReleaseTag(strings.TrimPrefix(got, "ccl version ")) != canonicalReleaseTag(want) {
		return fmt.Errorf("npm-installed %s reports %q, expected ccl version %s", binary, got, canonicalReleaseTag(want))
	}
	return nil
}

// npm can remove its command symlink during a failed install. Restore only an
// absent link; never overwrite another installation that owns the command.
func restoreNpmCommand(paths npmInstallPaths) error {
	if target, err := filepath.EvalSymlinks(paths.command); err == nil {
		binary, err := filepath.EvalSymlinks(paths.binary)
		if err != nil {
			return err
		}
		if target == binary {
			return nil
		}
		return fmt.Errorf("%s points to another installation", paths.command)
	}
	if _, err := os.Lstat(paths.command); err == nil || !os.IsNotExist(err) {
		return fmt.Errorf("cannot restore existing npm command %s", paths.command)
	}
	if err := os.MkdirAll(filepath.Dir(paths.command), 0o755); err != nil {
		return err
	}
	target, err := filepath.Rel(filepath.Dir(paths.command), paths.binary)
	if err != nil {
		return err
	}
	return os.Symlink(target, paths.command)
}
