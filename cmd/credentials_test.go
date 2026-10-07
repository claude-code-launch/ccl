package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func seedCredentials(t *testing.T, files ...string) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".ccl", "auth")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"type":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestProviderRemoveCredentialHandling(t *testing.T) {
	setup := func(t *testing.T) string {
		t.Setenv("HOME", t.TempDir())
		if err := config.Save(&provider.Config{ActiveProvider: "a", Providers: map[string]provider.Provider{
			"a":      {Name: "a", OAuthProvider: "gpt", OAuthAccountCredential: "gpt-a.json"},
			"shared": {Name: "shared", OAuthProvider: "gpt", OAuthAccountCredential: "gpt-s.json"},
			"copy":   {Name: "copy", OAuthProvider: "gpt", OAuthAccountCredential: "gpt-s.json"},
		}}); err != nil {
			t.Fatal(err)
		}
		return seedCredentials(t, "gpt-a.json", "gpt-s.json")
	}

	t.Run("-y alone keeps the credential", func(t *testing.T) {
		dir := setup(t)
		if err := runProviderRemove("a", true, false); err != nil {
			t.Fatal(err)
		}
		if !exists(filepath.Join(dir, "gpt-a.json")) {
			t.Fatal("-y deleted the credential without --purge")
		}
	})
	t.Run("--purge deletes an unused credential", func(t *testing.T) {
		dir := setup(t)
		if err := runProviderRemove("a", true, true); err != nil {
			t.Fatal(err)
		}
		if exists(filepath.Join(dir, "gpt-a.json")) {
			t.Fatal("--purge kept the unused credential")
		}
	})
	t.Run("--purge keeps a credential another provider uses", func(t *testing.T) {
		dir := setup(t)
		if err := runProviderRemove("copy", true, true); err != nil {
			t.Fatal(err)
		}
		if !exists(filepath.Join(dir, "gpt-s.json")) {
			t.Fatal("--purge deleted a credential still bound to another provider")
		}
	})
}

func TestOAuthPruneDeletesOnlyOrphans(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&provider.Config{Providers: map[string]provider.Provider{
		"a": {Name: "a", OAuthProvider: "zed", OAuthAccountCredential: "zed-a.json"},
	}}); err != nil {
		t.Fatal(err)
	}
	dir := seedCredentials(t, "zed-a.json", "orphan-1.json", "orphan-2.json")
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	// Unanswered confirmation (stdin is not a terminal) must not delete.
	if err := runOAuthPrune(&out, false); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "orphan-1.json")) {
		t.Fatal("prune deleted without confirmation")
	}
	if !bytes.Contains(out.Bytes(), []byte("orphan-1.json")) || bytes.Contains(out.Bytes(), []byte("zed-a.json")) {
		t.Fatalf("listing = %q", out.String())
	}

	out.Reset()
	if err := runOAuthPrune(&out, true); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"zed-a.json": true, "orphan-1.json": false, "orphan-2.json": false, "notes.txt": true, "logs": true,
	} {
		if got := exists(filepath.Join(dir, name)); got != want {
			t.Errorf("%s exists=%t, want %t", name, got, want)
		}
	}
}

func TestRemoveCredentialFileRefusesEscapes(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	if err := removeCredentialFile(dir, "link.json"); err == nil {
		t.Fatal("a symlinked credential was deleted")
	}
	if err := removeCredentialFile(dir, "../"+filepath.Base(outside)); err == nil {
		t.Fatal("a path escape was accepted")
	}
	if !exists(outside) {
		t.Fatal("a file outside the auth directory was deleted")
	}
}
