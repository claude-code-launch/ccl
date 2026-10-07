package acp

import "testing"

func TestDynamicSessionLoadFailureKeepsClosedSessionClosed(t *testing.T) {
	source := newDynamicLaunchSource(t, 1, "")
	c := initializeDynamicACP(t, source, "")
	sid := newDynamicSession(t, c, 2, t.TempDir())

	closed := c.call(t, 3, "session/close", map[string]any{"sessionId": sid})
	if closed["error"] != nil {
		t.Fatalf("session/close: %v", closed)
	}
	leases, starts := source.snapshot()
	if len(leases) != 1 || len(starts) != 1 || leases[0].releases.Load() != 1 {
		t.Fatalf("closed session lease state: leases=%d starts=%d releases=%d", len(leases), len(starts), leases[0].releases.Load())
	}

	source.setStartFailure(2)
	loaded := c.call(t, 4, "session/load", map[string]any{
		"sessionId":  sid,
		"cwd":        t.TempDir(),
		"mcpServers": []any{},
	})
	if rpcErrorCode(loaded) != errInternal {
		t.Fatalf("failed session/load: %v", loaded)
	}
	leases, starts = source.snapshot()
	if len(starts) != 2 || leases[len(leases)-1].releases.Load() != 1 {
		t.Fatalf("failed load candidate state: starts=%v releases=%d", starts, leases[len(leases)-1].releases.Load())
	}

	prompt := c.call(t, 5, "session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "must remain closed"}},
	})
	if rpcErrorCode(prompt) != errInvalidParams {
		t.Fatalf("prompt after failed load reopened session: %v", prompt)
	}
}
