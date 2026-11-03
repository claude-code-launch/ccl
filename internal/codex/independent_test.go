package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndependentOverlayUsesSeparateAuthAndRestoresProfile(t *testing.T) {
	for _, profile := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "profile"}[profile], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			original := "# settings\nmodel = 'native-model'\nopenai_base_url = 'https://native.invalid/v1'\n"
			if profile {
				original += "profile = 'work'\n[profiles.work]\nmodel_provider = 'openai'\nmodel = 'native-model'\nsandbox_mode = 'read-only'"
			}
			path := filepath.Join(home, "config.toml")
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"models":[{"slug":"native-model","tool_mode":"code_mode_only"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
				writeMessage(w, []map[string]any{{"type": "text", "text": "done"}})
			}, "anthropic", Options{Desktop: true, Independent: true})
			overlay, err := Plan(context.Background(), g, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := overlay.Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			fields, err := configFields(data)
			if err != nil {
				t.Fatal(err)
			}
			provider := scopeFields(fields, []string{"model_providers", IndependentProviderID})
			if fields["model_provider"] != IndependentProviderID || provider["requires_openai_auth"] != false || provider["experimental_bearer_token"] != g.Token() || fields["openai_base_url"] != "https://native.invalid/v1" {
				t.Fatal("independent auth/routing was not configured")
			}
			if profile && scopeFields(fields, []string{"profiles", "work"})["model_provider"] != IndependentProviderID {
				t.Fatal("profile overrode independent mode")
			}
			raw, _ := os.ReadFile(overlay.CatalogPath)
			var catalog Catalog
			if json.Unmarshal(raw, &catalog) != nil || len(catalog.Models) != 1 || catalog.Models[0]["tool_mode"] != "code_mode_only" {
				t.Fatal("independent catalog contains native routes or lost client tools")
			}
			request, _ := http.NewRequest("POST", g.BaseURL()+"/responses", strings.NewReader(`{"model":"ccl/chosen/model-a","input":"hi"}`))
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 401 {
				t.Fatal("independent route accepted a missing loopback credential")
			}
			request, _ = http.NewRequest("POST", g.BaseURL()+"/responses", strings.NewReader(`{"model":"ccl/chosen/model-a","input":"hi"}`))
			request.Header.Set("Authorization", "Bearer "+g.Token())
			response, err = http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("independent model failed: %d", response.StatusCode)
			}
			for _, payload := range []string{
				`{"model":"native-model","input":"normal inference"}`,
				`{"model":"ccl/unknown/model","input":[{"type":"compaction_trigger"}]}`,
				`{"model":"native-model","input":"normal inference","tools":[{"type":"function","name":"run"}],"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"compaction\"}"}}`,
			} {
				request, _ := http.NewRequest("POST", g.BaseURL()+"/responses", strings.NewReader(payload))
				request.Header.Set("Authorization", "Bearer "+g.Token())
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != 400 {
					t.Fatal("historical compaction widened the ordinary model routing allow-list")
				}
			}
			if err := overlay.Restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			data, _ = os.ReadFile(path)
			fields, err = configFields(data)
			if err != nil || fields["model"] != "native-model" || fields["model_provider"] != nil || strings.Contains(string(data), g.Token()) {
				t.Fatal("stop did not restore native selection/remove temporary credentials")
			}
			if profile && scopeFields(fields, []string{"profiles", "work"})["sandbox_mode"] != "read-only" {
				t.Fatal("profile preferences lost")
			}
		})
	}
}

func TestIndependentOverlayRetainsLaterUserEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	block := "\n[model_providers.ccl_proxy]\nname = 'CCL'\n"
	o := &Overlay{Path: path, ProviderBlock: block, Changes: []ConfigChange{{Key: "model_provider", Applied: pointer(IndependentProviderID)}}}
	if err := o.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	data = []byte(strings.Replace(string(data), "name = 'CCL'", "name = 'User changed'", 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := o.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "User changed") {
		t.Fatal("modified provider table was removed")
	}
}

func TestRealCodexIndependentProviderUsesOverlayCredential(t *testing.T) {
	home, run := isolatedCodexClient(t)
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeMessage(w, []map[string]any{{"type": "text", "text": "independent-done"}})
	}, "anthropic", Options{Independent: true})
	applyClientFixture(t, g, home)
	output := run("-c", `web_search="disabled"`, "exec", "--json", "--skip-git-repo-check", "Reply.")
	if !strings.Contains(output, "independent-done") {
		t.Fatalf("custom provider did not complete: %s", output)
	}
}
