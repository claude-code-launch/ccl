package oauthproxy

import (
	"strings"
	"testing"
)

func TestGeminiJSONPayload(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`data: {"a":1}`, `{"a":1}`},
		{`data:{"a":1}`, `{"a":1}`},
		{"  {\"a\":1}  ", `{"a":1}`},
		{"", ""},
		{"data: [DONE]", ""},
		{"event: message_start", ""},
		{"data: not-json", ""},
		{"data: [1,2]", ""},
	}
	for _, tc := range cases {
		if got := geminiJSONPayload(tc.in); got != tc.want {
			t.Errorf("geminiJSONPayload(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func geminiTestAssembler(t *testing.T) *anthropicResponseAssembler {
	t.Helper()
	return newAnthropicResponseAssembler(&anthropicAdapterRequest{
		upstreamModel: "gemini-3-flash", clientModel: "gemini-3-flash", stream: true,
		thinkingEnabled: true, thinkingSignature: "gemini", maxTokens: 32000, inputTokens: 10,
		toolNameMap: map[string]string{"get_weather": "get_weather_original"},
	}, nil)
}

func TestProcessGeminiStreamTextThinkingToolUse(t *testing.T) {
	assembler := geminiTestAssembler(t)
	sse := strings.Join([]string{
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":"Let me check","thought":true}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":" the weather."}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"SF"}}}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"thoughtsTokenCount":5,"cachedContentTokenCount":3}}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	if err := processGeminiStream(strings.NewReader(sse), assembler); err != nil {
		t.Fatal(err)
	}

	blocks := assembler.contentBlocks()
	if len(blocks) != 3 {
		t.Fatalf("content blocks = %d, want 3: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != "thinking" || blocks[0].Thinking != "Let me check" {
		t.Errorf("block[0] = %+v, want thinking \"Let me check\"", blocks[0])
	}
	if blocks[1].Type != "text" || blocks[1].Text != " the weather." {
		t.Errorf("block[1] = %+v, want text \" the weather.\"", blocks[1])
	}
	if blocks[2].Type != "tool_use" || blocks[2].Name != "get_weather_original" {
		t.Errorf("block[2] = %+v, want tool_use named get_weather_original", blocks[2])
	}
	if blocks[2].Input == nil || (*blocks[2].Input)["city"] != "SF" {
		t.Errorf("block[2].Input = %+v, want city=SF", blocks[2].Input)
	}
	if got := assembler.resolvedStopReason(); got != "tool_use" {
		t.Errorf("stop reason = %q, want tool_use", got)
	}
	input, output := assembler.tokenTotals()
	if input != 100 || output != 25 {
		t.Errorf("token totals = (%d, %d), want (100, 25)", input, output)
	}
	if assembler.cacheReadTokens != 3 {
		t.Errorf("cache read tokens = %d, want 3", assembler.cacheReadTokens)
	}
}

func TestProcessGeminiStreamMaxTokensStopReason(t *testing.T) {
	assembler := geminiTestAssembler(t)
	sse := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"thoughtsTokenCount":0}}}` + "\n\n"

	if err := processGeminiStream(strings.NewReader(sse), assembler); err != nil {
		t.Fatal(err)
	}
	if got := assembler.resolvedStopReason(); got != "max_tokens" {
		t.Errorf("stop reason = %q, want max_tokens", got)
	}
}

func TestProcessGeminiNonStream(t *testing.T) {
	assembler := geminiTestAssembler(t)
	body := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"thoughtsTokenCount":0}}}`)

	if err := processGeminiNonStream(body, assembler); err != nil {
		t.Fatal(err)
	}
	blocks := assembler.contentBlocks()
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "answer" {
		t.Fatalf("content blocks = %+v, want single text block \"answer\"", blocks)
	}
	if got := assembler.resolvedStopReason(); got != "end_turn" {
		t.Errorf("stop reason = %q, want end_turn", got)
	}
	input, output := assembler.tokenTotals()
	if input != 10 || output != 3 {
		t.Errorf("token totals = (%d, %d), want (10, 3)", input, output)
	}
}
