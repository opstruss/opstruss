package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opstruss/opstruss/internal/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestLocalPut(t *testing.T) {
	root := t.TempDir()
	b := NewLocal(root)
	if _, _, err := b.Get("prod/web/state.json"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("absent key: %v", err)
	}
	e1, err := b.Put("prod/web/state.json", []byte("one"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put("prod/web/state.json", []byte("again"), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("a new key that exists: %v", err)
	}
	if _, err := b.Put("prod/web/state.json", []byte("two"), "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("an old etag: %v", err)
	}
	e2, err := b.Put("prod/web/state.json", []byte("two"), e1)
	if err != nil {
		t.Fatal(err)
	}
	data, etag, err := b.Get("prod/web/state.json")
	if err != nil || string(data) != "two" || etag != e2 {
		t.Fatalf("get: %q %s %v", data, etag, err)
	}
	backup, err := os.ReadFile(filepath.Join(root, "prod/web/state.backup.json"))
	if err != nil || string(backup) != "one" {
		t.Fatalf("backup: %q %v", backup, err)
	}
	info, err := os.Stat(filepath.Join(root, "prod/web/state.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", info.Mode(), err)
	}
	if err := b.Delete("prod/web/state.json", e1); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete with an old etag: %v", err)
	}
	if err := b.Delete("prod/web/state.json", e2); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "/abs", "../out", "a/../b"} {
		if _, err := b.Put(bad, nil, ""); err == nil {
			t.Errorf("key %q: want an error", bad)
		}
	}
}

func TestLocalLock(t *testing.T) {
	b := NewLocal(t.TempDir())
	h := NewHolder("20261020T101530Z-4b2e91d0", "host1")
	token, err := b.Lock("prod/web/state.lock", h)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Lock("prod/web/state.lock", NewHolder("other", "host1"))
	var le *LockedError
	if !errors.As(err, &le) || le.Holder.Run != h.Run || le.Holder.PID != os.Getpid() {
		t.Fatalf("a held lock names its holder: %v", err)
	}
	if err := b.Unlock("prod/web/state.lock", "wrong"); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("unlock with another token: %v", err)
	}
	if err := b.Unlock("prod/web/state.lock", token); err != nil {
		t.Fatal(err)
	}
	if err := b.Unlock("prod/web/state.lock", token); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("an old holder releases nothing: %v", err)
	}
	token2, err := b.Lock("prod/web/state.lock", NewHolder("next", "host1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Unlock("prod/web/state.lock", token); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("the old token must not release the new lock: %v", err)
	}
	if err := b.Unlock("prod/web/state.lock", token2); err != nil {
		t.Fatal(err)
	}
}

func TestStore(t *testing.T) {
	root := t.TempDir()
	b := NewLocal(root)
	s, err := Open(b, "prod", "platform")
	if err != nil {
		t.Fatal(err)
	}
	r := &Record{ID: "7f3a9c1e", StateKey: "prod/web.7f3a9c1e", Kind: "run",
		Inputs: map[string]any{"port": int64(8080), "ratio": 0.5}}
	etag, err := s.Put("prod/web", r, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Get(MetaKey("prod")); err != nil {
		t.Fatalf("the first record writes _meta: %v", err)
	}
	got, getEtag, err := s.Get("prod/web")
	if err != nil || getEtag != etag {
		t.Fatalf("get: %v, etag %s vs %s", err, getEtag, etag)
	}
	if got.Version != RecordVersion || got.Address != "prod/web" || !reflect.DeepEqual(got.Inputs, r.Inputs) {
		t.Fatalf("record %+v", got)
	}
	if _, err := s.Put("dev/web", r, ""); err == nil {
		t.Fatal("an address of another stack")
	}

	// Another project at the same place may read, never write.
	other, err := Open(b, "prod", "copy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Put("prod/db", &Record{ID: "1"}, ""); err == nil || !strings.Contains(err.Error(), "belongs to project platform") {
		t.Fatalf("a foreign stack: %v", err)
	}

	// Records without _meta stop the run.
	if err := os.Remove(filepath.Join(root, "prod", "_meta")); err != nil {
		t.Fatal(err)
	}
	var ie *IncompleteError
	if _, err := Open(b, "prod", "platform"); !errors.As(err, &ie) {
		t.Fatalf("records without _meta: %v", err)
	}

	// A newer layout or record stops the run before it reads further.
	if err := os.WriteFile(filepath.Join(root, "prod", "_meta"), []byte(`{"version": 9, "project": "platform"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var ne *NewerError
	if _, err := Open(b, "prod", "platform"); !errors.As(err, &ne) {
		t.Fatalf("a newer _meta: %v", err)
	}
	if _, err := DecodeRecord([]byte(`{"version": 2, "id": "x"}`), "prod/web"); !errors.As(err, &ne) {
		t.Fatalf("a newer record: %v", err)
	}
}

func TestInventoryAndForget(t *testing.T) {
	root := t.TempDir()
	b := NewLocal(root)
	s, err := Open(b, "prod", "platform")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"prod/web", "prod/image[lxc]", "prod/dns/zone"} {
		if _, err := s.Put(a, &Record{ID: "1"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Put("prod/_kv/last", []byte("1"), ""); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "prod/web/tool"), 0o700); err != nil {
		t.Fatal(err)
	}
	addrs, err := s.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"prod/dns/zone", "prod/image[lxc]", "prod/web"}; !reflect.DeepEqual(addrs, want) {
		t.Fatalf("inventory %v, want %v", addrs, want)
	}
	if _, err := s.Forget("prod/web", "20261020T101530Z-4b2e91d0"); err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Get("prod/web")
	if err != nil || r.Tombstone == nil || !r.Tombstone.Forgotten || r.ID != "1" {
		t.Fatalf("tombstone %+v, %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(root, "prod/_forgotten/20261020T101530Z-4b2e91d0/web/tool")); err != nil {
		t.Fatalf("tool/ goes to _forgotten: %v", err)
	}
	if msg, err := s.Forget("prod/web", "x"); err != nil || !strings.Contains(msg, "removed the tombstone") {
		t.Fatalf("forgetting a tombstone removes it: %q %v", msg, err)
	}
	if r, _, _ := s.Get("prod/web"); r != nil {
		t.Fatal("the tombstone is gone")
	}
}

// TestSchema checks what the store writes against the schemas of records
// and _meta.
func TestSchema(t *testing.T) {
	root := t.TempDir()
	s, err := Open(NewLocal(root), "prod", "platform")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHolder("20261020T101530Z-4b2e91d0", "4c1a2b3d")
	r := &Record{ID: "7f3a9c1e", StateKey: "prod/web.7f3a9c1e", Kind: "run",
		Inputs: map[string]any{"port": int64(8080)}, Outputs: map[string]any{"url": "https://x"},
		Down:   &Down{Run: []string{"./clean.sh"}, Dir: ".", Pass: map[string]string{"AWS_PROFILE": "sha256:ab"}},
		Digest: "sha256:v1:41d0", Run: "20261020T101530Z-4b2e91d0", Updated: "2026-10-20T10:15:51Z",
		Pending:   &Attempt{Op: "up", Run: "20261020T101530Z-4b2e91d0", Holder: h, Inputs: map[string]any{"port": int64(9090)}},
		Abandoned: []*Attempt{{Op: "up", Run: "20261020T091530Z-1b2e91d0", Holder: h}}}
	if _, err := s.Put("prod/web", r, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget("prod/web", "20261020T111530Z-0b2e91d0"); err != nil {
		t.Fatal(err)
	}
	for key, kind := range map[string]schema.Kind{"prod/web/state.backup.json": schema.Record, "prod/web/state.json": schema.Record, "prod/_meta": schema.Meta} {
		data, err := os.ReadFile(filepath.Join(root, key))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		probs, err := schema.Validate(kind, doc)
		if err != nil || len(probs) > 0 {
			t.Errorf("%s: %v %v", key, probs, err)
		}
	}
}
