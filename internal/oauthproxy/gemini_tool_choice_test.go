package oauthproxy

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestGeminiValidatedToolsPreserveClientChoice(t *testing.T) {
	for _, tc := range []struct {
		name, model, mode string
		choice            any
		noTools           bool
	}{
		{name: "implicit", model: "gemini-3-flash", mode: "VALIDATED"},
		{name: "explicit auto", model: "gemini-3-flash", choice: map[string]any{"type": "auto"}, mode: "VALIDATED"},
		{name: "string auto", model: "gemini-3-flash", choice: "auto", mode: "VALIDATED"},
		{name: "newer family", model: "gemini-4-pro[1m]", mode: "VALIDATED"},
		{name: "none", model: "gemini-3-flash", choice: map[string]any{"type": "none"}, mode: "NONE"},
		{name: "any", model: "gemini-3-flash", choice: map[string]any{"type": "any"}, mode: "ANY"},
		{name: "specific", model: "gemini-3-flash", choice: map[string]any{"type": "tool", "name": "1tool"}, mode: "ANY"},
		{name: "claude", model: "claude-sonnet-4-6"},
		{name: "claude auto", model: "claude-opus-4-6-thinking", choice: map[string]any{"type": "auto"}, mode: "AUTO"},
		{name: "older gemini", model: "gemini-2.5-pro"},
		{name: "no tools", model: "gemini-3-flash", noTools: true},
		{name: "no tools auto", model: "gemini-3-flash", noTools: true, choice: "auto", mode: "AUTO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"model": tc.model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			if !tc.noTools {
				body["tools"] = []any{map[string]any{"name": "1tool", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"}}}}
			}
			if tc.choice != nil {
				body["tool_choice"] = tc.choice
			}
			raw, _ := json.Marshal(body)
			converted, err := convertAnthropicToAntigravity(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(converted.geminiBody, "toolConfig.functionCallingConfig.mode").String(); got != tc.mode {
				t.Fatalf("function calling mode = %q, want %q", got, tc.mode)
			}
			if tc.name == "specific" {
				allowed := gjson.GetBytes(converted.geminiBody, "toolConfig.functionCallingConfig.allowedFunctionNames.0").String()
				declared := gjson.GetBytes(converted.geminiBody, "tools.0.functionDeclarations.0.name").String()
				if allowed == "" || allowed != declared || converted.toolNameMap[allowed] != "1tool" {
					t.Fatal("explicit tool choice lost its sanitized declaration name")
				}
			}
		})
	}
}

func TestGeminiLargeToolCatalogAvoidsAutoValidatedMode(t *testing.T) {
	tools := make([]any, 0, geminiValidatedMaxTools+2)
	for index := 0; index < geminiValidatedMaxTools+2; index++ {
		tools = append(tools, map[string]any{
			"name": "tool_" + string(rune('a'+index)),
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"arg": map[string]any{"type": "string"}},
			},
		})
	}
	raw, _ := json.Marshal(map[string]any{
		"model":    "gemini-4-argon-high",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    tools,
	})
	converted, err := convertAnthropicToAntigravity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(converted.geminiBody, "toolConfig.functionCallingConfig.mode").String(); got != "" {
		t.Fatalf("large tool catalog mode = %q, want empty (default AUTO)", got)
	}
}
