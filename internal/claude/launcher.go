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
	"time"

	"github.com/haiboyuwen/claude-code-launch/internal/protocol"
	"github.com/haiboyuwen/claude-code-launch/internal/provider"
	"github.com/haiboyuwen/claude-code-launch/internal/proxy"
)

// determineModelTier matches a model name to one of the standard Claude tiers: sonnet, opus, or haiku.
func determineModelTier(model string) string {
	if model == "" {
		return "sonnet"
	}
	modelLower := strings.ToLower(model)
	if containsAny(modelLower, "opus", "reasoner", "reasoning", "o1", "o3", "pro") {
		return "opus"
	}
	if containsAny(modelLower, "haiku", "mini", "flash", "lite", "turbo", "fast") {
		return "haiku"
	}
	return "sonnet"
}

func containsAny(s string, keywords ...string) bool {
	for _, kw := range keywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// parseModelPool splits a comma-separated model string into individual model names.
func parseModelPool(modelPool string) []string {
	parts := strings.Split(modelPool, ",")
	var result []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

// mapPoolToTiers assigns each model in a pool to its Claude tier.
// The first model matching a tier claims it; unclaimed tiers fall back to the first pool model.
func mapPoolToTiers(models []string) map[string]string {
	tierMap := make(map[string]string)
	for _, model := range models {
		tier := determineModelTier(model)
		if _, exists := tierMap[tier]; !exists {
			tierMap[tier] = model
		}
	}
	if len(models) > 0 {
		for _, tier := range []string{"sonnet", "opus", "haiku"} {
			if _, exists := tierMap[tier]; !exists {
				tierMap[tier] = models[0]
			}
		}
	}
	return tierMap
}

// settingsJSON represents the per-session settings file passed via --settings.
type settingsJSON struct {
	Env                    map[string]string `json:"env"`
	HasCompletedOnboarding bool              `json:"hasCompletedOnboarding"`
}

// buildSettingsEnv constructs the environment variable overrides for the per-session settings file.
func buildSettingsEnv(p provider.Provider, baseURL string, needsProxy bool) map[string]string {
	env := make(map[string]string)

	if baseURL != "" {
		env["ANTHROPIC_BASE_URL"] = baseURL
	}

	if needsProxy {
		env["ANTHROPIC_API_KEY"] = "local-proxy-dummy-key"
	} else if p.APIKey != "" {
		env["ANTHROPIC_API_KEY"] = p.APIKey
	}

	if p.Model != "" {
		if strings.Contains(p.Model, ",") {
			modelList := parseModelPool(p.Model)
			tierMap := mapPoolToTiers(modelList)

			env["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] = "1"
			env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
			for tier, model := range tierMap {
				tierUpper := strings.ToUpper(tier)
				env["ANTHROPIC_DEFAULT_"+tierUpper+"_MODEL"] = model
				env["ANTHROPIC_DEFAULT_"+tierUpper+"_MODEL_NAME"] = model
			}

			if m, ok := tierMap["sonnet"]; ok {
				env["ANTHROPIC_MODEL"] = m
			} else if len(modelList) > 0 {
				env["ANTHROPIC_MODEL"] = modelList[0]
			}
		} else {
			env["ANTHROPIC_MODEL"] = p.Model
		}
	}

	for k, v := range p.Env {
		env[k] = v
	}

	return env
}

// PreviewSettings generates a settings file using the exact same pipeline as Run(),
// including starting the proxy to get the real dynamic URL. This ensures the preview
// matches what would be written to the actual temp file.
func PreviewSettings(p provider.Provider) string {
	var baseURL string
	var srv *proxy.Server

	needsProxy := p.Type == "openai"
	if needsProxy {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv = proxy.NewServer("127.0.0.1:0", p, logger)
		if err := srv.Start(); err != nil {
			return fmt.Sprintf("Error: failed to start proxy: %v", err)
		}
		defer srv.Stop()
		baseURL = "http://" + srv.Addr()
	} else {
		baseURL = p.Endpoint
	}

	if srv != nil && p.Model == "" {
		if discovered := srv.AvailableModels(); len(discovered) > 0 {
			p.Model = discovered
		}
	}

	env := buildSettingsEnv(p, baseURL, needsProxy)
	settings := settingsJSON{Env: env, HasCompletedOnboarding: true}

	path, err := writeSettingsFile(settings)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	return string(data)
}

func writeSettingsFile(content settingsJSON) (string, error) {
	data, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal settings JSON: %w", err)
	}

	f, err := os.CreateTemp("", "claude_*_settings.json")
	if err != nil {
		return "", fmt.Errorf("failed to create temp settings file: %w", err)
	}
	path := f.Name()

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("failed to write settings file: %w", err)
	}

	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("failed to close settings file: %w", err)
	}

	return path, nil
}

func Run(p provider.Provider, args []string) error {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI is not installed or not in PATH. Install with: npm install -g @anthropic-ai/claude-code. Error: %w", err)
	}

	var srv *proxy.Server
	var baseURL string

	needsProxy := p.Type == "openai"
	if needsProxy {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv = proxy.NewServer("127.0.0.1:0", p, logger)
		if err := srv.Start(); err != nil {
			return fmt.Errorf("failed to start local proxy: %w", err)
		}
		defer srv.Stop()
		baseURL = "http://" + srv.Addr()
		time.Sleep(200 * time.Millisecond)
	} else {
		baseURL = p.Endpoint
	}

	if p.Model == "" {
		if srv != nil {
			if discovered := srv.AvailableModels(); len(discovered) > 0 {
				p.Model = discovered
			}
		} else {
			m, err := protocol.GetAnthropicModels(p.Endpoint, p.APIKey)
			if err != nil {
				return err
			}
			tmp := protocol.BatchToGatewayModelAlias(strings.Split(m, ","))
			p.Model = strings.Join(tmp, ",")
		}

	}

	// Anthropic 直连只注入 base url 和 api key，其余走 buildSettingsEnv
	// var envOverrides map[string]string
	// if p.Type != "openai" {
	// 	envOverrides = make(map[string]string)
	// 	if p.APIKey != "" {
	// 		envOverrides["ANTHROPIC_API_KEY"] = p.APIKey
	// 	}
	// 	if p.Endpoint != "" && p.Endpoint != "https://api.anthropic.com" {
	// 		envOverrides["ANTHROPIC_BASE_URL"] = p.Endpoint
	// 	}
	// }

	claudeArgs := make([]string, 0, len(args)+2)
	// if p.Type == "openai" {
	// 	settings := settingsJSON{Env: buildSettingsEnv(p, baseURL, needsProxy)}
	// 	settingsPath, err := writeSettingsFile(settings)
	// 	if err != nil {
	// 		return fmt.Errorf("failed to create settings file: %w", err)
	// 	}
	// 	defer os.Remove(settingsPath)
	// 	fmt.Println("Using provider-specific claude config:", settingsPath)
	// 	claudeArgs = append(claudeArgs, "--settings", settingsPath)
	// }

	settings := settingsJSON{Env: buildSettingsEnv(p, baseURL, needsProxy)}
	settingsPath, err := writeSettingsFile(settings)
	if err != nil {
		return fmt.Errorf("failed to create settings file: %w", err)
	}
	defer os.Remove(settingsPath)

	fmt.Println("Using provider-specific claude config:", settingsPath)

	claudeArgs = append(claudeArgs, "--settings", settingsPath)
	claudeArgs = append(claudeArgs, args...)

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
	// if p.Type == "openai" {
	// 	cmd.Env = os.Environ()
	// } else {
	// 	filteredEnv := make([]string, 0, len(os.Environ())+len(envOverrides))
	// 	for _, e := range os.Environ() {
	// 		key := strings.SplitN(e, "=", 2)[0]
	// 		if _, overridden := envOverrides[key]; !overridden {
	// 			filteredEnv = append(filteredEnv, e)
	// 		}
	// 	}
	// 	for k, v := range envOverrides {
	// 		filteredEnv = append(filteredEnv, k+"="+v)
	// 	}
	// 	cmd.Env = filteredEnv
	// }

	return cmd.Run()
}
