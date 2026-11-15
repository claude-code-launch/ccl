package cmd

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/claude-code-launch/ccl/internal/provider"
	tui "github.com/grindlemire/go-tui"
)

// Test-only shorthands over the live probe and layout helpers.

func testSingleModelForProtocolContext(ctx context.Context, model, endpoint, apiKey, wireProtocol string, timeout time.Duration) bool {
	status, err := probeSingleModelForProtocolStatusContext(ctx, model, endpoint, apiKey, wireProtocol, timeout)
	return err == nil && status >= 200 && status < 300
}

func testSingleOpenAIModelContext(parent context.Context, model, endpoint, apiKey string, timeout time.Duration) bool {
	status, err := probeSingleOpenAIModelStatusContext(parent, model, endpoint, apiKey, timeout)
	return err == nil && status >= 200 && status < 300
}

func detectProtocolAndModels(endpoint, apiKey string) (string, string, error) {
	result := detectProtocolAndModelsDetailed(endpoint, apiKey)
	return result.protocol, result.models, result.err
}

func rowAtLine(lines []string, y int) (configRowKind, bool) {
	return rowAtLineAt(lines, y, -1)
}

// renderView renders the model offline like a terminal frame would, so tests
// can assert on the visible text (labels, hints, badges) the same way they
// asserted on the old bubbletea View().Content.
func renderView(t *testing.T, m *AdvancedConfigModel) string {
	t.Helper()
	s := tui.Sprint(m.Render(nil), tui.WithPrintWidth(80))
	return strings.TrimRight(s, " \n")
}

// readyOAuthPage marks the subscription's loopback runtime as up. The page gates
// model editing and saving on that, so tests exercising post-start behavior have
// to put it in place; handleOAuthRuntimeDone is what does it for real.
func readyOAuthPage(m *AdvancedConfigModel) *AdvancedConfigModel {
	m.runtimeLoading = false
	m.runtimeErr = nil
	m.runtimeReady = true
	return m
}

// keyPress builds the KeyEvent a terminal sends for a printable character.
func keyPress(r rune) tui.KeyEvent {
	return tui.KeyEvent{Key: tui.KeyRune, Rune: r}
}

func TestRemoveLastRune(t *testing.T) {
	for _, input := range []string{"", "ascii", "fake-key、、中文😀", "https://example.invalid/中文、"} {
		text := input
		for len(text) > 0 {
			before := utf8.RuneCountInString(text)
			text = removeLastRune(text)
			if !utf8.ValidString(text) || utf8.RuneCountInString(text) != before-1 {
				t.Fatalf("invalid deletion for %q: %q", input, text)
			}
		}
		if removeLastRune(text) != "" {
			t.Fatal("empty deletion")
		}
	}
}

func TestConfigBackspaceUnicode(t *testing.T) {
	for _, field := range []string{"key", "url", "provider-filter"} {
		t.Run(field, func(t *testing.T) {
			p := providerFrom("fake", "https://example.invalid", "openai")
			m := NewAdvancedConfigModel(&p)
			state := m.keyText
			m.cursor = m.mainRowIndex(rowAPIKey)
			switch field {
			case "url":
				state = m.urlText
				m.cursor = m.mainRowIndex(rowEndpoint)
			case "provider-filter":
				state = m.modelsDevText
			}
			state.Set("fake、中、😀")
			for _, want := range []string{"fake、中、", "fake、中", "fake、", "fake"} {
				if field == "provider-filter" {
					m.handleModelsDevPickerKey(tui.KeyEvent{Key: tui.KeyBackspace})
				} else {
					m.handleKey(tui.KeyEvent{Key: tui.KeyBackspace})
				}
				if got := state.Get(); got != want || !utf8.ValidString(got) {
					t.Fatalf("got %q, want %q", got, want)
				}
			}
		})
	}
}

// TestConnectedProviderEditsURLKey verifies that an already-connected (Custom)
// provider still lets the user edit Endpoint URL and API Key, and that an edit
// flips the connection to dirty (requiring a fresh Auto Configure) and blocks
// saving until re-detected.
func TestConnectedProviderEditsURLKey(t *testing.T) {
	p := providerFrom("resp", "https://example.test/v1", "openai")
	m := NewAdvancedConfigModel(&p)
	enterDetectedReview(m, "gpt-x")

	if m.canSave() == false {
		t.Fatalf("freshly detected provider must be saveable, got dirty=%t", m.live().connectionDirty)
	}

	// Move to Endpoint URL and type a character: it must append and mark dirty.
	idx := m.mainRowIndex(rowEndpoint)
	if idx < 0 {
		t.Fatalf("rowEndpoint missing from visibleRows")
	}
	m.cursor = idx
	m.handleKey(tui.KeyEvent{Key: tui.KeyRune, Rune: 'x'})
	if !strings.HasSuffix(m.urlText.Get(), "x") {
		t.Fatalf("url edit not applied: %q", m.urlText.Get())
	}
	if !m.live().connectionDirty {
		t.Fatalf("url edit did not mark connection dirty")
	}
	if m.canSave() {
		t.Fatalf("dirty connection must not be saveable")
	}

	// Same for API Key.
	keyIdx := m.mainRowIndex(rowAPIKey)
	m.cursor = keyIdx
	m.handleKey(tui.KeyEvent{Key: tui.KeyRune, Rune: 'y'})
	if !strings.HasSuffix(m.keyText.Get(), "y") {
		t.Fatalf("key edit not applied: %q", m.keyText.Get())
	}
}

// TestAutoDetectDoesNotDowngradeResponsesToChat guards a regression where
// reopening an existing openai_responses (API-key) provider let the auto-probe
// overwrite its stored Type with the probe's coarse "openai" result. The probe
// cannot tell Chat Completions from Responses (both share GET /v1/models), so
// an OpenAI-family result must preserve an existing Responses choice instead of
// downgrading it to chat.
func TestAutoDetectDoesNotDowngradeResponsesToChat(t *testing.T) {
	p := provider.Provider{
		Name:     "resp",
		Type:     "openai_responses",
		Endpoint: "https://example.test/v1",
		APIKey:   "sk-test",
		Model:    "gpt-x",
	}
	m := NewAdvancedConfigModel(&p)
	m.applyModelDetectionResult("openai", "gpt-x,gpt-y", "https://example.test/v1", nil)
	if m.p.Type != "openai_responses" {
		t.Fatalf("type downgraded to %q after OpenAI-family probe; want openai_responses", m.p.Type)
	}
}
