package oauthproxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeKiroCredentialMetadata(t *testing.T) {
	metadata := map[string]any{
		"accessToken":  "access",
		"refreshToken": "refresh",
		"authMethod":   "builder-id",
		"clientId":     "client",
		"userId":       "user-1",
	}
	normalizeKiroCredentialMetadata(metadata)

	for key, want := range map[string]string{
		"type":          ProviderKiro,
		"access_token":  "access",
		"refresh_token": "refresh",
		"auth_method":   "builder-id",
		"client_id":     "client",
		"user_id":       "user-1",
	} {
		if got, _ := metadata[key].(string); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
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
