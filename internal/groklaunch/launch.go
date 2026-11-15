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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/providersession"
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

func resolveGrokDefaultModel(p provider.Provider) string {
	if def := provider.StripContextSuffix(provider.NormalizeModelID(p.DefaultModel)); def != "" {
		return def
	}
	for _, candidate := range modelrouting.SplitCSV(provider.RuntimeModelSpec(p)) {
		if m := provider.StripContextSuffix(candidate); m != "" {
			return m
		}
	}
	return ""
}

func resolveGrokDefaultModelWithRuntime(p provider.Provider, runtimeModels []string, modelNames map[string]string) string {
	if len(runtimeModels) == 0 {
		return resolveGrokDefaultModel(p)
	}
	lookupDisplay := func(key string) string {
		for k, v := range modelNames {
			if strings.EqualFold(strings.TrimSpace(k), key) ||
				strings.EqualFold(provider.StripContextSuffix(k), key) {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	candidates := make([]string, 0, 1+len(modelrouting.SplitCSV(provider.RuntimeModelSpec(p))))
	if def := provider.StripContextSuffix(provider.NormalizeModelID(p.DefaultModel)); def != "" {
		candidates = append(candidates, def)
	}
	candidates = append(candidates, modelrouting.SplitCSV(provider.RuntimeModelSpec(p))...)
	for _, rawCandidate := range candidates {
		candidate := provider.StripContextSuffix(rawCandidate)
		if candidate == "" {
			continue
		}
		for _, rm := range runtimeModels {
			stripped := provider.StripContextSuffix(rm)
			if stripped != "" && strings.EqualFold(stripped, candidate) {
				return stripped
			}
		}
		candDisplay := lookupDisplay(candidate)
		for _, rm := range runtimeModels {
			stripped := provider.StripContextSuffix(rm)
			if stripped == "" {
				continue
			}
			rmDisplay := lookupDisplay(stripped)
			if rmDisplay != "" && (strings.EqualFold(rmDisplay, candidate) || (candDisplay != "" && strings.EqualFold(rmDisplay, candDisplay))) {
				return stripped
			}
		}
	}
	for _, rm := range runtimeModels {
		if stripped := provider.StripContextSuffix(rm); stripped != "" {
			return stripped
		}
	}
	return resolveGrokDefaultModel(p)
}

func resolveGrokAllowedModels(p provider.Provider, extraModels ...string) []string {
	var allowed []string
	seen := make(map[string]struct{})
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		allowed = append(allowed, id)
	}
	for _, candidate := range modelrouting.SplitCSV(provider.RuntimeModelSpec(p)) {
		add(provider.StripContextSuffix(candidate))
		add(candidate)
	}
	for _, extra := range extraModels {
		add(provider.StripContextSuffix(extra))
		add(extra)
	}
	return allowed
}

// ConfigOverlay renders model defaults without starting a runtime.
func ConfigOverlay(p provider.Provider) string {
	return buildConfigOverlay(p, resolveGrokDefaultModel(p))
}

func buildConfigOverlay(p provider.Provider, defaultModel string) string {
	raw, _ := json.Marshal(modelOverlay(p, defaultModel))
	return string(raw)
}

// NormalizeReasoningEffort maps a CCL reasoning effort tier onto Grok Build's
// accepted ReasoningEffort enum (none, minimal, low, medium, high, xhigh, max),
// clamping Codex's "ultra" tier to "max" and dropping unrecognized values.
func NormalizeReasoningEffort(level string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(level)); normalized {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return normalized
	case "ultra":
		return "max"
	default:
		return ""
	}
}

func modelOverlay(p provider.Provider, defaultModel string, runtimeModels ...string) map[string]any {
	modelsCfg := make(map[string]any)
	if defaultModel != "" {
		modelsCfg["default"] = defaultModel
		modelsCfg["web_search"] = defaultModel
		modelsCfg["image_description"] = defaultModel
		modelsCfg["session_summary"] = defaultModel
		modelsCfg["prompt_suggestion"] = defaultModel
	}
	allowed := make([]string, 0, 1+len(runtimeModels))
	seen := make(map[string]struct{})
	if def := strings.TrimSpace(defaultModel); def != "" {
		allowed = append(allowed, def)
		seen[def] = struct{}{}
	}
	for _, id := range resolveGrokAllowedModels(p, runtimeModels...) {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		allowed = append(allowed, id)
	}
	if len(allowed) > 0 {
		modelsCfg["allowed_models"] = allowed
	}
	if effort := NormalizeReasoningEffort(p.EffortLevel); effort != "" {
		modelsCfg["default_reasoning_effort"] = effort
	}
	out := make(map[string]any)
	if len(modelsCfg) > 0 {
		out["models"] = modelsCfg
	}
	if defaultModel != "" || len(allowed) > 0 {
		out["features"] = map[string]any{
			"subagent_model_inheritance": true,
		}
	}
	return out
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

// LookPath locates the Grok Build executable in PATH or ~/.grok/bin.
func LookPath() (string, error) {
	path, err := exec.LookPath("grok")
	if err == nil {
		return path, nil
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		name := "grok"
		if runtime.GOOS == "windows" {
			name = "grok.exe"
		}
		if local, localErr := exec.LookPath(filepath.Join(home, ".grok", "bin", name)); localErr == nil {
			return local, nil
		}
	}
	return "", err
}

// NativeBrowserEnv builds the process environment for a browser worker running
// on Grok Build's own ~/.grok login.
func NativeBrowserEnv(inherited []string, permissionMode string) []string {
	_ = permissionMode
	env := make([]string, 0, len(inherited)+2)
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			env = append(env, entry)
			continue
		}
		switch strings.ToUpper(key) {
		case "GROK_CONFIG", "GROK_CONFIG_PATH", "GROK_SANDBOX", "GROK_DISABLE_AUTOUPDATER":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GROK_SANDBOX=workspace", "GROK_DISABLE_AUTOUPDATER=1")
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
	return prepare(ctx, grokPath, p, sessionName, false, "")
}

// PrepareBrowser gives the ACP worker its own Grok profile and keeps the
// surrounding server's log sink. Browser workers start in Ask mode by default.
func PrepareBrowser(ctx context.Context, grokPath string, p provider.Provider, profile string) (*Launch, error) {
	return PrepareBrowserWithMode(ctx, grokPath, p, profile, "ask")
}

// PrepareBrowserWithMode prepares an isolated Grok profile with an explicit permission mode.
func PrepareBrowserWithMode(ctx context.Context, grokPath string, p provider.Provider, profile, permissionMode string) (*Launch, error) {
	_ = permissionMode
	return prepare(ctx, grokPath, p, newGrokSessionName(), true, profile)
}

func prepare(ctx context.Context, grokPath string, p provider.Provider, sessionName string, browser bool, profile string) (*Launch, error) {
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
	var runtimeModels []string
	var runtimeNames map[string]string
	if session.Runtime != nil {
		runtimeModels = session.Runtime.Models()
		runtimeNames = session.Runtime.ModelDisplayNames()
	}
	if provider.IsModelsDevType(resolved.Type) && len(runtimeModels) > 0 {
		// A saved pool can still contain models with a retired wire protocol.
		resolved.Model = strings.Join(runtimeModels, ",")
	}
	defaultModel := resolveGrokDefaultModelWithRuntime(resolved, runtimeModels, runtimeNames)
	overlay := modelOverlay(resolved, defaultModel, runtimeModels...)
	env := os.Environ()
	if browser {
		env, resolved.Env = browserEnvironment(env, resolved.Env, profile)
	}
	var configJSON string
	if len(overlay) > 0 {
		raw, _ := json.Marshal(overlay) // The overlay contains only JSON primitives and maps.
		configJSON = string(raw)
	}
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
		Env:          buildGrokProcessEnv(env, resolved, configJSON, authPath),
		Session:      session,
	}, nil
}

var allowedBrowserGrokEnv = map[string]struct{}{
	"GROK_CLAUDE_SKILLS_ENABLED": {},
	"GROK_CURSOR_SKILLS_ENABLED": {},
	"GROK_SUBAGENTS":             {},
	"GROK_SUBAGENTS_MAX_DEPTH":   {},
	"GROK_MEMORY":                {},
}

func browserEnvironment(inherited []string, overrides map[string]string, profile string) ([]string, map[string]string) {
	keepKey := func(key string) bool {
		upper := strings.ToUpper(key)
		if !strings.HasPrefix(upper, "GROK_") {
			return true
		}
		if profile == "" && upper == "GROK_HOME" {
			return true
		}
		_, ok := allowedBrowserGrokEnv[upper]
		return ok
	}
	env := make([]string, 0, len(inherited)+3)
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || keepKey(key) {
			env = append(env, entry)
		}
	}
	filtered := make(map[string]string, len(overrides))
	for key, value := range overrides {
		if keepKey(key) {
			filtered[key] = value
		}
	}
	if profile != "" {
		env = append(env, "GROK_HOME="+profile)
	}
	return append(env, "GROK_SANDBOX=workspace", "GROK_DISABLE_AUTOUPDATER=1"), filtered
}

func shouldSilenceBanner(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "--json" || arg == "--output-format=json" {
			return true
		}
		if arg == "--output-format" && i+1 < len(args) && args[i+1] == "json" {
			return true
		}
	}
	return false
}

// Run launches the Grok Build CLI with settings derived from p, forwarding extra args.
func Run(grokPath string, p provider.Provider, args []string) error {
	launch, err := Prepare(context.Background(), grokPath, p)
	if err != nil {
		return err
	}
	defer launch.Close()

	quiet := shouldSilenceBanner(args)
	if !quiet {
		if launch.DefaultModel != "" {
			fmt.Fprintf(os.Stderr, "Using provider %s with Grok Build (default model: %s)\n", p.Name, launch.DefaultModel)
		} else {
			fmt.Fprintf(os.Stderr, "Using provider %s with Grok Build\n", p.Name)
		}
	}

	cmd := launch.Command(args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	start := time.Now()
	runErr := RunCommand(cmd)

	if !quiet {
		if summary := oauthproxy.FormatUsageSummary(usageSnapshot(launch.Session.Runtime)); summary != "" {
			fmt.Fprintln(os.Stderr, "\n"+summary)
		}
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
			"fast", p.FastMode, "outcome", outcome,
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
