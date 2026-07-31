package cmd

import (
	"errors"
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
