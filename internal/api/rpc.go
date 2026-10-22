package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The socket of a daemon speaks JSON-RPC 2.0, one message per line, as MCP
// does over stdio. A caller opens with initialize, then makes one call, a
// method of Methods with a Request. While it runs, the daemon sends the
// events of its run as event notifications and its questions as approve
// and ask requests, which the caller answers; a cancel notification stops
// the call. A caller that goes away answers no more questions, as at a run
// without a terminal, and its run goes on.

// Methods are the calls that a daemon runs for its callers: those that
// make a run.
var Methods = []string{"plan", "up", "down", "status", "run", "unlock"}

// Codes of errors beside those of JSON-RPC.
const (
	codeInvalid  = -32600
	codeMethod   = -32601
	codeParams   = -32602
	codeFailed   = -32000 // the call failed; data holds the problems of a project that does not load
	codeProtocol = -32001 // the caller speaks another protocol
	codeStopping = -32002 // the daemon stops and takes no new call
)

// waitTimeout bounds the wait for the opening messages of a connection, so
// a caller that never speaks does not hold a stopping daemon, and for a
// write, so a caller that stops reading does not hold a run.
const waitTimeout = 10 * time.Second

// Initialize is what each side says of itself when a connection opens;
// Terminal says that the caller answers questions.
type Initialize struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	Terminal bool   `json:"terminal,omitempty"`
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int        `json:"code"`
	Message string     `json:"message"`
	Data    *errorData `json:"data,omitempty"`
}

type errorData struct {
	Protocol int            `json:"protocol,omitempty"`
	Version  string         `json:"version,omitempty"`
	Problems model.Problems `json:"problems,omitempty"`
}

type approval struct {
	Approved bool `json:"approved"`
}

type askParams struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Sensitive   bool   `json:"sensitive,omitempty"`
}

type answer struct {
	Value string `json:"value"`
}

// ProtocolError is a daemon that speaks another protocol than this
// program, from another version of OpsTruss.
type ProtocolError struct {
	Protocol int
	Version  string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("the daemon runs opstruss %s, which speaks protocol %d, not %d", e.Version, e.Protocol, Protocol)
}

// ErrStopping is the answer of a daemon that stops.
var ErrStopping = errors.New("the daemon is stopping")

var errGone = errors.New("the caller is gone")

type conn struct {
	nc     net.Conn
	dec    *json.Decoder
	mu     sync.Mutex // writes
	enc    *json.Encoder
	broken bool
}

func newConn(nc net.Conn) *conn {
	enc := json.NewEncoder(nc)
	enc.SetEscapeHTML(false)
	return &conn{nc: nc, dec: json.NewDecoder(nc), enc: enc}
}

// send writes m; after one write fails, the others are dropped.
func (c *conn) send(m message) error {
	m.JSONRPC = "2.0"
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken {
		return errGone
	}
	_ = c.nc.SetWriteDeadline(time.Now().Add(waitTimeout))
	err := c.enc.Encode(m)
	c.broken = err != nil
	return err
}

// read reads one message, within timeout unless it is 0.
func (c *conn) read(timeout time.Duration) (message, error) {
	if timeout > 0 {
		_ = c.nc.SetReadDeadline(time.Now().Add(timeout))
		defer func() { _ = c.nc.SetReadDeadline(time.Time{}) }()
	}
	var m message
	return m, c.dec.Decode(&m)
}

func raw(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// Server answers the connections to the socket of a daemon.
type Server struct {
	Version string
	// Call runs one call, with the callbacks of its caller.
	Call func(ctx context.Context, method string, r Request, h Handlers) (*Result, error)
	// Stopping reports that the daemon takes no new call.
	Stopping func() bool
}

// ServeConn answers one connection, from a process of this user only:
// initialize, then one call, which ctx cancels.
func (s *Server) ServeConn(ctx context.Context, nc net.Conn) {
	defer func() { _ = nc.Close() }()
	if uid, err := peerUID(nc); err != nil || uid != os.Getuid() {
		return
	}
	c := newConn(nc)
	m, err := c.read(waitTimeout)
	if err != nil {
		return
	}
	var init Initialize
	if m.Method != "initialize" || json.Unmarshal(m.Params, &init) != nil {
		_ = c.send(message{ID: m.ID, Error: &rpcError{Code: codeInvalid, Message: "a connection opens with initialize"}})
		return
	}
	if init.Protocol != Protocol {
		_ = c.send(message{ID: m.ID, Error: &rpcError{Code: codeProtocol,
			Message: fmt.Sprintf("this daemon speaks protocol %d, not %d", Protocol, init.Protocol),
			Data:    &errorData{Protocol: Protocol, Version: s.Version}}})
		return
	}
	if c.send(message{ID: m.ID, Result: raw(Initialize{Protocol: Protocol, Version: s.Version})}) != nil {
		return
	}
	if m, err = c.read(waitTimeout); err != nil {
		return
	}
	req, rerr := s.request(m)
	if rerr != nil {
		_ = c.send(message{ID: m.ID, Error: rerr})
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := &questions{c: c, pending: map[string]chan message{}, gone: make(chan struct{})}
	go q.listen(cancel)
	h := Handlers{Event: func(ev record.Event) { _ = c.send(message{Method: "event", Params: raw(ev)}) }}
	if init.Terminal {
		h.Approve, h.Ask = q.approve, q.ask
	}
	res, err := s.Call(ctx, m.Method, req, h)
	if err != nil {
		_ = c.send(message{ID: m.ID, Error: failed(err)})
		return
	}
	_ = c.send(message{ID: m.ID, Result: raw(res)})
}

// request checks a call against the schema of requests.
func (s *Server) request(m message) (Request, *rpcError) {
	var r Request
	switch {
	case s.Stopping != nil && s.Stopping():
		return r, &rpcError{Code: codeStopping, Message: ErrStopping.Error()}
	case !slices.Contains(Methods, m.Method):
		return r, &rpcError{Code: codeMethod, Message: fmt.Sprintf("the daemon has no method %q", m.Method)}
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(m.Params))
	if err != nil {
		return r, &rpcError{Code: codeParams, Message: err.Error()}
	}
	probs, err := schema.Validate(schema.APIRequest, doc)
	if err == nil && len(probs) > 0 {
		err = fmt.Errorf("%s: %s", probs[0].Path, probs[0].Message)
	}
	if err == nil {
		err = json.Unmarshal(m.Params, &r)
	}
	if err != nil {
		return r, &rpcError{Code: codeParams, Message: err.Error()}
	}
	return r, nil
}

func failed(err error) *rpcError {
	e := &rpcError{Code: codeFailed, Message: err.Error()}
	var probs model.Problems
	if errors.As(err, &probs) {
		e.Data = &errorData{Problems: probs}
	}
	return e
}

// questions are the requests that a call sends its caller, with the
// answers that come back.
type questions struct {
	c       *conn
	mu      sync.Mutex
	next    int
	pending map[string]chan message
	gone    chan struct{} // closed when the caller is gone
}

// listen reads what the caller sends while its call runs.
func (q *questions) listen(cancel context.CancelFunc) {
	defer close(q.gone)
	for {
		m, err := q.c.read(0)
		if err != nil {
			return
		}
		if m.Method == "cancel" {
			cancel()
			continue
		}
		q.mu.Lock()
		ch := q.pending[string(m.ID)]
		delete(q.pending, string(m.ID))
		q.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

// request asks the caller and waits for the answer.
func (q *questions) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	q.mu.Lock()
	q.next++
	id := raw(q.next)
	ch := make(chan message, 1)
	q.pending[string(id)] = ch
	q.mu.Unlock()
	if err := q.c.send(message{ID: id, Method: method, Params: raw(params)}); err != nil {
		return nil, errGone
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, errors.New(m.Error.Message)
		}
		return m.Result, nil
	case <-q.gone:
		return nil, errGone
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func (q *questions) approve(ctx context.Context, qu engine.Question) (bool, error) {
	res, err := q.request(ctx, "approve", qu)
	if errors.Is(err, errGone) {
		return false, nil
	}
	var a approval
	if err == nil {
		err = json.Unmarshal(res, &a)
	}
	return a.Approved, err
}

func (q *questions) ask(ctx context.Context, name string, in *model.Input) (string, error) {
	res, err := q.request(ctx, "ask", askParams{Name: name, Description: in.Description, Sensitive: in.Sensitive})
	var a answer
	if err == nil {
		err = json.Unmarshal(res, &a)
	}
	return a.Value, err
}

// Client is a connection to the socket of a daemon, for one call.
type Client struct {
	c *conn
	// Version is the version of OpsTruss that the daemon runs.
	Version string
}

// Dial connects to the socket at path and opens the connection with
// initialize; terminal says whether the caller answers questions.
func Dial(path, version string, terminal bool) (*Client, error) {
	nc, err := net.DialTimeout("unix", path, waitTimeout)
	if err != nil {
		return nil, err
	}
	c := newConn(nc)
	cl, err := open(c, version, terminal)
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	return cl, nil
}

func open(c *conn, version string, terminal bool) (*Client, error) {
	if err := c.send(message{ID: raw(0), Method: "initialize", Params: raw(Initialize{Protocol: Protocol, Version: version, Terminal: terminal})}); err != nil {
		return nil, err
	}
	m, err := c.read(waitTimeout)
	if err != nil {
		return nil, err
	}
	if m.Error != nil {
		if m.Error.Code == codeProtocol && m.Error.Data != nil {
			return nil, &ProtocolError{Protocol: m.Error.Data.Protocol, Version: m.Error.Data.Version}
		}
		return nil, errors.New(m.Error.Message)
	}
	var init Initialize
	if err := json.Unmarshal(m.Result, &init); err != nil {
		return nil, err
	}
	return &Client{c: c, Version: init.Version}, nil
}

// Close closes a connection that makes no call.
func (cl *Client) Close() error { return cl.c.nc.Close() }

// Call makes the one call of the connection, then closes it. Events and
// questions go to h while the call runs; when ctx ends, the call is
// cancelled, and Call waits for its result.
func (cl *Client) Call(ctx context.Context, method string, r Request, h Handlers) (*Result, error) {
	defer func() { _ = cl.Close() }()
	id := raw(1)
	if err := cl.c.send(message{ID: id, Method: method, Params: raw(r)}); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = cl.c.send(message{Method: "cancel"}) })
	defer stop()
	for {
		m, err := cl.c.read(0)
		if err != nil {
			return nil, fmt.Errorf("lost the daemon: %w", err)
		}
		switch m.Method {
		case "event":
			var ev record.Event
			if json.Unmarshal(m.Params, &ev) == nil && h.Event != nil {
				h.Event(ev)
			}
		case "approve", "ask":
			go cl.answer(ctx, m, h)
		case "":
			if !bytes.Equal(m.ID, id) {
				continue
			}
			if m.Error != nil {
				return nil, callError(m.Error)
			}
			var res Result
			if err := json.Unmarshal(m.Result, &res); err != nil {
				return nil, err
			}
			return &res, nil
		}
	}
}

// answer answers one question of the daemon with h.
func (cl *Client) answer(ctx context.Context, m message, h Handlers) {
	var res any
	var err error
	switch m.Method {
	case "approve":
		var q engine.Question
		if err = json.Unmarshal(m.Params, &q); err == nil && h.Approve != nil {
			var ok bool
			ok, err = h.Approve(ctx, q)
			res = approval{Approved: ok}
		} else if err == nil {
			res = approval{}
		}
	case "ask":
		var p askParams
		if err = json.Unmarshal(m.Params, &p); err == nil && h.Ask != nil {
			var v string
			v, err = h.Ask(ctx, p.Name, &model.Input{Description: p.Description, Sensitive: p.Sensitive})
			res = answer{Value: v}
		} else if err == nil {
			err = errors.New("no terminal answers")
		}
	}
	reply := message{ID: m.ID, Result: raw(res)}
	if err != nil {
		reply = message{ID: m.ID, Error: &rpcError{Code: codeFailed, Message: err.Error()}}
	}
	_ = cl.c.send(reply)
}

func callError(e *rpcError) error {
	switch {
	case e.Code == codeStopping:
		return ErrStopping
	case e.Data != nil && len(e.Data.Problems) > 0:
		return e.Data.Problems
	}
	return errors.New(e.Message)
}
