package codex

import "testing"

func TestCodexClientToolModeFollowsNativeCatalog(t *testing.T) {
	for _, test := range []struct {
		name      string
		native    Catalog
		want      bool
		shellType string
	}{
		{"older client", Catalog{Models: []Model{{"slug": "native"}}}, false, "default"},
		{"stale CCL entry is not a template", Catalog{Models: []Model{{"slug": "ccl/old/model", "tool_mode": "code_mode_only", "shell_type": "unified_exec"}}}, false, "default"},
		{"native code mode", Catalog{Models: []Model{{"slug": "native", "tool_mode": "code_mode_only", "shell_type": "unified_exec", "node_repl_auto_review_required": true}}}, true, "unified_exec"},
	} {
		t.Run(test.name, func(t *testing.T) {
			converted := newModel("ccl/test/converted", "converted", nil, nil)
			passthrough := newModel("ccl/test/native", "native", nil, nil)
			codexOAuth := newModel("ccl/gpt/gpt-5.4", "gpt-5.4", nil, nil)
			g := &Gateway{models: []Model{converted, passthrough, codexOAuth}, routes: map[string]Route{
				"ccl/test/converted": {},
				"ccl/test/native":    {NativeResponses: true},
				"ccl/gpt/gpt-5.4":    {NativeResponses: true, CodexNativeTools: true},
			}}
			g.inheritClientToolMode(test.native)
			if (converted["tool_mode"] == "code_mode_only") != test.want || passthrough["tool_mode"] != nil || (codexOAuth["tool_mode"] == "code_mode_only") != test.want {
				t.Fatalf("converted=%+v passthrough=%+v codexOAuth=%+v", converted, passthrough, codexOAuth)
			}
			if converted["shell_type"] != test.shellType || codexOAuth["shell_type"] != test.shellType || passthrough["shell_type"] != "default" {
				t.Fatalf("shell_type mismatch: converted=%v codexOAuth=%v passthrough=%v", converted["shell_type"], codexOAuth["shell_type"], passthrough["shell_type"])
			}
			if test.want && (converted["node_repl_auto_review_required"] != true || codexOAuth["node_repl_auto_review_required"] != true) {
				t.Fatal("native approval requirement was relaxed")
			}
		})
	}
}
