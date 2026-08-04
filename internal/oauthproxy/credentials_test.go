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

func TestImportCredentialKeepsCopilotAsDistinctBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "github-device.json")
	if err := os.WriteFile(source, []byte(`{"type":"copilot","github_token":"secret","login":"octocat"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credential, _, err := ImportCredential(source, "")
	if err != nil {
		t.Fatal(err)
	}
	if credential.OAuthProvider != "copilot" || credential.Backend != "copilot" {
		t.Fatalf("credential = %+v", credential)
	}
	if credential.FileName != "copilot-octocat.json" {
		t.Fatalf("credential filename = %q", credential.FileName)
	}
}

func TestImportCredentialKeepsQoderAsDirectBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "qoder.json")
	if err := os.WriteFile(source, []byte(`{"type":"qoder","access_token":"secret","refresh_token":"refresh","user_id":"user-1","machine_id":"machine-1","email":"qoder@example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credential, _, err := ImportCredential(source, "")
	if err != nil {
		t.Fatal(err)
	}
	if credential.OAuthProvider != ProviderQoder || credential.Backend != ProviderQoder || credential.FileName != "qoder-qoder@example.com.json" {
		t.Fatalf("credential = %+v", credential)
	}
}

func TestImportCredentialRejectsCopilotHintForCodexBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "codex.json")
	if err := os.WriteFile(source, []byte(`{"type":"codex","access_token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ImportCredential(source, "copilot"); err == nil {
		t.Fatal("Codex credential should not be importable as Copilot")
	}
}

func TestImportCredentialNormalizesKiroIDEToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "kiro-auth-token.json")
	raw := []byte(`{
		"accessToken":"access",
		"refreshToken":"refresh",
		"expiresAt":"2026-07-29T12:00:00Z",
		"authMethod":"builder-id",
		"provider":"AWS",
		"clientId":"client",
		"clientSecret":"secret",
		"csrfToken":"csrf",
		"userId":"user-1",
		"visitorId":"visitor-1",
		"email":"dev@example.com"
	}`)
	if err := os.WriteFile(source, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	credential, target, err := ImportCredential(source, "")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Backend != "kiro" || credential.OAuthProvider != "kiro" {
		t.Fatalf("credential = %+v", credential)
	}
	if credential.FileName != "kiro-dev@example.com.json" {
		t.Fatalf("filename = %q", credential.FileName)
	}
	stored, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(stored, &metadata); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"type":          "kiro",
		"access_token":  "access",
		"refresh_token": "refresh",
		"auth_method":   "builder-id",
		"client_id":     "client",
		"client_secret": "secret",
		"csrf_token":    "csrf",
		"user_id":       "user-1",
		"visitor_id":    "visitor-1",
	} {
		if got, _ := metadata[key].(string); got != want {
			t.Fatalf("%s = %q, want %q; metadata=%#v", key, got, want, metadata)
		}
	}
}

func TestImportCredentialNormalizesKiroWebCookieFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "kiro-web-session.json")
	raw := []byte(`{
		"AccessToken":"access",
		"RefreshToken":"refresh",
		"Idp":"Google",
		"UserId":"user-1",
		"csrfToken":"csrf",
		"profileArn":"arn:aws:codewhisperer:us-east-1:123:profile/example"
	}`)
	if err := os.WriteFile(source, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	credential, target, err := ImportCredential(source, "")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Backend != ProviderKiro || credential.OAuthProvider != ProviderKiro {
		t.Fatalf("credential = %+v", credential)
	}
	stored, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(stored, &metadata); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"access_token":  "access",
		"refresh_token": "refresh",
		"provider":      "Google",
		"user_id":       "user-1",
		"csrf_token":    "csrf",
		"profile_arn":   "arn:aws:codewhisperer:us-east-1:123:profile/example",
	} {
		if got, _ := metadata[key].(string); got != want {
			t.Fatalf("%s = %q, want %q; metadata=%#v", key, got, want, metadata)
		}
	}
}

func TestKiroCredentialIdentityDoesNotUseSharedProfileARN(t *testing.T) {
	first := map[string]any{
		"type":        "kiro",
		"profile_arn": kiroBuilderProfileARN,
		"client_id":   "client-a",
	}
	second := map[string]any{
		"type":        "kiro",
		"profile_arn": kiroBuilderProfileARN,
		"client_id":   "client-b",
	}
	if got := credentialIdentity(first, []byte("a")); got != "client-a" {
		t.Fatalf("first identity = %q", got)
	}
	if got := credentialIdentity(second, []byte("b")); got != "client-b" {
		t.Fatalf("second identity = %q", got)
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
