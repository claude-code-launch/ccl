package oauthproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExchangeGeminiCodeIncludesPKCEVerifier(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		if got := request.Form.Get("code_verifier"); got != "verifier-value" {
			t.Errorf("code_verifier = %q, want verifier-value", got)
		}
		if got := request.Form.Get("redirect_uri"); got != "http://127.0.0.1:51121/oauth-callback" {
			t.Errorf("redirect_uri = %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"token"}`)
	}))
	defer server.Close()
	previous := antigravityTokenURL
	antigravityTokenURL = server.URL
	t.Cleanup(func() { antigravityTokenURL = previous })

	token, err := exchangeGeminiCode(context.Background(), server.Client(), "code", "http://127.0.0.1:51121/oauth-callback", "verifier-value")
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "token" {
		t.Fatalf("access token = %q", token.AccessToken)
	}
}
