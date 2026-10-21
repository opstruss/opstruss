// Package builtin runs the built-in tasks that need no program: http,
// read, write, set, assert, wait, and log. command runs through the
// program contract in the engine, and kv through the state of the stack.
package builtin

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
)

// Call is one call of a built-in task, with its inputs conformed to the
// task's schema.
type Call struct {
	Task   string
	Inputs map[string]any
	Dir    string                  // where relative paths start
	Log    func(level, msg string) // writes to the log and events of the run
}

// Result is how a call ended. A call that failed has Err, and may still
// have outputs, as http has for a status that it does not accept.
type Result struct {
	Outputs map[string]any
	Changed bool
	Err     error
}

func failed(err error) *Result { return &Result{Outputs: map[string]any{}, Err: err} }

// Run runs c.
func Run(ctx context.Context, c *Call) *Result {
	switch c.Task {
	case "http":
		return httpCall(ctx, c.Inputs)
	case "read":
		return read(c.Dir, c.Inputs)
	case "write":
		return write(c.Dir, c.Inputs)
	case "set":
		return &Result{Outputs: maps.Clone(c.Inputs)}
	case "assert":
		return assert(c.Inputs)
	case "wait":
		return wait(ctx, c.Inputs)
	case "log":
		c.Log(c.Inputs["level"].(string), expr.Text(c.Inputs["msg"]))
		return &Result{Outputs: map[string]any{}}
	}
	return failed(fmt.Errorf("there is no built-in task %s", c.Task))
}

// assert fails when that, a condition or a list of them, has one that is
// false.
func assert(in map[string]any) *Result {
	conds, ok := in["that"].([]any)
	if !ok {
		conds = []any{in["that"]}
	}
	for i, c := range conds {
		b, ok := c.(bool)
		switch {
		case !ok:
			return failed(fmt.Errorf("that takes conditions, true or false; item %d is %s", i, expr.JSON(c)))
		case b:
			continue
		case in["msg"] != nil:
			return failed(errors.New(in["msg"].(string)))
		case len(conds) > 1:
			return failed(fmt.Errorf("item %d of that is false", i))
		}
		return failed(errors.New("that is false"))
	}
	return &Result{Outputs: map[string]any{}}
}

// wait waits for a number of seconds, for a TCP port to take a
// connection, or for a URL to answer with a 2xx status, checking once a
// second until timeout.
func wait(ctx context.Context, in map[string]any) *Result {
	set := 0
	for _, k := range []string{"seconds", "tcp", "http"} {
		if in[k] != nil {
			set++
		}
	}
	if set != 1 {
		return failed(errors.New("wait takes exactly one of seconds, tcp, or http"))
	}
	if s, ok := in["seconds"]; ok && s != nil {
		d := time.Duration(toFloat(s) * float64(time.Second))
		select {
		case <-ctx.Done():
			return failed(ctx.Err())
		case <-time.After(d):
			return &Result{Outputs: map[string]any{}}
		}
	}
	if t, ok := in["timeout"].(string); ok {
		d, err := time.ParseDuration(t)
		if err != nil {
			return failed(fmt.Errorf("timeout: %w", err))
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	ready := func() error {
		if addr, ok := in["tcp"].(string); ok {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", addr)
			if err == nil {
				_ = conn.Close()
			}
			return err
		}
		res := httpCall(ctx, map[string]any{"url": in["http"], "method": "GET", "status_code": "2xx"})
		return res.Err
	}
	for {
		err := ready()
		if err == nil {
			return &Result{Outputs: map[string]any{}}
		}
		select {
		case <-ctx.Done():
			return failed(fmt.Errorf("still waiting when time ran out: %w", err))
		case <-time.After(time.Second):
		}
	}
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return 0
}
