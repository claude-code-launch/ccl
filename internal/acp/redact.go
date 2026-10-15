package acp

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"
)

// The upstream SDK may include its settings argument in command errors. CCL
// owns those credentials, so redact their literal and JSON-escaped forms at
// the stderr boundary, including values split across individual writes.
type credentialLog struct {
	mu      sync.Mutex
	out     io.Writer
	pending []byte
	secrets map[string]bool
}

func newCredentialLog(out io.Writer, env []string) *credentialLog {
	if out == nil {
		out = io.Discard
	}
	log := &credentialLog{out: out, secrets: make(map[string]bool)}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		log.add(key, value)
	}
	return log
}

func (l *credentialLog) add(key, value string) {
	key = strings.ToUpper(key)
	if value == "" || !(strings.Contains(key, "KEY") || strings.Contains(key, "TOKEN") ||
		strings.Contains(key, "SECRET") || strings.Contains(key, "PASSWORD") || key == "ANTHROPIC_CUSTOM_HEADERS") {
		return
	}
	l.secrets[value] = true
	encoded, _ := json.Marshal(value)
	l.secrets[string(encoded[1:len(encoded)-1])] = true
}

func (l *credentialLog) options(options json.RawMessage) {
	var parsed struct {
		Env map[string]string `json:"env"`
	}
	if json.Unmarshal(options, &parsed) == nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		for key, value := range parsed.Env {
			l.add(key, value)
		}
	}
}

func (l *credentialLog) safe(data []byte) []byte {
	keys := make([]string, 0, len(l.secrets))
	for key := range l.secrets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, key := range keys {
		data = bytes.ReplaceAll(data, []byte(key), []byte("[redacted]"))
	}
	return data
}

func (l *credentialLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = append(l.pending, data...)
	if last := bytes.LastIndexByte(l.pending, '\n'); last >= 0 {
		if _, err := l.out.Write(l.safe(l.pending[:last+1])); err != nil {
			return 0, err
		}
		l.pending = append(l.pending[:0], l.pending[last+1:]...)
	}
	return len(data), nil
}

func (l *credentialLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) > 0 {
		_, _ = l.out.Write(l.safe(l.pending))
		l.pending = nil
	}
}
