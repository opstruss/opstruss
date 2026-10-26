package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// fakecloud makes resources that are files in a directory, each holding
// the OPSTRUSS_STATE_KEY of the unit that made it, as a cloud tags a
// resource, so an up that runs again finds what an earlier attempt made.
// It is the program of the plugins in testdata/plugins, which the tests
// copy to testplugins/:
//
//	fakecloud up <dir>       find or make the unit's resource; id= to $OPSTRUSS_OUTPUT
//	fakecloud destroy <dir>     remove the unit's resource
//	fakecloud check <dir> <state>   every resource has a record, and no unit has two
//	fakecloud serve <dir> <port-file> <break-file>   the same as an HTTP API
//
// With OPSTRUSS_STATE_DIR, as for a plugin with state: tool, up keeps the
// id there as the tool's own state and finds the resource by it first, as
// a tool reads its state file. A file break-output in the working
// directory makes up write output that does not parse, after it made the
// resource.
func fakecloud(args []string) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "fakecloud: "+format+"\n", a...)
		return 1
	}
	if len(args) < 2 {
		return fail("usage: fakecloud up|destroy|check|serve <dir> [...]")
	}
	cloud := cloud(args[1])
	if err := os.MkdirAll(cloud.dir(), 0o700); err != nil {
		return fail("%v", err)
	}
	key := os.Getenv("OPSTRUSS_STATE_KEY")
	switch args[0] {
	case "up":
		if key == "" {
			return fail("no OPSTRUSS_STATE_KEY")
		}
		id, err := cloud.make(key, os.Getenv("OPSTRUSS_STATE_DIR"))
		if err != nil {
			return fail("%v", err)
		}
		out := "id=" + id + "\n"
		if fileExists("break-output") {
			out += "this line does not parse\n"
		}
		f, err := os.OpenFile(os.Getenv("OPSTRUSS_OUTPUT"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return fail("%v", err)
		}
		_, err = f.WriteString(out)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fail("%v", err)
		}
	case "destroy":
		for _, id := range cloud.find(key) {
			if err := os.Remove(filepath.Join(cloud.dir(), id)); err != nil {
				return fail("%v", err)
			}
		}
	case "check":
		if len(args) < 3 {
			return fail("usage: fakecloud check <dir> <state>")
		}
		n, err := cloud.check(args[2])
		if err != nil {
			return fail("%v", err)
		}
		fmt.Printf("%d resources, each with a record\n", n)
	case "serve":
		if len(args) < 4 {
			return fail("usage: fakecloud serve <dir> <port-file> <break-file>")
		}
		if err := cloud.serve(args[2], args[3]); err != nil {
			return fail("%v", err)
		}
	default:
		return fail("unknown command %s", args[0])
	}
	return 0
}

// cloud is the directory that holds the resources.
type cloud string

func (c cloud) dir() string { return string(c) }

// find returns the resources tagged with key.
func (c cloud) find(key string) []string {
	var ids []string
	ents, _ := os.ReadDir(c.dir())
	for _, e := range ents {
		if data, err := os.ReadFile(filepath.Join(c.dir(), e.Name())); err == nil && string(data) == key {
			ids = append(ids, e.Name())
		}
	}
	return ids
}

// make finds the resource of key, by the tool's state in stateDir first
// when there is one, or makes it.
func (c cloud) make(key, stateDir string) (string, error) {
	if stateDir != "" {
		if data, err := os.ReadFile(filepath.Join(stateDir, "id")); err == nil {
			if id := strings.TrimSpace(string(data)); fileExists(filepath.Join(c.dir(), id)) {
				return id, nil
			}
		}
	}
	ids := c.find(key)
	id := ""
	switch len(ids) {
	case 0:
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		id = "r-" + hex.EncodeToString(b)
		if err := os.WriteFile(filepath.Join(c.dir(), id), []byte(key), 0o600); err != nil {
			return "", err
		}
	case 1:
		id = ids[0]
	default:
		return "", fmt.Errorf("%s has %d resources", key, len(ids))
	}
	if stateDir != "" {
		if err := os.WriteFile(filepath.Join(stateDir, "id"), []byte(id+"\n"), 0o600); err != nil {
			return "", err
		}
	}
	return id, nil
}

// check counts the resources and fails when one has no record to take it
// destroy, or a unit has two.
func (c cloud) check(state string) (int, error) {
	keys := map[string]bool{}
	err := filepath.WalkDir(state, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "state.json" {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var rec struct {
			StateKey string `json:"state_key"`
			ID       string `json:"id"`
			Address  string `json:"address"`
		}
		if err := json.Unmarshal(data, &rec); err != nil {
			return fmt.Errorf("%s does not read: %w", p, err)
		}
		if rec.StateKey != "" {
			keys[rec.StateKey] = true
		} else if rec.ID != "" {
			keys[rec.Address+"."+rec.ID] = true
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	seen := map[string]string{}
	ents, _ := os.ReadDir(c.dir())
	for _, e := range ents {
		data, _ := os.ReadFile(filepath.Join(c.dir(), e.Name()))
		k := string(data)
		if !keys[k] {
			return 0, fmt.Errorf("resource %s of %s has no record to destroy it", e.Name(), k)
		}
		if other, dup := seen[k]; dup {
			return 0, fmt.Errorf("%s has two resources, %s and %s", k, other, e.Name())
		}
		seen[k] = e.Name()
	}
	return len(ents), nil
}

// serve answers POST /resources with the resource of the request's
// Idempotency-Key, made when it has none, as {"id": …}, and with a body
// that does not parse while breakFile exists. It writes its port to
// portFile and runs until it is killed.
func (c cloud) serve(portFile, breakFile string) error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	http.HandleFunc("POST /resources", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			http.Error(w, "no Idempotency-Key", http.StatusBadRequest)
			return
		}
		id, err := c.make(key, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if fileExists(breakFile) {
			_, _ = w.Write([]byte(`{"id": `))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	port := fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(portFile, []byte(port), 0o600); err != nil {
		return err
	}
	return http.Serve(l, nil)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
