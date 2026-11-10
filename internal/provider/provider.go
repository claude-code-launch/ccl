package provider

import (
	"fmt"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
)

// Claude Code env vars that ccl manages through Provider.Env. They live here so
// the launcher, the config TUI and the diagnostics all agree on the spelling.
const (
	// EnvMaxContextTokens is the fallback context size Claude Code assumes for a
	// model it does not recognize.
	EnvMaxContextTokens = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	// EnvAutoCompactWindow is the window Claude Code uses as the basis for its
	// auto-compact threshold. EnvAutoCompactPct selects the percentage of it.
	EnvAutoCompactWindow = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"

	// EnvAutoCompactPct is Claude Code's percentage-based auto-compact threshold.
	// It only ever lowers the trigger point, and Claude Code has repeatedly
	// ignored it when it arrives through the settings file, so ccl also exports it
	// to the child process environment.
	EnvAutoCompactPct = "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"

	Balanced500KMaxContextTokens  = "500000"
	Balanced500KAutoCompactWindow = "500000"
	Balanced500KAutoCompactPct    = "85"
	Balanced800KMaxContextTokens  = "800000"
	Balanced800KAutoCompactWindow = "800000"
	Balanced800KAutoCompactPct    = "85"

	// BalancedMaxContextTokens and companions retain the original 500K names for
	// callers that do not need to distinguish the two supported Balanced tiers.
	BalancedMaxContextTokens  = Balanced500KMaxContextTokens
	BalancedAutoCompactWindow = Balanced500KAutoCompactWindow
	BalancedAutoCompactPct    = Balanced500KAutoCompactPct
)

// ContextPreset identifies a provider-wide context configuration managed by ccl.
type ContextPreset uint8

const (
	ContextPresetDefault ContextPreset = iota
	ContextPresetBalanced500K
	ContextPresetBalanced800K
)

// ManagedContextEnvKeys are the context-sizing variables ccl forwards. They are
// exported to the Claude Code process as well as written to the settings file,
// because the settings-file channel has proven unreliable for them.
func ManagedContextEnvKeys() []string {
	return []string{EnvMaxContextTokens, EnvAutoCompactWindow, EnvAutoCompactPct}
}

// ContextPresetFromEnv recognizes the exact provider-wide context triplets that
// ccl supports. Missing, partial, and hand-written combinations resolve
// to Default; HasManagedContextEnv distinguishes those unsupported overrides
// from a genuinely empty Default configuration.
func ContextPresetFromEnv(env map[string]string) ContextPreset {
	maxContext := strings.TrimSpace(env[EnvMaxContextTokens])
	compactWindow := strings.TrimSpace(env[EnvAutoCompactWindow])
	compactPct := strings.TrimSpace(env[EnvAutoCompactPct])
	switch {
	case maxContext == Balanced500KMaxContextTokens &&
		compactWindow == Balanced500KAutoCompactWindow &&
		compactPct == Balanced500KAutoCompactPct:
		return ContextPresetBalanced500K
	case maxContext == Balanced800KMaxContextTokens &&
		compactWindow == Balanced800KAutoCompactWindow &&
		compactPct == Balanced800KAutoCompactPct:
		return ContextPresetBalanced800K
	default:
		return ContextPresetDefault
	}
}

// ContextPresetValues returns the exact managed environment values for a
// supported Balanced tier. Default has no values and returns ok=false.
func ContextPresetValues(preset ContextPreset) (maxContext, compactWindow, compactPct string, ok bool) {
	switch preset {
	case ContextPresetBalanced500K:
		return Balanced500KMaxContextTokens, Balanced500KAutoCompactWindow, Balanced500KAutoCompactPct, true
	case ContextPresetBalanced800K:
		return Balanced800KMaxContextTokens, Balanced800KAutoCompactWindow, Balanced800KAutoCompactPct, true
	default:
		return "", "", "", false
	}
}

// IsBalancedContextPreset reports whether env contains either exact Balanced
// tier managed by ccl.
func IsBalancedContextPreset(env map[string]string) bool {
	return ContextPresetFromEnv(env) != ContextPresetDefault
}

// HasManagedContextEnv reports whether any Claude Code context variable is set.
func HasManagedContextEnv(env map[string]string) bool {
	for _, key := range ManagedContextEnvKeys() {
		if strings.TrimSpace(env[key]) != "" {
			return true
		}
	}
	return false
}

type Provider struct {
	Name string `yaml:"name"`
	// Type selects the upstream protocol for a manual gateway. For OAuth
	// subscriptions it is only the local adapter compatibility type; the real
	// backend and authentication flow are selected by OAuthProvider.
	Type string `yaml:"type,omitempty"`
	// Endpoint is an HTTP API base for manual gateways and an oauth:// descriptor
	// for persisted subscriptions. Provider Session replaces the latter with a
	// loopback address only in its runtime copy.
	Endpoint string `yaml:"endpoint"`
	APIKey   string `yaml:"apikey"`
	// Model supplies the local model pool, routes, and aliases.
	Model string            `yaml:"model"`
	Env   map[string]string `yaml:"env,omitempty"`
	// AnthropicAuth is retained when editing saved legacy providers.
	AnthropicAuth string `yaml:"anthropicAuth,omitempty"`
	// OAuthProvider selects an embedded subscription runtime. Supported
	// values are gpt, gemini, grok, qoder, kimi, kiro, workbuddy,
	// workbuddycn, autoclaw, and cursor. Existing codex and xai bindings are
	// also accepted.
	OAuthProvider string `yaml:"oauthProvider,omitempty"`
	// OAuthAccountCredential binds this provider to a single credential file
	// (basename of the JSON under ~/.ccl/auth). Subscription runtimes require
	// this binding and load only that account.
	OAuthAccountCredential string `yaml:"oauthAccountCredential,omitempty"`

	// Model slots supply defaults and contribute to the runtime catalog.
	CustomModelID  string            `yaml:"customModelId,omitempty"`
	OpusModel      string            `yaml:"opusModel,omitempty"`
	SonnetModel    string            `yaml:"sonnetModel,omitempty"`
	HaikuModel     string            `yaml:"haikuModel,omitempty"`
	FableModel     string            `yaml:"fableModel,omitempty"`
	SubagentModel  string            `yaml:"subagentModel,omitempty"`
	ModelOverrides map[string]string `yaml:"modelOverrides,omitempty"`
	EffortLevel    string            `yaml:"effortLevel,omitempty"`
	// FastMode requests priority service from GPT's Codex Responses backend.
	FastMode bool `yaml:"fastMode,omitempty"`
	// StatuslineDisabled is a retained legacy setting.
	StatuslineDisabled bool `yaml:"statuslineDisabled,omitempty"`
	// ModelProtocols maps model IDs to the upstream protocol on a mixed gateway.
	// Legacy Messages entries are kept in configuration and excluded from routing.
	ModelProtocols map[string]string `yaml:"modelProtocols,omitempty"`
	// ModelsDevProvider is the models.dev catalog ID a "modelsdev" provider was
	// created from. It is separate from Name so the user can call the provider
	// whatever they like; empty (configs saved before this field) falls back to
	// Name, which used to be forced to the catalog ID.
	ModelsDevProvider string `yaml:"modelsDevProvider,omitempty"`
	// ContextPreset selects Codex context and compaction metadata.
	ContextPreset string `yaml:"contextPreset,omitempty"`
	// Ultracode is a retained legacy setting.
	Ultracode bool `yaml:"ultracode,omitempty"`
}

// IsClaudeAccount identifies a legacy profile with credentials outside the OAuth store.
func IsClaudeAccount(p Provider) bool {
	return strings.EqualFold(strings.TrimSpace(p.OAuthProvider), "claude")
}

// ModelsDevCatalogID returns the models.dev catalog ID behind p.
func ModelsDevCatalogID(p Provider) string {
	if id := strings.TrimSpace(p.ModelsDevProvider); id != "" {
		return id
	}
	return strings.TrimSpace(p.Name)
}

// DefaultProvider is the reserved selector for Grok Build's own
// login and settings. Selecting it
// loads no provider; it is never a key of Config.Providers.
const DefaultProvider = "default"

// IsDefaultProvider reports whether name selects Grok Build's own
// configuration.
func IsDefaultProvider(name string) bool {
	return strings.TrimSpace(name) == DefaultProvider
}

type Config struct {
	ActiveProvider string `yaml:"active_provider"`
	// Retain the legacy selection so upgrading cannot silently change accounts.
	LegacyClaudeDefault string `yaml:"default_claude_provider,omitempty"`
	Lang                string `yaml:"lang,omitempty"`
	// BypassMode automatically passes --always-approve to Grok Build. It is a global launcher setting.
	BypassMode bool `yaml:"bypass_mode,omitempty"`
	// ConfigVersion is the schema the file was last written with; see
	// config.CurrentVersion.
	ConfigVersion int `yaml:"config_version,omitempty"`
	// LogLevel is the threshold for ccl's per-session slog files: debug, info,
	// warn, error, or off. Config loading normalizes an omitted value to off.
	LogLevel  string              `yaml:"log_level,omitempty"`
	Providers map[string]Provider `yaml:"providers"`
}

// ValidateRuntime rejects retired providers without changing the saved entry
// or treating their credentials as belonging to another backend.
func ValidateRuntime(p Provider) error {
	if IsDefaultProvider(p.Name) {
		return fmt.Errorf("select a configured CCL provider for the Responses runtime")
	}
	if backend := strings.ToLower(strings.TrimSpace(p.OAuthProvider)); backend != "" {
		if _, ok := OAuthRuntimeType(backend); ok {
			return nil
		}
		switch backend {
		case "claude", "zed":
			return fmt.Errorf("OAuth provider %q has been retired; select another provider with ccl use", backend)
		default:
			return fmt.Errorf("unsupported OAuth provider %q", backend)
		}
	}
	switch {
	case IsOpenAICompatibleType(p.Type), IsModelsDevType(p.Type):
		return nil
	case IsAnthropicType(p.Type):
		return fmt.Errorf("provider %q uses the retired Anthropic Messages protocol; configure a Responses or Chat Completions endpoint with ccl set", p.Name)
	default:
		return fmt.Errorf("unsupported provider protocol %q", p.Type)
	}
}

// OAuthRuntimeType identifies the runtime backing a subscription.
func OAuthRuntimeType(oauthProvider string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(oauthProvider)) {
	case "gpt", "codex", "grok", "xai":
		return "openai_responses", true
	case "gemini", "kimi", "workbuddy", "workbuddycn":
		return "openai", true
	case "kiro", "qoder", "cursor":
		return "openai_responses", true
	case "autoclaw":
		return "autoclaw", true
	default:
		return "", false
	}
}

func IsOpenAIResponsesType(providerType string) bool {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	return providerType == "openai_responses" ||
		providerType == "openai-responses" ||
		providerType == "responses" ||
		providerType == "openai(responses)" ||
		providerType == "openai(agent)"
}

func IsOpenAICompatibleType(providerType string) bool {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	return providerType == "openai" ||
		providerType == "openai-chat" ||
		providerType == "openai(chat)" ||
		IsOpenAIResponsesType(providerType)
}

// IsAnthropicType identifies saved providers that use the retired Messages protocol.
func IsAnthropicType(providerType string) bool {
	trimmed := strings.TrimSpace(providerType)
	return strings.EqualFold(trimmed, "anthropic")
}

// IsModelsDevType reports whether the provider is a mixed-protocol models.dev
// gateway, whose per-model protocols live in ModelProtocols rather than a
// single provider-level Type.
func IsModelsDevType(providerType string) bool {
	return strings.EqualFold(strings.TrimSpace(providerType), "modelsdev")
}

// IsAutoClawType reports whether the provider is AutoClaw Code (ZCode coding
// plan). Its credential is resolved from ~/.ccl/auth and kept inside a local
// runtime that adds the managed proxy's desktop-compatible headers.
func IsAutoClawType(providerType string) bool {
	return strings.EqualFold(strings.TrimSpace(providerType), "autoclaw")
}

// ProtocolForAISdkNPM maps a models.dev AI SDK package to ccl's provider Type
// protocol. ok is false for an unknown package, in which case the model should
// be skipped by the caller.
func ProtocolForAISdkNPM(npm string) (string, bool) {
	switch strings.TrimSpace(npm) {
	case "@ai-sdk/openai":
		return "openai_responses", true
	case "@ai-sdk/openai-compatible":
		return "openai", true
	default:
		return "", false
	}
}

// RuntimeModelSpec returns every model ID that Claude Code may send for this
// provider. Embedded runtimes use the list to register model routes and aliases.
func RuntimeModelSpec(p Provider) string {
	models := make([]string, 0)
	seen := make(map[string]bool)
	add := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		key := strings.ToLower(model)
		if seen[key] {
			return
		}
		seen[key] = true
		models = append(models, model)
	}
	for _, model := range modelrouting.SplitCSV(p.Model) {
		add(model)
	}
	for _, model := range []string{
		p.CustomModelID,
		p.OpusModel,
		p.SonnetModel,
		p.HaikuModel,
		p.FableModel,
		p.SubagentModel,
	} {
		add(model)
	}
	overrideKeys := make([]string, 0, len(p.ModelOverrides))
	for key := range p.ModelOverrides {
		overrideKeys = append(overrideKeys, key)
	}
	sort.Strings(overrideKeys)
	for _, key := range overrideKeys {
		add(p.ModelOverrides[key])
	}
	return strings.Join(models, ",")
}

// ProtocolLabel returns the display name of a configured protocol.
func ProtocolLabel(providerType string) string {
	trimmed := strings.TrimSpace(providerType)
	switch {
	case trimmed == "":
		return ""
	case IsModelsDevType(trimmed):
		return "models.dev / auto"
	case IsAutoClawType(trimmed):
		return "openai-chat / autoclaw"
	case IsOpenAIResponsesType(trimmed):
		return "openai-responses"
	case IsAnthropicType(trimmed):
		return "anthropic / retired"
	default:
		return "openai-chat"
	}
}

// ProtocolLabelForProvider reports the user-facing protocol, including OAuth
// backends whose real behavior cannot be inferred from the internal Type field.
func ProtocolLabelForProvider(p Provider) string {
	switch strings.ToLower(strings.TrimSpace(p.OAuthProvider)) {
	case "claude", "zed":
		return strings.ToLower(strings.TrimSpace(p.OAuthProvider)) + " / retired"
	}
	return ProtocolLabel(p.Type)
}
