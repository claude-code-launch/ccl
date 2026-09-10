package oauthproxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAssemblerToolArgumentPrecision(t *testing.T) {
	const input = `{"id":9007199254740993,"nested":[1e1000,0.123456789012345678901]}`
	w := httptest.NewRecorder()
	a := newAnthropicResponseAssembler(&anthropicAdapterRequest{stream: true}, w)
	if err := a.addToolUse("call1", "lookup", input); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(a.contentBlocks())
	for _, number := range []string{"9007199254740993", "1e1000", "0.123456789012345678901"} {
		if !strings.Contains(string(encoded), number) || !strings.Contains(w.Body.String(), number) {
			t.Fatalf("number %s changed in JSON or SSE", number)
		}
	}
}
