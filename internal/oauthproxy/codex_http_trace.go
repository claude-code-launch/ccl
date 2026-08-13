package oauthproxy

import (
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"
)

type codexHTTPTraceTimings struct {
	connectionReused bool
	connectionWait   time.Duration
	dns              time.Duration
	connect          time.Duration
	tls              time.Duration
	requestWrite     time.Duration
	ttfb             time.Duration
	writeToFirstByte time.Duration
	wroteRequestErr  bool
}

// newCodexHTTPTrace records transport milestones only. It deliberately avoids
// remote addresses, headers, and request content so timing diagnostics remain
// safe to retain in a normal CCL log.
func newCodexHTTPTrace(started time.Time) (*httptrace.ClientTrace, func() codexHTTPTraceTimings) {
	var mu sync.Mutex
	var getConnAt, gotConnAt, dnsStarted, dnsDone time.Time
	var connectStarted, connectDone, tlsStarted, tlsDone time.Time
	var wroteRequestAt, firstResponseByteAt time.Time
	var connectionReused, wroteRequestErr bool
	trace := &httptrace.ClientTrace{
		GetConn: func(string) {
			mu.Lock()
			getConnAt = time.Now()
			mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			gotConnAt = time.Now()
			connectionReused = info.Reused
			mu.Unlock()
		},
		DNSStart: func(httptrace.DNSStartInfo) {
			mu.Lock()
			dnsStarted = time.Now()
			mu.Unlock()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			mu.Lock()
			dnsDone = time.Now()
			mu.Unlock()
		},
		ConnectStart: func(string, string) {
			mu.Lock()
			if connectStarted.IsZero() {
				connectStarted = time.Now()
			}
			mu.Unlock()
		},
		ConnectDone: func(string, string, error) {
			mu.Lock()
			connectDone = time.Now()
			mu.Unlock()
		},
		TLSHandshakeStart: func() {
			mu.Lock()
			tlsStarted = time.Now()
			mu.Unlock()
		},
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			mu.Lock()
			tlsDone = time.Now()
			mu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			mu.Lock()
			wroteRequestAt = time.Now()
			wroteRequestErr = info.Err != nil
			mu.Unlock()
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			firstResponseByteAt = time.Now()
			mu.Unlock()
		},
	}
	snapshot := func() codexHTTPTraceTimings {
		mu.Lock()
		defer mu.Unlock()
		return codexHTTPTraceTimings{
			connectionReused: connectionReused,
			connectionWait:   durationBetween(getConnAt, gotConnAt),
			dns:              durationBetween(dnsStarted, dnsDone),
			connect:          durationBetween(connectStarted, connectDone),
			tls:              durationBetween(tlsStarted, tlsDone),
			requestWrite:     durationBetween(started, wroteRequestAt),
			ttfb:             durationBetween(started, firstResponseByteAt),
			writeToFirstByte: durationBetween(wroteRequestAt, firstResponseByteAt),
			wroteRequestErr:  wroteRequestErr,
		}
	}
	return trace, snapshot
}

func durationBetween(start, end time.Time) time.Duration {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Round(time.Millisecond)
}
