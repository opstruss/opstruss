package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// TestCriteria runs the scripts that prove the guarantees of the
// foundation with failpoints and many iterations, in testdata/criteria;
// make criteria runs them in a build with -tags failpoints.
func TestCriteria(t *testing.T) {
	if os.Getenv("OPSTRUSS_CRITERIA") == "" {
		t.Skip("set OPSTRUSS_CRITERIA=1, as make criteria does")
	}
	testscript.Run(t, params("testdata/criteria"))
}

// fakecloud makes resources that are files in a directory, each holding
// the OPSTRUSS_STATE_KEY of the unit that made it, as a cloud tags a
// resource, so an up that runs again finds what an earlier attempt made:
//
//	fakecloud up <dir>       find or make the unit's resource; id= to $OPSTRUSS_OUTPUT
//	fakecloud down <dir>     remove the unit's resource
//	fakecloud check <dir> <state>   every resource has a record, and no unit has two
//
// FAKECLOUD_BREAK=1 makes up write output that does not parse, after it
// made the resource.
func fakecloud(args []string) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "fakecloud: "+format+"\n", a...)
		return 1
	}
	if len(args) < 2 {
		return fail("usage: fakecloud up|down|check <dir> [state]")
	}
	dir := args[1]
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail("%v", err)
	}
	key := os.Getenv("OPSTRUSS_STATE_KEY")
	find := func(key string) []string {
		var ids []string
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if data, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && string(data) == key {
				ids = append(ids, e.Name())
			}
		}
		return ids
	}
	switch args[0] {
	case "up":
		if key == "" {
			return fail("no OPSTRUSS_STATE_KEY")
		}
		ids := find(key)
		id := ""
		switch len(ids) {
		case 0:
			b := make([]byte, 4)
			_, _ = rand.Read(b)
			id = "r-" + hex.EncodeToString(b)
			if err := os.WriteFile(filepath.Join(dir, id), []byte(key), 0o600); err != nil {
				return fail("%v", err)
			}
		case 1:
			id = ids[0]
		default:
			return fail("%s has %d resources", key, len(ids))
		}
		out := "id=" + id + "\n"
		if os.Getenv("FAKECLOUD_BREAK") != "" {
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
	case "down":
		for _, id := range find(key) {
			if err := os.Remove(filepath.Join(dir, id)); err != nil {
				return fail("%v", err)
			}
		}
	case "check":
		if len(args) < 3 {
			return fail("usage: fakecloud check <dir> <state>")
		}
		keys := map[string]bool{}
		err := filepath.WalkDir(args[2], func(p string, d os.DirEntry, err error) error {
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
			return fail("%v", err)
		}
		seen := map[string]string{}
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			k := string(data)
			if !keys[k] {
				return fail("resource %s of %s has no record to take it down", e.Name(), k)
			}
			if other, dup := seen[k]; dup {
				return fail("%s has two resources, %s and %s", k, other, e.Name())
			}
			seen[k] = e.Name()
		}
		fmt.Printf("%d resources, each with a record\n", len(ents))
	default:
		return fail("unknown command %s", args[0])
	}
	return 0
}
