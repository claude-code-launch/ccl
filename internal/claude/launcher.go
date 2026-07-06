package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/proxy"
)

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

	// 1. Custom model option shown as the persistent "Custom model" row in /model.
	if p.CustomModelID != "" {
		env["ANTHROPIC_CUSTOM_MODEL_OPTION"] = p.CustomModelID
		env["ANTHROPIC_CUSTOM_MODEL_OPTION_NAME"] = p.CustomModelID
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

	models := modelrouting.SplitCSV(modelSpec)
	opus := modelrouting.MapModel("claude-3-opus", "", models)
	sonnet := modelrouting.MapModel("claude-3-5-sonnet", "", models)
	haiku := modelrouting.MapModel("claude-3-5-haiku", "", models)

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
	ctx := &providerContext{provider: providerCopy, useProxy: provider.IsOpenAICompatibleType(p.Type)}

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
