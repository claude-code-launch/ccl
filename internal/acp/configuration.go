package acp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

type configurationEntry struct {
	lease  LaunchLease
	cancel context.CancelFunc
	closed bool
	ready  bool
}

// This private loopback service carries configuration, not ACP. Its token is
// removed from the adapter's environment before the SDK or tools are started.
type configurationService struct {
	listener net.Listener
	server   *http.Server
	token    string
	acquire  func(context.Context) (LaunchLease, error)
	observe  func(json.RawMessage)
	mu       sync.Mutex
	entries  map[string]*configurationEntry
	closed   bool
}

func startConfigurationService(acquire func(context.Context) (LaunchLease, error), observe func(json.RawMessage)) (*configurationService, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		_ = listener.Close()
		return nil, err
	}
	s := &configurationService{listener: listener, token: hex.EncodeToString(token[:]),
		acquire: acquire, observe: observe, entries: make(map[string]*configurationEntry)}
	s.server = &http.Server{Handler: http.HandlerFunc(s.handle), ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 15 * time.Second}
	go func() { _ = s.server.Serve(listener) }()
	return s, nil
}

func (s *configurationService) URL() string { return "http://" + s.listener.Addr().String() }

func (s *configurationService) Close() {
	s.mu.Lock()
	s.closed = true
	for _, entry := range s.entries {
		entry.closed = true
		if entry.cancel != nil {
			entry.cancel()
		}
	}
	s.mu.Unlock()
	_ = s.server.Close()
	s.mu.Lock()
	for _, entry := range s.entries {
		if entry.lease != nil {
			entry.lease.Release()
			entry.lease = nil
		}
	}
	s.mu.Unlock()
}

func (s *configurationService) release(id string) {
	s.mu.Lock()
	entry := s.entries[id]
	if entry == nil {
		// A release may overtake a cancelled acquire request on another HTTP
		// connection. Keep a tombstone so that acquire cannot leak a lease.
		entry = &configurationEntry{}
		s.entries[id] = entry
	}
	entry.closed = true
	if entry.cancel != nil {
		entry.cancel()
	}
	lease := entry.lease
	entry.lease = nil
	if entry.ready {
		delete(s.entries, id)
	}
	s.mu.Unlock()
	if lease != nil {
		lease.Release()
	}
}

func (s *configurationService) handle(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		LeaseID   string `json:"leaseId"`
		CWD       string `json:"cwd"`
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		http.Error(w, "invalid configuration request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if request.LeaseID == "" || len(request.LeaseID) > 128 {
		http.Error(w, "invalid configuration lease id", http.StatusBadRequest)
		return
	}
	if r.URL.Path == "/release" {
		s.release(request.LeaseID)
		_, _ = io.WriteString(w, `{}`)
		return
	}
	if r.URL.Path != "/acquire" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	entry := &configurationEntry{cancel: cancel}
	s.mu.Lock()
	if s.closed || s.entries[request.LeaseID] != nil {
		s.mu.Unlock()
		http.Error(w, "configuration lease already closed or in use", http.StatusConflict)
		return
	}
	s.entries[request.LeaseID] = entry
	s.mu.Unlock()
	lease, err := s.acquire(ctx)
	if err == nil && (lease == nil || lease.Generation() == 0) {
		err = fmt.Errorf("invalid provider configuration lease")
	}
	var options json.RawMessage
	if err == nil {
		options, err = lease.Options(request.CWD)
		if err == nil && s.observe != nil {
			s.observe(options)
		}
	}
	s.mu.Lock()
	if err == nil && (entry.closed || s.closed || ctx.Err() != nil) {
		err = context.Canceled
	}
	if err == nil {
		entry.lease = lease
		entry.ready = true
		entry.cancel = nil
	} else {
		delete(s.entries, request.LeaseID)
	}
	s.mu.Unlock()
	if err != nil {
		if lease != nil {
			lease.Release()
		}
		http.Error(w, "prepare ACP provider: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := json.NewEncoder(w).Encode(struct {
		LeaseID    string          `json:"leaseId"`
		Generation uint64          `json:"generation"`
		Options    json.RawMessage `json:"options"`
	}{request.LeaseID, lease.Generation(), options}); err != nil {
		s.release(request.LeaseID)
	}
}
