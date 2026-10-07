package oauthproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoopbackAuthorized(t *testing.T) {
	for name, testCase := range map[string]struct {
		header, value string
		want          bool
	}{
		"x-api-key":           {"x-api-key", "k-123", true},
		"bearer":              {"Authorization", "Bearer k-123", true},
		"wrong key":           {"x-api-key", "k-124", false},
		"prefix of the key":   {"x-api-key", "k-12", false},
		"no credentials":      {"", "", false},
		"bearer of wrong key": {"Authorization", "Bearer nope", false},
	} {
		request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if testCase.header != "" {
			request.Header.Set(testCase.header, testCase.value)
		}
		if got := loopbackAuthorized(request, "k-123"); got != testCase.want {
			t.Fatalf("%s: authorized = %t", name, got)
		}
	}
}

// TestLoopbackFrontsServeTheSameShape pins R2: every runtime now serves the
// same paths and the same model-list shape (the per-model routers used to
// omit the pagination fields).
func TestLoopbackFrontsServeTheSameShape(t *testing.T) {
	routes := []runtimeModelRoute{{Name: "m1", Alias: "m1"}, {Name: "m2", Alias: "m2"}}
	fronts := map[string]http.Handler{
		"chat":    newChatCompletionsService("key", "http://upstream.invalid/v1", routes, "up", NewUsageTracker()).handler(),
		"mixed":   (&mixedProtocolRouter{apiKey: "key", models: []string{"m1", "m2"}}).handler(),
		"zed":     (&zedProtocolRouter{apiKey: "key", models: []string{"m1", "m2"}}).handler(),
		"copilot": (&copilotProtocolRouter{apiKey: "key", models: []string{"m1", "m2"}}).handler(),
	}
	for name, front := range fronts {
		for _, path := range []string{"/v1/models", "/models"} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("x-api-key", "key")
			recorder := httptest.NewRecorder()
			front.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s %s: status %d", name, path, recorder.Code)
			}
			var body struct {
				Data    []map[string]any `json:"data"`
				FirstID string           `json:"first_id"`
				LastID  string           `json:"last_id"`
				HasMore *bool            `json:"has_more"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s %s: %v", name, path, err)
			}
			if len(body.Data) != 2 || body.FirstID == "" || body.LastID == "" || body.HasMore == nil {
				t.Fatalf("%s %s: model list = %s", name, path, recorder.Body.String())
			}
		}

		unauthorized := httptest.NewRecorder()
		front.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if unauthorized.Code != http.StatusUnauthorized {
			t.Fatalf("%s: unauthenticated model list = %d", name, unauthorized.Code)
		}
		post := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
		post.Header.Set("x-api-key", "key")
		wrongMethod := httptest.NewRecorder()
		front.ServeHTTP(wrongMethod, post)
		if wrongMethod.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: POST /v1/models = %d", name, wrongMethod.Code)
		}
		health := httptest.NewRecorder()
		front.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if health.Code != http.StatusOK {
			t.Fatalf("%s: /healthz = %d", name, health.Code)
		}
	}
}
