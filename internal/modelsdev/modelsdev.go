// Package modelsdev fetches and parses the models.dev public model database
// (https://models.dev/api.json). It is a read-only wire-format layer: it knows
// how to decode the catalog and resolve each model's AI SDK provider, but it
// does not know anything about ccl's provider model or protocol dispatch.
//
// models.dev is the metadata source OpenCode itself uses to decide which AI SDK
// provider (and therefore which wire protocol) each model speaks. A provider
// carries a default npm package (e.g. "@ai-sdk/openai-compatible") and
// individual models may override it via their own provider.npm field.
package modelsdev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultAPIURL = "https://models.dev/api.json"

// Provider is one entry in the models.dev catalog, keyed by its id at the top
// level. Fields we do not consume (cost, modalities, reasoning options, …) are
// intentionally omitted.
type Provider struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	NPM    string           `json:"npm"`
	API    string           `json:"api"`
	Env    []string         `json:"env"`
	Doc    string           `json:"doc"`
	Models map[string]Model `json:"models"`
}

// Model is a single model under a provider. Status is optional ("deprecated",
// "beta", …); most models omit it.
type Model struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Status   string         `json:"status"`
	Provider *ModelProvider `json:"provider"`
	Limit    ModelLimit     `json:"limit"`
}

// ModelProvider carries a model-level npm override. Absent means the model
// inherits its provider's default npm.
type ModelProvider struct {
	NPM string `json:"npm"`
}

// ModelLimit carries the advertised context/output token limits.
type ModelLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// ResolvedNPM returns the AI SDK package that describes a model's protocol:
// the model-level provider.npm when present, otherwise the provider default.
func ResolvedNPM(p Provider, m Model) string {
	if m.Provider != nil {
		if npm := strings.TrimSpace(m.Provider.NPM); npm != "" {
			return npm
		}
	}
	return strings.TrimSpace(p.NPM)
}

// Fetch downloads and decodes the models.dev catalog. The top level is a map of
// provider id → provider with no envelope key, so it is decoded with a raw map
// and tolerant of entries that do not parse into a full provider (they are
// skipped rather than failing the whole fetch).
func Fetch(ctx context.Context) (map[string]Provider, error) {
	body, err := fetchBody(ctx)
	if err != nil {
		return nil, err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}

	providers := make(map[string]Provider, len(raw))
	for id, entry := range raw {
		var p Provider
		if err := json.Unmarshal(entry, &p); err != nil {
			continue
		}
		if strings.TrimSpace(p.API) == "" || len(p.Models) == 0 {
			continue
		}
		if p.ID == "" {
			p.ID = id
		}
		if p.Name == "" {
			p.Name = id
		}
		providers[id] = p
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("models.dev catalog contained no usable providers")
	}
	return providers, nil
}

func fetchBody(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, defaultAPIURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build models.dev request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ccl")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models.dev catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("fetch models.dev catalog: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read models.dev catalog: %w", err)
	}
	return body, nil
}
