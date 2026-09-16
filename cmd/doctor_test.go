package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestModelVerificationSummary(t *testing.T) {
	tests := []struct {
		name        string
		available   []string
		unavailable []string
		want        string
	}{
		{name: "mixed", available: []string{"a", "b"}, unavailable: []string{"c"}, want: "2 available · 1 unavailable"},
		{name: "all available", available: []string{"a"}, want: "1 available · 0 unavailable"},
		{name: "all unavailable", unavailable: []string{"a", "b"}, want: "0 available · 2 unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modelVerificationSummary(tt.available, tt.unavailable); got != tt.want {
				t.Fatalf("summary = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSmallestMappedWindowCountsUnknownModels(t *testing.T) {
	p := provider.Provider{
		OpusModel:   "gpt-5.6-sol[1m]",
		SonnetModel: "gpt-5.6-terra",
		HaikuModel:  "not-in-catalog",
	}
	windows := map[string]int{
		"gpt-5.6-sol":   272_000,
		"gpt-5.6-terra": 128_000,
	}
	smallest, model, unknown := smallestMappedWindow(p, windows)
	if smallest != 128_000 || model != "gpt-5.6-terra" {
		t.Fatalf("smallest = %d (%q)", smallest, model)
	}
	if unknown != 1 {
		t.Fatalf("unknown = %d, want 1", unknown)
	}
}

func TestFormatTokenCountIsReadableAndExact(t *testing.T) {
	cases := map[int]string{
		272_000:   "272K (272000)",
		1_000_000: "1M (1000000)",
		258_400:   "258400",
	}
	for tokens, want := range cases {
		if got := formatTokenCount(tokens); got != want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tokens, got, want)
		}
	}
}

func TestPrintDoctorOneMConsistencyFlagsOversizedMarkers(t *testing.T) {
	// The check is output-only; assert the decision inputs it depends on, so a
	// [1m] slot whose backend window is small is recognizable.
	p := provider.Provider{
		OpusModel:   "gpt-5.6-sol[1m]",
		SonnetModel: "claude-sonnet-4-6[1m]",
		HaikuModel:  "gpt-5.6-luna",
	}
	slots := oneMSlotsFromProvider(p)
	if !slots["opus"] || !slots["sonnet"] || slots["haiku"] {
		t.Fatalf("one-M slots = %#v", slots)
	}
	windows := map[string]int{
		"gpt-5.6-sol":       272_000,
		"claude-sonnet-4-6": 1_000_000,
	}
	if protocol.ContextWindowSuggests1M(windows["gpt-5.6-sol"]) {
		t.Error("272K must not count as a 1M-class window")
	}
	if !protocol.ContextWindowSuggests1M(windows["claude-sonnet-4-6"]) {
		t.Error("1M must count as a 1M-class window")
	}
	// Must not panic or warn when no catalog is available.
	printDoctorOneMConsistency(p, nil)
}

// TestProbeAutoClawMessagesUsesLocalAdapter pins the request ccl sends when it
// diagnoses AutoClaw: the local Messages route, the loopback Bearer key, and a
// minimal payload for the catalog's first model.
func TestProbeAutoClawMessagesUsesLocalAdapter(t *testing.T) {
	var gotPath, gotBearer, gotKey, gotVersion, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBearer = r.Header.Get("Authorization")
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
	}))
	t.Cleanup(server.Close)

	p := provider.Provider{
		Type:     "autoclaw",
		Endpoint: server.URL + "/v1",
		APIKey:   "local-runtime-key",
	}
	status, body, err := probeAutoClawMessages(context.Background(), p)
	if err != nil {
		t.Fatalf("probeAutoClawMessages() error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("probe path = %q", gotPath)
	}
	if gotBearer != "Bearer local-runtime-key" || gotKey != "" || gotVersion != "2023-06-01" {
		t.Fatalf("auth headers = bearer %q / x-api-key %q / version %q", gotBearer, gotKey, gotVersion)
	}
	if !strings.Contains(gotBody, `"model":"zai_auto"`) || !strings.Contains(gotBody, `"max_tokens":1`) {
		t.Fatalf("probe payload = %s", gotBody)
	}
	if !autoClawResponseHasContent(body) {
		t.Fatalf("a message_start event must count as content: %q", body)
	}
}

// TestAutoClawResponseHasContentDetectsEmptyStream pins the diagnosis for an
// empty stream returned by the local adapter.
func TestAutoClawResponseHasContentDetectsEmptyStream(t *testing.T) {
	empty := map[string]bool{
		"":                 false,
		"\n\n":             false,
		": keep-alive\n\n": false,
		"event: ping\n\n":  false,
		"data:\n\n":        false,
		"data: [DONE]\n\n": false,
		"event: message_start\n\ndata: {\"type\":\"message_start\"}\n\n": true,
		`{"id":"msg_1","type":"message"}`:                                true,
	}
	for body, want := range empty {
		if got := autoClawResponseHasContent(body); got != want {
			t.Errorf("autoClawResponseHasContent(%q) = %t, want %t", body, got, want)
		}
	}
}

func TestAutoClawModelAvailabilityUsesLiveLocalRuntime(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.URL.Path != "/v1/messages" || request.Header.Get("Authorization") != "Bearer local-key" {
			http.Error(writer, "wrong local runtime contract", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(request.Body)
		if strings.Contains(string(body), `"model":"available-model"`) {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		http.Error(writer, `{"error":{"message":"quota used up"}}`, http.StatusForbidden)
	}))
	defer server.Close()

	if !testSingleModelForProtocolContext(context.Background(), "available-model", server.URL+"/v1", "local-key", "autoclaw", "", time.Second) {
		t.Fatal("working AutoClaw route reported unavailable")
	}
	if testSingleModelWithProtocolsContext(context.Background(), "quota-exhausted", server.URL+"/v1", "local-key", "autoclaw", "", nil, time.Second) {
		t.Fatal("quota-exhausted AutoClaw route reported available")
	}
	if calls != 2 {
		t.Fatalf("live availability calls = %d, want 2", calls)
	}
}
