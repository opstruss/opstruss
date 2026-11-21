package state

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

// BackendProtocol is the version of the protocol that a plugin backend
// speaks, which initialize agrees on.
const BackendProtocol = 1

// maxAnswer is the largest message that a plugin backend may send, its
// newline included; a larger one ends the program, as a record with its
// data escaped in a string can be a few times the 1 MiB of its outputs. A
// scan that would be larger answers unsupported.
const maxAnswer = 64 << 20

// Remote is a plugin backend: a program that answers the operations of the
// contract as JSON-RPC 2.0, one message a line, on its stdin and stdout.
// One request runs at a time. Open sends initialize, which gives Location.
type Remote struct {
	In  io.Writer // the program's stdin
	Out io.Reader // the program's stdout
	// Timeout bounds the wait for each answer; then Stop stops the program,
	// and this operation and those after it fail.
	Timeout time.Duration
	Stop    func()
	// Ended says why the program's stdout ended, such as its exit code and
	// the last line it wrote to stderr.
	Ended func() error

	// Location is where the program keeps the state, without credentials,
	// as initialize said, and CanScan whether it named the capability scan.
	Location string
	CanScan  bool

	mu    sync.Mutex
	next  int64
	lines chan []byte
	read  error         // why lines closed
	done  chan struct{} // closed once the program is no longer read
	err   error         // what broke the connection, which every later operation returns
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// errorData is what the contract keeps in the data of an error; data of
// another shape names no error of the contract.
type errorData struct {
	Code   string `json:"code"`
	Holder Holder `json:"holder"`
	Token  string `json:"token"`
}

func (e *rpcError) data() errorData {
	var d errorData
	_ = json.Unmarshal(e.Data, &d)
	return d
}

// Open starts reading the program and sends initialize.
func (r *Remote) Open() error {
	r.lines, r.done = make(chan []byte), make(chan struct{})
	go r.readLines()
	var res struct {
		Protocol     int    `json:"protocol"`
		Location     string `json:"location"`
		Capabilities struct {
			Scan bool `json:"scan"`
		} `json:"capabilities"`
	}
	if err := r.call("initialize", map[string]any{"protocol": BackendProtocol}, &res); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case res.Protocol != BackendProtocol:
		return r.broken(fmt.Errorf("the backend speaks protocol %d, and this OpsTruss %d", res.Protocol, BackendProtocol))
	case res.Location == "":
		return r.broken(errors.New("the backend gave no location in its answer to initialize"))
	}
	r.Location, r.CanScan = res.Location, res.Capabilities.Scan
	return nil
}

// Broken is what broke the connection, after which every operation fails,
// or nil.
func (r *Remote) Broken() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Close stops reading the program, which the caller then ends.
func (r *Remote) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		r.err = errors.New("the backend is closed")
		close(r.done)
	}
}

func (r *Remote) readLines() {
	defer close(r.lines)
	br := bufio.NewReaderSize(r.Out, 64<<10)
	for {
		var line []byte
		for {
			chunk, err := br.ReadSlice('\n')
			if len(line)+len(chunk) > maxAnswer {
				r.read = fmt.Errorf("the backend wrote a message larger than %d bytes", maxAnswer)
				return
			}
			line = append(line, chunk...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				r.read = err
				return
			}
			break
		}
		select {
		case r.lines <- line:
		case <-r.done:
			return
		}
	}
}

// broken ends the connection with err, the error of every later operation.
func (r *Remote) broken(err error) error {
	if r.err == nil {
		r.err = err
		close(r.done)
	}
	return r.err
}

// call sends one request and waits for its answer: a *rpcError is the
// program's answer, any other error a connection that is broken.
func (r *Remote) call(method string, params, result any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.next++
	id := r.next
	msg, err := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return err
	}
	// A program that stops reading would hold the write, as a large record
	// fills the pipe, so the write waits within the same time as the answer.
	timer := time.NewTimer(r.Timeout)
	defer timer.Stop()
	wrote := make(chan error, 1)
	go func() {
		_, err := r.In.Write(append(msg, '\n'))
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if err != nil {
			return r.broken(r.gone(err))
		}
	case <-timer.C:
		if r.Stop != nil {
			r.Stop()
		}
		return r.broken(fmt.Errorf("the backend did not read %s within %s, so it was stopped", method, r.Timeout))
	}
	for {
		select {
		case line, ok := <-r.lines:
			if !ok {
				return r.broken(r.gone(r.read))
			}
			var m rpcMessage
			if err := json.Unmarshal(line, &m); err != nil {
				return r.broken(fmt.Errorf("the backend wrote what is not JSON-RPC: %w", err))
			}
			if m.ID == nil {
				// A notification, which this protocol has none of yet.
				continue
			}
			if *m.ID != id {
				return r.broken(fmt.Errorf("the backend answered request %d while %d waited", *m.ID, id))
			}
			if m.Error != nil {
				return m.Error
			}
			if result != nil {
				if err := json.Unmarshal(m.Result, result); err != nil {
					return r.broken(fmt.Errorf("the answer of the backend to %s: %w", method, err))
				}
			}
			return nil
		case <-timer.C:
			if r.Stop != nil {
				r.Stop()
			}
			return r.broken(fmt.Errorf("the backend did not answer %s within %s, so it was stopped", method, r.Timeout))
		}
	}
}

// gone says why the program stopped answering.
func (r *Remote) gone(err error) error {
	if r.Ended != nil {
		if why := r.Ended(); why != nil {
			return fmt.Errorf("the backend stopped: %w", why)
		}
	}
	if err == nil || errors.Is(err, io.EOF) {
		return errors.New("the backend stopped")
	}
	return fmt.Errorf("the backend stopped: %w", err)
}

func (e *rpcError) Error() string { return e.Message }

// opError is the error of an operation that the program refused with an
// error the contract does not name.
func opError(method, key string, err error) error {
	return fmt.Errorf("backend %s %s: %w", method, key, err)
}

// answer turns the program's error into the one of the contract that its
// code names, or into an operation error.
func answer(method, key string, err error, codes map[string]error) error {
	var re *rpcError
	if !errors.As(err, &re) {
		return err
	}
	if known, ok := codes[re.data().Code]; ok {
		return known
	}
	return opError(method, key, err)
}

func (r *Remote) Get(key string) ([]byte, string, error) {
	var res struct {
		Data *string `json:"data"`
		ETag string  `json:"etag"`
	}
	err := r.call("get", map[string]any{"key": key}, &res)
	if err != nil {
		return nil, "", answer("get", key, err, map[string]error{"not_found": ErrNotExist})
	}
	if res.Data == nil || res.ETag == "" {
		return nil, "", opError("get", key, errors.New("the answer has no data or etag"))
	}
	return []byte(*res.Data), res.ETag, nil
}

func (r *Remote) Put(key string, data []byte, ifMatch string) (string, error) {
	params := map[string]any{"key": key, "data": string(data)}
	if ifMatch == "" {
		params["if_none_match"] = true
	} else {
		params["if_match"] = ifMatch
	}
	var res struct {
		ETag string `json:"etag"`
	}
	if err := r.call("put", params, &res); err != nil {
		return "", answer("put", key, err, map[string]error{"conflict": ErrConflict})
	}
	if res.ETag == "" {
		return "", opError("put", key, errors.New("the answer has no etag"))
	}
	return res.ETag, nil
}

func (r *Remote) Delete(key string, ifMatch string) error {
	err := r.call("delete", map[string]any{"key": key, "if_match": ifMatch}, nil)
	return answer("delete", key, err, map[string]error{"conflict": ErrConflict, "not_found": ErrNotExist})
}

func (r *Remote) List(prefix string) ([]string, error) {
	var res struct {
		Keys []string `json:"keys"`
	}
	if err := r.call("list", map[string]any{"prefix": prefix}, &res); err != nil {
		return nil, answer("list", prefix, err, nil)
	}
	slices.Sort(res.Keys)
	return res.Keys, nil
}

// Scan gives every key under prefix with its content. It is
// errors.ErrUnsupported from a program without the capability scan, and
// from one that answers unsupported, as one does whose answer would be
// larger than a message can be: then the caller lists the keys instead.
func (r *Remote) Scan(prefix string) (map[string][]byte, error) {
	if !r.CanScan {
		return nil, errors.ErrUnsupported
	}
	var res struct {
		Items []struct {
			Key  string  `json:"key"`
			Data *string `json:"data"`
		} `json:"items"`
	}
	if err := r.call("scan", map[string]any{"prefix": prefix}, &res); err != nil {
		return nil, answer("scan", prefix, err, map[string]error{"unsupported": errors.ErrUnsupported})
	}
	// An answer without items would read as a scope without records.
	if res.Items == nil {
		return nil, opError("scan", prefix, errors.New("the answer has no items"))
	}
	out := make(map[string][]byte, len(res.Items))
	for _, it := range res.Items {
		switch {
		case !strings.HasPrefix(it.Key, prefix):
			return nil, opError("scan", prefix, fmt.Errorf("the answer has the key %s, which is not under the prefix", it.Key))
		case it.Data == nil:
			return nil, opError("scan", prefix, fmt.Errorf("the answer has no data for %s", it.Key))
		}
		out[it.Key] = []byte(*it.Data)
	}
	return out, nil
}

func (r *Remote) Lock(key string, h Holder) (string, error) {
	h.Version = HolderVersion
	var res struct {
		Token string `json:"token"`
	}
	err := r.call("lock", map[string]any{"key": key, "holder": h}, &res)
	var re *rpcError
	if errors.As(err, &re) {
		if d := re.data(); d.Code == "locked" {
			return "", &LockedError{Key: key, Holder: d.Holder, Token: d.Token}
		}
	}
	if err != nil {
		return "", answer("lock", key, err, nil)
	}
	if res.Token == "" {
		return "", opError("lock", key, errors.New("the answer has no token"))
	}
	return res.Token, nil
}

func (r *Remote) Unlock(key, token string) error {
	err := r.call("unlock", map[string]any{"key": key, "token": token}, nil)
	return answer("unlock", key, err, map[string]error{"conflict": ErrNotHolder, "not_found": ErrNotHolder})
}
