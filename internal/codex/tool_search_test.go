package codex

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func largeToolCatalog() []any {
	tools := []any{
		map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "namespace", "name": "shipping", "tools": []any{map[string]any{"type": "function", "name": "lookup_shipping_eta", "description": "Look up the shipping ETA for an order", "defer_loading": true, "parameters": map[string]any{"type": "object", "properties": map[string]any{"order_id": map[string]any{"type": "string"}}}}}},
	}
	for i := 0; i < 40; i++ {
		tools = append(tools, map[string]any{"type": "function", "name": fmt.Sprintf("unrelated_%d", i), "description": strings.Repeat("unrelated-service-documentation ", 200), "defer_loading": true, "parameters": map[string]any{"type": "object"}})
	}
	return tools
}

func findTool(t *testing.T, body map[string]any, fragment string) string {
	t.Helper()
	var names []string
	for _, raw := range body["tools"].([]any) {
		tool := raw.(map[string]any)
		if function, ok := tool["function"].(map[string]any); ok {
			tool = function
		}
		name, _ := tool["name"].(string)
		names = append(names, name)
		if strings.Contains(name, fragment) {
			return name
		}
	}
	t.Fatalf("tool %q missing from %v", fragment, names)
	return ""
}

func clientToolSearch() map[string]any {
	return map[string]any{"type": "tool_search", "execution": "client", "description": "Search local client tools", "parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}}
}

func TestCodexClientToolSearchRoundTrip(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			var calls atomic.Int32
			g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				raw, _ := json.Marshal(body)
				if strings.Contains(string(raw), "unrelated-service-documentation") {
					t.Error("deferred catalog was eagerly loaded")
				}
				switch calls.Add(1) {
				case 1:
					if strings.Contains(string(raw), `"order_id"`) {
						t.Error("deferred schema reached first request")
					}
					writeChatTool(w, r, "search_shipping", findTool(t, body, "tool_search"), map[string]any{"query": "shipping_eta"})
				case 2:
					if !strings.Contains(string(raw), `"order_id"`) {
						t.Error("client search did not load selected schema")
					}
					writeChatTool(w, r, "call_shipping", findTool(t, body, "lookup_shipping_eta"), map[string]any{"order_id": "order-42"})
				default:
					if !strings.Contains(string(raw), "shipping-result") {
						t.Error("client execution result was lost")
					}
					writeChatText(w, r, "continued")
				}
			}, "openai", Options{})
			catalog := append(largeToolCatalog(), clientToolSearch())
			response := postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "input": "Find shipping ETA", "tools": catalog, "stream": streaming})
			raw, _ := io.ReadAll(response.Body)
			if response.StatusCode != 200 {
				t.Fatalf("HTTP %d: %s", response.StatusCode, raw)
			}
			var first map[string]any
			if streaming {
				for _, line := range strings.Split(string(raw), "\n") {
					if strings.HasPrefix(line, "data: ") {
						var event map[string]any
						_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
						if event["type"] == "response.completed" {
							first, _ = event["response"].(map[string]any)
						}
					}
				}
			} else {
				_ = json.Unmarshal(raw, &first)
			}
			if first == nil {
				t.Fatalf("missing completed response: %s", raw)
			}
			search := first["output"].([]any)[0].(map[string]any)
			if search["type"] != "tool_search_call" || search["execution"] != "client" || search["call_id"] != "search_shipping" || search["arguments"].(map[string]any)["query"] != "shipping_eta" {
				t.Fatalf("search call changed: %+v", search)
			}
			if calls.Load() != 1 {
				t.Fatal("proxy executed the search instead of returning it to Codex")
			}
			found := largeToolCatalog()[1]
			second := responseObject(t, postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "previous_response_id": first["id"], "tools": []any{clientToolSearch()}, "input": []any{map[string]any{"type": "tool_search_output", "execution": "client", "call_id": "search_shipping", "tools": []any{found}}}}))
			call := second["output"].([]any)[0].(map[string]any)
			if call["name"] != "lookup_shipping_eta" || call["namespace"] != "shipping" {
				t.Fatalf("discovered call changed: %+v", call)
			}
			third := responseObject(t, postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "previous_response_id": second["id"], "tools": []any{clientToolSearch()}, "input": []any{map[string]any{"type": "function_call_output", "call_id": "call_shipping", "output": "shipping-result"}}}))
			if calls.Load() != 3 || third["status"] != "completed" {
				t.Fatal("tool continuation failed")
			}
		})
	}
}
