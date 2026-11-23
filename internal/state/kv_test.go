package state

import (
	"errors"
	"testing"
	"time"
)

// failing fails one operation on one key, as a plugin backend may for a
// moment.
type failing struct {
	Backend
	op, key string
}

var errFlaky = errors.New("flaky")

func (f *failing) Get(key string) ([]byte, string, error) {
	if f.op == "get" && key == f.key {
		return nil, "", errFlaky
	}
	return f.Backend.Get(key)
}

func (f *failing) Delete(key, ifMatch string) error {
	if f.op == "delete" && key == f.key {
		return errFlaky
	}
	return f.Backend.Delete(key, ifMatch)
}

func (f *failing) List(prefix string) ([]string, error) {
	if f.op == "list" {
		return nil, errFlaky
	}
	return f.Backend.List(prefix)
}

func TestKVPutPurge(t *testing.T) {
	start := time.Now()
	later := start.Add(time.Hour)
	for _, op := range []string{"list", "get", "delete"} {
		t.Run(op, func(t *testing.T) {
			b := &failing{Backend: NewLocal(t.TempDir())}
			s := &Store{Backend: b, Stack: "default", Project: "p"}
			if err := s.KVPut("old", "x", false, time.Minute, "", start); err != nil {
				t.Fatal(err)
			}
			if err := s.KVPut("job", "pending", false, 0, "", start); err != nil {
				t.Fatal(err)
			}
			_, _, etag, err := s.KVGet("job", later)
			if err != nil {
				t.Fatal(err)
			}

			// A write that cannot remove an expired name fails before it
			// changes anything, so the same write can be tried again.
			b.op, b.key = op, kvKey("default", "old")
			if err := s.KVPut("job", "done", false, 0, etag, later); !errors.Is(err, errFlaky) {
				t.Fatalf("put = %v, want %v", err, errFlaky)
			}
			b.op = ""
			v, _, etag2, err := s.KVGet("job", later)
			if err != nil || v != "pending" || etag2 != etag {
				t.Fatalf("job = %v at %q, %v; want pending at %q", v, etag2, err, etag)
			}

			if err := s.KVPut("job", "done", false, 0, etag, later); err != nil {
				t.Fatal(err)
			}
			if v, _, _, _ := s.KVGet("job", later); v != "done" {
				t.Errorf("job = %v, want done", v)
			}
			if _, _, err := b.Get(kvKey("default", "old")); !errors.Is(err, ErrNotExist) {
				t.Errorf("old is still kept: %v", err)
			}
		})
	}
}

// An expired name that is written again keeps the etag its writer read.
func TestKVPutExpiredTarget(t *testing.T) {
	start := time.Now()
	later := start.Add(time.Hour)
	s := &Store{Backend: NewLocal(t.TempDir()), Stack: "default", Project: "p"}
	if err := s.KVPut("job", "old", false, time.Minute, "", start); err != nil {
		t.Fatal(err)
	}
	v, _, etag, err := s.KVGet("job", later)
	if err != nil || v != nil || etag == "" {
		t.Fatalf("job = %v at %q, %v; want absent with an etag", v, etag, err)
	}
	if err := s.KVPut("job", "new", false, 0, etag, later); err != nil {
		t.Fatal(err)
	}
	if v, _, _, _ := s.KVGet("job", later); v != "new" {
		t.Errorf("job = %v, want new", v)
	}
}
