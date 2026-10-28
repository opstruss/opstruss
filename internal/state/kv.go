package state

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	Version   int    `json:"version"`
	Value     any    `json:"value"`
	Sensitive bool   `json:"sensitive,omitempty"` // Value is encrypted with the data key
	Expires   string `json:"expires,omitempty"`
}

// kvAAD binds an encrypted value to its name, so it cannot move to
// another.
func kvAAD(name string) string { return "_kv/" + name }

// open decrypts a sensitive value with the data key of the store.
func (s *Store) openKV(e *KVEntry, name string) error {
	if !e.Sensitive {
		return nil
	}
	text, _ := e.Value.(string)
	sl := &sealer{key: s.Key}
	key, err := sl.dataKey()
	if err != nil {
		return fmt.Errorf("kv %s: %w", name, err)
	}
	if e.Value, err = Unseal(key, kvAAD(name), text); err != nil {
		return fmt.Errorf("kv %s: %w", name, err)
	}
	return nil
}

func (e *KVEntry) expired(now time.Time) bool {
	t, err := time.Parse(time.RFC3339Nano, e.Expires)
	return err == nil && !now.Before(t)
}

func decodeKV(data []byte, name string) (*KVEntry, error) {
	if err := CheckVersion(data, "version", "kv "+name, KVVersion); err != nil {
		return nil, err
	}
	var e KVEntry
	if err := decodeJSON(data, &e); err != nil {
		return nil, fmt.Errorf("kv %s: %w", name, err)
	}
	e.Value = fixNumbers(e.Value)
	return &e, nil
}

// KVGet returns the value of name, whether it is sensitive, and its etag,
// or nil when there is none or its ttl passed.
func (s *Store) KVGet(name string, now time.Time) (any, bool, string, error) {
	if err := CheckKVName(name); err != nil {
		return nil, false, "", err
	}
	data, etag, err := s.Backend.Get(kvKey(s.Stack, name))
	if errors.Is(err, ErrNotExist) {
		return nil, false, "", nil
	}
	if err != nil {
		return nil, false, "", err
	}
	e, err := decodeKV(data, name)
	if err != nil {
		return nil, false, "", err
	}
	if e.expired(now) {
		return nil, false, etag, nil
	}
	if err := s.openKV(e, name); err != nil {
		return nil, false, "", err
	}
	return e.Value, e.Sensitive, etag, nil
}

// KVList returns the values of the names that start with prefix, at most
// MaxKVKeys of them and MaxKV in all.
// The names of sensitive values come back too.
func (s *Store) KVList(prefix string, now time.Time) (map[string]any, map[string]bool, error) {
	if prefix != "" {
		if err := CheckKVName(prefix); err != nil {
			return nil, nil, err
		}
	}
	keys, err := s.Backend.List(kvKey(s.Stack, ""))
	if err != nil {
		return nil, nil, err
	}
	out, sensitive := map[string]any{}, map[string]bool{}
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
			return nil, nil, err
		}
		e, err := decodeKV(data, name)
		if err != nil {
			return nil, nil, err
		}
		if e.expired(now) {
			continue
		}
		if size += len(data); len(out) >= MaxKVKeys || size > MaxKV {
			return nil, nil, fmt.Errorf("kv('%s*') reads more than %d keys or %d bytes", prefix, MaxKVKeys, MaxKV)
		}
		if err := s.openKV(e, name); err != nil {
			return nil, nil, err
		}
		out[name] = e.Value
		if e.Sensitive {
			sensitive[name] = true
		}
	}
	return out, sensitive, nil
}

// KVInfo is a name of the memory of a stack as state list shows it,
// without its value.
type KVInfo struct {
	Key       string `json:"key"` // _kv/<name>
	Sensitive bool   `json:"sensitive,omitempty"`
	Expires   string `json:"expires,omitempty"`
}

// KVInfos lists the names of the memory of the stack, also those whose ttl
// passed, which the next write removes.
func (s *Store) KVInfos() ([]KVInfo, error) {
	keys, err := s.Backend.List(kvKey(s.Stack, ""))
	if err != nil {
		return nil, err
	}
	out := []KVInfo{}
	for _, k := range keys {
		name := strings.TrimPrefix(k, kvKey(s.Stack, ""))
		if CheckKVName(name) != nil {
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
		out = append(out, KVInfo{Key: "_kv/" + name, Sensitive: e.Sensitive, Expires: e.Expires})
	}
	slices.SortFunc(out, func(a, b KVInfo) int { return strings.Compare(a.Key, b.Key) })
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
// A sensitive value is kept encrypted with the data key.
func (s *Store) KVPut(name string, value any, sensitive bool, ttl time.Duration, etag string, now time.Time) error {
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
		e := &KVEntry{Version: KVVersion, Value: floats(value)}
		if sensitive {
			key, err := (&sealer{key: s.Key}).dataKey()
			if err != nil {
				return fmt.Errorf("kv %s: %w", name, err)
			}
			if e.Value, err = Seal(key, kvAAD(name), value); err != nil {
				return err
			}
			e.Sensitive = true
		}
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
