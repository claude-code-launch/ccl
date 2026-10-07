package oauthproxy

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// The helpers below exercise the AutoClaw catalog and request normalization
// the way the runtime does — against the effective contract — without the
// production wrappers that only tests ever called.

func autoClawSupportsModelForTest(model string) bool {
	contract, _ := autoClawEffectiveContract()
	model = autoClawCanonicalModelWithCatalog(model, contract.models)
	_, ok := autoClawModelDefinition(contract.models, model)
	return ok
}

func normalizeAutoClawBodyForTest(raw []byte) ([]byte, error) {
	contract, _ := autoClawEffectiveContract()
	var initial map[string]any
	if err := json.Unmarshal(raw, &initial); err != nil {
		return nil, fmt.Errorf("decode AutoClaw Chat body: %w", err)
	}
	route, _ := initial["model"].(string)
	converted := &chatCompletionsConvertedRequest{
		anthropicAdapterRequest: anthropicAdapterRequest{upstreamModel: route},
		body:                    raw,
		model:                   route,
	}
	if err := normalizeAutoClawRequest(converted, contract); err != nil {
		return nil, err
	}
	return converted.body, nil
}

// autoClawAccessTokenForTest reads an imported credential through the same
// loader the runtime's authorizer uses.
func autoClawAccessTokenForTest(credentialFile string) (string, error) {
	authDir, err := ensureAuthDir()
	if err != nil {
		return "", err
	}
	credential, err := (&autoClawOAuthAuthorizer{path: filepath.Join(authDir, filepath.Base(credentialFile))}).load()
	if err != nil {
		return "", err
	}
	return credential.accessToken, nil
}
