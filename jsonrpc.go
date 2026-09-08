package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"sync"
)

// message is one JSON-RPC 2.0 message: a request (method and id), a
// notification (method, no id) or a response (id, result or error).
// Params, Result and Error are kept raw: the proxy forwards them
// untouched and only ever reads the few it answers itself.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`

	raw []byte // the line as it arrived; forwarded verbatim
}

func (m *message) hasID() bool          { return len(m.ID) > 0 && string(m.ID) != "null" }
func (m *message) isRequest() bool      { return m.Method != "" && m.hasID() }
func (m *message) isNotification() bool { return m.Method != "" && !m.hasID() }
func (m *message) isResponse() bool     { return m.Method == "" && len(m.ID) > 0 }

// idKey is the id as a map key: the raw JSON, so 1 and "1" stay distinct.
func (m *message) idKey() string { return string(m.ID) }

func request(id json.RawMessage, method string, params any) *message {
	return &message{JSONRPC: "2.0", ID: id, Method: method, Params: mustJSON(params)}
}

func response(id json.RawMessage, result any) *message {
	return &message{JSONRPC: "2.0", ID: id, Result: mustJSON(result)}
}

func errorResponse(id json.RawMessage, code int, text string) *message {
	return &message{JSONRPC: "2.0", ID: id, Error: mustJSON(map[string]any{"code": code, "message": text})}
}

func notification(method string) *message { return &message{JSONRPC: "2.0", Method: method} }

// mustJSON marshals values the proxy builds itself; they can't fail.
func mustJSON(v any) json.RawMessage {
	if raw, ok := v.(json.RawMessage); ok {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// reader yields newline-delimited messages from a stream.
type reader struct{ r *bufio.Reader }

func newReader(r io.Reader) *reader { return &reader{bufio.NewReaderSize(r, 64<<10)} }

// next returns the next message. A line that isn't a JSON-RPC message
// (a server that prints to stdout before it speaks the protocol) comes
// back as junk with a nil message; io.EOF ends the stream.
func (r *reader) next() (m *message, junk []byte, err error) {
	for {
		line, err := r.r.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			if err != nil {
				return nil, nil, err
			}
			continue
		}
		var m message
		if jerr := json.Unmarshal(line, &m); jerr != nil || (m.Method == "" && len(m.ID) == 0) {
			return nil, line, nil
		}
		m.raw = append([]byte(nil), line...)
		return &m, nil, nil
	}
}

// sameJSON compares two documents regardless of formatting and of the
// order of object keys.
func sameJSON(a, b json.RawMessage) bool {
	ca, oka := canonical(a)
	cb, okb := canonical(b)
	return oka && okb && ca == cb
}

func canonical(raw json.RawMessage) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return "", false
	}
	out, err := json.Marshal(v) // maps marshal with sorted keys
	if err != nil {
		return "", false
	}
	return string(out), true
}

// writer serialises messages onto one stream.
type writer struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *writer) send(m *message) error {
	b := m.raw
	if b == nil {
		var err error
		if b, err = json.Marshal(m); err != nil {
			return err
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.w.Write(append(b, '\n'))
	return err
}
