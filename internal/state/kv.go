package state

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// KVVersion is the version of a value that kv keeps.
	KVVersion = 1
	// MaxKV is the largest value that kv keeps, and the most that one read
	// of a prefix returns.
	MaxKV = 1 << 20
	// MaxKVKeys is the most keys that one read of a prefix returns.
	MaxKVKeys = 1000
)

// kvName is the rule of names in kv, that of the keys of for_each, starting
// with a letter or digit so no name is a path of its own.
var kvName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// CheckKVName says why name cannot be a key of kv.
func CheckKVName(name string) error {
	if !kvName.MatchString(name) {
		return fmt.Errorf("kv key %q takes lower-case letters, digits, '.', '-', and '_', and starts with a letter or digit; shape it with lowerAscii() or replace()", name)
	}
	return nil
}

// KVEntry is one value in the memory of a stack, with the time it expires
// when it was written with a ttl.
type KVEntry struct {
	Version int    `json:"version"`
	Value   any    `json:"value"`
	Expires string `json:"expires,omitempty"`
}

func (e *KVEntry) expired(now time.Time) bool {
	t, err := time.Parse(time.RFC3339Nano, e.Expires)
	return err == nil && !now.Before(t)
}

func decodeKV(data []byte, name string) (*KVEntry, error) {
	var e KVEntry
	if err := decodeJSON(data, &e); err != nil {
		return nil, fmt.Errorf("kv %s: %w", name, err)
	}
	if e.Version > KVVersion {
		return nil, &NewerError{What: "kv " + name, Version: e.Version, Known: KVVersion}
	}
	e.Value = fixNumbers(e.Value)
	return &e, nil
}

// KVGet returns the value of name and its etag, or nil when there is none
// or its ttl passed.
func (s *Store) KVGet(name string, now time.Time) (any, string, error) {
	if err := CheckKVName(name); err != nil {
		return nil, "", err
	}
	data, etag, err := s.Backend.Get(kvKey(s.Stack, name))
	if errors.Is(err, ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	e, err := decodeKV(data, name)
	if err != nil {
		return nil, "", err
	}
	if e.expired(now) {
		return nil, etag, nil
	}
	return e.Value, etag, nil
}

// KVList returns the values of the names that start with prefix, at most
// MaxKVKeys of them and MaxKV in all.
func (s *Store) KVList(prefix string, now time.Time) (map[string]any, error) {
	if prefix != "" {
		if err := CheckKVName(prefix); err != nil {
			return nil, err
		}
	}
	keys, err := s.Backend.List(kvKey(s.Stack, ""))
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	size := 0
	for _, k := range keys {
		name := strings.TrimPrefix(k, kvKey(s.Stack, ""))
		if !strings.HasPrefix(name, prefix) || CheckKVName(name) != nil {
			continue
		}
		data, _, err := s.Backend.Get(k)
		if errors.Is(err, ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		e, err := decodeKV(data, name)
		if err != nil {
			return nil, err
		}
		if e.expired(now) {
			continue
		}
		if size += len(data); len(out) >= MaxKVKeys || size > MaxKV {
			return nil, fmt.Errorf("kv('%s*') reads more than %d keys or %d bytes", prefix, MaxKVKeys, MaxKV)
		}
		out[name] = e.Value
	}
	return out, nil
}

// KVLock takes the lock of the memory of the stack without waiting; one
// lock holds every name, since a local lock file is never removed.
func (s *Store) KVLock(h Holder) (string, error) { return s.Backend.Lock(kvLockKey(s.Stack), h) }

// KVUnlock releases the lock of the memory of the stack.
func (s *Store) KVUnlock(token string) error { return s.Backend.Unlock(kvLockKey(s.Stack), token) }

// KVPut writes value under name, still at etag, or removes it when value
// is nil; ttl, when above zero, makes it expire. The caller holds the lock,
// and every write also removes the names whose ttl passed.
func (s *Store) KVPut(name string, value any, ttl time.Duration, etag string, now time.Time) error {
	if err := CheckKVName(name); err != nil {
		return err
	}
	if err := s.EnsureMeta(); err != nil {
		return err
	}
	key := kvKey(s.Stack, name)
	if value == nil {
		if etag != "" {
			if err := s.Backend.Delete(key, etag); err != nil && !errors.Is(err, ErrNotExist) {
				return err
			}
		}
	} else {
		e := &KVEntry{Version: KVVersion, Value: value}
		if ttl > 0 {
			e.Expires = now.Add(ttl).UTC().Format(time.RFC3339Nano)
		}
		data, err := encodeJSON(e)
		if err != nil {
			return err
		}
		if len(data) > MaxKV {
			return fmt.Errorf("kv %s: the value takes %d bytes, more than %d", name, len(data), MaxKV)
		}
		if _, err := s.Backend.Put(key, data, etag); err != nil {
			return err
		}
	}
	return s.purge(now)
}

// purge removes the names whose ttl passed.
func (s *Store) purge(now time.Time) error {
	keys, err := s.Backend.List(kvKey(s.Stack, ""))
	if err != nil {
		return err
	}
	for _, k := range keys {
		data, etag, err := s.Backend.Get(k)
		if errors.Is(err, ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		e, err := decodeKV(data, strings.TrimPrefix(k, kvKey(s.Stack, "")))
		if err != nil {
			continue
		}
		if e.expired(now) {
			if err := s.Backend.Delete(k, etag); err != nil && !errors.Is(err, ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
