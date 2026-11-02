package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestProxyMenuListsOnlyCompatibleNames(t *testing.T) {
	saveToggleFixture(t, &provider.Config{ActiveProvider: "work", Providers: map[string]provider.Provider{
		"work":     {Type: "openai", Endpoint: "https://private-endpoint.invalid/v1", APIKey: "private-key-must-not-appear", Model: "m"},
		"second":   {Type: "anthropic", Endpoint: "https://private-second.invalid", APIKey: "another-secret", Model: "m"},
		"personal": {OAuthProvider: "claude", OAuthAccountCredential: "private-profile"},
	}})
	command := newProxyCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"menu-configs"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Current   string   `json:"current"`
		Providers []string `json:"providers"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Current != "work" || strings.Join(result.Providers, ",") != "second,work" {
		t.Fatalf("wrong menu data: %s", output.String())
	}
	for _, secret := range []string{"private-key", "private-endpoint", "private-second", "another-secret", "private-profile", "personal"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("menu disclosed excluded account or connection data")
		}
	}
}
