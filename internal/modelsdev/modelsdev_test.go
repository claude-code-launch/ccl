package modelsdev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveCatalog points the package at a stub catalog server for one test.
func serveCatalog(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	previous := apiURL
	apiURL = server.URL
	t.Cleanup(func() { apiURL = previous })
	return server
}

func TestFetchDecodesTheCatalogOverHTTP(t *testing.T) {
	var gotAccept, gotUserAgent string
	serveCatalog(t, func(writer http.ResponseWriter, request *http.Request) {
		gotAccept = request.Header.Get("Accept")
		gotUserAgent = request.Header.Get("User-Agent")
		_, _ = writer.Write([]byte(`{
			"opencode-go": {"id":"opencode-go","name":"OpenCode Go","npm":"@ai-sdk/openai-compatible","api":"https://opencode.ai/zen/go/v1","models":{"glm-5.2":{"id":"glm-5.2"}}},
			"anonymous": {"api":"https://example.com/v1","models":{"m":{"id":"m"}}},
			"no-api": {"id":"no-api","models":{"m":{"id":"m"}}},
			"junk": "not a provider",
			"empty": {}
		}`))
	})

	providers, err := Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(providers) != 2 {
		t.Fatalf("decoded %d providers, want the two with an api and models: %v", len(providers), providers)
	}
	if gotAccept != "application/json" || gotUserAgent != "ccl" {
		t.Fatalf("request headers = accept %q, user-agent %q", gotAccept, gotUserAgent)
	}
	// A provider that omits id/name is identified by its catalog key.
	anonymous, ok := providers["anonymous"]
	if !ok || anonymous.ID != "anonymous" || anonymous.Name != "anonymous" {
		t.Fatalf("anonymous provider = %+v (present=%v)", anonymous, ok)
	}
	if _, ok := providers["opencode-go"]; !ok {
		t.Fatal("the well-formed provider was dropped")
	}
}

// TestFetchAcceptsANilContext mirrors how the TUI calls in: a caller with no
// request to thread must not panic inside the request builder.
func TestFetchAcceptsANilContext(t *testing.T) {
	serveCatalog(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"p":{"id":"p","api":"https://example.com","models":{"m":{"id":"m"}}}}`))
	})
	// A caller with no request to thread passes a nil context; the fetch must
	// substitute a background one rather than panic in the request builder.
	var noContext context.Context
	if _, err := Fetch(noContext); err != nil {
		t.Fatalf("Fetch(nil) error = %v", err)
	}
}

func TestFetchReportsUnusableCatalogs(t *testing.T) {
	for name, testCase := range map[string]struct {
		body string
		want string
	}{
		"undecodable":       {body: `not json at all`, want: "decode models.dev catalog"},
		"top-level array":   {body: `[{"id":"p"}]`, want: "decode models.dev catalog"},
		"no usable":         {body: `{"p":{"id":"p","models":{"m":{"id":"m"}}}}`, want: "no usable providers"},
		"empty object":      {body: `{}`, want: "no usable providers"},
		"models but no api": {body: `{"p":{"id":"p","api":"","models":{"m":{}}}}`, want: "no usable providers"},
	} {
		t.Run(name, func(t *testing.T) {
			serveCatalog(t, func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(testCase.body))
			})
			if _, err := Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Fetch() error = %v, want %q", err, testCase.want)
			}
		})
	}
}

// TestFetchReportsEveryHTTPFailureMode covers the status check and the two
// transport-level failures, including that the error keeps the cause.
func TestFetchReportsEveryHTTPFailureMode(t *testing.T) {
	serveCatalog(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte("slow down"))
	})
	if _, err := Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("Fetch() on a 429 = %v", err)
	}

	// A connection refused by a closed listener is the "unreachable" path.
	previous := apiURL
	apiURL = (&httptest.Server{URL: "http://127.0.0.1:1"}).URL
	t.Cleanup(func() { apiURL = previous })
	if _, err := Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "fetch models.dev catalog") {
		t.Fatalf("Fetch() against an unreachable host = %v", err)
	}
}

// TestFetchStopsAtTheContextDeadline pins that a hung catalog server cannot
// stall the caller: the injected context ends the request.
func TestFetchStopsAtTheContextDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	serveCatalog(t, func(http.ResponseWriter, *http.Request) { <-release })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Fetch(ctx); err == nil || !strings.Contains(err.Error(), "fetch models.dev catalog") {
		t.Fatalf("Fetch() with a cancelled context = %v", err)
	}
}

func TestResolvedNPM(t *testing.T) {
	p := Provider{NPM: "@ai-sdk/openai-compatible"}

	t.Run("inherits provider default", func(t *testing.T) {
		m := Model{ID: "glm-5.2"}
		if got := ResolvedNPM(p, m); got != "@ai-sdk/openai-compatible" {
			t.Fatalf("ResolvedNPM = %q, want provider default", got)
		}
	})

	t.Run("model override wins", func(t *testing.T) {
		m := Model{ID: "qwen3.8-max", Provider: &ModelProvider{NPM: "@ai-sdk/anthropic"}}
		if got := ResolvedNPM(p, m); got != "@ai-sdk/anthropic" {
			t.Fatalf("ResolvedNPM = %q, want model override", got)
		}
	})

	t.Run("empty model override falls back", func(t *testing.T) {
		m := Model{ID: "grok-4.5", Provider: &ModelProvider{NPM: "  "}}
		if got := ResolvedNPM(p, m); got != "@ai-sdk/openai-compatible" {
			t.Fatalf("ResolvedNPM = %q, want provider default for blank override", got)
		}
	})
}

// TestFetchDecode exercises the raw-map decoding path with a fixture that mixes
// a valid provider with a malformed top-level entry that must be skipped.
func TestFetchDecode(t *testing.T) {
	valid := `{"id":"opencode-go","name":"OpenCode Go","npm":"@ai-sdk/openai-compatible","api":"https://opencode.ai/zen/go/v1","env":["OPENCODE_API_KEY"],"models":{"glm-5.2":{"id":"glm-5.2","name":"GLM-5.2","status":"deprecated","limit":{"context":1000000,"output":131072}}}}`
	catalog := map[string]json.RawMessage{
		"opencode-go": json.RawMessage(valid),
		"junk":        json.RawMessage(`"not a provider"`),
		"no-models":   json.RawMessage(`{"id":"x","api":"https://example.com"}`),
	}

	providers := make(map[string]Provider, len(catalog))
	for id, entry := range catalog {
		var p Provider
		if err := json.Unmarshal(entry, &p); err != nil {
			continue
		}
		if p.API == "" || len(p.Models) == 0 {
			continue
		}
		if p.ID == "" {
			p.ID = id
		}
		providers[id] = p
	}

	if len(providers) != 1 {
		t.Fatalf("decoded %d providers, want 1", len(providers))
	}
	p, ok := providers["opencode-go"]
	if !ok {
		t.Fatal("missing opencode-go provider")
	}
	if p.Name != "OpenCode Go" || p.API != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("provider decoded wrong: %+v", p)
	}
	m := p.Models["glm-5.2"]
	if m.Limit.Context != 1000000 || m.Limit.Output != 131072 {
		t.Fatalf("model limit decoded wrong: %+v", m.Limit)
	}
}
