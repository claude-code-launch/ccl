package cmd

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallConsentRequiresATerminal(t *testing.T) {
	// The prompt reads "(Y/n)", so an empty answer means yes — but only when a
	// person typed it. Without a terminal there is nobody to answer: an empty
	// string is EOF, and taking it as consent used to download and bash-execute
	// the installer unattended, e.g. for `ccl doctor > report.txt` or in CI.
	for _, answer := range []string{"", "y", "yes", "n", "no", "\n"} {
		if err := checkInstallConsent(answer, false); !errors.Is(err, errInstallNeedsTerminal) {
			t.Fatalf("answer %q with no terminal: err = %v, want errInstallNeedsTerminal", answer, err)
		}
	}
}

func TestInstallConsentOnATerminalDefaultsToYes(t *testing.T) {
	for _, answer := range []string{"", "y", "Y", "yes", "  ", "anything"} {
		if err := checkInstallConsent(answer, true); err != nil {
			t.Fatalf("answer %q on a terminal: err = %v, want the install to proceed", answer, err)
		}
	}
}

func TestInstallConsentHonorsAnExplicitNo(t *testing.T) {
	for _, answer := range []string{"n", "N", "no", " NO "} {
		err := checkInstallConsent(answer, true)
		if err == nil {
			t.Fatalf("answer %q: install proceeded despite being declined", answer)
		}
		if errors.Is(err, errInstallNeedsTerminal) {
			t.Fatalf("answer %q: reported as a missing terminal rather than a decline", answer)
		}
		// The user still needs to know how to get Claude Code.
		if !strings.Contains(err.Error(), "code.claude.com") {
			t.Fatalf("answer %q: decline message has no manual install pointer: %v", answer, err)
		}
	}
}

func TestManualInstallHintOffersTheOfficialInstallerAndNpm(t *testing.T) {
	for _, err := range []error{checkInstallConsent("", false), checkInstallConsent("n", true)} {
		for _, text := range []string{"claude.ai/install", "npm install -g @anthropic-ai/claude-code"} {
			if err == nil || !strings.Contains(err.Error(), text) {
				t.Fatalf("hint missing %q: %v", text, err)
			}
		}
	}
}

func TestInstalledClaudeOutsidePathIsUsedForThisLaunch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	var out strings.Builder
	if err := ensureInstalledClaudeOnPath(&out); err == nil {
		t.Fatal("a missing claude was reported as installed")
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	writeExecutable(t, filepath.Join(home, ".local", "bin", name))
	if err := ensureInstalledClaudeOnPath(&out); err != nil {
		t.Fatal(err)
	}
	if !IsInstalled() || !strings.Contains(out.String(), filepath.Join(home, ".local", "bin")) {
		t.Fatalf("claude not on PATH after install; output %q", out.String())
	}
}

// Cloud Shell: the installer put claude in ~/.local/bin, which a new shell's
// PATH lacks. A later ccl run must use it instead of offering to install again.
func TestLaterRunsFindClaudeInLocalBin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	if dir := useLocalClaudeInstall(); dir != "" || IsInstalled() {
		t.Fatalf("no install: dir %q", dir)
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	writeExecutable(t, filepath.Join(home, ".local", "bin", name))
	if dir := useLocalClaudeInstall(); dir != filepath.Join(home, ".local", "bin") || !IsInstalled() {
		t.Fatalf("local install not used: dir %q", dir)
	}
	if dir := useLocalClaudeInstall(); dir != "" {
		t.Fatalf("PATH changed twice: %q", dir)
	}
	var out strings.Builder
	printLocalClaudeHint(&out, filepath.Join(home, ".local", "bin"))
	if !strings.Contains(out.String(), `export PATH="$HOME/.local/bin:$PATH"`) {
		t.Fatalf("hint = %q", out.String())
	}
}
