package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Store is the state of one stack in a backend. It reads _meta before
// anything else: a newer layout stops the run, and a stack that belongs to
// another project is never written. Units of one run use it at once.
type Store struct {
	Backend Backend
	Stack   string
	Project string

	mu   sync.Mutex
	meta *Meta
}

// Key names of a stack.
func MetaKey(stack string) string           { return stack + "/_meta" }
func RecordKey(address string) string       { return address + "/state.json" }
func LockKey(address string) string         { return address + "/state.lock" }
func metaLockKey(stack string) string       { return stack + "/_meta.lock" }
func kvKey(stack, name string) string       { return stack + "/_kv/" + name }
func kvLockKey(stack string) string         { return stack + "/_kv.lock" }
func forgottenKey(stack, run string) string { return stack + "/_forgotten/" + run }

// IncompleteError is a stack whose records have no _meta.
type IncompleteError struct{ Stack string }

func (e *IncompleteError) Error() string {
	return fmt.Sprintf("stack %s has records but no _meta, so it is incomplete; restore _meta from _meta.backup with opstruss state push _meta", e.Stack)
}

// Open reads the stack's _meta. A local stack without records has none
// yet; the first write makes it.
func Open(b Backend, stack, project string) (*Store, error) {
	s := &Store{Backend: b, Stack: stack, Project: project}
	data, _, err := b.Get(MetaKey(stack))
	switch {
	case errors.Is(err, ErrNotExist):
		addrs, err := s.Inventory()
		if err != nil {
			return nil, err
		}
		if len(addrs) > 0 {
			return nil, &IncompleteError{Stack: stack}
		}
		return s, nil
	case err != nil:
		return nil, err
	}
	m, err := DecodeMeta(data)
	if err != nil {
		return nil, err
	}
	s.meta = m
	return s, nil
}

// Writable reports whether runs may write this stack: not when its _meta
// names another project.
func (s *Store) Writable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writable()
}

func (s *Store) writable() error {
	if s.meta != nil && s.meta.Project != s.Project {
		return fmt.Errorf("stack %s belongs to project %s, not %s; a renamed project updates it with opstruss state pull _meta and push", s.Stack, s.meta.Project, s.Project)
	}
	return nil
}

// EnsureMeta writes _meta before the stack's first record, and checks
// that the stack may be written.
func (s *Store) EnsureMeta() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meta != nil {
		return s.writable()
	}
	m := &Meta{Version: MetaVersion, Project: s.Project}
	data, err := encodeJSON(m)
	if err != nil {
		return err
	}
	_, err = s.Backend.Put(MetaKey(s.Stack), data, "")
	if errors.Is(err, ErrConflict) {
		// Another run made it first; read and check what it wrote.
		data, _, err = s.Backend.Get(MetaKey(s.Stack))
		if err != nil {
			return err
		}
		if m, err = DecodeMeta(data); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	s.meta = m
	return s.writable()
}

func (s *Store) checkAddress(address string) error {
	if !strings.HasPrefix(address, s.Stack+"/") {
		return fmt.Errorf("address %s is not in stack %s", address, s.Stack)
	}
	return nil
}

// Get returns the record at address and its etag, or nil when there is
// none.
func (s *Store) Get(address string) (*Record, string, error) {
	if err := s.checkAddress(address); err != nil {
		return nil, "", err
	}
	data, etag, err := s.Backend.Get(RecordKey(address))
	if errors.Is(err, ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	r, err := DecodeRecord(data, address)
	if err != nil {
		return nil, "", err
	}
	return r, etag, nil
}

// Put writes the record at address when it still has etag, or, with an
// empty etag, when there is no record yet.
func (s *Store) Put(address string, r *Record, etag string) (string, error) {
	if err := s.checkAddress(address); err != nil {
		return "", err
	}
	if err := s.EnsureMeta(); err != nil {
		return "", err
	}
	r.Address = address
	data, err := EncodeRecord(r)
	if err != nil {
		return "", err
	}
	return s.Backend.Put(RecordKey(address), data, etag)
}

// Delete removes the record at address when it still has etag.
func (s *Store) Delete(address, etag string) error {
	if err := s.checkAddress(address); err != nil {
		return err
	}
	if err := s.Writable(); err != nil {
		return err
	}
	return s.Backend.Delete(RecordKey(address), etag)
}

// Lock takes the lock of an address without waiting.
func (s *Store) Lock(address string, h Holder) (string, error) {
	if err := s.checkAddress(address); err != nil {
		return "", err
	}
	return s.Backend.Lock(LockKey(address), h)
}

// Unlock releases the lock of an address.
func (s *Store) Unlock(address, token string) error {
	return s.Backend.Unlock(LockKey(address), token)
}

// Inventory returns every address in the stack that has a record,
// tombstones included, read from the backend rather than from the files,
// so a new checkout finds units that left them.
func (s *Store) Inventory() ([]string, error) {
	keys, err := s.Backend.List(s.Stack + "/")
	if err != nil {
		return nil, err
	}
	var addrs []string
	for _, k := range keys {
		rest := strings.TrimPrefix(k, s.Stack+"/")
		if strings.HasPrefix(rest, "_") || !strings.HasSuffix(k, "/state.json") {
			continue
		}
		addrs = append(addrs, strings.TrimSuffix(k, "/state.json"))
	}
	return addrs, nil
}

// Forget leaves a tombstone at address instead of its record, without
// taking anything down, and moves the tool's state of a local unit to
// <stack>/_forgotten/<run>/. On a tombstone it removes the tombstone, so
// the name can be used again.
func (s *Store) Forget(address, run string) (string, error) {
	r, etag, err := s.Get(address)
	if err != nil {
		return "", err
	}
	if r == nil {
		return "", fmt.Errorf("there is no record at %s", address)
	}
	if r.Tombstone != nil {
		if err := s.Delete(address, etag); err != nil {
			return "", err
		}
		return "removed the tombstone", nil
	}
	if err := s.ForgetTool(address, run); err != nil {
		return "", err
	}
	tomb := &Record{ID: r.ID, Tombstone: &Tombstone{Forgotten: true, Run: run, Time: time.Now().UTC().Format(time.RFC3339)}}
	if _, err := s.Put(address, tomb, etag); err != nil {
		return "", err
	}
	return "forgotten; a tombstone stays", nil
}

// ForgetTool moves tool/, the tool's own state of a local unit, to
// <stack>/_forgotten/<run>/<unit>/tool, where no new unit takes it.
func (s *Store) ForgetTool(address, run string) error {
	l, ok := s.Backend.(*Local)
	if !ok {
		return nil
	}
	from := filepath.Join(l.root, filepath.FromSlash(address), "tool")
	if _, err := os.Stat(from); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	to := filepath.Join(l.root, filepath.FromSlash(forgottenKey(s.Stack, run)), filepath.FromSlash(strings.TrimPrefix(address, s.Stack+"/")), "tool")
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	return os.Rename(from, to)
}

// MoveTool moves tool/, the tool's own state of a local unit, with its
// record from one address to another. A tool/ that moved already is left
// as it is, so a move that was interrupted can be repeated.
func (s *Store) MoveTool(from, to string) error {
	l, ok := s.Backend.(*Local)
	if !ok {
		return nil
	}
	src := filepath.Join(l.root, filepath.FromSlash(from), "tool")
	dst := filepath.Join(l.root, filepath.FromSlash(to), "tool")
	if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("both %s and %s have the state of a tool", from, to)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// KeyOf names the backend key and lock of a key given to opstruss state:
// an address without its stack, _meta, or _kv/<name>.
func (s *Store) KeyOf(key string) (data, lock string, address string) {
	switch {
	case key == "_meta":
		return MetaKey(s.Stack), metaLockKey(s.Stack), ""
	case strings.HasPrefix(key, "_kv/"):
		return kvKey(s.Stack, strings.TrimPrefix(key, "_kv/")), kvLockKey(s.Stack), ""
	}
	address = s.Stack + "/" + key
	return RecordKey(address), LockKey(address), address
}
