// Package jsonrpc is a minimal bidirectional JSON-RPC 2.0 peer over
// newline-delimited JSON, which is what the MCP stdio transport speaks.
//
// Both sides of an MCP session send requests: the client calls tools/call
// and tasks/*, and a server talking to an older client calls
// elicitation/create. A Conn handles incoming
// requests with a Handler and lets the caller issue its own with Call.
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// Standard JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Error is a JSON-RPC error object. Handlers return it to choose the code.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// Errorf returns an *Error with the given code.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Handler serves an incoming request or notification. For a notification
// the result is discarded.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Conn is one end of a JSON-RPC session.
type Conn struct {
	r       io.Reader
	w       io.Writer
	handler Handler

	// Trace, if set, sees every frame: dir is "send" or "recv".
	Trace func(dir string, frame []byte)

	wmu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan *message
	closed  error
}

// NewConn returns a Conn reading from r and writing to w. Call Run to start it.
func NewConn(r io.Reader, w io.Writer, h Handler) *Conn {
	if h == nil {
		h = func(context.Context, string, json.RawMessage) (any, error) {
			return nil, Errorf(CodeMethodNotFound, "method not found")
		}
	}
	return &Conn{r: r, w: w, handler: h, pending: map[string]chan *message{}}
}

// Run reads frames until r is exhausted or ctx is done. Each incoming request
// is served on its own goroutine, so a handler may block (tasks/result does).
func (c *Conn) Run(ctx context.Context) error {
	sc := bufio.NewScanner(c.r)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(line) == 0 {
			continue
		}
		if c.Trace != nil {
			c.Trace("recv", line)
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			c.send(&message{ID: json.RawMessage("null"), Error: Errorf(CodeParseError, "parse error: %v", err)})
			continue
		}
		switch {
		case m.Method != "":
			go c.serve(ctx, &m)
		case m.ID != nil:
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- &m
			}
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	c.mu.Lock()
	c.closed = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	return err
}

type idKey struct{}

// RequestID returns the ID of the request a handler is serving, or nil for a
// notification.
func RequestID(ctx context.Context) json.RawMessage {
	id, _ := ctx.Value(idKey{}).(json.RawMessage)
	return id
}

func (c *Conn) serve(ctx context.Context, m *message) {
	if m.ID != nil {
		ctx = context.WithValue(ctx, idKey{}, m.ID)
	}
	res, err := c.handler(ctx, m.Method, m.Params)
	if m.ID == nil {
		return // notification
	}
	reply := &message{ID: m.ID}
	if err != nil {
		var rpcErr *Error
		if !errors.As(err, &rpcErr) {
			rpcErr = &Error{Code: CodeInternalError, Message: err.Error()}
		}
		reply.Error = rpcErr
	} else {
		if res == nil {
			res = struct{}{}
		}
		b, mErr := json.Marshal(res)
		if mErr != nil {
			reply.Error = Errorf(CodeInternalError, "marshaling result: %v", mErr)
		} else {
			reply.Result = b
		}
	}
	c.send(reply)
}

func (c *Conn) send(m *message) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if c.Trace != nil {
		c.Trace("send", b)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// Call sends a request and decodes its result into result (which may be nil).
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed != nil {
		c.mu.Unlock()
		return fmt.Errorf("connection closed: %w", c.closed)
	}
	c.nextID++
	id := json.RawMessage(strconv.FormatInt(c.nextID, 10))
	ch := make(chan *message, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()

	if err := c.send(&message{ID: id, Method: method, Params: p}); err != nil {
		return err
	}
	select {
	case reply, ok := <-ch:
		if !ok {
			return errors.New("connection closed while waiting for " + method)
		}
		if reply.Error != nil {
			return reply.Error
		}
		if result != nil {
			return json.Unmarshal(reply.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		return ctx.Err()
	}
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.send(&message{Method: method, Params: p})
}
