package cmd

import (
	"bytes"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestRunFastToggleActiveProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &provider.Config{
		ActiveProvider: "work",
		Providers: map[string]provider.Provider{
			"work": {
				Name:          "work",
				Type:          "openai_responses",
				OAuthProvider: "chatgpt",
				Endpoint:      "oauth://codex",
			},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runFast(&out, []string{"on"}); err != nil {
		t.Fatalf("runFast(on) error: %v", err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Providers["work"].FastMode {
		t.Fatalf("FastMode not enabled: %+v", loaded.Providers["work"])
	}
	if !bytes.Contains(out.Bytes(), []byte("Fast = on")) {
		t.Fatalf("output = %q", out.String())
	}

	out.Reset()
	if err := runFast(&out, []string{"off"}); err != nil {
		t.Fatalf("runFast(off) error: %v", err)
	}
	loaded, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Providers["work"].FastMode {
		t.Fatalf("FastMode still on: %+v", loaded.Providers["work"])
	}
}

func TestRunFastShowActiveProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &provider.Config{
		ActiveProvider: "work",
		Providers: map[string]provider.Provider{
			"work": {Name: "work", OAuthProvider: "chatgpt", FastMode: true},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runFast(&out, nil); err != nil {
		t.Fatalf("runFast() error: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Fast = on")) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunFastRejectsGemini(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &provider.Config{
		ActiveProvider: "g",
		Providers: map[string]provider.Provider{
			"g": {Name: "g", OAuthProvider: "gemini"},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := runFast(&bytes.Buffer{}, []string{"on"}); err == nil {
		t.Fatal("gemini fast toggle should fail")
	}
}

func TestRunFastRequiresActiveProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&provider.Config{Providers: map[string]provider.Provider{}}); err != nil {
		t.Fatal(err)
	}
	if err := runFast(&bytes.Buffer{}, []string{"on"}); err == nil {
		t.Fatal("expected error without active provider")
	}
}

func TestParseOnOff(t *testing.T) {
	on, ok := parseOnOff("on")
	if !ok || !on {
		t.Fatalf("on = %v %v", on, ok)
	}
	off, ok := parseOnOff("off")
	if !ok || off {
		t.Fatalf("off = %v %v", off, ok)
	}
	if _, ok := parseOnOff("maybe"); ok {
		t.Fatal("maybe should fail")
	}
}
