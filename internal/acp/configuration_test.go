package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureLease struct{ released atomic.Int32 }

func (l *fixtureLease) Generation() uint64 { return 7 }
func (l *fixtureLease) Options(cwd string) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"env": map[string]string{"ANTHROPIC_AUTH_TOKEN": "fake-token"}, "settings": map[string]any{"model": "fixture", "clientCWD": cwd, "futureSetting": []any{nil, false, 7}}})
}
func (l *fixtureLease) Release() { l.released.Add(1) }

func configRequest(t *testing.T, s *configurationService, route, token string, value any) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(value)
	request, _ := http.NewRequest(http.MethodPost, s.URL()+route, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, data
}

func TestConfigurationServiceLeasesAndAuthentication(t *testing.T) {
	leases := make(chan *fixtureLease, 2)
	s, err := startConfigurationService(func(context.Context) (LaunchLease, error) {
		lease := &fixtureLease{}
		leases <- lease
		return lease, nil
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	request := map[string]string{"leaseId": "first", "cwd": t.TempDir()}
	if status, _ := configRequest(t, s, "/acquire", "wrong", request); status != 401 {
		t.Fatalf("unauthenticated acquire = %d", status)
	}
	status, data := configRequest(t, s, "/acquire", s.token, request)
	first := <-leases
	if status != 200 || !bytes.Contains(data, []byte(`"generation":7`)) || !bytes.Contains(data, []byte(`"futureSetting":[null,false,7]`)) {
		t.Fatalf("configuration response = %d %s", status, data)
	}
	if status, _ := configRequest(t, s, "/acquire", s.token, request); status != 409 {
		t.Fatalf("duplicate acquire = %d", status)
	}
	configRequest(t, s, "/release", s.token, request)
	if first.released.Load() != 1 {
		t.Fatal("configuration lease was not released exactly once")
	}
	s.mu.Lock()
	remaining := len(s.entries)
	s.mu.Unlock()
	if remaining != 0 {
		t.Fatal("completed prompt configuration retained a service entry")
	}
	configRequest(t, s, "/acquire", s.token, map[string]string{"leaseId": "last"})
	last := <-leases
	s.Close()
	if last.released.Load() != 1 {
		t.Fatal("connection shutdown retained a runtime lease")
	}
}

func TestConfigurationReleaseCancelsPendingPreparation(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	s, err := startConfigurationService(func(ctx context.Context) (LaunchLease, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		configRequest(t, s, "/acquire", s.token, map[string]string{"leaseId": "pending"})
	}()
	<-started
	configRequest(t, s, "/release", s.token, map[string]string{"leaseId": "pending"})
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("release did not cancel provider preparation")
	}
	<-done
	configRequest(t, s, "/release", s.token, map[string]string{"leaseId": "cancelled-before-acquire"})
	if status, _ := configRequest(t, s, "/acquire", s.token, map[string]string{"leaseId": "cancelled-before-acquire"}); status != 409 {
		t.Fatalf("release-before-acquire status = %d", status)
	}
}

func TestLegacyMappingIsReadOnlyAndRejectsTraversal(t *testing.T) {
	dir, cwd := t.TempDir(), t.TempDir()
	data, _ := json.Marshal(map[string]string{"sessionId": "old", "claudeSessionId": "real", "cwd": cwd})
	file := filepath.Join(dir, "old.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if id, got := legacySession(dir, "old"); id != "real" || got != cwd {
		t.Fatalf("legacy mapping = %q %q", id, got)
	}
	for _, invalid := range []string{"../old", `..\old`, "missing"} {
		if id, _ := legacySession(dir, invalid); id != "" {
			t.Fatalf("accepted invalid legacy id %q", invalid)
		}
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(data, after) {
		t.Fatal("legacy session mapping was modified")
	}
}

func TestAdapterErrorsRedactFragmentedAndRotatedCredentials(t *testing.T) {
	var output bytes.Buffer
	log := newCredentialLog(&output, []string{"ANTHROPIC_API_KEY=old-fixture-secret"})
	_, _ = log.Write([]byte("initial old-fixture-"))
	_, _ = log.Write([]byte("secret retained log\n"))
	log.options(json.RawMessage(`{"env":{"ANTHROPIC_AUTH_TOKEN":"next-fixture-secret","PROVIDER_SECRET":"quoted\"key"}}`))
	_, _ = log.Write([]byte(`settings={"token":"next-fixture-secret","escaped":"quoted\"key"}`))
	log.flush()
	for _, secret := range []string{"old-fixture-secret", "next-fixture-secret", `quoted\"key`} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("adapter error exposed a configured credential: %s", output.String())
		}
	}
	if !strings.Contains(output.String(), "[redacted] retained log") {
		t.Fatal("redaction lost ordinary diagnostics")
	}
}
