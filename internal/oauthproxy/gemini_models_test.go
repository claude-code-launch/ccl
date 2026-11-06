package oauthproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
)

const geminiCatalogFixture = `{"defaultModel":"gemini-4-argon-high","models":{"gemini-4-argon-high":{"displayName":"Gemini 4 Argon (High)","maxTokens":1048576,"maxOutputTokens":32768,"supportsImages":true},"gemini-4-argon-low":{"displayName":"Gemini 4 Argon (Low)","maxTokens":1048576},"gemini-4-argon-medium":{"disabled":true},"gemini-3.8-flash-medium":{"displayName":"Gemini 3.8 Flash (Medium)"},"claude-sonnet-5-5":{"displayName":"Claude Sonnet 5.5"},"chat_internal":{},"tab_internal":{}}}`

func TestGeminiCatalogUsesActualWireVariants(t *testing.T) {
	c, err := parseGeminiCatalog([]byte(geminiCatalogFixture))
	if err != nil {
		t.Fatal(err)
	}
	ids, names, details := c.publicModels()
	if strings.Join(ids, ",") != "claude-sonnet-5-5,gemini-3.8-flash,gemini-4-argon" || names["gemini-4-argon"] != "Gemini 4 Argon" {
		t.Fatalf("catalog = %v %v", ids, names)
	}
	info := details["gemini-4-argon"]
	if info.ContextWindow != 1048576 || info.MaxOutputTokens != 32768 || len(info.ReasoningLevels) != 2 || info.DefaultReasoningLevel != "high" || len(info.InputModalities) != 2 {
		t.Fatalf("metadata = %+v", info)
	}
	for _, test := range []struct{ model, effort, want string }{
		{"gemini-4-argon", "", "gemini-4-argon-high"}, {"gemini-4-argon", "low", "gemini-4-argon-low"}, {"gemini-4-argon", "xhigh", "gemini-4-argon-high"},
		{"gemini-4-argon-low", "high", "gemini-4-argon-low"}, {"gemini-3.8-flash", "", "gemini-3.8-flash-medium"},
		{"Gemini 4 Argon", "", "gemini-4-argon-high"}, {"Gemini 4 Argon", "low", "gemini-4-argon-low"}, {"Gemini 4 Argon[1m]", "low", "gemini-4-argon-low"},
		{"Gemini 4 Argon (Low)", "high", "gemini-4-argon-low"}, {"Gemini 3.8 Flash", "", "gemini-3.8-flash-medium"},
	} {
		got, body, err := c.resolve(test.model, test.effort, []byte(`{"generationConfig":{"thinkingConfig":{"thinkingBudget":2000}}}`))
		if err != nil || got != test.want || gjson.GetBytes(body, "generationConfig.thinkingConfig.thinkingBudget").Exists() {
			t.Fatalf("%+v: %s %s %v", test, got, body, err)
		}
	}
	for _, test := range []struct{ model, effort, want string }{
		{"claude-sonnet-5-5", "high", "claude-sonnet-5-5"},
		{"Claude Sonnet 5.5", "high", "claude-sonnet-5-5"},
		{"Claude Sonnet 5.5[1m]", "high", "claude-sonnet-5-5"},
	} {
		got, _, err := c.resolve(test.model, test.effort, []byte(`{}`))
		if err != nil || got != test.want {
			t.Fatalf("%+v: %s %v", test, got, err)
		}
	}
	for _, test := range []struct{ model, effort string }{{"gemini-4-argon", "medium"}, {"gemini-4-pro", "high"}, {"gemini-4-argon", "minimal"}} {
		if _, _, err := c.resolve(test.model, test.effort, []byte(`{}`)); err == nil {
			t.Fatalf("invented route for %+v", test)
		}
	}
	converted, err := convertAnthropicToAntigravityWithCatalog([]byte(`{"model":"Gemini 4 Argon","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"echo","input_schema":{"type":"object"}}]}`), c)
	if err != nil {
		t.Fatal(err)
	}
	if mode := gjson.GetBytes(converted.geminiBody, "toolConfig.functionCallingConfig.mode").String(); mode != "VALIDATED" {
		t.Fatalf("display alias tool mode = %q, want VALIDATED", mode)
	}
	opusCatalog, err := parseGeminiCatalog([]byte(`{"models":{"claude-opus-4-6-thinking":{"displayName":"Claude Opus 5.5 (High)"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	wire, _, err := opusCatalog.resolve("Claude Opus 5.5 (High)", "high", []byte(`{}`))
	if err != nil || wire != "claude-opus-4-6-thinking" {
		t.Fatalf("opus display alias resolved to %q, %v", wire, err)
	}
}

func TestGeminiDiscoveryBoundAccountCacheAndFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var fail bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/v1internal:fetchAvailableModels" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer access-old" {
			t.Errorf("unexpected catalog request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["project"] != "test-project" {
			t.Error("wrong account project")
		}
		if fail {
			http.Error(w, "offline", 503)
			return
		}
		_, _ = io.WriteString(w, geminiCatalogFixture)
	}))
	defer server.Close()
	oldDaily, oldProd := antigravityBaseURLDaily, antigravityBaseURLProd
	antigravityBaseURLDaily, antigravityBaseURLProd = server.URL, server.URL
	t.Cleanup(func() { antigravityBaseURLDaily, antigravityBaseURLProd = oldDaily, oldProd })
	path := writeGeminiCredential(t, home, "account-one.json")
	a := &antigravityOAuthAuthorizer{path: path, client: server.Client()}
	cred, err := a.load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, fallback := a.discoverModels(context.Background(), cred)
	if fallback || catalog == nil || len(catalog.Models) != 4 {
		t.Fatalf("catalog=%+v fallback=%v", catalog, fallback)
	}
	files, _ := filepath.Glob(filepath.Join(home, ".ccl", "cache", "gemini", "*.json"))
	if len(files) != 1 {
		t.Fatal("account catalog not saved")
	}
	raw, _ := os.ReadFile(files[0])
	if strings.Contains(string(raw), "access-old") || strings.Contains(string(raw), "refresh-old") {
		t.Fatal("credential leaked into catalog")
	}
	fail = true
	cached, fallback := a.discoverModels(context.Background(), cred)
	if !fallback || cached == nil || len(cached.Models) != 4 {
		t.Fatal("last good inventory not retained")
	}
	other := *cred
	other.email = "different-account@example.test"
	if c, _ := a.discoverModels(context.Background(), &other); c != nil {
		t.Fatal("another account inherited the cache")
	}
	if err := os.WriteFile(files[0], []byte(`{broken`), 0600); err != nil {
		t.Fatal(err)
	}
	if c, _ := a.discoverModels(context.Background(), cred); c != nil {
		t.Fatal("corrupt catalog accepted")
	}
}

func TestGeminiDiscoveryFallsBackFromRejectedAccountTiers(t *testing.T) {
	for _, tt := range []struct {
		name, metadata string
		want           []string
	}{
		{"eligible", `{"cloudaicompanionProject":"test-project","paidTier":{"id":"example-paid"},"currentTier":{"id":"example-current"}}`, []string{"example-paid", "example-current", ""}},
		{"duplicate", `{"paidTier":{"id":"same-tier"},"currentTier":{"id":"same-tier"}}`, []string{"same-tier", ""}},
		{"free", `{"currentTier":{"id":"free-tier"},"allowedTiers":[{"id":"never-ask"}]}`, []string{""}},
		{"different project", `{"cloudaicompanionProject":"other-project","paidTier":{"id":"never-ask"}}`, []string{""}},
		{"lookup unavailable", "", []string{""}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			attempts := make(chan string, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if r.Header.Get("Authorization") != "Bearer access-old" || r.Method != http.MethodPost {
					t.Error("metadata request did not use the bound account")
				}
				switch r.URL.Path {
				case "/v1internal:loadCodeAssist":
					if tt.metadata == "" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					_, _ = io.WriteString(w, tt.metadata)
				case "/v1internal:fetchAvailableModels":
					if gjson.GetBytes(raw, "project").String() != "test-project" {
						t.Error("discovery changed the bound project")
					}
					tier := gjson.GetBytes(raw, "entitlement.userTier").String()
					attempts <- tier
					if tier != "" {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					_, _ = io.WriteString(w, `{"models":{"gemini-3.8-flash-medium":{}}}`)
				default:
					t.Errorf("unexpected request to %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			oldDaily, oldProd := antigravityBaseURLDaily, antigravityBaseURLProd
			antigravityBaseURLDaily, antigravityBaseURLProd = server.URL, server.URL
			t.Cleanup(func() { antigravityBaseURLDaily, antigravityBaseURLProd = oldDaily, oldProd })
			a := &antigravityOAuthAuthorizer{path: writeGeminiCredential(t, t.TempDir(), "bound.json"), client: server.Client()}
			credential, err := a.load()
			if err != nil {
				t.Fatal(err)
			}
			catalog, fallback := a.discoverModels(t.Context(), credential)
			if fallback || catalog == nil || len(catalog.Models) != 1 || catalog.Models[0].ID != "gemini-3.8-flash-medium" {
				t.Fatalf("project-only success should use the returned catalog: %+v, fallback=%v", catalog, fallback)
			}
			var got []string
			for len(attempts) != 0 {
				got = append(got, <-attempts)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("tiers requested = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGeminiRuntimeDiscoversAndRoutesFamily(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"test-project","paidTier":{"id":"example-paid"}}`)
			return
		case "/v1internal:fetchAvailableModels":
			raw, _ := io.ReadAll(r.Body)
			if gjson.GetBytes(raw, "entitlement.userTier").String() != "example-paid" {
				t.Error("the upstream catalog requires the account's paid tier")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, geminiCatalogFixture)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(raw, "model").String() != "gemini-4-argon-low" {
			t.Errorf("wire model = %s", gjson.GetBytes(raw, "model"))
		}
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"parts":[{"text":"live catalog"}]},"finishReason":"STOP"}]}}`)
	}))
	defer server.Close()
	oldDaily, oldProd := antigravityBaseURLDaily, antigravityBaseURLProd
	antigravityBaseURLDaily, antigravityBaseURLProd = server.URL, server.URL
	t.Cleanup(func() { antigravityBaseURLDaily, antigravityBaseURLProd = oldDaily, oldProd })
	path := writeGeminiCredential(t, home, "bound.json")
	runtime, err := StartOAuth(context.Background(), ProviderGemini, "stale-model", filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	if runtime.ModelCatalogIsFallback() || !strings.Contains(strings.Join(runtime.Models(), ","), "gemini-4-argon") || strings.Contains(strings.Join(runtime.Models(), ","), "stale-model") {
		t.Fatalf("models = %v", runtime.Models())
	}
	req, _ := http.NewRequest(http.MethodPost, runtime.ClaudeBaseURL()+"/v1/messages", strings.NewReader(`{"model":"gemini-4-argon","max_tokens":100,"output_config":{"effort":"low"},"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("X-Api-Key", runtime.APIKey())
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(raw), "live catalog") {
		t.Fatalf("response = %d %s", response.StatusCode, raw)
	}
	displayReq, _ := http.NewRequest(http.MethodPost, runtime.ClaudeBaseURL()+"/v1/messages", strings.NewReader(`{"model":"Gemini 4 Argon","max_tokens":100,"output_config":{"effort":"low"},"messages":[{"role":"user","content":"hello"}]}`))
	displayReq.Header.Set("X-Api-Key", runtime.APIKey())
	displayResp, err := server.Client().Do(displayReq)
	if err != nil {
		t.Fatal(err)
	}
	defer displayResp.Body.Close()
	displayRaw, _ := io.ReadAll(displayResp.Body)
	if displayResp.StatusCode != 200 || !strings.Contains(string(displayRaw), "live catalog") {
		t.Fatalf("display response = %d %s", displayResp.StatusCode, displayRaw)
	}
}

func TestGeminiDiscoveryRefreshesCredentialAcrossMetadataCalls(t *testing.T) {
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		old := r.Header.Get("Authorization") == "Bearer access-old"
		switch r.URL.Path {
		case "/token":
			if !strings.Contains(string(raw), "refresh_token=refresh-old") {
				t.Error("refresh did not use the bound credential")
			}
			refreshes.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"access-new","expires_in":3600}`)
		case "/v1internal:loadCodeAssist":
			if old {
				_, _ = io.WriteString(w, `{"paidTier":{"id":"old-tier"}}`)
			} else {
				_, _ = io.WriteString(w, `{"paidTier":{"id":"new-tier"}}`)
			}
		case "/v1internal:fetchAvailableModels":
			if old {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Authorization") != "Bearer access-new" || gjson.GetBytes(raw, "entitlement.userTier").String() != "new-tier" {
				t.Error("discovery reused stale token or tier")
			}
			_, _ = io.WriteString(w, geminiCatalogFixture)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	oldDaily, oldProd, oldToken := antigravityBaseURLDaily, antigravityBaseURLProd, antigravityTokenURL
	antigravityBaseURLDaily, antigravityBaseURLProd, antigravityTokenURL = server.URL, server.URL, server.URL+"/token"
	t.Cleanup(func() {
		antigravityBaseURLDaily, antigravityBaseURLProd, antigravityTokenURL = oldDaily, oldProd, oldToken
	})
	a := &antigravityOAuthAuthorizer{path: writeGeminiCredential(t, t.TempDir(), "bound.json"), client: server.Client()}
	credential, err := a.load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, stale := a.discoverModels(t.Context(), credential)
	if stale || catalog == nil || catalog.DefaultModel != "gemini-4-argon-high" || refreshes.Load() != 1 {
		t.Fatalf("expected live catalog after one token refresh; catalog=%+v, stale=%v, refreshes=%d", catalog, stale, refreshes.Load())
	}
}

func TestGeminiCachedInventoryIsKnownButStale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			http.Error(w, "offline", 503)
			return
		}
		_, _ = io.WriteString(w, geminiCatalogFixture)
	}))
	defer server.Close()
	oldDaily, oldProd := antigravityBaseURLDaily, antigravityBaseURLProd
	antigravityBaseURLDaily, antigravityBaseURLProd = server.URL, server.URL
	t.Cleanup(func() { antigravityBaseURLDaily, antigravityBaseURLProd = oldDaily, oldProd })
	path := writeGeminiCredential(t, home, "cached-account.json")
	first, err := StartOAuth(t.Context(), ProviderGemini, "stale-model", filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	first.Stop()
	unavailable.Store(true)
	cached, err := StartOAuth(t.Context(), ProviderGemini, "stale-model", filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	defer cached.Stop()
	if cached.ModelCatalogIsFallback() || !cached.ModelCatalogIsStale() || !strings.Contains(strings.Join(cached.Models(), ","), "gemini-4-argon") {
		t.Fatalf("known cache confused with a guessed fallback: %v", cached.Models())
	}
	plain, err := parseGeminiCatalog([]byte(`{"models":{"gemini-2.5-flash":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _, details := plain.publicModels()
	if info := details["gemini-2.5-flash"]; !info.ReasoningKnown || len(info.ReasoningLevels) != 0 {
		t.Fatal("invented thinking variants for a model without published tiers")
	}
}

func TestGeminiConfiguredFallbackResolvesKnownFamily(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var wire string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:fetchAvailableModels" || r.URL.Path == "/v1internal:loadCodeAssist" {
			http.Error(w, "offline", 503)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		wire = gjson.GetBytes(raw, "model").String()
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"parts":[{"text":"fallback response"}]},"finishReason":"STOP"}]}}`)
	}))
	defer server.Close()
	oldDaily, oldProd := antigravityBaseURLDaily, antigravityBaseURLProd
	antigravityBaseURLDaily, antigravityBaseURLProd = server.URL, server.URL
	t.Cleanup(func() { antigravityBaseURLDaily, antigravityBaseURLProd = oldDaily, oldProd })
	path := writeGeminiCredential(t, home, "offline-first-run.json")
	runtime, err := StartOAuth(t.Context(), ProviderGemini, "Gemini 3.8 Flash,gemini-4-argon-low", filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	if !runtime.ModelCatalogIsFallback() || strings.Join(runtime.Models(), ",") != "Gemini 3.8 Flash,gemini-4-argon-low" {
		t.Fatalf("configured pool was replaced by guesses: %v", runtime.Models())
	}
	if got := runtime.modelNames["gemini-4-argon-low"]; got != "Gemini 4 Argon (Low)" {
		t.Fatalf("explicit variant display name = %q", got)
	}
	req, _ := http.NewRequest(http.MethodPost, runtime.ClaudeBaseURL()+"/v1/messages", strings.NewReader(`{"model":"Gemini 3.8 Flash","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("X-Api-Key", runtime.APIKey())
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || wire != "gemini-3.8-flash-medium" {
		t.Fatalf("fallback family not resolved: status=%d wire=%s", response.StatusCode, wire)
	}
}
