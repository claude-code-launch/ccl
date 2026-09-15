package cmd

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	tui "github.com/grindlemire/go-tui"
)

func TestProtocolDetectionRejectsFallbackPages(t *testing.T) {
	for _, body := range []string{"", "<html>app</html>", `{"ok":true}`, `{"data":[]}`, `{"name":"API gateway"}`, `{"id":"site-id"}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("protocol detection made a mutating request")
				}
				w.Write([]byte(body))
			}))
			defer s.Close()
			result := detectProtocolAndModelsDetailed(s.URL, "fake-key")
			if result.err == nil {
				t.Fatalf("generic fallback page detected: %+v", result)
			}
		})
	}
}

func TestAutoClawProviderIsNotUserSelectable(t *testing.T) {
	// An AutoClaw provider is created by ccl oauth/import and connects straight
	// to its fixed remote endpoint, so the manual protocol selector must not
	// offer to reinterpret it.
	p := providerFrom("autoclaw", "https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw", "autoclaw")
	p.OAuthProvider = "autoclaw"
	m := NewAdvancedConfigModel(&p)
	if m.canSelectCustomProtocol() {
		t.Fatal("AutoClaw provider unexpectedly has a manual protocol selector")
	}
	if got := m.getProtocol(); got != "openai-chat / autoclaw" {
		t.Fatalf("protocol row = %q", got)
	}
	if got := m.getProtocolFamily(); got != "OpenAI" {
		t.Fatalf("protocol family = %q", got)
	}
}

// `ccl oauth autoclaw` persists the managed catalog into the provider, and a
// subscription has no probe that would fill the pool later, so the picker has to
// open on the persisted pool. Skipping it for OAuth left the list empty.
func TestOAuthProviderOpensWithItsPersistedModelPool(t *testing.T) {
	catalog := oauthproxy.AutoClawModelIDs()
	p := providerFrom("autoclaw", "https://example.invalid/autoclaw-proxy/proxy/autoclaw", "autoclaw")
	p.OAuthProvider = "autoclaw"
	p.Model = strings.Join(catalog, ",")

	m := readyOAuthPage(NewAdvancedConfigModel(&p))
	if len(m.live().modelPool) != len(catalog) {
		t.Fatalf("model pool = %v, want the persisted AutoClaw catalog", m.live().modelPool)
	}

	m.cursor = m.mainRowIndex(rowOpus)
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if !m.filterFocused {
		t.Fatal("enter on a model row did not open the model picker")
	}
	view := renderView(t, m)
	for _, want := range catalog {
		if !strings.Contains(view, want) {
			t.Fatalf("model picker missing %q: %q", want, view)
		}
	}
	// The catalog's friendly names stay out of the rows: an "Auto (zai_auto)"
	// alias is what the picker used to print, and the ID alone is enough.
	if strings.Contains(view, "(zai_auto)") {
		t.Fatalf("model picker still renders the display alias: %q", view)
	}
}

// The status line is ccl's own, injected through --settings, which outranks the
// user's ~/.claude/settings.json. This row is the only place to opt out, so it
// has to be reachable and it has to write the persisted field.
func TestStatusLineRowRendersAndTogglesTheOptOut(t *testing.T) {
	p := providerFrom("qoder", "https://example.invalid", "anthropic")
	p.OAuthProvider = "qoder"
	m := readyOAuthPage(NewAdvancedConfigModel(&p))

	if view := renderView(t, m); !strings.Contains(view, locale.T("状态栏", "Status Line")) {
		t.Fatalf("Runtime section is missing the status line row: %q", view)
	}
	m.cursor = m.mainRowIndex(rowStatusline)
	if got := m.currentRow(); got != rowStatusline {
		t.Fatalf("status line row is not navigable: current row = %v", got)
	}
	m.handleKey(tui.KeyEvent{Key: tui.KeyRight})
	if !m.p.StatuslineDisabled {
		t.Fatal("right did not switch the status line off")
	}
	m.handleKey(tui.KeyEvent{Key: tui.KeyRight})
	if m.p.StatuslineDisabled {
		t.Fatal("right did not switch the status line back on")
	}
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if !m.p.StatuslineDisabled {
		t.Fatal("enter did not toggle the status line")
	}
}

// The reported repro: `ccl set sharedchat`, turn 1M off on a model, save, then
// reopen. The provider page re-probes on open, whose auto-recommendation used to
// re-enable the marker for every allowlisted model — so the window came back.
func TestReopeningTheProviderPageKeepsTheOneMOptOut(t *testing.T) {
	p := providerFrom("sharedchat", "https://example.invalid/v1", "openai")
	p.OpusModel, p.SonnetModel, p.HaikuModel = "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"
	p.Model = "gpt-5.6-sol,gpt-5.6-terra,gpt-5.6-luna"

	m := NewAdvancedConfigModel(&p)
	m.applyModelDetectionResult("openai", p.Model, "", m.live().probeEndpoint, nil)
	if m.live().oneMSlots["opus"] {
		t.Fatal("opening the page re-enabled 1M on a slot with no persisted marker")
	}
	applyCompactConfig(m.p, m.live().oneMSlots, m.live().compactPreset)
	if hasOneMSuffix(m.p.OpusModel) {
		t.Fatalf("save wrote a [1m] marker the user never asked for: %q", m.p.OpusModel)
	}

	// The opposite direction still works: a slot the user did open keeps it.
	m.live().oneMSlots["opus"] = true
	m.applyModelDetectionResult("openai", p.Model, "", m.live().probeEndpoint, nil)
	if !m.live().oneMSlots["opus"] {
		t.Fatal("re-detection dropped a 1M marker the user had set")
	}
}

// The panel opens before the subscription's runtime is up, so the Local Proxy
// row has to carry the wait: a spinner while the runtime starts, then the ready
// line. Starting it synchronously is what used to leave the terminal blank.
func TestOAuthLocalProxyRowTracksTheBackgroundRuntimeStart(t *testing.T) {
	p := providerFrom("qoder", "oauth://qoder", "anthropic")
	p.OAuthProvider = "qoder"
	p.Model = "dmodel"
	m := NewAdvancedConfigModel(&p)

	m.runtimeLoading = true
	view := renderView(t, m)
	label := locale.T("本地代理", "Local Proxy")
	starting := locale.T("启动中…", "Starting...")
	if !strings.Contains(view, label) || !strings.Contains(view, starting) {
		t.Fatalf("loading Local Proxy row = %q", view)
	}
	if fixture := view; !strings.Contains(fixture, spinnerAt(m.runtimeFrame)) {
		t.Fatalf("loading row has no spinner frame: %q", fixture)
	}
	// The page must not be editable, nor saveable, while the catalog is pending.
	if m.connectionReady() || m.canSave() {
		t.Fatalf("page editable before the runtime answered: ready=%t save=%t", m.connectionReady(), m.canSave())
	}

	m.handleOAuthRuntimeDone(oauthRuntimeDoneMsg{
		endpoint: "http://127.0.0.1:1234/v1",
		apiKey:   "session-key",
		models:   []string{"dmodel"},
		names:    map[string]string{"dmodel": "Runtime name"},
	})
	if m.runtimeLoading || !m.runtimeReady || m.runtimeErr != nil {
		t.Fatalf("runtime result not applied: loading=%t ready=%t err=%v", m.runtimeLoading, m.runtimeReady, m.runtimeErr)
	}
	if !m.connectionReady() || !m.canSave() {
		t.Fatalf("page still locked after the runtime came up: ready=%t save=%t", m.connectionReady(), m.canSave())
	}
	if !strings.Contains(m.modelSearchLabel("dmodel"), "Runtime name") {
		t.Fatalf("runtime catalog names were dropped: %q", m.modelSearchLabel("dmodel"))
	}
	if view := renderView(t, m); !strings.Contains(view, locale.T("已就绪（仅本次会话）", "Ready (this session only)")) {
		t.Fatalf("Local Proxy row did not switch to ready: %q", view)
	}

	// A failed start is reported and keeps the page unsaveable rather than
	// silently persisting a half-configured subscription.
	failed := readyOAuthPage(NewAdvancedConfigModel(&p))
	failed.runtimeLoading = true
	failed.handleOAuthRuntimeDone(oauthRuntimeDoneMsg{err: errors.New("no qoder credentials found")})
	if failed.runtimeReady || failed.canSave() {
		t.Fatalf("failed start left the page saveable: ready=%t save=%t", failed.runtimeReady, failed.canSave())
	}
	if view := renderView(t, failed); !strings.Contains(view, locale.T("启动失败（仅本次会话）", "Start failed (this session only)")) {
		t.Fatalf("failure not surfaced on the Local Proxy row: %q", view)
	}
}

// The whole point of the change: the start runs on its own goroutine, reports
// back through the channel, and teardown does not hang on it. A credential-less
// qoder fails fast, which is what makes this hermetic.
func TestBeginOAuthRuntimeReportsBackAndTearsDown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := providerFrom("qoder", "oauth://qoder", "anthropic")
	p.OAuthProvider = "qoder"
	m := NewAdvancedConfigModel(&p)
	m.beginOAuthRuntime(p)
	if !m.runtimeLoading {
		t.Fatal("the start did not register as in flight")
	}

	var msg oauthRuntimeDoneMsg
	select {
	case msg = <-m.oauthDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime start never reported back")
	}
	m.handleOAuthRuntimeDone(msg)
	if m.runtimeLoading {
		t.Fatal("the result did not clear the loading state")
	}
	if m.runtimeReady || m.runtimeErr == nil {
		t.Fatalf("a credential-less start reported ready: ready=%t err=%v", m.runtimeReady, m.runtimeErr)
	}

	// stopOAuthRuntime is deferred in RunProviderSet; it must return even when
	// the start already finished.
	done := make(chan struct{})
	go func() { m.stopOAuthRuntime(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stopOAuthRuntime did not return")
	}
	if m.oauthCancel == nil {
		t.Fatal("beginOAuthRuntime did not install a cancel func")
	}
}

// Qoder against a credential it cannot refresh: discovery fails and the backend
// substitutes a five-model compatibility list. Adopting that as the account's
// catalog is what replaced a saved 17-model pool with 5, and saving the page made
// it permanent. A fallback must not overwrite what is already there.
func TestFallbackCatalogDoesNotReplaceTheSavedModelPool(t *testing.T) {
	saved := []string{
		"auto", "ultimate", "performance", "efficient", "lite", "cmodel",
		"qmodel_38max", "qfmodel", "qmodel_latest", "qmodel", "kmodel_latest",
		"kmodel", "gmodel", "gfmodel", "dmodel", "dfmodel", "mmodel",
	}
	p := providerFrom("qoder", "oauth://qoder", "anthropic")
	p.OAuthProvider, p.Model = "qoder", strings.Join(saved, ",")
	m := NewAdvancedConfigModel(&p)
	before := append([]string(nil), m.live().modelPool...)
	if len(before) != len(saved) {
		t.Fatalf("page did not open on the saved pool: %v", before)
	}

	m.runtimeLoading = true
	m.handleOAuthRuntimeDone(oauthRuntimeDoneMsg{
		endpoint:        "http://127.0.0.1:1/v1",
		apiKey:          "session-key",
		models:          []string{"auto", "ultimate", "performance", "efficient", "lite"},
		catalogFallback: true,
	})

	if !m.runtimeReady || m.runtimeErr != nil {
		t.Fatalf("fallback runtime did not come up: ready=%t err=%v", m.runtimeReady, m.runtimeErr)
	}
	if !m.runtimeCatalogFallback {
		t.Fatal("fallback catalog was not recorded")
	}
	if !slices.Equal(m.live().modelPool, before) {
		t.Fatalf("fallback replaced the saved pool: %v", m.live().modelPool)
	}
	if p.Model != strings.Join(saved, ",") {
		t.Fatalf("provider catalog shrank to %q", p.Model)
	}
	// The page is still usable — the runtime is up, only its catalog is a guess.
	if !m.canSave() {
		t.Fatal("page unsaveable after the runtime came up")
	}
	view := renderView(t, m)
	if !strings.Contains(view, locale.T("已就绪 · 模型列表获取失败，沿用已保存的", "Ready · model list unavailable, keeping the saved one")) {
		t.Fatalf("fallback catalog was not surfaced: %q", view)
	}

	// A real catalog still lands: the fallback is a state, not a latch.
	m.handleOAuthRuntimeDone(oauthRuntimeDoneMsg{
		endpoint: "http://127.0.0.1:1/v1",
		apiKey:   "session-key",
		models:   []string{"auto", "ultimate", "mmodel"},
	})
	if m.runtimeCatalogFallback {
		t.Fatal("fallback state survived a successful catalog fetch")
	}
	// The page keeps its pool sorted, so compare against a sorted copy.
	if want := []string{"auto", "ultimate", "mmodel"}; !slices.Equal(m.live().modelPool, slices.Sorted(slices.Values(want))) {
		t.Fatalf("real catalog not adopted: %v", m.live().modelPool)
	}
}

func TestOAuthConfigShowsModelIDButKeepsRuntimeNameSearchable(t *testing.T) {
	p := providerFrom("qoder", "https://example.invalid", "anthropic")
	p.OAuthProvider = "qoder"
	p.OpusModel = "dmodel"
	m := NewAdvancedConfigModel(&p)
	m.setRuntimeModelNames(map[string]string{"dmodel": "Catalog display name"})
	if label := m.modelDisplayLabel("dmodel"); label != "dmodel" {
		t.Fatalf("row label = %q, want the bare model ID", label)
	}
	// The catalog's own name still has to be findable through the picker filter.
	if search := m.modelSearchLabel("dmodel"); !strings.Contains(search, "Catalog display name") {
		t.Fatalf("search label = %q", search)
	}
	if m.p.OpusModel != "dmodel" {
		t.Fatal("display label replaced upstream ID")
	}
}

func TestOAuthRuntimeNamesSurviveCatalogRefresh(t *testing.T) {
	p := providerFrom("qoder", "oauth://qoder", "anthropic")
	p.OAuthProvider, p.Model, p.OpusModel = "qoder", "dmodel", "dmodel"
	m := NewAdvancedConfigModel(&p)
	m.configureOAuthRuntime("http://127.0.0.1:12345", "fake-local-key", nil, false)
	m.setRuntimeModelNames(map[string]string{"dmodel": "Runtime name"})
	for _, name := range []string{"", "Fresh catalog name"} {
		m.live().detecting = true
		m.handleFetchDone(modelFetchDoneMsg{endpoint: m.live().probeEndpoint, apiKey: m.live().probeAPIKey, discoveredModelsRaw: "dmodel", modelInfos: []protocol.ModelInfo{{ID: "dmodel", DisplayName: name, ContextWindow: 128000}}})
		want := name
		if want == "" {
			want = "Runtime name"
		}
		if !strings.Contains(m.modelSearchLabel("dmodel"), want) {
			t.Fatalf("display name lost: %q", m.modelSearchLabel("dmodel"))
		}
		if got := m.modelDisplayLabel("dmodel"); got != "dmodel" {
			t.Fatalf("row label = %q, want the bare model ID", got)
		}
		if p.OpusModel != "dmodel" || p.Endpoint != "oauth://qoder" {
			t.Fatal("display refresh mutated stored mapping or endpoint")
		}
		if m.live().modelContextWindows["dmodel"] != 128000 {
			t.Fatal("context metadata lost")
		}
	}
}

func TestProviderSelectorLabelUsesEditableColor(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	for _, selected := range []bool{false, true} {
		got := tui.Sprint(stepperRow("Provider", "‹ Select provider ›", selected), tui.WithPrintWidth(80))
		label := strings.Index(got, "Provider")
		color := "38;2;183;156;255m"
		if selected {
			color = "38;2;101;183;255m"
		}
		if label < 0 || !strings.Contains(got[:label], color) {
			t.Fatalf("editable color missing before label: %q", got)
		}
	}
}
