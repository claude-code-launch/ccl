package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
)

const jsonRPCVersion = "2.0"

const (
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcCall struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  any             `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func isNotification(id json.RawMessage) bool {
	return len(id) == 0
}

type encoder struct {
	mu      sync.Mutex
	out     writeFlusher
	nextID  atomic.Int64
	pending map[string]chan rpcResponse
}

type writeFlusher interface {
	Write([]byte) (int, error)
}

func (e *encoder) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// One Write: io.Pipe delivers a Write as a single Read. Splitting the
	// JSON and newline lets json.Decoder return after the payload and then
	// blocks forever on the leftover '\n'.
	frame := make([]byte, len(data)+1)
	copy(frame, data)
	frame[len(data)] = '\n'
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err = e.out.Write(frame)
	return err
}

func (e *encoder) reply(id json.RawMessage, result any) error {
	if isNotification(id) {
		return nil
	}
	raw := marshalRaw(result)
	return e.write(rpcResponse{JSONRPC: jsonRPCVersion, ID: id, Result: raw})
}

func (e *encoder) replyError(id json.RawMessage, code int, message string) error {
	if isNotification(id) {
		return nil
	}
	return e.write(rpcResponse{
		JSONRPC: jsonRPCVersion,
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	})
}

func (e *encoder) notify(method string, params any) error {
	return e.write(rpcNotification{JSONRPC: jsonRPCVersion, Method: method, Params: params})
}

func (e *encoder) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	idNum := e.nextID.Add(1)
	idRaw, err := json.Marshal(idNum)
	if err != nil {
		return nil, err
	}
	ch := make(chan rpcResponse, 1)
	key := string(idRaw)
	e.mu.Lock()
	if e.pending == nil {
		e.pending = map[string]chan rpcResponse{}
	}
	e.pending[key] = ch
	e.mu.Unlock()

	if err := e.write(rpcCall{JSONRPC: jsonRPCVersion, ID: idRaw, Method: method, Params: params}); err != nil {
		e.finish(idRaw)
		return nil, err
	}

	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		e.finish(idRaw)
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			msg := resp.Error.Message
			if msg == "" {
				msg = "request failed"
			}
			return nil, fmt.Errorf("json-rpc %s: %s", method, msg)
		}
		return resp.Result, nil
	}
}

func (e *encoder) complete(id json.RawMessage, line []byte) {
	if isNotification(id) {
		return
	}
	var wire struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	_ = json.Unmarshal(line, &wire)
	key := string(id)
	e.mu.Lock()
	ch, ok := e.pending[key]
	if ok {
		delete(e.pending, key)
	}
	e.mu.Unlock()
	if !ok {
		return
	}
	ch <- rpcResponse{ID: id, Result: wire.Result, Error: wire.Error}
}

func (e *encoder) finish(id json.RawMessage) {
	key := string(id)
	e.mu.Lock()
	ch, ok := e.pending[key]
	if ok {
		delete(e.pending, key)
	}
	e.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- rpcResponse{ID: id, Error: &rpcError{Code: errInternal, Message: "cancelled"}}:
	default:
	}
}
