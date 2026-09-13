package cmd

import (
	"testing"
	"unicode/utf8"

	tui "github.com/grindlemire/go-tui"
)

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
	for _, field := range []string{"key", "url", "model-filter", "provider-filter"} {
		t.Run(field, func(t *testing.T) {
			p := providerFrom("fake", "https://example.invalid", "openai")
			m := NewAdvancedConfigModel(&p)
			state := m.keyText
			m.cursor = m.mainRowIndex(rowAPIKey)
			switch field {
			case "url":
				state = m.urlText
				m.cursor = m.mainRowIndex(rowEndpoint)
			case "model-filter":
				state = m.filterText
				m.filterFocused = true
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
