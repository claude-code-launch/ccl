package codex

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestConvertedOutputBudgetDefaultsAndExplicitLimits(t *testing.T) {
	for _, test := range []struct {
		name      string
		requested int
		capacity  int
		want      int
	}{
		{"missing capacity", 0, 0, 32768},
		{"advertised capacity", 0, 65536, 65536},
		{"small advertised capacity", 0, 8192, 8192},
		{"explicit limit", 1234, 65536, 1234},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _, err := toMessages(responseRequest{MaxOutputTokens: test.requested}, []map[string]any{{"role": "user", "content": "fixture"}}, Route{Model: "fixture", MaxOutputTokens: test.capacity})
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if got := number(body["max_tokens"]); got != test.want {
				t.Fatalf("max_tokens = %d, want %d", got, test.want)
			}
		})
	}
}

func TestDefaultThinkingBudgetFitsAdvertisedOutputCapacity(t *testing.T) {
	raw, _, err := toMessages(responseRequest{Reasoning: map[string]any{"effort": "max"}}, []map[string]any{{"role": "user", "content": "fixture"}}, Route{Model: "fixture", MaxOutputTokens: 8192, ThinkingBudget: true})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	thinking := body["thinking"].(map[string]any)
	if number(body["max_tokens"]) != 8192 || number(thinking["budget_tokens"]) != 4096 {
		t.Fatalf("reasoning must leave answer space within the model capacity: %s", raw)
	}
}

// The Chat provider returns length at 4K, reproducing the early cutoff seen in
// Codex. A missing budget must no longer hit that adapter default, while a
// genuinely exhausted explicit budget must still report an incomplete response.
func TestChatOutputBudgetAndTruncationSurviveBothConversions(t *testing.T) {
	for _, requested := range []int{0, 128} {
		t.Run(fmt.Sprint(requested), func(t *testing.T) {
			g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				want := requested
				if want == 0 {
					want = 32768
				}
				if number(body["max_tokens"]) != want {
					t.Errorf("upstream max_tokens = %v, want %d", body["max_tokens"], want)
				}
				reason := "stop"
				if number(body["max_tokens"]) <= 4096 {
					reason = "length"
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fixture\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", reason)
			}, "openai", Options{})
			payload := map[string]any{"model": "ccl/chosen/model-a", "input": "fixture", "reasoning": map[string]any{"effort": "max"}}
			if requested > 0 {
				payload["max_output_tokens"] = requested
			}
			for _, stream := range []bool{false, true} {
				payload["stream"] = stream
				response := postResponses(t, g, payload)
				var result map[string]any
				if stream {
					raw, err := io.ReadAll(response.Body)
					if err != nil {
						t.Fatal(err)
					}
					for _, line := range strings.Split(string(raw), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event map[string]any
						if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
							continue
						}
						if event["type"] == "response.completed" || event["type"] == "response.incomplete" {
							result, _ = event["response"].(map[string]any)
						}
					}
				} else {
					result = responseObject(t, response)
				}
				if requested == 0 {
					if result["status"] != "completed" || result["incomplete_details"] != nil {
						t.Fatalf("default request still truncated (stream=%v): %v", stream, result)
					}
				} else {
					details, _ := result["incomplete_details"].(map[string]any)
					if result["status"] != "incomplete" || details["reason"] != "max_output_tokens" {
						t.Fatalf("real truncation must remain visible (stream=%v): %v", stream, result)
					}
				}
			}
		})
	}
}
