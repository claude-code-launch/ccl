package groklaunch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/providersession"
	"github.com/claude-code-launch/ccl/internal/slotrec"
)

// Launch is a prepared Grok Build process spec: its embedded loopback
// runtime, environment, and default model selection. The caller must Close it.
type Launch struct {
	Path         string
	AuthPath     string
	DefaultModel string
	Env          []string
	Session      *providersession.Session
}

// Close removes the per-session empty auth file and stops the embedded proxy.
// It is safe to call more than once.
func (l *Launch) Close() {
	if l == nil {
		return
	}
	if l.AuthPath != "" {
		_ = os.Remove(l.AuthPath)
		l.AuthPath = ""
	}
	if l.Session != nil {
		l.Session.Close()
	}
}

// Command returns a Grok Build process using this launch's path and env.
// Extra args are passed verbatim. Stdio is left unset.
func (l *Launch) Command(args ...string) *exec.Cmd {
	if l == nil {
		return nil
	}
	cmd := process.Command(l.Path, args...)
	cmd.Env = l.Env
	return cmd
}

func newGrokSessionName() string {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "grok_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "grok_" + hex.EncodeToString(raw)
}

func resolveGrokModels(p provider.Provider) (defaultModel, fastModel string) {
	models := modelrouting.SplitCSV(p.Model)
	var rec slotrec.Recommendation
	if len(models) > 0 {
		rec = slotrec.Recommend(provider.Provider{}, models, nil)
	}
	for _, candidate := range []string{p.CustomModelID, p.SonnetModel, p.OpusModel, p.HaikuModel, p.FableModel, rec.Sonnet, rec.Opus, rec.Haiku} {
		if m := strings.TrimSpace(candidate); m != "" {
			defaultModel = m
			break
		}
	}
	if defaultModel == "" && len(models) > 0 {
		defaultModel = strings.TrimSpace(models[0])
	}
	for _, candidate := range []string{p.HaikuModel, rec.Haiku, defaultModel} {
		if m := strings.TrimSpace(candidate); m != "" {
			fastModel = m
			break
		}
	}
	return defaultModel, fastModel
}

// ConfigOverlay renders model defaults without starting a runtime.
func ConfigOverlay(p provider.Provider) string {
	defaultModel, fastModel := resolveGrokModels(p)
	return buildConfigOverlay(p, defaultModel, fastModel)
}

func buildConfigOverlay(p provider.Provider, defaultModel, fastModel string) string {
	raw, _ := json.Marshal(modelOverlay(p, defaultModel, fastModel))
	return string(raw)
}

func modelOverlay(p provider.Provider, defaultModel, fastModel string) map[string]any {
	modelsCfg := make(map[string]any)
	if defaultModel != "" {
		modelsCfg["default"] = defaultModel
		modelsCfg["web_search"] = defaultModel
		modelsCfg["image_description"] = defaultModel
	}
	if fastModel != "" {
		modelsCfg["session_summary"] = fastModel
		modelsCfg["prompt_suggestion"] = fastModel
	}
	if effort := strings.TrimSpace(p.EffortLevel); effort != "" {
		modelsCfg["default_reasoning_effort"] = effort
	}
	if len(modelsCfg) > 0 {
		return map[string]any{"models": modelsCfg}
	}
	return map[string]any{}
}

var reservedEnv = map[string]struct{}{
	"GROK_MODELS_BASE_URL":          {},
	"GROK_MODELS_LIST_URL":          {},
	"GROK_CLI_CHAT_PROXY_BASE_URL":  {},
	"XAI_API_KEY":                   {},
	"GROK_CODE_XAI_API_KEY":         {},
	"GROK_AUTH":                     {},
	"GROK_AUTH_PATH":                {},
	"GROK_AUTH_PROVIDER_COMMAND":    {},
	"GROK_CONFIG":                   {},
	"GROK_CONFIG_PATH":              {},
	"GROK_DEFAULT_MODEL":            {},
	"GROK_WEB_SEARCH_MODEL":         {},
	"GROK_IMAGE_DESCRIPTION_MODEL":  {},
	"GROK_SESSION_SUMMARY_MODEL":    {},
	"GROK_PROMPT_SUGGESTIONS_MODEL": {},
}

// ReservedEnvKey reports whether Grok routing owns a variable.
func ReservedEnvKey(key string) bool {
	_, ok := reservedEnv[strings.ToUpper(key)]
	return ok
}

func buildGrokProcessEnv(inherited []string, p provider.Provider, configJSON, authPath string) []string {
	suppressed := make(map[string]struct{}, len(reservedEnv)+len(p.Env))
	for key := range reservedEnv {
		suppressed[key] = struct{}{}
	}
	for key := range p.Env {
		suppressed[key] = struct{}{}
	}
	env := make([]string, 0, len(inherited)+len(p.Env)+4)
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			env = append(env, entry)
			continue
		}
		if _, drop := suppressedProcessEnvKey(suppressed, key); drop {
			continue
		}
		env = append(env, entry)
	}
	for key, value := range p.Env {
		if ReservedEnvKey(key) {
			continue
		}
		env = append(env, key+"="+value)
	}
	if configJSON != "" {
		env = append(env, "GROK_CONFIG="+configJSON)
	}
	if authPath != "" {
		env = append(env, "GROK_AUTH_PATH="+authPath)
	}
	if p.Endpoint != "" {
		env = append(env, "GROK_MODELS_BASE_URL="+p.Endpoint)
	}
	if p.APIKey != "" {
		env = append(env, "XAI_API_KEY="+p.APIKey)
	}
	return env
}

// Prepare resolves the provider runtime and creates an isolated Grok launch.
func Prepare(ctx context.Context, grokPath string, p provider.Provider) (*Launch, error) {
	sweepStaleSessionFiles()

	sessionName := newGrokSessionName()
	if oauthproxy.LogConfigured() {
		oauthproxy.PruneSessionLogs(oauthproxy.DefaultLogMaxAge, oauthproxy.DefaultLogMaxFiles)
		if err := oauthproxy.SetLogLevel(oauthproxy.CurrentLogLevel(), oauthproxy.SessionLogPath(sessionName)); err != nil {
			return nil, fmt.Errorf("open session log: %w", err)
		}
		oauthproxy.LogInfoEvent("session_start", "session", sessionName, "agent", "grokbuild", "provider", p.Name,
			"oauth", p.OAuthProvider, "protocol", provider.ProtocolLabelForProvider(p))
	}
	return prepare(ctx, grokPath, p, sessionName, "")
}

// PrepareBrowser gives the ACP worker its own Grok profile and keeps the
// surrounding server's log sink. Browser workers always start in Ask mode.
func PrepareBrowser(ctx context.Context, grokPath string, p provider.Provider, profile string) (*Launch, error) {
	return prepare(ctx, grokPath, p, newGrokSessionName(), profile)
}

func prepare(ctx context.Context, grokPath string, p provider.Provider, sessionName, profile string) (*Launch, error) {
	session, err := providersession.Prepare(ctx, p)
	if err != nil {
		oauthproxy.LogErrorEvent("session_setup_failed", "session", sessionName, "agent", "grokbuild", "provider", p.Name,
			"oauth", p.OAuthProvider, "protocol", provider.ProtocolLabelForProvider(p), "error", err)
		return nil, err
	}
	oauthproxy.LogInfoEvent("provider_ready", "session", sessionName, "agent", "grokbuild", "provider", p.Name,
		"oauth", p.OAuthProvider, "protocol", provider.ProtocolLabelForProvider(p),
		"data_plane", "ccl_proxy", "upstream_errors_visible", true,
		"base", oauthproxy.SafeLogEndpoint(session.Provider.Endpoint))

	resolved := session.Provider
	provider.ApplyOAuthSlotDefaults(&resolved)
	models := session.Runtime.Models()
	provider.ClearUnavailablePreferredDefaults(&resolved, models)
	if provider.IsModelsDevType(resolved.Type) {
		// A saved pool can still contain models with a retired wire protocol.
		resolved.Model = strings.Join(models, ",")
		for _, slot := range []*string{&resolved.CustomModelID, &resolved.SonnetModel, &resolved.OpusModel, &resolved.HaikuModel, &resolved.FableModel} {
			if !hasModel(models, *slot) {
				*slot = ""
			}
		}
	}
	defaultModel, fastModel := resolveGrokModels(resolved)
	overlay := modelOverlay(resolved, defaultModel, fastModel)
	env := os.Environ()
	if profile != "" {
		overlay["ui"] = map[string]any{"permission_mode": "ask"}
		overlay["cli"] = map[string]any{"auto_update": false}
		env, resolved.Env = browserEnvironment(env, resolved.Env, profile)
	}
	configJSON, _ := json.Marshal(overlay) // The overlay contains only JSON primitives and maps.
	// Isolate Grok Build from any cached OIDC session in ~/.grok/auth.json.
	// Without this, Grok prefers its cached grok.com JWT over XAI_API_KEY on
	// inference requests, causing the CCL loopback runtime to reject the JWT
	// with 401 invalid_local_api_key.
	authPath, err := writeSettingsBytes([]byte("{}\n"), sessionName)
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("create grok auth file: %w", err)
	}

	return &Launch{
		Path:         grokPath,
		AuthPath:     authPath,
		DefaultModel: defaultModel,
		Env:          buildGrokProcessEnv(env, resolved, string(configJSON), authPath),
		Session:      session,
	}, nil
}

func browserEnvironment(inherited []string, overrides map[string]string, profile string) ([]string, map[string]string) {
	isGrokSetting := func(key string) bool { return strings.HasPrefix(strings.ToUpper(key), "GROK_") }
	env := make([]string, 0, len(inherited)+2)
	for _, entry := range inherited {
		key, _, _ := strings.Cut(entry, "=")
		if !isGrokSetting(key) {
			env = append(env, entry)
		}
	}
	filtered := make(map[string]string, len(overrides))
	for key, value := range overrides {
		if !isGrokSetting(key) {
			filtered[key] = value
		}
	}
	return append(env, "GROK_HOME="+profile, "GROK_SANDBOX=workspace"), filtered
}

// Run launches the Grok Build CLI with settings derived from p, forwarding extra args.
func Run(grokPath string, p provider.Provider, args []string) error {
	launch, err := Prepare(context.Background(), grokPath, p)
	if err != nil {
		return err
	}
	defer launch.Close()

	if launch.DefaultModel != "" {
		fmt.Fprintf(os.Stderr, "Using provider %s with Grok Build (default model: %s)\n", p.Name, launch.DefaultModel)
	} else {
		fmt.Fprintf(os.Stderr, "Using provider %s with Grok Build\n", p.Name)
	}

	cmd := launch.Command(args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	start := time.Now()
	runErr := RunCommand(cmd)

	if summary := oauthproxy.FormatUsageSummary(usageSnapshot(launch.Session.Runtime)); summary != "" {
		fmt.Fprintln(os.Stderr, "\n"+summary)
	}

	if oauthproxy.LogEnabled() {
		spec := provider.RuntimeModelSpec(p)
		modelCount := 0
		if spec != "" {
			modelCount = len(strings.Split(spec, ","))
		}
		outcome := "ok"
		if runErr != nil {
			outcome = runErr.Error()
		}
		oauthproxy.LogInfoEvent("session_exit", "agent", "grokbuild", "provider", p.Name, "oauth", p.OAuthProvider,
			"protocol", provider.ProtocolLabelForProvider(p), "base", oauthproxy.SafeLogEndpoint(launch.Session.Provider.Endpoint),
			"model_count", modelCount, "env_override", len(p.Env),
			"custom_model", p.CustomModelID, "fast", p.FastMode, "outcome", outcome,
			"duration", time.Since(start).Round(time.Millisecond))
	}
	return runErr
}

func suppressedProcessEnvKey(suppressed map[string]struct{}, key string) (string, bool) {
	if _, ok := suppressed[key]; ok {
		return key, true
	}
	for candidate := range suppressed {
		if sameEnvKey(candidate, key) {
			return candidate, true
		}
	}
	return "", false
}

func sameEnvKey(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

// RunCommand runs cmd while catching the signals that would
// otherwise kill ccl before its deferred cleanup (settings file, embedded
// runtime) can run, forwarding the ones Grok Build would not receive itself.
func RunCommand(cmd *exec.Cmd) error {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, launchSignals...)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-signals:
				if forwardLaunchSignal(sig) && cmd.Process != nil {
					_ = cmd.Process.Signal(sig)
				}
			case <-done:
				return
			}
		}
	}()
	return cmd.Wait()
}

func writeSettingsBytes(data []byte, session string) (string, error) {

	path := sessionSettingsPath(session)
	// O_EXCL: never reuse or overwrite another session's settings file.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create settings file: %w", err)
	}

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

func usageSnapshot(runtime *oauthproxy.Runtime) []oauthproxy.UsageModelTotals {
	totals, ok := runtime.Usage().Snapshot()
	if !ok {
		return nil
	}
	return totals
}

func hasModel(models []string, candidate string) bool {
	for _, model := range models {
		if strings.EqualFold(model, strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}
