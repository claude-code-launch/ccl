package cmd

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

// TestProviderSaveNameNeverOverwritesAnotherProvider pins the S4 fix: a new
// provider whose generated placeholder became the models.dev catalog ID must
// not replace an existing provider of that name (and with it, its API key).
func TestProviderSaveNameNeverOverwritesAnotherProvider(t *testing.T) {
	existing := map[string]provider.Provider{
		"opencode-go":   {Name: "opencode-go"},
		"opencode-go-2": {Name: "opencode-go-2"},
		"work":          {Name: "work"},
	}
	for name, testCase := range map[string]struct {
		openedAs, name, want string
		collided             bool
	}{
		"catalog ID taken twice":         {"provider-abc234", "opencode-go", "opencode-go-3", true},
		"catalog ID free":                {"provider-abc234", "openrouter", "openrouter", false},
		"editing keeps its own name":     {"work", "work", "work", false},
		"new provider with a fresh name": {"oc", "oc", "oc", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, collided := providerSaveName(existing, testCase.openedAs, testCase.name)
			if got != testCase.want || collided != testCase.collided {
				t.Fatalf("providerSaveName(%q, %q) = %q, %t; want %q, %t",
					testCase.openedAs, testCase.name, got, collided, testCase.want, testCase.collided)
			}
		})
	}
}
