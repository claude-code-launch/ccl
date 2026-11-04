package proxymanager

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Exercise the production AppKit menu and its actual Objective-C callbacks.
// Fake commands record argv; no real provider/configuration is accessed.
func TestMacMenuBarNativeActions(t *testing.T) {
	if err := exec.Command("/bin/launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())).Run(); err != nil {
		t.Skip("no macOS GUI bootstrap session; CI still compiles the JXA script")
	}
	dir := t.TempDir()
	executable := filepath.Join(dir, "fake ccl")
	log := filepath.Join(dir, "menu actions")
	chosen := "second config \"quoted\" $(not-a-command)"
	config, _ := json.Marshal(map[string]any{"current": "first", "providers": []string{"first", chosen}})
	quoted := "'" + strings.ReplaceAll(string(config), "'", "'\"'\"'") + "'"
	script := "#!/bin/sh\n[ \"$1\" = codex ] || exit 2\ncase \"$2\" in\nmenu-configs) printf '%s' " + quoted + ";;\nmenu-switch) printf '%s' \"$4\" > \"$CCL_MENU_TEST_LOG.switch\";;\nstop) printf stop > \"$CCL_MENU_TEST_LOG.stop\";;\nesac\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCL_MENU_TEST_LOG", log)
	probe := strings.Replace(menuBarScript, "  app.run;", `  if (menu.itemAtIndex(2).state !== $.NSControlStateValueOn) throw new Error('current configuration is not checked');
  menu.performActionForItemAtIndex(3);
  menu.performActionForItemAtIndex(menu.numberOfItems - 1);
  $.NSRunLoop.currentRunLoop.runUntilDate($.NSDate.dateWithTimeIntervalSinceNow(0.5));`, 1)
	command := exec.Command("/usr/bin/osascript", "-l", "JavaScript", "-", executable, "first")
	command.Stdin = strings.NewReader(probe)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AppKit menu failed: %v\n%s", err, output.String())
		}
	case <-time.After(15 * time.Second):
		_ = command.Process.Kill()
		<-done
		t.Fatal("AppKit menu timed out")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		switched, _ := os.ReadFile(log + ".switch")
		stopped, _ := os.ReadFile(log + ".stop")
		if string(switched) == chosen && string(stopped) == "stop" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("native menu callbacks lost arguments: switch=%q stop=%q", switched, stopped)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
