package oauthproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRequestXaiModelsUsesOfficialSessionContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/models" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		wantHeaders := map[string]string{
			"Authorization":            "Bearer access-test",
			"X-XAI-Token-Auth":         "xai-grok-cli",
			"x-grok-client-version":    xaiClientVersion,
			"x-grok-client-identifier": xaiClientIdentifier,
			"x-grok-client-mode":       xaiClientMode,
			"x-authenticateresponse":   "authenticate-response",
			"x-userid":                 "user-test",
			"x-email":                  "grok@example.com",
		}
		for name, want := range wantHeaders {
			if got := request.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"object":"list","data":[`+
			`{"id":"grok-4.6","name":"Grok 4.6","context_window":500000,"api_backend":"responses"},`+
			`{"model":"grok-4.5","name":"Grok 4.5","context_window":500000,"api_backend":"responses"},`+
			`{"id":"legacy-chat","api_backend":"chat"}]}`)
	}))
	defer server.Close()

	models, status, err := requestXaiModels(context.Background(), server.Client(), server.URL+"/v1", codexResponsesAuthorization{
		token: "access-test", userID: "user-test", email: "grok@example.com",
	})
	if err != nil {
		t.Fatalf("requestXaiModels() error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got, want := xaiModelIDs(models), []string{"grok-4.6", "grok-4.5"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	if models[0].ContextWindow != 500_000 {
		t.Fatalf("context window = %d", models[0].ContextWindow)
	}
}

func TestXaiInferenceHeadersSelectFinalRoutedModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", request.URL.Path)
		}
		wantHeaders := map[string]string{
			"Authorization":         "Bearer access-test",
			"x-grok-model-override": "grok-4.6",
			"x-grok-conv-id":        "conversation-test",
			"x-grok-session-id":     "conversation-test",
			"x-grok-agent-id":       "agent-test",
			"x-userid":              "user-test",
			"x-email":               "grok@example.com",
		}
		for name, want := range wantHeaders {
			if got := request.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		if strings.TrimSpace(request.Header.Get("x-grok-req-id")) == "" {
			t.Error("x-grok-req-id is empty")
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	service := newResponsesService("local", server.URL+"/v1", []runtimeModelRoute{{Name: "grok-4.6", Alias: "opus"}}, &codexStaticAuthorizer{token: "unused"}, NewUsageTracker(), true)
	service.installationID = "agent-test"
	response, err := service.callOnce(context.Background(), []byte(`{"model":"grok-4.6"}`), "conversation-test", codexResponsesAuthorization{
		token: "access-test", userID: "user-test", email: "grok@example.com",
	}, false)
	if err != nil {
		t.Fatalf("callOnce() error: %v", err)
	}
	_ = response.Body.Close()
}

func TestXaiAuthorizerCarriesAccountIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xai-test.json")
	if err := os.WriteFile(path, []byte(`{"type":"xai","access_token":"access-test","sub":"user-test","email":"grok@example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	authorizer := &xaiOAuthAuthorizer{path: path, client: http.DefaultClient}
	auth, err := authorizer.authorize(context.Background(), false)
	if err != nil {
		t.Fatalf("authorize() error: %v", err)
	}
	if auth.userID != "user-test" || auth.email != "grok@example.com" {
		t.Fatalf("identity = user %q email %q", auth.userID, auth.email)
	}
}

func TestXaiFallbackCatalogTracksCurrentGrokBuildModels(t *testing.T) {
	if got, want := xaiModelIDs(xaiFallbackModels()), []string{"grok-4.6", "grok-4.5"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback models = %v, want %v", got, want)
	}
	if got := xaiModelSpec("grok-4.6[1m]", xaiFallbackModels()); got != "grok-4.6[1m],grok-4.6,grok-4.5" {
		t.Fatalf("model spec = %q", got)
	}
}
