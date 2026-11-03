package codex

import "testing"

func TestCodexClientToolModeFollowsNativeCatalog(t *testing.T) {
	for _, test := range []struct {
		name   string
		native Catalog
		want   bool
	}{
		{"older client", Catalog{Models: []Model{{"slug": "native"}}}, false},
		{"stale CCL entry is not a template", Catalog{Models: []Model{{"slug": "ccl/old/model", "tool_mode": "code_mode_only"}}}, false},
		{"native code mode", Catalog{Models: []Model{{"slug": "native", "tool_mode": "code_mode_only", "node_repl_auto_review_required": true}}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			converted := newModel("ccl/test/converted", "converted", nil, nil)
			passthrough := newModel("ccl/test/native", "native", nil, nil)
			g := &Gateway{models: []Model{converted, passthrough}, routes: map[string]Route{
				"ccl/test/converted": {}, "ccl/test/native": {NativeResponses: true},
			}}
			g.inheritClientToolMode(test.native)
			if (converted["tool_mode"] == "code_mode_only") != test.want || passthrough["tool_mode"] != nil {
				t.Fatalf("converted=%+v passthrough=%+v", converted, passthrough)
			}
			if test.want && converted["node_repl_auto_review_required"] != true {
				t.Fatal("native approval requirement was relaxed")
			}
		})
	}
}
