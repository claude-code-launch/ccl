package oauthproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoppedChatRuntimesCloseUpstreamIdleConnections(t *testing.T) {
	var mu sync.Mutex
	states := map[net.Conn]http.ConnState{}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fake-response","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	upstream.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		if state == http.StateClosed {
			delete(states, conn)
		} else {
			states[conn] = state
		}
	}
	upstream.Start()
	defer upstream.Close()
	defer upstream.CloseClientConnections()
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for range 4 {
		runtime, err := StartOpenAIChatAPI(context.Background(), upstream.URL+"/v1", "fake-key", "gpt-test")
		if err != nil {
			t.Fatal(err)
		}
		r, _ := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/messages", strings.NewReader(`{"model":"gpt-test","max_tokens":16,"messages":[{"role":"user","content":"fake"}]}`))
		r.Header.Set("x-api-key", runtime.APIKey())
		response, err := client.Do(r)
		if err != nil {
			runtime.Stop()
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		runtime.Stop()
		if response.StatusCode != 200 {
			t.Fatal(fmt.Sprintf("HTTP %d: %s", response.StatusCode, body))
		}
	}
	idle := 0
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		idle = len(states)
		mu.Unlock()
		if idle == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if idle != 0 {
		t.Fatalf("stopped runtimes retained %d upstream connections", idle)
	}
	t.Logf("Closed upstream idle connections after stopping four runtimes")
}
