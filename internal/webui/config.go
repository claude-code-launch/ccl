package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/claudeaccount"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// apiMaskSuffix is how many trailing characters of an API key /api/config
// returns. The key itself never leaves the machine.
const apiMaskSuffix = 4

// ConfigView is the JSON shape of ccl's own configuration.
type ConfigView struct {
	Active    ActiveView              `json:"active"`
	Lang      string                  `json:"lang"`
	Bypass    bool                    `json:"bypassMode"`
	SignOn    bool                    `json:"signOn"`
	LogLevel  string                  `json:"logLevel"`
	Default   DefaultView             `json:"default"`
	Providers map[string]ProviderView `json:"providers"`
	Order     []string                `json:"order"`
}

// ActiveView is the persisted selection pair.
type ActiveView struct {
	Normal string `json:"normal"`
	ACP    string `json:"acp"`
}

// DefaultView describes the reserved `default` selector. It is never a key of
// the providers map, cannot be edited, and always exists.
type DefaultView struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
	Active    bool   `json:"active"`
	Provider  string `json:"provider,omitempty"`
}

// ProviderView is one provider with its credential masked.
type ProviderView struct {
	Name           string            `json:"name"`
	Type           string            `json:"type"`
	Endpoint       string            `json:"endpoint"`
	APIKey         string            `json:"apiKey"`
	HasKey         bool              `json:"hasKey"`
	OAuthProvider  string            `json:"oauthProvider,omitempty"`
	OpusModel      string            `json:"opusModel,omitempty"`
	SonnetModel    string            `json:"sonnetModel,omitempty"`
	HaikuModel     string            `json:"haikuModel,omitempty"`
	FableModel     string            `json:"fableModel,omitempty"`
	CustomModelID  string            `json:"customModelId,omitempty"`
	SubagentModel  string            `json:"subagentModel,omitempty"`
	EffortLevel    string            `json:"effortLevel,omitempty"`
	AnthropicAuth  string            `json:"anthropicAuth,omitempty"`
	Models         string            `json:"models,omitempty"`
	ModelOverrides map[string]string `json:"modelOverrides,omitempty"`
	FastMode       bool              `json:"fastMode,omitempty"`
	ContextPreset  string            `json:"contextPreset,omitempty"`
	// ContextWindow is the window a Balanced preset caps every slot to; zero
	// for Default, where a slot's [1m] marker decides.
	ContextWindow int  `json:"contextWindow,omitempty"`
	Active        bool `json:"active"`
	ACPActive     bool `json:"acpActive"`
	// ModelChoices is what a conversation on this provider can pick.
	ModelChoices []ModelChoice `json:"modelChoices"`
}

// ProviderInput is the writable subset of a provider. Its fields are pointers
// so PATCH can distinguish "leave alone" (nil) from "clear" ("").
type ProviderInput struct {
	Name           *string            `json:"name"`
	Type           *string            `json:"type"`
	Endpoint       *string            `json:"endpoint"`
	APIKey         *string            `json:"apiKey"`
	OAuthProvider  *string            `json:"oauthProvider"`
	OpusModel      *string            `json:"opusModel"`
	SonnetModel    *string            `json:"sonnetModel"`
	HaikuModel     *string            `json:"haikuModel"`
	FableModel     *string            `json:"fableModel"`
	CustomModelID  *string            `json:"customModelId"`
	SubagentModel  *string            `json:"subagentModel"`
	EffortLevel    *string            `json:"effortLevel"`
	AnthropicAuth  *string            `json:"anthropicAuth"`
	Models         *string            `json:"models"`
	ModelOverrides *map[string]string `json:"modelOverrides"`
	FastMode       *bool              `json:"fastMode"`
	ContextPreset  *string            `json:"contextPreset"`
}

var errNoProvider = fmt.Errorf("provider not found")

// handleConfig serves GET /api/config and PUT /api/config.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		view, err := s.configView()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, view)
	case "PUT", "PATCH":
		var body struct {
			Active      *ActiveView `json:"active"`
			Provider    *string     `json:"activeProvider"`
			ACPProvider *string     `json:"acpProvider"`
			Lang        *string     `json:"lang"`
			BypassMode  *bool       `json:"bypassMode"`
			SignOn      *bool       `json:"signOn"`
			LogLevel    *string     `json:"logLevel"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.updateConfig(func(cfg *provider.Config) error {
			normal := body.Provider
			acp := body.ACPProvider
			if body.Active != nil {
				normal = &body.Active.Normal
				acp = &body.Active.ACP
			}
			if normal != nil {
				name := strings.TrimSpace(*normal)
				if err := validatorForConfig(cfg, name); err != nil {
					return err
				}
				cfg.ActiveProvider = name
			}
			if acp != nil {
				name := strings.TrimSpace(*acp)
				if err := validatorForConfig(cfg, name); err != nil {
					return err
				}
				cfg.ACPProvider = name
			}
			if body.Lang != nil {
				cfg.Lang = strings.TrimSpace(*body.Lang)
			}
			if body.BypassMode != nil {
				cfg.BypassMode = *body.BypassMode
			}
			if body.SignOn != nil {
				cfg.SignOn = *body.SignOn
			}
			if body.LogLevel != nil {
				level := strings.ToLower(strings.TrimSpace(*body.LogLevel))
				switch level {
				case "", "off", "debug", "info", "warn", "error":
					cfg.LogLevel = level
				default:
					return fmt.Errorf("logLevel: expected off, debug, info, warn or error, got %q", *body.LogLevel)
				}
			}
			return nil
		}); err != nil {
			writeConfigError(w, err)
			return
		}
		view, err := s.configView()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, view)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleConfigProviders serves POST /api/config/providers (create or overwrite).
func (s *Server) handleConfigProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" && r.Method != "PUT" {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var input struct {
		ProviderInput
		Name string `json:"name"`
	}
	if err := decodeBody(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := validateProviderName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var created bool
	if err := s.updateConfig(func(cfg *provider.Config) error {
		existing, exists := cfg.Providers[name]
		if input.OAuthProvider != nil && *input.OAuthProvider == "" && input.Type == nil {
			return fmt.Errorf("type is required: an OAuth provider needs its runtime type")
		}
		if !exists && (input.Type == nil || strings.TrimSpace(*input.Type) == "") {
			return fmt.Errorf("type is required for a new provider")
		}
		p := existing
		if !exists {
			p = provider.Provider{Name: name}
		}
		if err := applyProviderInput(&p, &input.ProviderInput, true); err != nil {
			return err
		}
		if strings.TrimSpace(p.Endpoint) == "" && strings.TrimSpace(p.OAuthProvider) == "" {
			return fmt.Errorf("endpoint is required")
		}
		p.Name = name
		cfg.Providers[name] = p
		created = !exists
		return nil
	}); err != nil {
		writeConfigError(w, err)
		return
	}
	view, err := s.configView()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, view)
}

// handleConfigProvider serves /api/config/providers/{name}[/test].
func (s *Server) handleConfigProvider(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/config/providers/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	name := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if provider.IsDefaultProvider(name) {
		writeError(w, http.StatusForbidden, "the default selector is reserved and cannot be edited")
		return
	}
	if action == "test" {
		if r.Method != "POST" {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleConfigProviderTest(w, r, name)
		return
	}
	if action != "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	switch r.Method {
	case "GET":
		cfg, err := config.Load()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		p, ok := cfg.Providers[name]
		if !ok {
			writeError(w, http.StatusNotFound, "provider not found")
			return
		}
		writeJSON(w, http.StatusOK, providerView(cfg, name, p))

	case "PATCH", "PUT":
		var input ProviderInput
		raw, err := readBody(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			writeError(w, http.StatusBadRequest, "decode request body: "+err.Error())
			return
		}
		// apiKey has three states: absent/"" leaves the stored key alone, null
		// clears it, and any other value replaces it. json.Unmarshal cannot tell
		// those apart through a *string, so the raw value decides.
		clearKey := false
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err == nil {
			if value, ok := fields["apiKey"]; ok {
				if string(value) == "null" {
					clearKey = true
					input.APIKey = nil
				} else if input.APIKey != nil && strings.TrimSpace(*input.APIKey) == "" {
					input.APIKey = nil
				}
			}
		}
		if err := s.updateConfig(func(cfg *provider.Config) error {
			p, ok := cfg.Providers[name]
			if !ok {
				return errNoProvider
			}
			if clearKey {
				p.APIKey = ""
			}
			if err := applyProviderInput(&p, &input, false); err != nil {
				return err
			}
			p.Name = name
			cfg.Providers[name] = p
			return nil
		}); err != nil {
			writeConfigError(w, err)
			return
		}
		view, err := s.configView()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, view)

	case "DELETE":
		if err := s.updateConfig(func(cfg *provider.Config) error {
			if _, ok := cfg.Providers[name]; !ok {
				return errNoProvider
			}
			if strings.TrimSpace(cfg.ActiveProvider) == name {
				return fmt.Errorf("provider %q is the active provider; switch first", name)
			}
			if strings.TrimSpace(cfg.ACPProvider) == name {
				return fmt.Errorf("provider %q is the ACP provider; switch first", name)
			}
			delete(cfg.Providers, name)
			return nil
		}); err != nil {
			writeConfigError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleConfigProviderTest probes a provider's endpoint once. Nothing is saved.
func (s *Server) handleConfigProviderTest(w http.ResponseWriter, r *http.Request, name string) {
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, ok := cfg.Providers[name]
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	var body struct {
		Endpoint *string `json:"endpoint"`
		APIKey   *string `json:"apiKey"`
		Type     *string `json:"type"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A test of unsaved form values must not need a save first; an empty key
	// means "use the stored one".
	if body.Endpoint != nil && strings.TrimSpace(*body.Endpoint) != "" {
		p.Endpoint = strings.TrimSpace(*body.Endpoint)
	}
	if body.APIKey != nil && strings.TrimSpace(*body.APIKey) != "" {
		p.APIKey = strings.TrimSpace(*body.APIKey)
	}
	if body.Type != nil && strings.TrimSpace(*body.Type) != "" {
		p.Type = strings.TrimSpace(*body.Type)
	}
	result := probeProvider(r.Context(), p)
	writeJSON(w, http.StatusOK, result)
}

// ProbeResult reports one connectivity check.
type ProbeResult struct {
	OK      bool     `json:"ok"`
	Status  int      `json:"status,omitempty"`
	Models  []string `json:"models,omitempty"`
	Message string   `json:"message,omitempty"`
	Elapsed int64    `json:"elapsedMs"`
}

// maxProbeModels bounds the model ids one probe returns.
const maxProbeModels = 2000

// probeProvider issues one /models request, the same metadata call `ccl set`
// uses for Auto Configure. It is bounded and never returns credentials.
func probeProvider(ctx context.Context, p provider.Provider) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	type outcome struct {
		infos []protocol.ModelInfo
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		var result outcome
		if provider.IsOpenAICompatibleType(p.Type) {
			result.infos, result.err = protocol.GetOpenAIModelInfos(p.Endpoint, p.APIKey)
		} else {
			result.infos, result.err = protocol.GetAnthropicModelInfosWithAuth(p.Endpoint, p.APIKey, p.AnthropicAuth)
		}
		done <- result
	}()
	select {
	case <-ctx.Done():
		return ProbeResult{OK: false, Message: "timed out", Elapsed: time.Since(start).Milliseconds()}
	case result := <-done:
		if result.err != nil {
			return ProbeResult{OK: false, Message: redactSecrets(result.err.Error()), Elapsed: time.Since(start).Milliseconds()}
		}
		models := make([]string, 0, len(result.infos))
		for _, info := range result.infos {
			models = append(models, info.ID)
		}
		// The provider editor offers these for its model slots.
		if len(models) > maxProbeModels {
			models = models[:maxProbeModels]
		}
		return ProbeResult{OK: true, Status: 200, Models: models, Elapsed: time.Since(start).Milliseconds()}
	}
}

func (s *Server) updateConfig(fn func(*provider.Config) error) error {
	return config.Update(fn)
}

func (s *Server) configView() (ConfigView, error) {
	cfg, err := config.Load()
	if err != nil {
		return ConfigView{}, err
	}
	view := ConfigView{
		Active:    ActiveView{Normal: cfg.ActiveProvider, ACP: cfg.EffectiveACPProvider()},
		Lang:      cfg.Lang,
		Bypass:    cfg.BypassMode,
		SignOn:    cfg.SignOn,
		LogLevel:  cfg.LogLevel,
		Default:   DefaultView{Name: provider.DefaultProvider, IsDefault: true, Active: provider.IsDefaultProvider(cfg.ActiveProvider), Provider: cfg.DefaultClaudeProvider},
		Providers: make(map[string]ProviderView, len(cfg.Providers)),
	}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		view.Providers[name] = providerView(cfg, name, cfg.Providers[name])
		view.Order = append(view.Order, name)
	}
	if view.Order == nil {
		view.Order = []string{}
	}
	return view, nil
}

// ModelChoice is one entry of a conversation's model picker. Value is passed to
// claude --model as is; Label is what the browser shows.
type ModelChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// modelChoices lists the models a turn can ask for. Tier slots are Claude Code
// aliases that ccl's launch environment maps to the provider's models. The
// custom model and the pool have no alias, so their ids are passed directly;
// the subagent slot only applies to subagents and is not offered.
func modelChoices(p provider.Provider) []ModelChoice {
	choices := []ModelChoice{}
	seen := map[string]bool{}
	add := func(value, label string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		choices = append(choices, ModelChoice{Value: value, Label: label})
	}
	for _, slot := range []struct{ alias, model string }{
		{"opus", p.OpusModel}, {"sonnet", p.SonnetModel}, {"haiku", p.HaikuModel}, {"fable", p.FableModel},
	} {
		if model := stripOneM(slot.model); model != "" {
			add(slot.alias, slot.alias+" · "+model)
		}
	}
	if custom := strings.TrimSpace(p.CustomModelID); custom != "" {
		add(custom, "custom · "+stripOneM(custom))
	}
	for _, model := range strings.Split(p.Model, ",") {
		if model = strings.TrimSpace(model); model != "" {
			add(model, stripOneM(model))
		}
	}
	return choices
}

func providerView(cfg *provider.Config, name string, p provider.Provider) ProviderView {
	return ProviderView{
		Name:           name,
		Type:           provider.EffectiveType(p),
		Endpoint:       p.Endpoint,
		APIKey:         maskKey(p.APIKey),
		HasKey:         strings.TrimSpace(p.APIKey) != "",
		OAuthProvider:  p.OAuthProvider,
		OpusModel:      p.OpusModel,
		SonnetModel:    p.SonnetModel,
		HaikuModel:     p.HaikuModel,
		FableModel:     p.FableModel,
		CustomModelID:  p.CustomModelID,
		SubagentModel:  p.SubagentModel,
		EffortLevel:    p.EffortLevel,
		AnthropicAuth:  p.AnthropicAuth,
		Models:         p.Model,
		ModelOverrides: p.ModelOverrides,
		FastMode:       p.FastMode,
		ContextPreset:  p.ContextPreset,
		ContextWindow:  presetContextWindow(p),
		Active:         cfg.ActiveProvider == name,
		ACPActive:      cfg.EffectiveACPProvider() == name,
		ModelChoices:   modelChoices(p),
	}
}

// presetContextWindow is the context size p's Balanced preset declares to
// Claude Code, or zero for Default.
func presetContextWindow(p provider.Provider) int {
	maxContext, _, _, ok := provider.ContextPresetValues(provider.ProviderContextPreset(p))
	if !ok {
		return 0
	}
	window, _ := strconv.Atoi(maxContext)
	return window
}

// maskKey keeps the last four characters; everything else is hidden.
func maskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	runes := []rune(key)
	if len(runes) <= apiMaskSuffix {
		return strings.Repeat("*", len(runes))
	}
	return "***" + string(runes[len(runes)-apiMaskSuffix:])
}

// applyProviderInput copies the fields present in input onto p. clearKey lets an
// empty apiKey nil out the credential; otherwise an empty string leaves it.
func applyProviderInput(p *provider.Provider, input *ProviderInput, clearKey bool) error {
	if input.Type != nil {
		p.Type = strings.TrimSpace(*input.Type)
	}
	if input.Endpoint != nil {
		p.Endpoint = strings.TrimSpace(*input.Endpoint)
	}
	if input.APIKey != nil {
		key := strings.TrimSpace(*input.APIKey)
		switch {
		case key == "" && clearKey:
			p.APIKey = ""
		case key != "":
			p.APIKey = key
		}
	} else if clearKey {
		p.APIKey = ""
	}
	if input.OAuthProvider != nil {
		p.OAuthProvider = strings.ToLower(strings.TrimSpace(*input.OAuthProvider))
	}
	if input.OpusModel != nil {
		p.OpusModel = strings.TrimSpace(*input.OpusModel)
	}
	if input.SonnetModel != nil {
		p.SonnetModel = strings.TrimSpace(*input.SonnetModel)
	}
	if input.HaikuModel != nil {
		p.HaikuModel = strings.TrimSpace(*input.HaikuModel)
	}
	if input.FableModel != nil {
		p.FableModel = strings.TrimSpace(*input.FableModel)
	}
	if input.CustomModelID != nil {
		p.CustomModelID = strings.TrimSpace(*input.CustomModelID)
	}
	if input.SubagentModel != nil {
		p.SubagentModel = strings.TrimSpace(*input.SubagentModel)
	}
	if input.Models != nil {
		p.Model = strings.TrimSpace(*input.Models)
	}
	if input.EffortLevel != nil {
		level := strings.ToLower(strings.TrimSpace(*input.EffortLevel))
		if level == "default" {
			level = ""
		}
		if level != "" && !containsString(EffortLevels, level) {
			return fmt.Errorf("effortLevel: expected one of %s, got %q", strings.Join(EffortLevels, ", "), *input.EffortLevel)
		}
		p.EffortLevel = level
	}
	if input.AnthropicAuth != nil {
		auth := strings.ToLower(strings.TrimSpace(*input.AnthropicAuth))
		if auth != "" && auth != "bearer" && auth != "x-api-key" {
			return fmt.Errorf("anthropicAuth: expected bearer or x-api-key, got %q", *input.AnthropicAuth)
		}
		p.AnthropicAuth = auth
	}
	if input.ModelOverrides != nil {
		if len(*input.ModelOverrides) == 0 {
			p.ModelOverrides = nil
		} else {
			p.ModelOverrides = *input.ModelOverrides
		}
	}
	if input.FastMode != nil {
		p.FastMode = *input.FastMode
	}
	if input.ContextPreset != nil {
		p.ContextPreset = strings.TrimSpace(*input.ContextPreset)
	}
	if p.Name == "" {
		p.Name = provider.DefaultProvider
	}
	if provider.IsClaudeAccount(*p) {
		return claudeaccount.ValidateProvider(*p)
	}
	return nil
}

func validateProviderName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if provider.IsDefaultProvider(name) {
		return fmt.Errorf("the name %q is reserved for Claude Code's own configuration", name)
	}
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_', char == '.':
		default:
			return fmt.Errorf("name may only contain letters, digits, '-', '_' and '.'")
		}
	}
	return nil
}

func validatorForConfig(cfg *provider.Config, name string) error {
	if name == "" {
		return fmt.Errorf("provider name is required")
	}
	if provider.IsDefaultProvider(name) {
		return nil
	}
	if _, ok := cfg.Providers[name]; !ok {
		return fmt.Errorf("provider %q not found; use ccl ls or ccl set", name)
	}
	return nil
}

func writeConfigError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case err == errNoProvider:
		writeError(w, http.StatusNotFound, "provider not found")
	case errorsIsConflict(err):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func errorsIsConflict(err error) bool {
	message := err.Error()
	return strings.Contains(message, "is the active provider") || strings.Contains(message, "is the ACP provider")
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func readBody(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	return data, nil
}
