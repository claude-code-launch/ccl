package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/haiboyuwen/claude-code-launch/internal/protocol"
	"github.com/haiboyuwen/claude-code-launch/internal/provider"
	"github.com/haiboyuwen/claude-code-launch/internal/proxy"
)

// ─────────────────────────────────────────────────────────────────────────────
// Model tier classification
// ─────────────────────────────────────────────────────────────────────────────

const (
	tierOpus   = "opus"
	tierSonnet = "sonnet"
	tierHaiku  = "haiku"
)

// tierKeywords maps each tier to its identifying keywords, checked in priority order.
var tierKeywords = [3]struct {
	tier     string
	keywords []string
}{
	{tierOpus, []string{"opus", "reasoning", "reasoner", "thinking", "pro", "max", "ultra"}},
	{tierHaiku, []string{"haiku", "mini", "nano", "air", "lite", "flash"}},
	{tierSonnet, []string{"sonnet", "turbo", "fast"}},
}

var versionRegex = regexp.MustCompile(`\d+(\.\d+)?`)

// determineModelTier classifies a model name into haiku / sonnet / opus.
func determineModelTier(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return tierSonnet
	}

	for _, entry := range tierKeywords {
		for _, kw := range entry.keywords {
			if strings.Contains(model, kw) {
				return entry.tier
			}
		}
	}

	// Fall back to embedded version number.
	switch v := extractVersion(model); {
	case v >= 5.0:
		return tierOpus
	case v >= 3.0:
		return tierSonnet
	default:
		return tierHaiku
	}
}

func extractVersion(model string) float64 {
	m := versionRegex.FindString(model)
	if m == "" {
		return 0
	}
	v, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return 0
	}
	return v
}

// ─────────────────────────────────────────────────────────────────────────────
// Model pool & routing
// ─────────────────────────────────────────────────────────────────────────────

// parseModelPool splits a comma-separated model string into trimmed, non-empty names.
func parseModelPool(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ModelRouter routes model requests to the first available model in a tier.
type ModelRouter struct {
	pools map[string][]string
}

// NewModelRouter groups a flat list of model names into per-tier pools.
func NewModelRouter(models []string) *ModelRouter {
	pools := map[string][]string{
		tierHaiku:  nil,
		tierSonnet: nil,
		tierOpus:   nil,
	}
	for _, m := range models {
		t := determineModelTier(m)
		pools[t] = append(pools[t], m)
	}
	return &ModelRouter{pools: pools}
}

// Select returns the first available model for tier, then tries each fallback in order.
func (r *ModelRouter) Select(tier string, fallbackTiers ...string) string {
	if models := r.pools[tier]; len(models) > 0 {
		return models[0]
	}
	for _, t := range fallbackTiers {
		if models := r.pools[t]; len(models) > 0 {
			return models[0]
		}
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────────────────
// Settings file
// ─────────────────────────────────────────────────────────────────────────────

// settingsJSON is the per-session settings file consumed by the Claude CLI (--settings).
type settingsJSON struct {
	Env                    map[string]string `json:"env"`
	HasCompletedOnboarding bool              `json:"hasCompletedOnboarding"`
	Model                  string            `json:"model,omitempty"`          // Lock to single model
	ModelOverrides         map[string]string `json:"modelOverrides,omitempty"` // Map standard IDs to provider-specific IDs
}

// buildEnv constructs the env-var overrides for a settings file.
func buildEnv(p provider.Provider, baseURL string, useProxy bool) map[string]string {
	env := make(map[string]string)

	if baseURL != "" {
		env["ANTHROPIC_BASE_URL"] = baseURL
	}

	switch {
	case useProxy:
		env["ANTHROPIC_API_KEY"] = "local-proxy-dummy-key"
	case p.APIKey != "":
		env["ANTHROPIC_API_KEY"] = p.APIKey
	}

	// 1. Custom model ID — bypasses ALL tier logic (Bedrock ARN, custom fine-tune, etc.)
	if p.CustomModelID != "" {
		env["CLAUDE_CODE_MODEL_ID"] = p.CustomModelID
	}

	// 2. Explicit tier model overrides (user-specified)
	if p.OpusModel != "" {
		env["ANTHROPIC_DEFAULT_OPUS_MODEL"] = p.OpusModel
		env["ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"] = p.OpusModel
	}
	if p.SonnetModel != "" {
		env["ANTHROPIC_DEFAULT_SONNET_MODEL"] = p.SonnetModel
		env["ANTHROPIC_DEFAULT_SONNET_MODEL_NAME"] = p.SonnetModel
	}
	if p.HaikuModel != "" {
		env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = p.HaikuModel
		env["ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME"] = p.HaikuModel
	}

	// 3. Effort level (low/medium/high)
	if p.EffortLevel != "" {
		env["CLAUDE_CODE_EFFORT_LEVEL"] = p.EffortLevel
	}

	// 4. Model pool routing (auto-assign tiers from comma-separated list)
	// Only used as fallback when explicit tier models aren't set
	if p.Model != "" && (p.OpusModel == "" || p.SonnetModel == "" || p.HaikuModel == "") {
		applyModelEnv(env, p.Model)
	}

	// Gateway discovery & traffic reduction (always enabled for multi-model setups)
	if p.Model != "" || p.CustomModelID != "" {
		env["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] = "1"
		env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
	}

	// Provider-level overrides take final precedence.
	for k, v := range p.Env {
		env[k] = v
	}
	return env
}

// applyModelEnv writes model-related env vars into env.
// A comma-separated model spec enables per-tier gateway routing;
// a single name sets ANTHROPIC_MODEL directly.
func applyModelEnv(env map[string]string, modelSpec string) {
	if !strings.Contains(modelSpec, ",") {
		env["ANTHROPIC_MODEL"] = modelSpec
		return
	}

	router := NewModelRouter(parseModelPool(modelSpec))
	opus := router.Select(tierOpus, tierSonnet, tierHaiku)
	sonnet := router.Select(tierSonnet, tierOpus, tierHaiku)
	haiku := router.Select(tierHaiku, tierSonnet)

	env["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] = "1"
	env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
	env["ANTHROPIC_MODEL"] = sonnet

	for _, kv := range []struct{ k, v string }{
		{"ANTHROPIC_DEFAULT_OPUS_MODEL", opus},
		{"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME", opus},
		{"ANTHROPIC_DEFAULT_SONNET_MODEL", sonnet},
		{"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME", sonnet},
		{"ANTHROPIC_DEFAULT_HAIKU_MODEL", haiku},
		{"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME", haiku},
	} {
		if kv.v != "" {
			env[kv.k] = kv.v
		}
	}
}

// writeSettingsFile serialises content to a temp JSON file and returns its path.
// The caller is responsible for removing the file when done.
func writeSettingsFile(content settingsJSON) (string, error) {
	data, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal settings: %w", err)
	}

	f, err := os.CreateTemp("", "claude_*_settings.json")
	if err != nil {
		return "", fmt.Errorf("create temp settings file: %w", err)
	}
	path := f.Name()

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("write settings file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close settings file: %w", err)
	}
	return path, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Provider context — shared setup for PreviewSettings and Run
// ─────────────────────────────────────────────────────────────────────────────

// providerContext holds resolved state needed to build a settings file.
type providerContext struct {
	provider provider.Provider // copy, not reference — safe to mutate
	baseURL  string
	useProxy bool
	srv      *proxy.Server // non-nil only when a local proxy was started
}

// setupProvider starts a proxy if needed and resolves the final model list.
// The caller must call cleanup() to release any proxy resources.
func setupProvider(p provider.Provider) (*providerContext, error) {
	// Make a COPY to avoid mutating the original provider (fixes mutation bug)
	providerCopy := p
	ctx := &providerContext{provider: providerCopy, useProxy: p.Type == "openai"}

	if ctx.useProxy {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv := proxy.NewServer("127.0.0.1:0", providerCopy, logger)
		if err := srv.Start(); err != nil {
			return nil, fmt.Errorf("start proxy: %w", err)
		}
		ctx.srv = srv
		ctx.baseURL = "http://" + srv.Addr()
	} else {
		ctx.baseURL = providerCopy.Endpoint
	}

	if err := ctx.resolveModel(); err != nil {
		ctx.cleanup()
		return nil, err
	}
	return ctx, nil
}

// resolveModel auto-discovers the model list when none is configured.
// Mutates the local copy only.
func (c *providerContext) resolveModel() error {
	if c.provider.Model != "" {
		return nil
	}
	if c.srv != nil {
		c.provider.Model = c.srv.AvailableModels()
		return nil
	}
	models, err := protocol.GetAnthropicModels(c.provider.Endpoint, c.provider.APIKey)
	if err != nil {
		return err
	}
	aliases := protocol.BatchToGatewayModelAlias(strings.Split(models, ","))
	c.provider.Model = strings.Join(aliases, ",")
	return nil
}

func (c *providerContext) cleanup() {
	if c.srv != nil {
		c.srv.Stop()
	}
}

func (c *providerContext) settings() settingsJSON {
	return settingsJSON{
		Env:                    buildEnv(c.provider, c.baseURL, c.useProxy),
		HasCompletedOnboarding: true,
		Model:                  c.provider.LockModel,
		ModelOverrides:         c.provider.ModelOverrides,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Public API
// ─────────────────────────────────────────────────────────────────────────────

// PreviewSettings returns the JSON that would be written to the settings temp file.
func PreviewSettings(p provider.Provider) string {
	ctx, err := setupProvider(p)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	defer ctx.cleanup()

	data, err := json.MarshalIndent(ctx.settings(), "", "  ")
	if err != nil {
		return fmt.Sprintf("Error: marshal settings: %v", err)
	}
	return string(data)
}

// Run launches the Claude CLI with settings derived from p, forwarding extra args.
func Run(p provider.Provider, args []string) error {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found in PATH (install with: npm install -g @anthropic-ai/claude-code): %w", err)
	}

	ctx, err := setupProvider(p)
	if err != nil {
		return err
	}
	defer ctx.cleanup()

	settingsPath, err := writeSettingsFile(ctx.settings())
	if err != nil {
		return fmt.Errorf("create settings file: %w", err)
	}
	defer os.Remove(settingsPath)

	fmt.Println("Using provider-specific claude config:", settingsPath)

	claudeArgs := append([]string{"--settings", settingsPath}, args...)

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", append([]string{"/c", claudePath}, claudeArgs...)...)
	} else {
		cmd = exec.Command(claudePath, claudeArgs...)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	return cmd.Run()
}
