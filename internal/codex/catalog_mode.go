package codex

import "fmt"

// CatalogMode chooses how Codex obtains catalog data. It does not describe
// whether a running client or its Desktop picker reloads that data.
// The zero value is unspecified for command inheritance and defaults to local.
type CatalogMode string

const (
	CatalogLocal  CatalogMode = "local"
	CatalogRemote CatalogMode = "remote"
)

func ParseCatalogMode(value string) (CatalogMode, error) {
	mode := CatalogMode(value)
	switch mode {
	case "", CatalogLocal, CatalogRemote:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid Codex catalog mode %q; use local or remote", value)
	}
}

func (mode CatalogMode) Effective() CatalogMode {
	if mode == "" {
		return CatalogLocal
	}
	return mode
}

// CatalogCapabilities reports CCL's supported client contract for the selected
// source. A false live-reload value means there is no verified guarantee; it
// does not assert that Codex has no background refresh mechanism.
type CatalogCapabilities struct {
	SupportsRemoteFetch bool `json:"supports_remote_fetch"`
	SupportsLiveReload  bool `json:"supports_live_reload"`
}

func (mode CatalogMode) Capabilities() CatalogCapabilities {
	return CatalogCapabilities{
		SupportsRemoteFetch: mode.Effective() == CatalogRemote,
		// No Desktop version has been qualified for immediate picker reload.
		// Remote transport, cache expiry and app-server refreshes are insufficient
		// evidence to make that promise to the product UI.
		SupportsLiveReload: false,
	}
}
