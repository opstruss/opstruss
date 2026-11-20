package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// fakebackend is a plugin backend for the tests, which serves the backend
// protocol on its stdin and stdout: each key is a file under $INPUT_DIR,
// its etag the sha256 of its content, and a lock a file made only when
// there is none, which holds its holder and token, so a lock outlives its
// holder as one in S3 does. A flock on .flock in the directory makes each
// operation one step for every process that shares it, as two checkouts
// do. $INPUT_BREAK names a rule of the contract to break, for the probe:
//
//	delete-if-match   delete a key whatever if_match says
//	stderr            write each method to stderr
func fakebackend(_ []string) int {
	dir, broken := os.Getenv("INPUT_DIR"), os.Getenv("INPUT_BREAK")
	if dir == "" {
		fmt.Fprintln(os.Stderr, "fakebackend: input dir is empty")
		return 1
	}
	dir, err := filepath.Abs(dir)
	if err == nil {
		err = os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakebackend: %v\n", err)
		return 1
	}
	fb := &fakeStore{dir: dir, broken: broken}
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := json.NewEncoder(os.Stdout)
	for {
		line, err := in.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return 0
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Fprintf(os.Stderr, "fakebackend: %v\n", err)
			return 1
		}
		if broken == "stderr" {
			fmt.Fprintf(os.Stderr, "fakebackend: %s\n", req.Method)
		}
		result, code, msg, data := fb.serve(req.Method, req.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if msg != "" {
			e := map[string]any{"code": code, "message": msg}
			if data != nil {
				e["data"] = data
			}
			resp["error"] = e
		} else {
			resp["result"] = result
		}
		if err := out.Encode(resp); err != nil {
			return 1
		}
	}
}

type fakeStore struct {
	dir, broken string
}

// serve answers one request: a result, or a JSON-RPC error with the code
// of the contract in its data.
func (fb *fakeStore) serve(method string, params map[string]any) (any, int, string, map[string]any) {
	str := func(k string) string { s, _ := params[k].(string); return s }
	contract := func(code, msg string) (any, int, string, map[string]any) {
		return nil, -32000, msg, map[string]any{"code": code}
	}
	if method == "initialize" {
		if p, _ := params["protocol"].(float64); p != 1 {
			return nil, -32000, fmt.Sprintf("fakebackend speaks protocol 1, not %v", params["protocol"]), nil
		}
		return map[string]any{"protocol": 1, "location": "fake:" + fb.dir, "capabilities": map[string]any{}}, 0, "", nil
	}
	key := str("key")
	if method == "list" {
		key = strings.TrimSuffix(str("prefix"), "/")
	}
	file, err := fb.path(key)
	if err != nil {
		return nil, -32602, err.Error(), nil
	}
	unlock, err := fb.flock()
	if err != nil {
		return nil, -32000, err.Error(), nil
	}
	defer unlock()
	var data []byte
	var readErr error
	if method != "list" {
		data, readErr = os.ReadFile(file)
	}
	exists := method != "list" && readErr == nil
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return nil, -32000, readErr.Error(), nil
	}
	etag := fakeETag(data)
	switch method {
	case "get":
		if !exists {
			return contract("not_found", "no such key: "+key)
		}
		return map[string]any{"data": string(data), "etag": etag}, 0, "", nil
	case "put":
		if none, _ := params["if_none_match"].(bool); none && exists {
			return contract("conflict", key+" exists")
		}
		if m := str("if_match"); m != "" && (!exists || m != etag) {
			return contract("conflict", key+" changed")
		}
		if err := fakeWrite(file, []byte(str("data"))); err != nil {
			return nil, -32000, err.Error(), nil
		}
		return map[string]any{"etag": fakeETag([]byte(str("data")))}, 0, "", nil
	case "delete":
		switch {
		case !exists:
			return contract("not_found", "no such key: "+key)
		case str("if_match") != etag && fb.broken != "delete-if-match":
			return contract("conflict", key+" changed")
		}
		if err := os.Remove(file); err != nil {
			return nil, -32000, err.Error(), nil
		}
		return map[string]any{}, 0, "", nil
	case "list":
		var keys []string
		err := filepath.WalkDir(file, func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
				return err
			}
			rel, err := filepath.Rel(fb.dir, p)
			keys = append(keys, filepath.ToSlash(rel))
			return err
		})
		if err != nil {
			return nil, -32000, err.Error(), nil
		}
		slices.Sort(keys)
		return map[string]any{"keys": keys}, 0, "", nil
	case "lock":
		if exists {
			var held map[string]any
			_ = json.Unmarshal(data, &held)
			return nil, -32000, key + " is locked", map[string]any{"code": "locked", "holder": held["holder"], "token": held["token"]}
		}
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		token := hex.EncodeToString(b)
		lock, _ := json.Marshal(map[string]any{"holder": params["holder"], "token": token})
		if err := fakeWrite(file, lock); err != nil {
			return nil, -32000, err.Error(), nil
		}
		return map[string]any{"token": token}, 0, "", nil
	case "unlock":
		if !exists {
			return contract("not_found", key+" is not locked")
		}
		var held map[string]any
		_ = json.Unmarshal(data, &held)
		if held["token"] != str("token") {
			return contract("conflict", key+" has another holder")
		}
		if err := os.Remove(file); err != nil {
			return nil, -32000, err.Error(), nil
		}
		return map[string]any{}, 0, "", nil
	}
	return nil, -32601, "fakebackend has no method " + method, nil
}

func (fb *fakeStore) path(key string) (string, error) {
	if key == "" || path.IsAbs(key) || path.Clean(key) != key || key == ".." || strings.HasPrefix(key, "../") {
		return "", fmt.Errorf("invalid key %q", key)
	}
	return filepath.Join(fb.dir, filepath.FromSlash(key)), nil
}

// flock makes one operation of every process that shares the directory
// happen at once.
func (fb *fakeStore) flock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(fb.dir, ".flock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

func fakeETag(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fakeWrite(file string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(file), ".tmp-"+filepath.Base(file))
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
