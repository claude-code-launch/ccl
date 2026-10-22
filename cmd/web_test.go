package cmd

import "testing"

func TestWebCommandFlags(t *testing.T) {
	web := newWebCommand()
	flags := web.Flags()
	for _, name := range []string{"port", "host", "open", "no-open", "workspace", "token", "max-concurrency", "provider", "model", "effort"} {
		if flags.Lookup(name) == nil {
			t.Fatalf("ccl web has no --%s flag", name)
		}
	}
	// A loopback bind is the zero-config default; anything else needs a token.
	if got := flags.Lookup("host").DefValue; got != "127.0.0.1" {
		t.Fatalf("--host default = %q, want 127.0.0.1", got)
	}
	if got := flags.Lookup("port").DefValue; got != "8099" {
		t.Fatalf("--port default = %q, want 8099", got)
	}
	if got := flags.Lookup("open").DefValue; got != "true" {
		t.Fatalf("--open default = %q, want true", got)
	}
	if got := flags.Lookup("no-open").DefValue; got != "false" {
		t.Fatalf("--no-open default = %q, want false", got)
	}
}

// TestWebCommandNoOpenOverridesOpen covers the container case: --no-open must
// turn the browser off even though --open defaults to true.
func TestWebCommandNoOpenOverridesOpen(t *testing.T) {
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
		web := newWebCommand()
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
