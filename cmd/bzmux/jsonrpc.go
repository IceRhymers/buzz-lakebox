package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// JSON-RPC 2.0 error codes.
const (
	errCodeMethodNotFound = -32601
	errCodeInternalError  = -32603
)

// msg is a JSON-RPC 2.0 message envelope.  bzmux treats params/result/error
// content as opaque bytes (json.RawMessage) and only rewrites the envelope id
// field for multiplexing.
type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// hasID returns true when the message carries a non-null id.
func (m *msg) hasID() bool {
	return len(m.ID) > 0 && string(m.ID) != "null"
}

// isRequest returns true for a JSON-RPC request (method AND non-null id).
func (m *msg) isRequest() bool { return m.Method != "" && m.hasID() }

// isNotification returns true for a notification (method, no id or null id).
func (m *msg) isNotification() bool { return m.Method != "" && !m.hasID() }

// isResponse returns true for a response (id, no method).
func (m *msg) isResponse() bool { return m.Method == "" && m.hasID() }

// rpcError is the JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// errorResp builds a JSON-RPC error response for id.
func errorResp(id json.RawMessage, code int, message string) *msg {
	return &msg{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	}
}

// rawIntID encodes an int64 as a JSON number id.
func rawIntID(id int64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf("%d", id))
}

// lineWriter serialises concurrent JSON-RPC message writes to an io.Writer.
// MCP stdio uses newline-delimited JSON-RPC 2.0.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newLineWriter(w io.Writer) *lineWriter { return &lineWriter{w: w} }

// send marshals m and writes it as a newline-terminated JSON line.
func (lw *lineWriter) send(m *msg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("lineWriter.send: %w", err)
	}
	lw.mu.Lock()
	defer lw.mu.Unlock()
	_, err = fmt.Fprintf(lw.w, "%s\n", data)
	return err
}

// lineScanner reads newline-delimited JSON-RPC messages from r.
type lineScanner struct {
	sc *bufio.Scanner
}

// maxFrameBytes is the maximum size of a single newline-delimited JSON-RPC
// frame that bzmux will read.  4 MB was the previous cap; 32 MB gives generous
// headroom for large tool results (e.g. shellbox reading a big file) without
// being unbounded.  This is the FRAME size limit and is unrelated to
// buzz-agent's 4096-byte inputSchema rule.
const maxFrameBytes = 32 * 1024 * 1024 // 32 MB

func newLineScanner(r io.Reader) *lineScanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, maxFrameBytes), maxFrameBytes)
	return &lineScanner{sc: sc}
}

// next returns the next message or (nil, io.EOF) on clean close.
// Blank lines are skipped; malformed lines are logged and skipped.
func (ls *lineScanner) next() (*msg, error) {
	for {
		if !ls.sc.Scan() {
			if err := ls.sc.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		line := ls.sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m msg
		if err := json.Unmarshal(line, &m); err != nil {
			logf("bzmux: JSON parse error: %v\n", err)
			continue
		}
		return &m, nil
	}
}
