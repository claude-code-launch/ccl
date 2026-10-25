package cmd

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestServeCommandFlags(t *testing.T) {
	web := newServeCommand()
	flags := web.Flags()
	for _, name := range []string{"socket", "port", "host", "open", "no-open", "workspace", "workspace-root", "lock-workspaces", "shell", "no-shell", "token", "max-concurrency", "provider", "model", "effort"} {
		if flags.Lookup(name) == nil {
			t.Fatalf("ccl serve has no --%s flag", name)
		}
	}
	// A loopback bind is the zero-config default; anything else needs a token.
	if got := flags.Lookup("host").DefValue; got != "127.0.0.1" {
		t.Fatalf("--host default = %q, want 127.0.0.1", got)
	}
	// Without --port the system picks a free port, so two servers never
	// collide; a fixed one has to be asked for.
	if got := flags.Lookup("port").DefValue; got != "0" {
		t.Fatalf("--port default = %q, want 0", got)
	}
	if got := flags.Lookup("open").DefValue; got != "true" {
		t.Fatalf("--open default = %q, want true", got)
	}
	if got := flags.Lookup("no-open").DefValue; got != "false" {
		t.Fatalf("--no-open default = %q, want false", got)
	}
	// The terminal is on unless --no-shell.
	if got := flags.Lookup("shell").DefValue; got != "true" {
		t.Fatalf("--shell default = %q, want true", got)
	}
}

// TestServeCommandNoOpenOverridesOpen covers the container case: --no-open must
// turn the browser off even though --open defaults to true.
func TestServeCommandNoOpenOverridesOpen(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{args: nil, want: true},
		{args: []string{"--open=false"}, want: false},
		{args: []string{"--no-open"}, want: false},
		{args: []string{"--no-open", "--open=true"}, want: false},
		{args: []string{"--open", "--no-open"}, want: false},
	} {
		web := newServeCommand()
		if err := web.Flags().Parse(test.args); err != nil {
			t.Fatalf("parse %v: %v", test.args, err)
		}
		open, _ := web.Flags().GetBool("open")
		noOpen, _ := web.Flags().GetBool("no-open")
		if got := open && !noOpen; got != test.want {
			t.Errorf("args %v: would open = %v, want %v", test.args, got, test.want)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "::1", "[::1]", "localhost", "LOCALHOST"} {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false, want true", host)
		}
	}
	// An empty host makes net.Listen bind every interface, so it is exposed.
	for _, host := range []string{"", "0.0.0.0", "::", "192.168.1.5", "example.com"} {
		if isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = true, want false", host)
		}
	}
}

func TestServeSocketPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, test := range []struct {
		args []string
		want string
		err  string
	}{
		{args: nil, want: ""},
		{args: []string{"--socket"}, want: filepath.Join(home, ".ccl", "serve", "ccl.sock")},
		// A bare --socket takes no value, so the path arrives as the argument.
		{args: []string{"--socket", "/tmp/a.sock"}, want: "/tmp/a.sock"},
		{args: []string{"--socket=/tmp/b.sock"}, want: "/tmp/b.sock"},
		{args: []string{"stray"}, err: "unexpected argument"},
		{args: []string{"--socket=/tmp/b.sock", "stray"}, err: "unexpected argument"},
		{args: []string{"--socket", "--port", "9000"}, err: "cannot be combined"},
		{args: []string{"--host", "127.0.0.1", "--socket"}, err: "cannot be combined"},
	} {
		serve := newServeCommand()
		if err := serve.Flags().Parse(test.args); err != nil {
			t.Fatalf("parse %v: %v", test.args, err)
		}
		socket, _ := serve.Flags().GetString("socket")
		got, err := socketPath(serve, socket, serve.Flags().Args())
		if test.err != "" {
			if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Errorf("args %v: err = %v, want %q", test.args, err, test.err)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("args %v: socket = %q, %v; want %q", test.args, got, err, test.want)
		}
	}
}
