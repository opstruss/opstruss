package state

import (
	"errors"
	"maps"
	"slices"
	"testing"
	"time"
)

// failing fails one operation on one key, as a plugin backend may for a
// moment; with scan, it scans as a plugin backend with the capability.
type failing struct {
	Backend
	op, key string
	scan    bool
}

func (f *failing) Scan(prefix string) (map[string][]byte, error) {
	switch {
	case !f.scan:
		return nil, errors.ErrUnsupported
	case f.op == "scan":
		return nil, errFlaky
	}
	keys, err := f.Backend.List(prefix)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, k := range keys {
		if out[k], _, err = f.Backend.Get(k); err != nil {
			return nil, err
		}
	}
	return out, nil
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
	for _, c := range []struct {
		name, op string
		scan     bool
	}{
		{"list", "list", false},
		{"get", "get", false},
		{"delete", "delete", false},
		{"scan", "scan", true},
		{"get after scan", "get", true},
		{"delete after scan", "delete", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := &failing{Backend: NewLocal(t.TempDir()), scan: c.scan}
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
			b.op, b.key = c.op, kvKey("default", "old")
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

// stale scans what the backend held before a write, as a scan that ran
// just before it.
type stale struct {
	Backend
	items map[string][]byte
}

func (s stale) Scan(string) (map[string][]byte, error) { return s.items, nil }

// A name that the scan shows expired, but that was written again since,
// stays: purge decides on what get gives.
func TestKVPutPurgeStaleScan(t *testing.T) {
	start := time.Now()
	later := start.Add(time.Hour)
	local := NewLocal(t.TempDir())
	s := &Store{Backend: local, Stack: "default", Project: "p"}
	if err := s.KVPut("old", "x", false, time.Minute, "", start); err != nil {
		t.Fatal(err)
	}
	expiring, _, err := local.Get(kvKey("default", "old"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, etag, err := s.KVGet("old", start)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.KVPut("old", "fresh", false, 0, etag, start); err != nil {
		t.Fatal(err)
	}

	s = &Store{Backend: stale{Backend: local, items: map[string][]byte{kvKey("default", "old"): expiring}}, Stack: "default", Project: "p"}
	if err := s.KVPut("job", "done", false, 0, "", later); err != nil {
		t.Fatal(err)
	}
	if v, _, _, err := s.KVGet("old", later); err != nil || v != "fresh" {
		t.Errorf("old = %v, %v; want fresh", v, err)
	}
}

// Reads through a scan give what reads that list and get each name give.
func TestKVReadScan(t *testing.T) {
	start := time.Now()
	later := start.Add(time.Hour)
	var infos [][]KVInfo
	for _, scan := range []bool{false, true} {
		s := &Store{Backend: &failing{Backend: NewLocal(t.TempDir()), scan: scan}, Stack: "default", Project: "p"}
		for name, ttl := range map[string]time.Duration{"pr-1": 0, "pr-2": 0, "pr-gone": time.Minute, "other": 0} {
			if err := s.KVPut(name, name, false, ttl, "", start); err != nil {
				t.Fatal(err)
			}
		}
		for prefix, want := range map[string]map[string]any{
			"pr-": {"pr-1": "pr-1", "pr-2": "pr-2"},
			"":    {"pr-1": "pr-1", "pr-2": "pr-2", "other": "other"},
		} {
			if m, _, err := s.KVList(prefix, later); err != nil || !maps.Equal(m, want) {
				t.Errorf("scan %v: kv('%s*') = %v, %v; want %v", scan, prefix, m, err, want)
			}
		}
		info, err := s.KVInfos()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := s.List(func(string, *Record) error { return nil }); err != nil || !slices.Equal(got, info) {
			t.Errorf("scan %v: List gives the names %v, %v; want %v", scan, got, err, info)
		}
		infos = append(infos, info)
	}
	if len(infos[0]) != 4 || !slices.Equal(infos[0], infos[1]) {
		t.Errorf("the names are %v without scan, %v with it", infos[0], infos[1])
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
