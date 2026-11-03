package codex

import "fmt"

// discoveredResponseTools reads only definitions Codex supplied in this request.
// Tool search itself is client-owned; the proxy neither indexes nor executes it.
func discoveredResponseTools(items []map[string]any) ([]map[string]any, error) {
	var result []map[string]any
	for _, item := range items {
		if item["type"] != "tool_search_output" {
			continue
		}
		if execution, _ := item["execution"].(string); execution != "" && execution != "client" {
			return nil, fmt.Errorf("only client-executed tool search can be replayed")
		}
		if item["tools"] == nil {
			continue
		}
		tools, ok := item["tools"].([]any)
		if !ok {
			return nil, fmt.Errorf("tool search output tools must be an array")
		}
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid discovered tool definition")
			}
			result = append(result, tool)
		}
	}
	return result, nil
}
