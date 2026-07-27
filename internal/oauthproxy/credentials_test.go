package oauthproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestImportCredentialCreatesCanonicalIndependentCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, "whatever-name.json")
	raw := []byte(`{"type":"grok","access_token":"secret","refresh_token":"refresh","email":"Haiboyuwen@icloud.com"}`)
	if err := os.WriteFile(source, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	credential, target, err := ImportCredential(source, "")
	if err != nil {
		t.Fatal(err)
	}
	if credential.FileName != "xai-haiboyuwen@icloud.com.json" {
		t.Fatalf("canonical filename = %q", credential.FileName)
	}
	if credential.Backend != "xai" || credential.OAuthProvider != "grok" {
		t.Fatalf("credential identity = %+v", credential)
	}
	if target != filepath.Join(home, ".ccl", "auth", credential.FileName) {
		t.Fatalf("target = %q", target)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("target mode = %o", info.Mode().Perm())
	}
	stored, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(stored, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["type"] != "xai" {
		t.Fatalf("normalized type = %#v", metadata["type"])
	}
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != string(raw) {
		t.Fatal("source credential was modified")
	}
}

func TestImportCredentialUsesCopilotHintForCodexBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "github-device.json")
	if err := os.WriteFile(source, []byte(`{"type":"codex","access_token":"secret","email":"dev@example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credential, _, err := ImportCredential(source, "copilot")
	if err != nil {
		t.Fatal(err)
	}
	if credential.OAuthProvider != "copilot" || credential.Backend != "codex" {
		t.Fatalf("credential = %+v", credential)
	}
}

func TestListCredentialsIgnoresNestedDirectoriesAndInvalidJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(filepath.Join(authDir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "xai-a.json"), []byte(`{"type":"xai","email":"a@example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "broken.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "nested", "xai-b.json"), []byte(`{"type":"xai","email":"b@example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	credentials, err := ListCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 1 || credentials[0].FileName != "xai-a.json" {
		t.Fatalf("credentials = %+v", credentials)
	}
}
